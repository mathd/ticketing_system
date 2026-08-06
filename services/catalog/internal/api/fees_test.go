package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/store"
)

// API-level tests for TKT-214's fee resolution. The comparator is proved by the
// store package's truth table (ADR-046); what is under test here is the guard,
// the handler, the contract mapping and the cache tier. Every response goes
// through env.do, so it is validated against the committed OpenAPI document and
// counted by the coverage gate (ADR-030).

const feeInternalToken = "test-internal-token"

func feePath(ttID uuid.UUID, channel string) string {
	p := "/internal/ticket-types/" + ttID.String() + "/fee-resolution"
	if channel != "" {
		p += "?channel_code=" + channel
	}
	return p
}

func resolveFees(t *testing.T, e *env, ttID uuid.UUID, channel string) (*httptest.ResponseRecorder, FeeResolution) {
	t.Helper()
	rec := e.doWithHeaders(http.MethodGet, feePath(ttID, channel), nil,
		map[string]string{"X-Internal-Token": feeInternalToken})
	var out FeeResolution
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec, out
}

// addFeeRule seeds through the store's own write gate, so a test cannot seed a
// rule production would refuse.
func addFeeRule(t *testing.T, e *env, in store.FeeRuleInput) store.FeeRule {
	t.Helper()
	r, err := e.store.CreateFeeRule(t.Context(), in)
	if err != nil {
		t.Fatalf("seed fee rule: %v", err)
	}
	return r
}

func fixedFee(scopes store.PricingScopes, level store.ScopeLevel, code string, amount int64) store.FeeRuleInput {
	var scopeID uuid.UUID
	switch level {
	case store.ScopeTicketType:
		scopeID = scopes.TicketTypeID
	case store.ScopeSlot:
		scopeID = scopes.SlotID
	case store.ScopeSeries:
		scopeID = *scopes.SeriesID
	case store.ScopeEvent:
		scopeID = scopes.EventID
	case store.ScopeVenue:
		scopeID = scopes.VenueID
	}
	a := amount
	return store.FeeRuleInput{
		OrganizerID: pricingOrganizer, ScopeLevel: level, ScopeID: scopeID,
		FeeCode: code, Basis: store.BasisPerTicketFixed, Amount: &a,
		Currency: "EUR", Incidence: store.IncidencePassedOn,
	}
}

// The headline: one winner per code, several codes at once, and the losing rule
// attributed to ITS code. The fixture carries two rules sharing a code and a
// third on another code — a single rule per code could not tell additive
// multi-code resolution from accidental single-rule behaviour.
func TestResolveTicketTypeFeesResolvesPerCode(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	house := addFeeRule(t, e, fixedFee(scopes, store.ScopeVenue, "service", 300))
	specific := addFeeRule(t, e, fixedFee(scopes, store.ScopeTicketType, "service", 500))
	facility := addFeeRule(t, e, fixedFee(scopes, store.ScopeEvent, "facility", 150))

	rec, out := resolveFees(t, e, ttID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if len(out.Fees) != 2 {
		t.Fatalf("want two fee codes, got %d: %+v", len(out.Fees), out.Fees)
	}
	if out.Fees[0].FeeCode != "facility" || out.Fees[1].FeeCode != "service" {
		t.Errorf("codes must be ordered for a stable document, got %q then %q",
			out.Fees[0].FeeCode, out.Fees[1].FeeCode)
	}
	if out.Fees[1].Winner == nil || out.Fees[1].Winner.RuleId != specific.ID {
		t.Errorf("the narrower service rule must win, got %+v", out.Fees[1].Winner)
	}
	if len(out.Fees[1].Candidates) != 1 || out.Fees[1].Candidates[0].Rule.RuleId != house.ID {
		t.Fatalf("the house rule must be the only service loser, got %+v", out.Fees[1].Candidates)
	}
	if out.Fees[1].Candidates[0].Reason != LosingFeeRuleReason(store.ReasonLessSpecific) {
		t.Errorf("reason = %q, want less_specific", out.Fees[1].Candidates[0].Reason)
	}
	if out.Fees[0].Winner == nil || out.Fees[0].Winner.RuleId != facility.ID {
		t.Errorf("facility winner = %+v, want %s", out.Fees[0].Winner, facility.ID)
	}
	// Identity travels with the answer so a consumer needs one call, not three.
	if out.OrganizerId != pricingOrganizer {
		t.Errorf("organizer_id = %s, want %s", out.OrganizerId, pricingOrganizer)
	}
	if out.PerformanceId != scopes.SlotID {
		t.Errorf("performance_id = %s, want %s", out.PerformanceId, scopes.SlotID)
	}
	if out.Currency != "EUR" {
		t.Errorf("currency = %q, want EUR", out.Currency)
	}
	if out.ResolverVersion != store.FeeResolverVersion {
		t.Errorf("resolver_version = %d, want %d", out.ResolverVersion, store.FeeResolverVersion)
	}
	if out.EvaluatedAt.IsZero() {
		t.Error("evaluated_at must be the server's instant, not the zero time")
	}
}

// Channel selectivity end to end, including the part that must NOT appear: a
// rule belonging to another channel is absent from provenance entirely.
func TestResolveTicketTypeFeesIsChannelSelective(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	agnostic := addFeeRule(t, e, fixedFee(scopes, store.ScopeEvent, "service", 300))

	reseller := fixedFee(scopes, store.ScopeEvent, "service", 900)
	code := "reseller"
	reseller.ChannelCode = &code
	resellerRule := addFeeRule(t, e, reseller)

	presale := fixedFee(scopes, store.ScopeEvent, "service", 100)
	other := "presale"
	presale.ChannelCode = &other
	presaleRule := addFeeRule(t, e, presale)

	rec, out := resolveFees(t, e, ttID, "reseller")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if out.ChannelCode == nil || *out.ChannelCode != "reseller" {
		t.Errorf("the resolution must echo which question was asked, got %v", out.ChannelCode)
	}
	if len(out.Fees) != 1 || out.Fees[0].Winner == nil {
		t.Fatalf("want one resolved code, got %+v", out.Fees)
	}
	if out.Fees[0].Winner.RuleId != resellerRule.ID {
		t.Errorf("the exact-channel rule must win, got %s", out.Fees[0].Winner.RuleId)
	}
	sawAgnostic := false
	for _, c := range out.Fees[0].Candidates {
		if c.Rule.RuleId == presaleRule.ID {
			t.Error("another channel's rule leaked into provenance")
		}
		if c.Rule.RuleId == agnostic.ID {
			sawAgnostic = true
			if c.Reason != LosingFeeRuleReason(store.ReasonLessChannelSpecific) {
				t.Errorf("reason = %q, want less_channel_specific", c.Reason)
			}
		}
	}
	if !sawAgnostic {
		t.Error("the channel-agnostic rule competed and lost; it must be reported")
	}

	// Omitting the channel is the default/public context, NOT a wildcard.
	_, pub := resolveFees(t, e, ttID, "")
	if len(pub.Fees) != 1 || pub.Fees[0].Winner == nil || pub.Fees[0].Winner.RuleId != agnostic.ID {
		t.Errorf("with no channel only the agnostic rule may apply, got %+v", pub.Fees)
	}
	if pub.ChannelCode != nil {
		t.Errorf("channel_code must be null in the default context, got %v", *pub.ChannelCode)
	}
}

// A ticket type with no fee rules — i.e. every ticket type that existed before
// this operation did — resolves to an empty ARRAY, never null.
func TestResolveTicketTypeFeesWithNoRulesIsAnEmptyArray(t *testing.T) {
	e := newEnv(t)
	ttID, _ := seedPricedTicketType(t, e, 4550, "EUR")

	rec, out := resolveFees(t, e, ttID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if len(out.Fees) != 0 {
		t.Errorf("want no fees, got %+v", out.Fees)
	}
	// The wire form matters, not just the decoded length: `null` and `[]` decode
	// to the same Go value and are different documents to every other consumer.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["fees"]) != "[]" {
		t.Errorf("fees serialized as %s, want []", raw["fees"])
	}
}

// A code whose every rule is outside its window is reported with a null winner
// and the expired rules as the reason — "considered, nothing applies" is a
// different answer from "no such code", and the second one is what a support
// question needs.
func TestResolveTicketTypeFeesReportsAnExpiredCode(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	closed := fixedFee(scopes, store.ScopeVenue, "booking", 200)
	past := time.Now().UTC().Add(-time.Hour)
	closed.EffectiveUntil = &past
	expired := addFeeRule(t, e, closed)

	rec, out := resolveFees(t, e, ttID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if len(out.Fees) != 1 {
		t.Fatalf("the expired code must still be reported, got %+v", out.Fees)
	}
	if out.Fees[0].Winner != nil {
		t.Errorf("an expired-only code must have a null winner, got %+v", out.Fees[0].Winner)
	}
	if len(out.Fees[0].Candidates) != 1 || out.Fees[0].Candidates[0].Rule.RuleId != expired.ID {
		t.Fatalf("the expired rule is the answer to \"why is it not applying\", got %+v", out.Fees[0].Candidates)
	}
	if out.Fees[0].Candidates[0].Reason != LosingFeeRuleReason(store.ReasonOutsideWindowPast) {
		t.Errorf("reason = %q, want outside_window_past", out.Fees[0].Candidates[0].Reason)
	}
}

// A percentage rule must carry rate_bps and a null amount — flattening either to
// a zero would be a lie that TKT-215 would persist into a settlement snapshot.
func TestResolveTicketTypeFeesCarriesTheBasisFaithfully(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	pct := fixedFee(scopes, store.ScopeVenue, "service", 0)
	pct.Basis, pct.Amount = store.BasisPercentageBps, nil
	bps := int32(333)
	pct.RateBps = &bps
	pct.Incidence = store.IncidenceAbsorbed
	addFeeRule(t, e, pct)

	_, out := resolveFees(t, e, ttID, "")
	if len(out.Fees) != 1 || out.Fees[0].Winner == nil {
		t.Fatalf("want one resolved code, got %+v", out.Fees)
	}
	w := out.Fees[0].Winner
	if w.Basis != FeeRuleProvenanceBasis(store.BasisPercentageBps) {
		t.Errorf("basis = %q, want percentage_bps", w.Basis)
	}
	if w.RateBps == nil || *w.RateBps != 333 {
		t.Errorf("rate_bps = %v, want 333", w.RateBps)
	}
	if w.Amount != nil {
		t.Errorf("amount must be null on a percentage rule, got %d", *w.Amount)
	}
	if w.Incidence != FeeRuleProvenanceIncidence(store.IncidenceAbsorbed) {
		t.Errorf("incidence = %q, want absorbed", w.Incidence)
	}
}

// The guard. This response carries absorbed fees — the organizer's cost
// structure — so unlike price-resolution it is not a public read. The gateway
// denies /internal/ at the edge; this is the check that stands between the route
// and the container network.
func TestResolveTicketTypeFeesRequiresTheInternalCredential(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	addFeeRule(t, e, fixedFee(scopes, store.ScopeVenue, "service", 300))

	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", token: "nope", want: http.StatusUnauthorized},
		{name: "valid", token: feeInternalToken, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.token != "" {
				hdr["X-Internal-Token"] = tc.token
			}
			rec := e.doWithHeaders(http.MethodGet, feePath(ttID, ""), nil, hdr)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			// An unauthorized caller must learn nothing about the fee schedule —
			// the whole point of the guard.
			if tc.want == http.StatusUnauthorized && bodyMentions(rec.Body.String(), "service") {
				t.Errorf("the refusal leaked a fee code: %s", rec.Body)
			}
		})
	}
}

func bodyMentions(body, needle string) bool {
	return len(body) > 0 && len(needle) > 0 && jsonContains(body, needle)
}

func jsonContains(body, needle string) bool {
	for i := 0; i+len(needle) <= len(body); i++ {
		if body[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A misconfigured currency is OUR data being wrong, not the caller's request —
// so it fails closed as a 500, and the offending rule id is logged rather than
// returned. Returning it would hand an internal identifier to a caller that
// cannot act on it.
func TestResolveTicketTypeFeesCurrencyMismatchFailsClosed(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	bad := fixedFee(scopes, store.ScopeVenue, "service", 300)
	bad.Currency = "USD"
	rule := addFeeRule(t, e, bad)

	rec, _ := resolveFees(t, e, ttID, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if jsonContains(rec.Body.String(), rule.ID.String()) {
		t.Errorf("the rule id must be logged, never returned: %s", rec.Body)
	}
}

// The cache tier is a correctness claim, not a performance one: a fee schedule
// feeds a money decision and its correctness expires at a known instant.
func TestResolveTicketTypeFeesIsNeverCached(t *testing.T) {
	e := newEnv(t)
	ttID, _ := seedPricedTicketType(t, e, 4550, "EUR")

	rec, _ := resolveFees(t, e, ttID, "")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// An unknown ticket type is a 404, not an empty resolution: "this thing has no
// fees" and "this thing does not exist" must not collapse into one answer.
func TestResolveTicketTypeFeesUnknownTicketType(t *testing.T) {
	e := newEnv(t)
	rec, _ := resolveFees(t, e, uuid.New(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
}

// The evaluation instant is the SERVER's. A caller-supplied one on a sale-time
// endpoint would let anyone ask for a fee schedule that has expired or has not
// opened — the same reason price resolution refuses one. Asserted against the
// contract document rather than the handler, because the handler cannot read a
// parameter the contract does not declare.
func TestResolveTicketTypeFeesTakesNoCallerInstant(t *testing.T) {
	e := newEnv(t)
	for _, forbidden := range []string{"at", "evaluated_at", "as_of", "instant"} {
		route, _, err := e.router.FindRoute(httptest.NewRequest(http.MethodGet,
			"http://catalog.local/internal/ticket-types/"+uuid.New().String()+"/fee-resolution", nil))
		if err != nil {
			t.Fatalf("route not found in the spec: %v", err)
		}
		for _, p := range route.Operation.Parameters {
			if p.Value != nil && p.Value.Name == forbidden {
				t.Errorf("the contract declares a caller-supplied instant %q", forbidden)
			}
		}
	}
}

// The finding this test exists for (ai-review): a credential check inside the
// handler runs AFTER the generated wrapper has bound and validated parameters,
// so a malformed id or channel_code answered 400-with-details to a caller
// holding no credential — a schema oracle on the internal surface, and a
// contradiction of the handler's own claim to check the credential first.
//
// Every row here is malformed in a way that would produce a DIFFERENT status if
// the guard sat downstream of validation. They must all be indistinguishable.
func TestResolveTicketTypeFeesRefusesMalformedRequestsUniformly(t *testing.T) {
	e := newEnv(t)
	ttID, _ := seedPricedTicketType(t, e, 4550, "EUR")

	for name, path := range map[string]string{
		"malformed uuid":          "/internal/ticket-types/not-a-uuid/fee-resolution",
		"empty uuid":              "/internal/ticket-types//fee-resolution",
		"channel below minLength": feePath(ttID, "") + "?channel_code=",
		"channel above maxLength": feePath(ttID, strings.Repeat("x", 101)),
		"unknown ticket type":     feePath(uuid.New(), ""),
		"unknown internal path":   "/internal/ticket-types/" + ttID.String() + "/fee-resolution/extra",
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.doWithHeaders(http.MethodGet, path, nil, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — an unauthenticated caller must not learn "+
					"whether the route or its parameters are well formed (body %s)", rec.Code, rec.Body)
			}
			if rec.Body.String() != `{"error":"unauthorized"}`+"\n" {
				t.Errorf("body = %s, want the same fixed refusal every other case gets", rec.Body)
			}
		})
	}
}

// The guard is a prefix guard, so the same test owes the negative: a PUBLIC
// route must be untouched by it. Without this, tightening the prefix into
// something that matched everything would pass every assertion above.
func TestTheInternalGuardDoesNotTouchPublicRoutes(t *testing.T) {
	e := newEnv(t)
	ttID, _ := seedPricedTicketType(t, e, 4550, "EUR")

	rec := e.do(http.MethodGet, "/ticket-types/"+ttID.String()+"/price-resolution", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the public price resolution must still answer without a credential: %d %s",
			rec.Code, rec.Body)
	}
}

// The prefix guard's load-bearing assumption, pinned: it reads r.URL.Path and
// chi routes on r.URL.Path, so a spelling that reaches the handler cannot be a
// spelling the guard missed. These are the variants that break naive prefix
// guards — a leading double slash, dot segments, a different case, an internal
// double slash. Each must answer 401 (guard caught it) or 404 (the router never
// matched it), and NEVER 200.
//
// Written because this is the one property the guard's whole design rests on,
// and it is exactly the kind of thing that silently stops holding when someone
// adds a path-normalising middleware upstream.
func TestTheInternalGuardCannotBeSpelledAround(t *testing.T) {
	e := newEnv(t)
	ttID, scopes := seedPricedTicketType(t, e, 4550, "EUR")
	addFeeRule(t, e, fixedFee(scopes, store.ScopeVenue, "service", 300))
	id := ttID.String()

	for name, path := range map[string]string{
		"leading double slash":  "//internal/ticket-types/" + id + "/fee-resolution",
		"dot segment":           "/internal/../internal/ticket-types/" + id + "/fee-resolution",
		"leading dot segment":   "/./internal/ticket-types/" + id + "/fee-resolution",
		"uppercase prefix":      "/INTERNAL/ticket-types/" + id + "/fee-resolution",
		"internal double slash": "/internal//ticket-types/" + id + "/fee-resolution",
		"prefix without slash":  "/internalticket-types/" + id + "/fee-resolution",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://catalog.local"+path, nil)
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("%s reached the handler without a credential: %s", path, rec.Body)
			}
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 401 (guarded) or 404 (unrouted); body %s", rec.Code, rec.Body)
			}
		})
	}
}

// The Go reason vocabulary and the CONTRACT's enum must be the same set.
//
// They are declared in two places — store/fees.go and the LosingFeeRule schema
// in openapi.yaml — and nothing else compares them. A reason the resolver can
// emit but the contract does not declare becomes a fail-closed 500 on a money
// read (ADR-028); a reason the contract declares but nothing emits is a lie to
// every consumer generating types from the document.
func TestFeeLossReasonEnumMatchesTheContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	schema, ok := doc.Components.Schemas["LosingFeeRule"]
	if !ok {
		t.Fatal("the contract declares no LosingFeeRule schema")
	}
	reason, ok := schema.Value.Properties["reason"]
	if !ok {
		t.Fatal("LosingFeeRule declares no reason property")
	}
	declared := map[string]bool{}
	for _, v := range reason.Value.Enum {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("non-string enum member %v", v)
		}
		declared[s] = true
	}
	emitted := map[string]bool{
		store.ReasonLessSpecific: true, store.ReasonForcedBroaderScope: true,
		store.ReasonExcludedByForcedRule: true, store.ReasonLowerForcedScope: true,
		store.ReasonLessChannelSpecific: true, store.ReasonLowerPriority: true,
		store.ReasonStableIDTiebreak: true, store.ReasonOutsideWindowPast: true,
		store.ReasonOutsideWindowFuture: true,
	}
	for r := range emitted {
		if !declared[r] {
			t.Errorf("the resolver can emit %q, which the contract does not declare — "+
				"response validation would turn that into a 500 on a money read", r)
		}
	}
	for r := range declared {
		if !emitted[r] {
			t.Errorf("the contract declares %q, which nothing emits", r)
		}
	}
}
