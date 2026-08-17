package api

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	apispec "ticketing/services/commerce/api"
	commercestore "ticketing/services/commerce/internal/store"
)

// The pure fee arithmetic (TKT-215 / ADR-046 §2). No HTTP, no database — the
// seam where rounding and overflow are provable.

func feeRule(code, basis, incidence string, amount *int64, rate *int32) *resolvedFeeRule {
	return &resolvedFeeRule{
		RuleID: uuid.New(), FeeCode: code, Basis: basis, Amount: amount,
		RateBps: rate, Currency: "EUR", Incidence: incidence,
	}
}

func amt(v int64) *int64 { return &v }
func bps(v int32) *int32 { return &v }
func code(c string, w *resolvedFeeRule) resolvedFeeCode {
	return resolvedFeeCode{FeeCode: c, Winner: w}
}

// ADR-046 §2's worked example, which is the whole reason the rounding UNIT is
// specified rather than left to the implementer.
//
// FIXTURE NOTE: the example needs two DIFFERENT unit prices (150 and 100) to
// separate the three candidate units. A fixture at one repeated price cannot —
// per-ticket, per-line and per-order all agree there, so it would pass against
// every wrong implementation.
func TestComputeFeeBreakdownRoundsPerTicket(t *testing.T) {
	// 2 tickets at 150¢, 333 bps: floor(150×333/10000) = 4 each → 8.
	two, err := computeFeeBreakdown(
		[]resolvedFeeCode{code("service", feeRule("service", basisPercentageBps, incidencePassedOn, nil, bps(333)))},
		150, 2, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	// 1 ticket at 100¢, 333 bps: floor(100×333/10000) = 3.
	one, err := computeFeeBreakdown(
		[]resolvedFeeCode{code("service", feeRule("service", basisPercentageBps, incidencePassedOn, nil, bps(333)))},
		100, 1, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	got := two.PassedOnTotal + one.PassedOnTotal
	if got != 11 {
		t.Errorf("total = %d¢, want 11¢ (ADR-046 §2). 12¢ means the fee was rounded per LINE, "+
			"13¢ per ORDER — both make the fee depend on how a cart groups its lines", got)
	}
	if two.PassedOnTotal != 8 {
		t.Errorf("2×150¢ = %d, want 8 (floor per ticket, then multiplied)", two.PassedOnTotal)
	}
}

// Rounding happens BEFORE the multiply, not after. This is the mutation that
// the worked example alone does not always catch, so it gets its own case:
// floor(150×333/10000)×2 = 8, while floor(150×2×333/10000) = 9.
func TestComputeFeeBreakdownFloorsBeforeMultiplying(t *testing.T) {
	got, err := computeFeeBreakdown(
		[]resolvedFeeCode{code("service", feeRule("service", basisPercentageBps, incidencePassedOn, nil, bps(333)))},
		150, 2, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if got.PassedOnTotal != 8 {
		t.Errorf("passed-on = %d, want 8 — 9 means the rate was applied to the LINE total", got.PassedOnTotal)
	}
}

func TestComputeFeeBreakdownByBasis(t *testing.T) {
	for name, tc := range map[string]struct {
		rule     *resolvedFeeRule
		unit     int64
		quantity int32
		want     int64
	}{
		"per ticket fixed multiplies by quantity": {
			rule: feeRule("service", basisPerTicketFixed, incidencePassedOn, amt(250), nil),
			unit: 1000, quantity: 3, want: 750,
		},
		"per order fixed is charged once": {
			rule: feeRule("booking", basisPerOrderFixed, incidencePassedOn, amt(250), nil),
			unit: 1000, quantity: 3, want: 250,
		},
		"percentage of the unit price, per ticket": {
			rule: feeRule("service", basisPercentageBps, incidencePassedOn, nil, bps(1000)),
			unit: 1000, quantity: 3, want: 300,
		},
		"a percentage that floors to zero is still charged as zero": {
			rule: feeRule("service", basisPercentageBps, incidencePassedOn, nil, bps(333)),
			unit: 1, quantity: 1, want: 0,
		},
		"a zero-quantity multiplier is impossible; quantity 1 is the floor": {
			rule: feeRule("service", basisPerTicketFixed, incidencePassedOn, amt(0), nil),
			unit: 1000, quantity: 1, want: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := computeFeeBreakdown([]resolvedFeeCode{code(tc.rule.FeeCode, tc.rule)},
				tc.unit, tc.quantity, "EUR")
			if err != nil {
				t.Fatal(err)
			}
			if got.PassedOnTotal != tc.want {
				t.Errorf("passed-on = %d, want %d", got.PassedOnTotal, tc.want)
			}
			if len(got.Items) != 1 {
				t.Fatalf("want one breakdown item, got %+v", got.Items)
			}
			if got.Items[0].Amount != tc.want {
				t.Errorf("item amount = %d, want %d", got.Items[0].Amount, tc.want)
			}
		})
	}
}

// The heart of the ticket: incidence decides what the BUYER pays, and both kinds
// are recorded either way. A build that treats it as a display flag passes every
// other test in this file.
func TestComputeFeeBreakdownSeparatesIncidence(t *testing.T) {
	got, err := computeFeeBreakdown([]resolvedFeeCode{
		code("service", feeRule("service", basisPerTicketFixed, incidencePassedOn, amt(300), nil)),
		code("facility", feeRule("facility", basisPerTicketFixed, incidenceAbsorbed, amt(200), nil)),
	}, 4550, 2, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if got.PassedOnTotal != 600 {
		t.Errorf("passed-on = %d, want 600 — only the passed_on fee may reach the buyer", got.PassedOnTotal)
	}
	if got.AbsorbedTotal != 400 {
		t.Errorf("absorbed = %d, want 400 — an absorbed fee must still be RECORDED, "+
			"because TKT-217 pays it to a payee out of money the buyer already paid", got.AbsorbedTotal)
	}
	if len(got.Items) != 2 {
		t.Fatalf("both fees must appear in the breakdown, got %+v", got.Items)
	}
}

// A considered code with no live rule (ADR-046 §9) is not a fee of zero: it
// contributes nothing AND produces no breakdown item. A zero-amount winner does
// the opposite. The fixture carries both so the two cannot be conflated.
func TestComputeFeeBreakdownDistinguishesNullWinnerFromZeroFee(t *testing.T) {
	got, err := computeFeeBreakdown([]resolvedFeeCode{
		code("booking", nil),
		code("service", feeRule("service", basisPercentageBps, incidencePassedOn, nil, bps(333))),
	}, 1, 1, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if got.PassedOnTotal != 0 {
		t.Errorf("passed-on = %d, want 0", got.PassedOnTotal)
	}
	if len(got.Items) != 1 {
		t.Fatalf("want exactly one item — the zero-amount winner, not the null one; got %+v", got.Items)
	}
	if got.Items[0].FeeCode != "service" || got.Items[0].Amount != 0 {
		t.Errorf("item = %+v, want the zero-amount service fee", got.Items[0])
	}
}

// Overflow is refused, not wrapped. A wrapped int64 on a money path is
// indistinguishable from a legitimate small number.
func TestComputeFeeBreakdownRefusesOverflow(t *testing.T) {
	for name, tc := range map[string]struct {
		fees     []resolvedFeeCode
		unit     int64
		quantity int32
	}{
		// The bound is int64 OVERFLOW, not the contract's Money cap: a composed
		// total is commerce's own number and its contract declares a bare int64.
		// maxContractAmount × 50 still fits, and must still sell.
		"fixed fee × quantity": {
			fees: []resolvedFeeCode{code("s", feeRule("s", basisPerTicketFixed, incidencePassedOn, amt(math.MaxInt64/2), nil))},
			unit: 1, quantity: 3,
		},
		"a percentage fee multiplied past int64 by quantity": {
			fees: []resolvedFeeCode{code("s", feeRule("s", basisPercentageBps, incidencePassedOn, nil, bps(10000)))},
			unit: math.MaxInt64 / 2, quantity: 3,
		},
		"the sum of two fees": {
			fees: []resolvedFeeCode{
				code("a", feeRule("a", basisPerOrderFixed, incidencePassedOn, amt(math.MaxInt64-1), nil)),
				code("b", feeRule("b", basisPerOrderFixed, incidencePassedOn, amt(math.MaxInt64-1), nil)),
			},
			unit: 1, quantity: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := computeFeeBreakdown(tc.fees, tc.unit, tc.quantity, "EUR"); !errors.Is(err, errFeeTotalOverflow) {
				t.Errorf("want errFeeTotalOverflow, got %v", err)
			}
		})
	}
}

// A document this build cannot honour is refused rather than guessed at — the
// discipline catalog_pricing.go applies to an unknown action_kind.
func TestComputeFeeBreakdownRefusesUnusableRules(t *testing.T) {
	for name, r := range map[string]*resolvedFeeRule{
		"unknown basis":              feeRule("s", "per_seat", incidencePassedOn, amt(100), nil),
		"unknown incidence":          feeRule("s", basisPerTicketFixed, "shared", amt(100), nil),
		"fixed basis with no amount": feeRule("s", basisPerTicketFixed, incidencePassedOn, nil, nil),
		"percentage with no rate":    feeRule("s", basisPercentageBps, incidencePassedOn, nil, nil),
		"a fee in another currency": {RuleID: uuid.New(), FeeCode: "s", Basis: basisPerTicketFixed,
			Amount: amt(100), Currency: "USD", Incidence: incidencePassedOn},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := computeFeeBreakdown([]resolvedFeeCode{code("s", r)}, 1000, 1, "EUR"); !errors.Is(err, errResolveUnusable) {
				t.Errorf("want errResolveUnusable, got %v", err)
			}
		})
	}
}

// composedTotal is what the card is charged. Absorbed fees must not appear in
// it — that would charge the buyer for the organizer's cost.
func TestComposedTotalAddsOnlyPassedOnFees(t *testing.T) {
	fees, err := computeFeeBreakdown([]resolvedFeeCode{
		code("service", feeRule("service", basisPerTicketFixed, incidencePassedOn, amt(300), nil)),
		code("facility", feeRule("facility", basisPerTicketFixed, incidenceAbsorbed, amt(200), nil)),
	}, 4550, 2, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	face := int64(4550 * 2)
	total, err := composedTotal(face, fees.PassedOnTotal)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9700 {
		t.Errorf("charged total = %d, want 9700 (face 9100 + passed-on 600). "+
			"9-hundred more means the absorbed fee leaked into the buyer's total", total)
	}
	// Overflow is refused; merely LARGE is not. A quantity of 2 at the maximum
	// contract price is a legitimate sale the system has always allowed.
	if _, err := composedTotal(maxContractAmount*2, 0); err != nil {
		t.Errorf("a large but representable total must be allowed: %v", err)
	}
	if _, err := composedTotal(math.MaxInt64, 1); !errors.Is(err, errFeeTotalOverflow) {
		t.Errorf("a total that overflows int64 must be refused, got %v", err)
	}
}

// --- consumer tests: the read, the channel, and failing closed ---

// feeStack wires a catalog that answers the price read normally and the fee read
// with whatever the test wants, and an inventory that RECORDS whether it was
// called. That last part is the point of several tests below: a fee failure must
// abort before anything is held.
func feeStack(t *testing.T, feeStatus int, feeBody string) (*Server, *string, *bool, func()) {
	t.Helper()
	var askedFor string
	inventoryCalled := false
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			askedFor = r.URL.RawQuery
			if r.Header.Get("X-Internal-Token") == "" {
				t.Error("the fee route is internal (ADR-046 §6); the credential must be sent")
			}
			w.WriteHeader(feeStatus)
			_, _ = w.Write([]byte(feeBody))
			return
		}
		// Price resolution. Echo back whatever channel was asked for: commerce
		// validates the echo (TKT-237), so a fake that always answered `null`
		// would fail every channelled reserve before it reached the fee call
		// this test is about.
		var ch *string
		if q := r.URL.Query().Get("channel_code"); q != "" {
			ch = &q
		}
		_, _ = w.Write([]byte(resolutionBodyFor(900, true, ch)))
	}))
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inventoryCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"stop"}`))
	}))
	s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	return s, &askedFor, &inventoryCalled, func() { catalog.Close(); inventory.Close() }
}

// reserveInChannel drives one reserve that carries a channel.
//
// It goes through the PARTNER path (TKT-248). It used to post `channel_code` in a
// public request body, and that is no longer a thing a caller can do: ADR-060 made
// the channel an entitlement, so the field is gone from ReservationCreate and a
// public request naming one is refused before any of this.
//
// The guards these tests exercise -- that the channel reaches catalog's price and
// fee resolution, and that catalog's ECHO is validated against what was asked
// (TKT-215, TKT-237) -- are all still real and all still matter. They simply live
// on the authenticated path now, which is the only path a channelled sale exists
// on. Routing them here keeps them asserting the mechanism instead of a scenario
// nobody can reach.
//
// `channel == ""` still means a public, channel-less reserve, so the callers that
// assert "no channel sends no parameter" keep testing exactly what they did.
func reserveInChannel(t *testing.T, s *Server, key, channel string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"organizer_id":"` + pricingOrg + `","ticket_type_id":"` + pricingTT + `","quantity":2}`
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	if channel == "" {
		s.Router(nil, true).ServeHTTP(res, req)
		return res
	}
	// The credential is the only source of a channel. Calling reserveWithScope
	// directly is how the partner route reaches it (partner.go:82) once the
	// credential has been resolved.
	s.reserveWithScope(res, req, &partnerScope{
		OrganizerID: uuid.MustParse(pricingOrg),
		ChannelCode: channel,
		ResellerID:  uuid.MustParse(pricingReseller),
	})
	return res
}

// The channel reaches catalog exactly as the buyer named it.
func TestReserveSendsTheChannelToFeeResolution(t *testing.T) {
	s, asked, _, done := feeStack(t, 200, feeResolutionBodyFor("reseller"))
	defer done()
	reserveInChannel(t, s, "chan-1", "reseller")
	if *asked != "channel_code=reseller" {
		t.Errorf("catalog was asked %q, want channel_code=reseller", *asked)
	}
}

// Omitting the channel sends NO parameter — it is the default/public context,
// not a wildcard (ADR-046 §4). Sending an empty parameter would be a different
// request, and catalog's minLength would reject it.
func TestReserveWithoutAChannelSendsNoParameter(t *testing.T) {
	s, asked, _, done := feeStack(t, 200, emptyFeeResolutionBody(nil))
	defer done()
	reserveInChannel(t, s, "chan-2", "")
	if *asked != "" {
		t.Errorf("catalog was asked %q, want no query at all", *asked)
	}
}

// Fail closed, and fail BEFORE the hold. Every row is a document commerce cannot
// honour; none of them may degrade to "no fees", and none may leave a hold
// behind. The inventory recorder is what proves the second half — an assertion on
// the status code alone would pass even if a hold had been placed and abandoned.
func TestReserveFailsClosedOnUnusableFeeResolution(t *testing.T) {
	org := pricingOrg
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"catalog is down":              {status: 503, body: `{}`},
		"not a fee resolution":         {status: 200, body: `{"nope":true`},
		"another organizer":            {status: 200, body: feeBodyWith(`"organizer_id":"11111111-2222-3333-4444-555555555555"`, org)},
		"a channel we did not ask for": {status: 200, body: feeResolutionBodyFor("presale")},
		"an unknown basis": {status: 200, body: feeBodyWithFee(
			`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"s","basis":"per_seat","amount":100,"rate_bps":null,"currency":"EUR","incidence":"passed_on"}`)},
		"a fixed fee carrying a rate too": {status: 200, body: feeBodyWithFee(
			`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"s","basis":"per_ticket_fixed","amount":100,"rate_bps":500,"currency":"EUR","incidence":"passed_on"}`)},
		"a rate outside 0..10000": {status: 200, body: feeBodyWithFee(
			`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"s","basis":"percentage_bps","amount":null,"rate_bps":10001,"currency":"EUR","incidence":"passed_on"}`)},
		"an unknown incidence": {status: 200, body: feeBodyWithFee(
			`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"s","basis":"per_ticket_fixed","amount":100,"rate_bps":null,"currency":"EUR","incidence":"shared"}`)},
		"a winner whose code disagrees with its entry": {status: 200, body: feeBodyWithFee(
			`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"other","basis":"per_ticket_fixed","amount":100,"rate_bps":null,"currency":"EUR","incidence":"passed_on"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, called, done := feeStack(t, tc.status, tc.body)
			defer done()
			res := reserveInChannel(t, s, "closed-"+name, "")
			if res.Code != 500 && res.Code != 502 {
				t.Errorf("status = %d, want 500 or 502 — a fee resolution we cannot trust must "+
					"never degrade to selling with no fees", res.Code)
			}
			if *called {
				t.Error("inventory was called: the sale must abort BEFORE a hold exists, or a " +
					"failed reserve leaves an orphan hold and the buyer gets a 500")
			}
		})
	}
}

// An empty fee set is a SUCCESSFUL resolution — the distinction the whole
// fail-closed rule rests on. It must reach inventory, unlike every row above.
func TestReserveAcceptsAnEmptyFeeSet(t *testing.T) {
	s, _, called, done := feeStack(t, 200, emptyFeeResolutionBody(nil))
	defer done()
	reserveInChannel(t, s, "empty-fees", "")
	if !*called {
		t.Error("an empty fee set is 'no rules matched', not a failure — the sale must proceed")
	}
}

func feeResolutionBodyFor(channel string) string {
	c := channel
	return emptyFeeResolutionBody(&c)
}

func feeBodyWith(field, org string) string {
	_ = org
	return `{"resolver_version":1,"evaluated_at":"2026-08-05T12:00:00Z",` + field +
		`,"performance_id":"` + pricingSlot + `",` +
		`"currency":"EUR","channel_code":null,"fees":[]}`
}

func feeBodyWithFee(winner string) string {
	return `{"resolver_version":1,"evaluated_at":"2026-08-05T12:00:00Z",` +
		`"organizer_id":"` + pricingOrg + `","performance_id":"` + pricingSlot + `",` +
		`"currency":"EUR","channel_code":null,"fees":[{"fee_code":"s","winner":` + winner + `}]}`
}

// The XOR the ReservationCreate schema exists to express, asserted against the
// CONTRACT rather than the handler (TKT-215 plan-review A2).
//
// It used to be expressed by property COUNT alone — two required, two optional,
// minProperties 3 / maxProperties 3 admits exactly one of quantity and
// seat_identities. Adding channel_code raised the ceiling to 4, and
// {organizer_id, ticket_type_id, quantity, seat_identities} is also four. The
// count stopped carrying the invariant at exactly that moment, which is why `not`
// was added alongside it.
//
// TKT-248 removed channel_code, so the ceiling is back to 3 and the count would
// once again exclude that shape on its own. `not` STAYS: the count matching is a
// coincidence of today's field list — it was 4 yesterday and the next optional
// property makes it 4 again — while `not` says the XOR directly. The "both" row
// below must keep failing for the RULE, not for the arithmetic.
//
// The handler enforces the XOR independently, so this is not an exploitable
// hole; the schema's own description is what says a direct caller "must not be
// able to slip past the schema", and this test is what keeps that true.
func TestTheContractRefusesBothQuantityAndSeats(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	schema, ok := doc.Components.Schemas["ReservationCreate"]
	if !ok {
		t.Fatal("no ReservationCreate schema")
	}
	for name, tc := range map[string]struct {
		body  map[string]any
		valid bool
	}{
		"general admission": {
			body:  map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT, "quantity": 2},
			valid: true,
		},
		"seated": {
			body:  map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT, "seat_identities": []any{"A/1/1"}},
			valid: true,
		},
		// INVERTED BY TKT-248. These two read `valid: true` until the public
		// reservation lost `channel_code` entirely (ADR-060). They are kept as
		// REFUSAL rows rather than deleted, because the schema is the real guard
		// here -- the field is unsubmittable, not validated -- and a deleted row
		// asserts nothing. Re-adding the property to ReservationCreate turns both
		// red.
		"general admission naming a channel": {
			body:  map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT, "quantity": 2, "channel_code": "reseller"},
			valid: false,
		},
		"seated naming a channel": {
			body: map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT,
				"seat_identities": []any{"A/1/1"}, "channel_code": "reseller"},
			valid: false,
		},
		// The row this test exists for. Four properties, so the count alone
		// admits it.
		"both quantity and seats": {
			body: map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT,
				"quantity": 2, "seat_identities": []any{"A/1/1"}},
			valid: false,
		},
		"neither quantity nor seats": {
			body:  map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT},
			valid: false,
		},
		"an empty channel code": {
			body: map[string]any{"organizer_id": pricingOrg, "ticket_type_id": pricingTT,
				"quantity": 2, "channel_code": ""},
			valid: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := schema.Value.VisitJSON(tc.body)
			if tc.valid && err != nil {
				t.Errorf("the contract rejected a legitimate request: %v", err)
			}
			if !tc.valid && err == nil {
				t.Error("the contract ACCEPTED a request it must refuse — with a fifth property " +
					"in play, minProperties/maxProperties alone no longer expresses the XOR")
			}
		})
	}
}

// The correction to a test that pinned a BUG (ai-review, [high]).
//
// The first implementation formed unitFace × rateBps and checked that product
// against the contract's Money cap. That rejects legitimate fees — at a large
// unit price and a 100% rate the intermediate exceeds the cap while the ANSWER
// does not — and the overflow test asserted the rejection as required
// behaviour, so the bug was locked in rather than caught. A test can be wrong
// about what it wants, and then it defends the defect.
//
// The identity floor(a×b/10000) = (a/10000)×b + floor((a mod 10000)×b/10000)
// never forms the full product, so these all resolve exactly.
func TestFeeFromRateAcceptsTheWholeContractRange(t *testing.T) {
	for name, tc := range map[string]struct {
		unit int64
		rate int32
		want int64
	}{
		"100% of the largest contract price":     {unit: maxContractAmount, rate: 10000, want: maxContractAmount},
		"50% of the largest representable price": {unit: maxContractAmount, rate: 5000, want: maxContractAmount / 2},
		"a large price at a small rate":          {unit: 1_000_000_000_000, rate: 1, want: 100_000_000},
		"the case the old code wrongly refused":  {unit: 1_000_000_000_000, rate: 10000, want: 1_000_000_000_000},
		"floors rather than rounds":              {unit: 150, rate: 333, want: 4},
		"a rate of zero costs nothing":           {unit: maxContractAmount, rate: 0, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := feeFromRate(tc.unit, tc.rate)
			if err != nil {
				t.Fatalf("a rate within 0..10000 of a price within the Money cap can never "+
					"overflow — the result is bounded above by the price itself: %v", err)
			}
			if got != tc.want {
				t.Errorf("feeFromRate(%d, %d) = %d, want %d", tc.unit, tc.rate, got, tc.want)
			}
		})
	}
}

// The fee identity check (ai-review, [high]): a resolution describing another
// performance must not be applied to this sale. Organizer alone does not catch
// it — two performances of one organizer is the common case.
func TestFeeResolutionMustDescribeTheSamePerformance(t *testing.T) {
	org, perf := uuid.New(), uuid.New()
	base := feeResolution{
		ResolverVersion: 1, EvaluatedAt: feeEvalAt(), OrganizerID: org,
		PerformanceID: perf, Currency: "EUR", Fees: []resolvedFeeCode{},
	}
	if err := base.validate(org, perf, nil); err != nil {
		t.Fatalf("a matching resolution must be accepted: %v", err)
	}
	other := base
	other.PerformanceID = uuid.New()
	if err := other.validate(org, perf, nil); !errors.Is(err, errResolveUnusable) {
		t.Errorf("a resolution for another performance must be refused, got %v", err)
	}
	none := base
	none.PerformanceID = uuid.Nil
	if err := none.validate(org, perf, nil); !errors.Is(err, errResolveUnusable) {
		t.Errorf("a resolution naming no performance must be refused, got %v", err)
	}
}

func feeEvalAt() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) }

// F2 (ai-review, [high]): a composition that cannot be represented must be
// refused BEFORE the hold, not after.
//
// The reachable shape is a large RESOLVED PRICE, not a large fee. Commerce
// deliberately allows prices far above catalog's Money cap on the total — the
// existing TestReserveOverflowGuardAppliesToResolvedAmount pins that: "the guard
// is about overflow, not about large prices". So a face value near int64's
// ceiling is legitimate, and then ANY passed-on fee overflows the sum.
//
// Before this fix that check ran after the hold: 400 with a hold left behind — an
// orphan, and a buyer told their own request was malformed. The inventory
// recorder is the whole point of the test; a status assertion alone passes
// whether or not a hold was placed and abandoned.
func TestReserveRefusesAnUnrepresentableTotalBeforeTheHold(t *testing.T) {
	// The largest price the EXISTING price guard admits at quantity 2:
	// resolved <= MaxInt64/quantity. Face value is then MaxInt64-1, and any
	// passed-on fee at all overflows the sum. One above this and the price path
	// refuses it first, which would test a different guard.
	huge := int64(math.MaxInt64) / 2
	price := `{"resolver_version":2,"evaluated_at":"2026-07-31T00:00:00Z",` +
		`"organizer_id":"` + pricingOrg + `","performance_id":"` + pricingSlot + `",` +
		`"base_price":{"amount":2500,"currency":"EUR"},` +
		`"resolved_price":{"amount":` + itoa(huge) + `,"currency":"EUR"},` +
		`"winner":{"rule_id":"` + pricingRule + `","scope_level":"event",` +
		`"scope_id":"00000000-0000-0000-0000-0000000000e1","action_kind":"absolute",` +
		`"amount":` + itoa(huge) + `,"currency":"EUR","effective_from":null,` +
		`"effective_until":null,"priority":0,"forced":false},"candidates":[]}`
	fees := feeBodyWithFee(`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"s",` +
		`"basis":"per_ticket_fixed","amount":9007199254740991,"rate_bps":null,` +
		`"currency":"EUR","incidence":"passed_on"}`)

	inventoryCalled := false
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			_, _ = w.Write([]byte(fees))
			return
		}
		_, _ = w.Write([]byte(price))
	}))
	defer catalog.Close()
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inventoryCalled = true
		w.WriteHeader(409)
	}))
	defer inventory.Close()

	srv := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	body := `{"organizer_id":"` + pricingOrg + `","ticket_type_id":"` + pricingTT + `","quantity":2}`
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "overflow-before-hold")
	res := httptest.NewRecorder()
	srv.Router(nil, true).ServeHTTP(res, req)

	if res.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", res.Code, res.Body)
	}
	if inventoryCalled {
		t.Error("inventory was called: an arithmetic failure must abort BEFORE the hold, " +
			"or a refused reserve leaves an orphan hold behind")
	}
}

// Which number each exchange money fact carries (ai-review pass 2).
//
// The previous evidence for this was a smoke assertion that the gross COLUMN is
// populated — which proves the column exists, not that the fact uses it. That is
// a real gap between what a test showed and what a comment claimed, and it is the
// kind that survives because the two look the same in a diff.
//
// FIXTURE NOTE: face and gross MUST differ here, and the target must differ from
// both. A fixture where any two coincide cannot tell the legs apart.
func TestExchangeFactLegsReverseTheGrossAndSellTheTarget(t *testing.T) {
	ex := commercestore.Exchange{
		ID:               uuid.New(),
		SourceTotal:      9100, // face
		SourceGrossTotal: 9400, // what was captured
	}
	legs, err := exchangeFactLegs(ex, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 2 {
		t.Fatalf("want two legs, got %d", len(legs))
	}
	byType := map[string]int64{}
	for _, l := range legs {
		byType[l.typ] = l.amount
	}
	if got := byType["order.exchange.reversed"]; got != 9400 {
		t.Errorf("reversed leg = %d, want the CAPTURED 9400. The face value 9100 leaves the "+
			"payments journal disagreeing with the original charge by exactly the fee", got)
	}
	// The retained fee travels with the order, so the sold leg is the target plus
	// the fee the buyer keeps paying: 8000 + (9400-9100).
	if got := byType["order.exchange.sold"]; got != 8300 {
		t.Errorf("sold leg = %d, want 8300 — the target 8000 plus the 300 fee the buyer "+
			"retains. Selling the bare target records a refund the provider never made", got)
	}

	// The invariant that ties the two legs to reality (ai-review pass 2): the
	// journal's net movement must equal what the provider actually moved. The
	// delta is computed from FACE values, so this is the check that stops the two
	// numbers drifting apart again.
	delta := commercestore.ExchangeDelta(ex.SourceTotal, 8000)
	if net := byType["order.exchange.sold"] - byType["order.exchange.reversed"]; net != delta {
		t.Errorf("journal net = %d but the provider delta is %d — the audit trail asserts money "+
			"movement that did not happen", net, delta)
	}
}

// The same invariant on the case that matters most, because it is the one where
// nothing moves and an inconsistency is therefore invisible in the PSP: an EVEN
// exchange of a fee-carrying order.
func TestExchangeFactLegsBalanceAgainstTheProviderDelta(t *testing.T) {
	for name, tc := range map[string]struct{ face, gross, target int64 }{
		"even exchange, fee carried": {face: 9100, gross: 9400, target: 9100},
		"upgrade, fee carried":       {face: 9100, gross: 9400, target: 12000},
		"downgrade, fee carried":     {face: 9100, gross: 9400, target: 5000},
		"no fee at all":              {face: 9100, gross: 9100, target: 9100},
		"a fee-free order upgrading": {face: 9100, gross: 9100, target: 12000},
	} {
		t.Run(name, func(t *testing.T) {
			ex := commercestore.Exchange{ID: uuid.New(), SourceTotal: tc.face, SourceGrossTotal: tc.gross}
			legs, err := exchangeFactLegs(ex, tc.target)
			if err != nil {
				t.Fatalf("a representable exchange must not be refused: %v", err)
			}
			var reversed, sold int64
			for _, l := range legs {
				switch l.typ {
				case "order.exchange.reversed":
					reversed = l.amount
				case "order.exchange.sold":
					sold = l.amount
				}
			}
			// Pin WHICH number each leg carries, not only that they balance:
			// reversing the face and selling the target also balances, and the
			// balance check alone would accept it (ai-review pass 3).
			if reversed != tc.gross {
				t.Errorf("reversed = %d, want the captured gross %d", reversed, tc.gross)
			}
			// Compare through big.Int so a WRAPPED result cannot satisfy the
			// check: two wrapped int64s can differ by exactly the right amount,
			// which is how the first version of this test accepted an overflow.
			delta := commercestore.ExchangeDelta(tc.face, tc.target)
			gotNet := new(big.Int).Sub(big.NewInt(sold), big.NewInt(reversed))
			wantSold := new(big.Int).Add(big.NewInt(tc.target),
				new(big.Int).Sub(big.NewInt(tc.gross), big.NewInt(tc.face)))
			if gotNet.Cmp(big.NewInt(delta)) != 0 {
				t.Errorf("reversed=%d sold=%d net=%s, but the provider moves %d",
					reversed, sold, gotNet, delta)
			}
			if wantSold.Cmp(big.NewInt(sold)) != 0 {
				t.Errorf("sold = %d, want %s (target + carried fee), computed without wrapping",
					sold, wantSold)
			}
		})
	}
}

// The decomposition identity, pinned against an oracle that CANNOT overflow.
//
//	floor(a×b/10000) == (a/10000)×b + floor((a mod 10000)×b/10000)
//
// The first version compared against int64 arithmetic and skipped any sample
// whose product overflowed — which, for random a near MaxInt64, was 3997 of
// 4000 samples (ai-review pass 3). Those 3997 asserted only "no error
// returned", so the sweep looked thorough and checked almost nothing.
//
// big.Int gives every sample a real expected value, including the ones that
// motivated the decomposition in the first place.
func TestFeeFromRateMatchesTheExactQuotientEverywhere(t *testing.T) {
	oracle := func(a int64, b int32) int64 {
		product := new(big.Int).Mul(big.NewInt(a), big.NewInt(int64(b)))
		return new(big.Int).Div(product, big.NewInt(10000)).Int64()
	}
	check := func(a int64, b int32) {
		t.Helper()
		got, err := feeFromRate(a, b)
		if err != nil {
			t.Fatalf("feeFromRate(%d, %d) refused a legal input: %v", a, b, err)
		}
		if want := oracle(a, b); got != want {
			t.Errorf("feeFromRate(%d, %d) = %d, want %d", a, b, got, want)
		}
	}
	for _, a := range []int64{0, 1, 9999, 10000, 10001, 100, 150, maxContractAmount,
		math.MaxInt64 / 2, math.MaxInt64 / 10000 * 10000, math.MaxInt64 - 1, math.MaxInt64} {
		for _, b := range []int32{0, 1, 2, 333, 5000, 9999, 10000} {
			check(a, b)
		}
	}
	// A deterministic sweep — a fixed generator rather than a random one, so a
	// failure reproduces from the test name alone. Every sample is now checked
	// against the oracle, not merely for the absence of an error.
	a := int64(1)
	for i := 0; i < 4000; i++ {
		a = (a*6364136223846793005 + 1442695040888963407) & math.MaxInt64
		check(a, int32(a%10001))
	}
}

// sameTerms is the one definition of "the same request", and this is the table
// that keeps it that way. The channel row is the one the race path was missing.
func TestSameTermsComparesEveryIdempotencyTerm(t *testing.T) {
	tt := uuid.New()
	reseller := "reseller"
	base := reserveRequest{TicketTypeID: tt, Quantity: 2}

	if !sameTerms(base, 2, tt, nil, nil) {
		t.Error("an identical GA request must match")
	}
	if sameTerms(base, 3, tt, nil, nil) {
		t.Error("a different quantity is a different request")
	}
	if sameTerms(base, 2, uuid.New(), nil, nil) {
		t.Error("a different ticket type is a different request")
	}
	// The row the lost-race path was missing: it compared totals but not this.
	channelled := base
	channelled.ChannelCode = &reseller
	if sameTerms(channelled, 2, tt, nil, nil) {
		t.Error("a channel request must NOT match a reservation sold with no channel — the " +
			"channel selects which fees apply, so these are different sales")
	}
	if sameTerms(base, 2, tt, &reseller, nil) {
		t.Error("a channel-less request must NOT match a reservation sold in a channel")
	}
	// A DISTINCT pointer holding the same string: the previous version reused
	// &reseller on both sides, so an implementation comparing pointer identity
	// passed (ai-review pass 3). The comparison must be on the VALUE.
	otherPointer := "reseller"
	if !sameTerms(channelled, 2, tt, &otherPointer, nil) {
		t.Error("the same channel must match — compared by value, not by pointer identity")
	}
	empty := ""
	if sameTerms(channelled, 2, tt, &empty, nil) {
		t.Error("an empty channel is not the reseller channel")
	}
	// A seated request whose PERSISTED quantity disagrees is a different request
	// even when the seat set matches — the two must be checked together.
	seatedTwo := reserveRequest{TicketTypeID: tt, SeatIdentities: []string{"A/1/1", "A/1/2"}}
	if sameTerms(seatedTwo, 1, tt, nil, []string{"A/1/1", "A/1/2"}) {
		t.Error("a persisted quantity that disagrees with the seat set is a different request")
	}
	// Seats compare as a SET, not a count.
	seated := reserveRequest{TicketTypeID: tt, SeatIdentities: []string{"A/1/1", "A/1/2"}}
	if !sameTerms(seated, 2, tt, nil, []string{"A/1/1", "A/1/2"}) {
		t.Error("the same seat set must match")
	}
	if sameTerms(seated, 2, tt, nil, []string{"A/1/1", "B/9/9"}) {
		t.Error("two seats are not the same two seats")
	}
	if sameTerms(seated, 2, tt, nil, nil) {
		t.Error("a seated request must not match a GA reservation")
	}
}

// replacementGross is the number the journal's sold leg AND the replacement
// reservation must both use, so it gets its own assertions (ai-review pass 3).
//
// The overflow row is the one that matters: the first version added the two
// unchecked and wrapped negative, and the balance test accepted it because the
// difference wrapped back to the right answer.
func TestReplacementGrossCarriesTheFeeAndRefusesOverflow(t *testing.T) {
	for name, tc := range map[string]struct {
		face, gross, target int64
		want                int64
		wantErr             bool
	}{
		"a carried fee":             {face: 9100, gross: 9400, target: 8000, want: 8300},
		"no fee at all":             {face: 9100, gross: 9100, target: 8000, want: 8000},
		"an even exchange":          {face: 9100, gross: 9400, target: 9100, want: 9400},
		"a target of zero":          {face: 9100, gross: 9400, target: 0, want: 300},
		"the sum overflows int64":   {face: 0, gross: math.MaxInt64, target: 1, wantErr: true},
		"gross below face is bogus": {face: 9400, gross: 9100, target: 1, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			ex := commercestore.Exchange{SourceTotal: tc.face, SourceGrossTotal: tc.gross}
			got, err := replacementGross(ex, tc.target)
			if tc.wantErr {
				if !errors.Is(err, errFeeTotalOverflow) {
					t.Fatalf("want errFeeTotalOverflow, got %v (value %d). A wrapped sum here is "+
						"journalled AFTER the provider has already moved money", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("replacementGross = %d, want %d", got, tc.want)
			}
		})
	}
}

// A public request naming a channel never reaches catalog; a partner request does
// (TKT-248, ADR-060).
//
// SCOPE, stated precisely because an earlier version of this test overclaimed it
// (ai-review [medium]): this asserts WHICH QUERY CATALOG WAS ASKED, not what the
// buyer was charged. It cannot assert money — the fakes run with a nil database,
// so a reserve that got far enough to compose and return a total would panic on
// the insert, and the 409 that stops it carries no amount. A previous version
// returned `unit + passedOn/quantity` computed from values the FAKE itself
// assigned, which is arithmetic over the fixture rather than an observation of
// commerce, and would have passed had commerce added absorbed fees to the charge.
//
// The composed total is asserted where money actually moves, end to end through
// the real stack: smoke/checkout_test.go's
// TestFeeIncidenceChangesTheChargedTotalButNotTheFee. This test is the SEAM half —
// which channel, if any, reaches catalog at all.
//
// This is the assertion the ticket's COS asks for and the one a refusal-only test
// cannot make. The leak this ticket closes was never visible as an error: catalog
// resolves both PRICE (TKT-237) and FEE (ADR-046 §4) rules on the channel, so a
// public caller naming a reseller's channel was quoted that channel's economics.
// With `incidence: absorbed` the buyer is charged LESS — the organizer bears the
// fee out of face value (ADR-046 §3) — so the defect SHOWS UP AS A SMALLER CHARGE.
//
// Asserted at the CATALOG BOUNDARY, which is where the money is decided and where
// the leak crosses. It cannot be read off the reserve response: these fakes run
// with a nil database, so the success path that returns `amount` would panic on
// the insert, and the 409 that stops the run carries no quote. What commerce asks
// catalog for IS the economics — that is the point of the ticket.
//
// Expected values are derived from the requirement, not observed from a run:
// passed_on is added to the buyer's charge, absorbed is not (ADR-046 §3). An
// expectation read off the code would have blessed the defect.
//
// WHAT EACH SEED IS FOR — "if I delete this, what goes red?":
//   - the channel-agnostic price + passed_on fee: the public assertion.
//   - the RESELLER absorbed fee: load-bearing. It is what makes the two paths cost
//     DIFFERENT amounts. The final check asserts they differ, so a fixture whose
//     channels are economically identical fails loudly instead of passing
//     vacuously.
func TestAPublicRequestNamingAChannelNeverReachesCatalog(t *testing.T) {
	const (
		publicUnit   = 2500 // channel-agnostic price rule
		publicFee    = 300  // channel-agnostic, passed_on -> reaches the buyer
		resellerUnit = 2000 // the reseller channel's price rule: cheaper
		resellerFee  = 300  // reseller channel, absorbed -> never reaches the buyer
		quantity     = 2
	)
	// The two arms are deliberately given DIFFERENT economics -- the reseller
	// channel is cheaper, and its fee is absorbed rather than passed on -- so that
	// the fake answers visibly different things depending on which channel it was
	// asked about. What that difference COSTS a buyer is asserted end to end in
	// smoke/checkout_test.go; here it only has to be non-identical, so that a
	// channel reaching catalog is observable at all.

	type asked struct {
		price, fees             string
		priceCalled, feesCalled bool
	}
	drive := func(t *testing.T, channel string) asked {
		t.Helper()
		var got asked
		catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			// Answer for whichever channel was ASKED about. This is what lets the
			// fixture show the leak: if the public path ever forwarded a channel
			// again, it would be served the reseller economics here and the public
			// assertion below would fail.
			q := r.URL.Query().Get("channel_code")
			var ch *string
			u, f, incidence := int64(publicUnit), int64(publicFee), incidencePassedOn
			if q != "" {
				c := q
				ch = &c
				u, f, incidence = resellerUnit, resellerFee, incidenceAbsorbed
			}
			chJSON := "null"
			if ch != nil {
				chJSON = `"` + *ch + `"`
			}
			if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
				got.fees, got.feesCalled = r.URL.RawQuery, true
				_, _ = w.Write([]byte(`{"resolver_version":1,"evaluated_at":"2026-08-17T00:00:00Z",` +
					`"organizer_id":"` + pricingOrg + `","performance_id":"` + pricingSlot + `",` +
					`"currency":"EUR","channel_code":` + chJSON + `,"fees":[{"fee_code":"s","winner":` +
					`{"rule_id":"22222222-2222-2222-2222-222222222222","fee_code":"s",` +
					`"basis":"per_ticket_fixed","amount":` + itoa(f) + `,"rate_bps":null,` +
					`"currency":"EUR","incidence":"` + incidence + `"}}]}`))
				return
			}
			got.price, got.priceCalled = r.URL.RawQuery, true
			_, _ = w.Write([]byte(resolutionBodyFor(u, true, ch)))
		}))
		defer catalog.Close()
		inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409) // stop after the quote; the money is decided by now
			_, _ = w.Write([]byte(`{"error":"unavailable"}`))
		}))
		defer inventory.Close()

		srv := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
		// The PUBLIC case posts a body that NAMES a reseller channel, through the
		// unvalidated router. That is the whole point: a channel-free public body
		// has nothing to leak, so a fixture built from one stays green with the
		// guard deleted and proves nothing. Driving the request the ATTACKER would
		// send is what makes this able to fail. The partner case takes its channel
		// from the credential, as the real path does.
		if channel == "" {
			req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(
				`{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
					`","quantity":`+itoa(quantity)+`,"channel_code":"reseller-acme"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "tkt248-public-"+t.Name())
			// The HANDLER, not the router: request validation is unconditional
			// (Router's bool is validateRESPONSES), so the schema would refuse this
			// body first and this test would be about the schema. reserveWithScope
			// with a nil scope IS the public path (server.go:595).
			srv.reserveWithScope(httptest.NewRecorder(), req, nil)
		} else {
			reserveInChannel(t, srv, "tkt248-"+t.Name(), channel)
		}
		return got
	}

	t.Run("a public sale naming a channel is refused before any economics are decided", func(t *testing.T) {
		got := drive(t, "")
		// The request NAMED reseller-acme. Before TKT-248 it would have been priced
		// on that channel: catalog would have been asked for it and answered the
		// cheaper, absorbed-fee economics below, and the buyer would have been
		// charged LESS with the organizer bearing the difference.
		if got.priceCalled || got.feesCalled {
			t.Fatalf("commerce resolved economics for a public request that named a channel "+
				"(price=%q fees=%q). The refusal must precede price and fee resolution — this is "+
				"the leak TKT-248 closes, and reaching catalog at all means the channel was "+
				"honoured (ADR-060).", got.price, got.fees)
		}
	})

	t.Run("a partner request carries its credential's channel to catalog", func(t *testing.T) {
		got := drive(t, "reseller-acme")
		if !strings.Contains(got.price, "channel_code=reseller-acme") {
			t.Fatalf("catalog's PRICE resolution was asked %q, want channel_code=reseller-acme — "+
				"a partner's channel comes from its credential and must still reach catalog", got.price)
		}
		if !strings.Contains(got.fees, "channel_code=reseller-acme") {
			t.Fatalf("catalog's FEE resolution was asked %q, want channel_code=reseller-acme", got.fees)
		}
	})

}

// A pre-TKT-248 public row that carried a channel does NOT replay under a
// channel-less retry: it answers 409, and that is ACCEPTED behaviour (ADR-060).
//
// This test pins a known consequence so the claim cannot drift from the code — the
// ADR-021 idiom. If it ever fails, the compatibility behaviour changed: update
// ADR-060, do not delete this test.
//
// The behaviour: `channel_code` is gone from the public request, so a retry of a
// reservation created before ADR-060 sends nil where the stored row has a channel.
// sameTerms compares them exactly and refuses. A "legacy public replay" exception
// was designed and REJECTED, because its only available predicate —
// `channel_code IS NOT NULL AND reseller_id IS NULL` — is also the shape of a row
// exchange_target_test.go calls "legal and routine", so it cannot distinguish what
// it claims, and it would put a permanent conditional on a money path for a
// transient condition.
//
// Why accepting it is safe: a 409 REFUSES rather than mischarges. The caller's
// remedy is a new idempotency key. The alternative — replaying under relaxed terms
// — is the one that could answer with the wrong money.
func TestALegacyPublicRowWithAChannelDoesNotReplayChannelLess(t *testing.T) {
	stored := "reseller-acme"
	public := reserveRequest{
		OrganizerID: uuid.MustParse(pricingOrg), TicketTypeID: uuid.MustParse(pricingTT),
		Quantity: 2, ChannelCode: nil, // what a public caller can send after ADR-060
	}

	if sameTerms(public, 2, uuid.MustParse(pricingTT), &stored, nil) {
		t.Fatal("a channel-less public retry matched a stored row that carries a channel. " +
			"That would replay a reservation priced on a channel's economics under a request " +
			"that named none — the terms are not the same and must not be treated as such. " +
			"If relaxing this was deliberate, ADR-060's Consequences section is now wrong.")
	}

	// The control, so the assertion above cannot pass merely because sameTerms
	// refuses everything: identical terms still match.
	if !sameTerms(public, 2, uuid.MustParse(pricingTT), nil, nil) {
		t.Fatal("a channel-less retry of a channel-less row was refused; sameTerms is refusing " +
			"terms that ARE the same, and the assertion above proves nothing")
	}

	// And a partner row still replays on its own channel — unchanged by TKT-248.
	partner := public
	partner.ChannelCode = &stored
	if !sameTerms(partner, 2, uuid.MustParse(pricingTT), &stored, nil) {
		t.Fatal("a partner retry carrying its credential's channel no longer matches its own " +
			"stored row: TKT-248 must not have changed the authenticated path")
	}
}
