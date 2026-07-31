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
func seedPricedTicketType(t *testing.T, e *env, amount int64, currency string) (ttID uuid.UUID, scopes store.PricingScopes) {
	t.Helper()
	ttID, scopes = uuid.New(), store.PricingScopes{}
	scopes.SlotID, scopes.EventID, scopes.VenueID = uuid.New(), uuid.New(), uuid.New()
	seriesID := uuid.New()
	scopes.SeriesID = &seriesID

	e.store.ticketTypes[ttID] = store.TicketType{
		ID: ttID, PerformanceID: scopes.SlotID, PriceAmount: amount, Currency: currency,
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
	if out.ResolverVersion != store.PricingResolverVersion {
		t.Errorf("ResolverVersion = %d, want %d", out.ResolverVersion, store.PricingResolverVersion)
	}
	// TKT-152 fills these; declared now so TKT-153's persisted snapshot shape
	// does not change between the two stories.
	if out.Winner.EffectiveFrom != nil || out.Winner.EffectiveUntil != nil {
		t.Errorf("window fields = %v/%v, want null until TKT-152",
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
