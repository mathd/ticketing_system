package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// API-level tests for TKT-151's price resolution. The comparator itself is
// proved by the store package's truth table (ADR-036 §4); what is under test
// here is the handler, the contract mapping and the cache tier — every response
// below goes through env.do, so it is validated against the committed OpenAPI
// document and counted by the coverage gate (ADR-030: catalog covers its own
// operations, it is not in the smoke happy-path gate).

// seedPricedTicketType wires a ticket type to a full scope chain in the fake
// store so resolution has all five levels available.
// pricingOrganizer is fixed so the identity fields can be ASSERTED. They were
// added for TKT-153, which trusts them to authorize a sale and to place a hold —
// an unset fixture would have let a regression emit a zero organizer with no
// test noticing.
var pricingOrganizer = uuid.MustParse("0e9a0000-0000-0000-0000-00000000000a")

func seedPricedTicketType(t *testing.T, e *env, amount int64, currency string) (ttID uuid.UUID, scopes store.PricingScopes) {
	t.Helper()
	ttID, scopes = uuid.New(), store.PricingScopes{}
	scopes.SlotID, scopes.EventID, scopes.VenueID = uuid.New(), uuid.New(), uuid.New()
	seriesID := uuid.New()
	scopes.SeriesID = &seriesID

	e.store.ticketTypes[ttID] = store.TicketType{
		ID: ttID, OrganizerID: pricingOrganizer, PerformanceID: scopes.SlotID,
		PriceAmount: amount, Currency: currency,
	}
	e.store.performances[scopes.SlotID] = store.Performance{ID: scopes.SlotID, EventID: scopes.EventID}
	e.store.events[scopes.EventID] = store.Event{ID: scopes.EventID}
	e.store.venues[scopes.VenueID] = store.Venue{ID: scopes.VenueID}
	e.store.series[seriesID] = store.Series{ID: seriesID}
	if e.store.priceScope == nil {
		e.store.priceScope = map[uuid.UUID]store.PricingScopes{}
	}
	scopes.TicketTypeID = ttID
	e.store.priceScope[ttID] = scopes
	return ttID, scopes
}

func resolvePrice(t *testing.T, e *env, ttID uuid.UUID) (*httptest.ResponseRecorder, PriceResolution) {
	t.Helper()
	rec := e.do(http.MethodGet, "/ticket-types/"+ttID.String()+"/price-resolution", nil)
	var out PriceResolution
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec, out
}

// The coverage-gate happy path: a real winner, real losers, and the full
// provenance the contract promises.
func TestResolveTicketTypePrice(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")

	// A venue house rule and a narrower event rule. The event rule must win,
	// and the venue rule must be REPORTED as having lost, not dropped — that
	// is what makes "which level won" falsifiable (ADR-036 §5).
	venueRule, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeVenue, ScopeID: scopes.VenueID, Amount: 6000, Currency: "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	eventRule, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeEvent, ScopeID: scopes.EventID, Amount: 5000, Currency: "EUR"})
	if err != nil {
		t.Fatal(err)
	}

	rec, out := resolvePrice(t, e, ttID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — a resolved price feeds a money decision", got)
	}
	if out.ResolvedPrice.Amount != 5000 || out.ResolvedPrice.Currency != "EUR" {
		t.Errorf("ResolvedPrice = %+v, want 5000 EUR", out.ResolvedPrice)
	}
	if out.BasePrice.Amount != 4550 {
		t.Errorf("BasePrice = %d, want the ticket type's own 4550", out.BasePrice.Amount)
	}
	if out.Winner == nil || out.Winner.RuleId != eventRule.ID {
		t.Fatalf("Winner = %+v, want the event rule %v", out.Winner, eventRule.ID)
	}
	if out.Winner.ScopeLevel != "event" {
		t.Errorf("Winner.ScopeLevel = %q, want event", out.Winner.ScopeLevel)
	}
	if len(out.Candidates) != 1 {
		t.Fatalf("got %d losers, want 1: %+v", len(out.Candidates), out.Candidates)
	}
	if out.Candidates[0].Rule.RuleId != venueRule.ID {
		t.Errorf("loser = %v, want the venue rule %v", out.Candidates[0].Rule.RuleId, venueRule.ID)
	}
	if out.Candidates[0].Reason != "less_specific" {
		t.Errorf("loser reason = %q, want less_specific", out.Candidates[0].Reason)
	}
	if out.FallbackReason != nil {
		t.Errorf("FallbackReason = %v, want none when a rule won", *out.FallbackReason)
	}
	// Asserted as a LITERAL, not against the constant: comparing the response
	// to the constant it came from passes at any version, including one bumped
	// by accident. TKT-152 raised it 1 -> 2 because window eligibility changes
	// what the comparator means, and TKT-153 persists this number.
	// Identity: commerce authorizes the sale with organizer_id and places the
	// hold against performance_id, so a zero here is a defect it cannot detect.
	if out.OrganizerId != pricingOrganizer {
		t.Errorf("organizer_id = %v, want %v", out.OrganizerId, pricingOrganizer)
	}
	if out.PerformanceId != scopes.SlotID {
		t.Errorf("performance_id = %v, want the ticket type's slot %v", out.PerformanceId, scopes.SlotID)
	}
	if out.ResolverVersion != 2 {
		t.Errorf("ResolverVersion = %d, want 2", out.ResolverVersion)
	}
	// This winner carries no window, so both bounds are null — the unbounded
	// case, which must stay representable now that windows exist.
	if out.Winner.EffectiveFrom != nil || out.Winner.EffectiveUntil != nil {
		t.Errorf("window fields = %v/%v, want null for an unbounded rule",
			out.Winner.EffectiveFrom, out.Winner.EffectiveUntil)
	}
	if time.Since(out.EvaluatedAt) > time.Minute {
		t.Errorf("EvaluatedAt = %v, want the server's own clock", out.EvaluatedAt)
	}
}

// The regression that matters most: a catalog with no rules prices exactly as
// it did before price_rules existed.
func TestResolveTicketTypePriceFallsBackToBasePrice(t *testing.T) {
	e := newEnv(t)
	ttID, _ := seedPricedTicketType(t, e, 4550, "EUR")

	rec, out := resolvePrice(t, e, ttID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if out.ResolvedPrice.Amount != 4550 || out.ResolvedPrice.Currency != "EUR" {
		t.Errorf("ResolvedPrice = %+v, want the unchanged base 4550 EUR", out.ResolvedPrice)
	}
	if out.Winner != nil {
		t.Errorf("Winner = %+v, want none", out.Winner)
	}
	if out.FallbackReason == nil || string(*out.FallbackReason) != store.FallbackNoEligibleRule {
		t.Errorf("FallbackReason = %v, want %q", out.FallbackReason, store.FallbackNoEligibleRule)
	}
	if len(out.Candidates) != 0 {
		t.Errorf("Candidates = %+v, want empty", out.Candidates)
	}
}

// A rule whose currency differs from the ticket type's is invalid configuration
// in OUR data (ADR-036 §2). It must fail loudly, and as a 5xx rather than a 4xx:
// no change to the request fixes it, so it is not client-actionable. The rule id
// must not reach the response — this operation is publicly routable.
func TestResolveTicketTypePriceCurrencyMismatchFailsClosed(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	rule, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeEvent, ScopeID: scopes.EventID, Amount: 5000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}

	rec := e.do(http.MethodGet, "/ticket-types/"+ttID.String()+"/price-resolution", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a misconfigured rule is not client-actionable", rec.Code)
	}
	if body := rec.Body.String(); containsUUID(body, rule.ID) {
		t.Errorf("response leaks the offending rule id: %s", body)
	}
}

func TestResolveTicketTypePriceNotFound(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/ticket-types/"+uuid.New().String()+"/price-resolution", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func containsUUID(body string, id uuid.UUID) bool {
	return strings.Contains(body, id.String())
}

// A populated window must survive the contract round trip, and a rule that lost
// on TIME must appear in the response with its own reason — the window fields
// were declared by TKT-151 and unreachable until now, so this is the first test
// that can prove they carry values rather than nulls.
func TestResolveTicketTypePriceReportsWindows(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	// Offsets from now, never calendar literals: a literal fixture is green at
	// merge and fails once the clock crosses it.
	base := time.Now().UTC()
	expiredFrom, expiredUntil := base.Add(-48*time.Hour), base.Add(-24*time.Hour)
	liveFrom := base.Add(-time.Hour)

	expired, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeVenue, ScopeID: scopes.VenueID, Amount: 3000, Currency: "EUR",
		EffectiveFrom: &expiredFrom, EffectiveUntil: &expiredUntil})
	if err != nil {
		t.Fatal(err)
	}
	live, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeEvent, ScopeID: scopes.EventID, Amount: 5000, Currency: "EUR",
		EffectiveFrom: &liveFrom})
	if err != nil {
		t.Fatal(err)
	}

	rec, out := resolvePrice(t, e, ttID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if out.Winner == nil || out.Winner.RuleId != live.ID {
		t.Fatalf("Winner = %+v, want the live rule %v", out.Winner, live.ID)
	}
	if out.Winner.EffectiveFrom == nil || !out.Winner.EffectiveFrom.Equal(liveFrom) {
		t.Errorf("winner effective_from = %v, want %v", out.Winner.EffectiveFrom, liveFrom)
	}
	if out.Winner.EffectiveUntil != nil {
		t.Errorf("winner effective_until = %v, want null (open-ended)", out.Winner.EffectiveUntil)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].Rule.RuleId != expired.ID {
		t.Fatalf("losers = %+v, want the expired rule reported", out.Candidates)
	}
	if out.Candidates[0].Reason != "outside_window_past" {
		t.Errorf("loser reason = %q, want outside_window_past", out.Candidates[0].Reason)
	}
	if out.Candidates[0].Rule.EffectiveUntil == nil {
		t.Error("the expired rule's window must be reported, not nulled — it is the answer to why it lost")
	}
}

// The schema cannot express it (OpenAPI 3.0 has no dependentRequired, and a
// oneOf would churn both generated clients for a pair nobody branches on), so
// the invariant is pinned here instead: exactly one of "a winner exists" and
// "fallback_reason is present" holds, in both directions. Without this the
// published contract would permit a state the server never produces and nothing
// would notice if the server started producing it.
func TestResolveTicketTypePriceWinnerAndFallbackAreExclusive(t *testing.T) {
	e := newEnv(t)
	withRule, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	if _, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeEvent, ScopeID: scopes.EventID, Amount: 5000, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	withoutRule, _ := seedPricedTicketType(t, e, 4550, "EUR")

	// The third shape, and the one a naive implementation misses: a fallback
	// that still HAS candidates. Mapping fallback_reason only when the
	// candidate list is empty passes the other two cases and produces
	// winner: null with no fallback_reason here.
	allExpired, expiredScopes := seedPricedTicketType(t, e, 4550, "EUR")
	past, pastEnd := time.Now().UTC().Add(-48*time.Hour), time.Now().UTC().Add(-24*time.Hour)
	if _, err := e.store.CreatePriceRule(t.Context(), store.PriceRuleInput{
		ScopeLevel: store.ScopeEvent, ScopeID: expiredScopes.EventID, Amount: 5000, Currency: "EUR",
		EffectiveFrom: &past, EffectiveUntil: &pastEnd}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		id             uuid.UUID
		wantWinner     bool
		wantCandidates int
	}{
		"a rule won":                   {withRule, true, 0},
		"no rules at all":              {withoutRule, false, 0},
		"every rule window-ineligible": {allExpired, false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, out := resolvePrice(t, e, tc.id)
			hasWinner, hasFallback := out.Winner != nil, out.FallbackReason != nil
			if hasWinner != tc.wantWinner {
				t.Fatalf("winner present = %t, want %t", hasWinner, tc.wantWinner)
			}
			if len(out.Candidates) != tc.wantCandidates {
				t.Errorf("candidates = %d, want %d", len(out.Candidates), tc.wantCandidates)
			}
			if hasWinner == hasFallback {
				t.Errorf("winner present = %t and fallback_reason present = %t — exactly one must hold",
					hasWinner, hasFallback)
			}
		})
	}
}
