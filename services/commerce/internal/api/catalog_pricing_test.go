package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TKT-153: the sale is priced by catalog's RULE RESOLUTION, not by the raw
// ticket-type price. ADR-036 §6 makes catalog the single authority for the
// resolved unit price; commerce consumes resolved_price and never recomputes it.

const (
	pricingOrg  = "00000000-0000-0000-0000-000000000001"
	pricingTT   = "00000000-0000-0000-0000-000000000002"
	pricingSlot = "00000000-0000-0000-0000-000000000009"
	pricingRule = "00000000-0000-0000-0000-0000000000a1"
)

// resolutionBody is a well-formed PriceResolution with a winning rule.
// organizer_id and performance_id are part of the response (TKT-153 added them)
// so ONE catalog read carries identity, slot, money and provenance — two reads
// would need a coherence check between them that can false-positive on a
// legitimate concurrent price edit.
func resolutionBody(resolved int64, winner bool) string {
	w := `null`
	fallback := `,"fallback_reason":"no_eligible_rule"`
	if winner {
		w = `{"rule_id":"` + pricingRule + `","scope_level":"event",` +
			`"scope_id":"00000000-0000-0000-0000-0000000000e1","action_kind":"absolute",` +
			`"amount":` + itoa(resolved) + `,"currency":"EUR","effective_from":null,` +
			`"effective_until":null,"priority":0,"forced":false}`
		fallback = ``
	}
	return `{"resolver_version":2,"evaluated_at":"2026-07-31T00:00:00Z",` +
		`"organizer_id":"` + pricingOrg + `","performance_id":"` + pricingSlot + `",` +
		`"base_price":{"amount":2500,"currency":"EUR"},` +
		`"resolved_price":{"amount":` + itoa(resolved) + `,"currency":"EUR"},` +
		`"winner":` + w + `,"candidates":[]` + fallback + `}`
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// pricingStack wires a fake catalog whose resolution endpoint returns `body`
// with `status`, plus an inventory that echoes the unit_amount it was handed.
func pricingStack(t *testing.T, status int, body string) (*Server, *int64, func()) {
	t.Helper()
	var heldUnitAmount int64 = -1
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/price-resolution") {
			// The internal credential must NEVER be sent to this route: it is a
			// declared, publicly routable operation, and leaking a service
			// credential onto a public path would be strictly worse than the
			// exposure TKT-155 already records.
			if r.Header.Get("X-Internal-Token") != "" {
				t.Errorf("internal credential leaked to the public resolution route")
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		t.Errorf("unexpected catalog path %q — the reserve path takes ONE read", r.URL.Path)
	}))
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			UnitAmount int64 `json:"unit_amount"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		heldUnitAmount = in.UnitAmount
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"stop here — the hold amount is what this test is about"}`))
	}))
	s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	return s, &heldUnitAmount, func() { catalog.Close(); inventory.Close() }
}

func reserve(t *testing.T, s *Server, key string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"organizer_id":"` + pricingOrg + `","ticket_type_id":"` + pricingTT + `","quantity":2}`
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	return res
}

// The headline behaviour: the hold is placed at the RESOLVED price (900), not
// the ticket type's own price (2500).
func TestReserveUsesCatalogResolvedPriceForInventoryHold(t *testing.T) {
	s, held, done := pricingStack(t, 200, resolutionBody(900, true))
	defer done()

	reserve(t, s, "resolved-price")

	if *held != 900 {
		t.Fatalf("inventory hold unit_amount = %d, want the RESOLVED 900 — not the base 2500", *held)
	}
}

// No rule matched is a SUCCESSFUL resolution answering with the base price. It
// is the case that must stay byte-identical to pre-TKT-153 behaviour, and it is
// emphatically not an error (ADR-036 §4 step 6 / ADR-028's distinction).
func TestReserveFallbackUsesBasePrice(t *testing.T) {
	s, held, done := pricingStack(t, 200, resolutionBody(2500, false))
	defer done()

	reserve(t, s, "fallback")

	if *held != 2500 {
		t.Fatalf("inventory hold unit_amount = %d, want the base 2500", *held)
	}
}

// "Could not resolve" is not "no rule matched". Every unusable answer must abort
// BEFORE inventory — never degrade to the base price, which would silently sell
// at the wrong price and look like nothing happened (ADR-028 fail-closed).
func TestReserveFailsClosedOnUnusableResolution(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"catalog error":            {502, `{"error":"boom"}`},
		"malformed json":           {200, `{`},
		"winner and fallback both": {200, strings.Replace(resolutionBody(900, true), `"candidates":[]`, `"candidates":[],"fallback_reason":"no_eligible_rule"`, 1)},
		"neither winner nor fallback": {200,
			strings.Replace(resolutionBody(2500, false), `,"fallback_reason":"no_eligible_rule"`, ``, 1)},
		"resolved price disagrees with winner": {200, strings.Replace(resolutionBody(900, true),
			`"resolved_price":{"amount":900`, `"resolved_price":{"amount":901`, 1)},
		"non-absolute action": {200, strings.Replace(resolutionBody(900, true), `"absolute"`, `"multiplier"`, 1)},
		// Both the resolved price AND the winner's amount, so this fails on the
		// NEGATIVE guard rather than on winner-disagreement. Changing only one
		// made the fixture pass for the wrong reason: deleting the negative
		// check would not have reddened it.
		"negative amount":  {200, strings.ReplaceAll(resolutionBody(900, true), `"amount":900`, `"amount":-1`)},
		"wrong organizer":  {200, strings.Replace(resolutionBody(900, true), pricingOrg, "00000000-0000-0000-0000-0000000000ff", 1)},
		"non-EUR currency": {200, strings.ReplaceAll(resolutionBody(900, true), `"EUR"`, `"USD"`)},
		// base_price absent decodes to a zero Money and is never inspected on
		// the winner path, so the response would sail through.
		"missing base_price":  {200, strings.Replace(resolutionBody(900, true), `"base_price":{"amount":2500,"currency":"EUR"},`, ``, 1)},
		"negative base_price": {200, strings.Replace(resolutionBody(900, true), `"base_price":{"amount":2500`, `"base_price":{"amount":-5`, 1)},
		// A base in a different currency from the resolved price is incoherent:
		// ADR-036 makes a currency mismatch fail resolution outright, so a
		// response claiming both cannot have come from a sound resolver.
		"base_price in another currency": {200, strings.Replace(resolutionBody(900, true), `"base_price":{"amount":2500,"currency":"EUR"}`, `"base_price":{"amount":2500,"currency":"GBP"}`, 1)},
		"missing evaluated_at":           {200, strings.Replace(resolutionBody(900, true), `"evaluated_at":"2026-07-31T00:00:00Z",`, ``, 1)},
		"winner without scope_id":        {200, strings.Replace(resolutionBody(900, true), `"scope_id":"00000000-0000-0000-0000-0000000000e1",`, ``, 1)},
		// Two shapes PostgreSQL's jsonb refuses but Go does not stop at the door.
		// Each would otherwise validate, create the hold, and only then fail the
		// INSERT -- orphan hold, 500 for the buyer. Both arrive in a field this
		// build does not know, which is exactly where a byte-scan for one
		// spelling of NUL missed them. TestStorableSnapshot covers the rest.
		"unstorable NUL in an unknown field": {200, strings.Replace(resolutionBody(900, true), `"candidates":[]`, `"candidates":[],"future_field":"\u0000"`, 1)},
		"unstorable out-of-range number":     {200, strings.Replace(resolutionBody(900, true), `"candidates":[]`, `"candidates":[],"future_field":1e400000`, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			s, held, done := pricingStack(t, tc.status, tc.body)
			defer done()

			res := reserve(t, s, "fail-closed-"+name)

			if *held != -1 {
				t.Errorf("inventory was called with unit_amount %d — an unusable resolution must abort BEFORE the hold", *held)
			}
			if res.Code < 400 {
				t.Errorf("status = %d, want a failure — never a silent fallback to the base price", res.Code)
			}
		})
	}
}

// The existing overflow guard must still hold for a rule-supplied amount: it is
// the amount that reaches the multiplication, so moving where the amount comes
// from must not move the guard (ADR-001).
func TestReserveOverflowGuardAppliesToResolvedAmount(t *testing.T) {
	huge := `{"resolver_version":2,"evaluated_at":"2026-07-31T00:00:00Z",` +
		`"organizer_id":"` + pricingOrg + `","performance_id":"` + pricingSlot + `",` +
		`"base_price":{"amount":2500,"currency":"EUR"},` +
		`"resolved_price":{"amount":9007199254740991,"currency":"EUR"},` +
		`"winner":{"rule_id":"` + pricingRule + `","scope_level":"event",` +
		`"scope_id":"00000000-0000-0000-0000-0000000000e1","action_kind":"absolute",` +
		`"amount":9007199254740991,"currency":"EUR","effective_from":null,` +
		`"effective_until":null,"priority":0,"forced":false},"candidates":[]}`
	s, held, done := pricingStack(t, 200, huge)
	defer done()

	// quantity 2 × 2^53-1 still fits int64, so this one must be ALLOWED through
	// — the guard is about overflow, not about large prices.
	reserve(t, s, "overflow-ok")
	if *held != 9007199254740991 {
		t.Fatalf("hold unit_amount = %d, want the resolved amount — the guard must not reject a representable price", *held)
	}
}

// A resolver version newer than this build is NOT a reason to refuse a sale,
// and getting that wrong cost a self-inflicted outage in review.
//
// Commerce consumes `resolved_price`, whose contract is "the unit price for this
// ticket type". That contract does not change when the comparator's DERIVATION
// does, so capping the version bought no price safety while guaranteeing that
// deploying catalog before commerce stops every new reservation. The version is
// recorded in the snapshot because a stored provenance document must stay
// interpretable — a read-side concern, not a gate on the sale.
func TestReserveAcceptsANewerResolverVersion(t *testing.T) {
	future := strings.Replace(resolutionBody(900, true), `"resolver_version":2`, `"resolver_version":999`, 1)
	s, held, done := pricingStack(t, 200, future)
	defer done()

	reserve(t, s, "future-resolver")

	if *held != 900 {
		t.Fatalf("inventory hold unit_amount = %d, want 900 — a newer resolver must not stop the sale", *held)
	}
}

// A legitimate string whose TEXT is the six characters of the NUL escape is
// storable, and the earlier byte-scan rejected it. The check works on decoded
// strings now, so this must go through.
func TestReserveAcceptsALiteralBackslashUZeroString(t *testing.T) {
	literal := strings.Replace(resolutionBody(900, true), `"candidates":[]`,
		`"candidates":[],"future_field":"\\u0000"`, 1)
	s, held, done := pricingStack(t, 200, literal)
	defer done()

	reserve(t, s, "literal-escape-text")

	if *held != 900 {
		t.Fatalf("inventory hold unit_amount = %d, want 900 — the TEXT of an escape is storable", *held)
	}
}

// storableSnapshot is tested DIRECTLY, because a reserve-level test cannot see
// it: the fake inventory answers 409 before persistence, so every one of these
// inputs reaches the same place whether the function works or not. Asserting the
// bytes it produces is the only way these cases can fail.
func TestStorableSnapshot(t *testing.T) {
	t.Run("an unpaired surrogate is normalised, not refused", func(t *testing.T) {
		// PostgreSQL's jsonb rejects the raw bytes, so this had to be handled
		// before the hold — but REFUSING would fail a sale over a byte in a
		// field nobody reads. Decoding and re-encoding replaces it with U+FFFD,
		// which is storable, and no field that decides a price is touched.
		out, err := storableSnapshot([]byte(`{"a":"\ud800"}`))
		if err != nil {
			t.Fatalf("an unpaired surrogate must be normalised, not refused: %v", err)
		}
		if !strings.Contains(string(out), "\ufffd") {
			t.Errorf("output = %s, want the surrogate replaced by U+FFFD", out)
		}
	})

	t.Run("a real NUL is refused", func(t *testing.T) {
		if _, err := storableSnapshot([]byte(`{"a":"\u0000"}`)); err == nil {
			t.Error("jsonb refuses NUL, so this must fail BEFORE the hold exists")
		}
		if _, err := storableSnapshot([]byte(`{"\u0000":"a"}`)); err == nil {
			t.Error("a NUL in a KEY must be refused too")
		}
		if _, err := storableSnapshot([]byte(`{"a":[{"b":"\u0000"}]}`)); err == nil {
			t.Error("a NUL nested inside an array/object must be refused")
		}
	})

	t.Run("the TEXT of an escape is storable", func(t *testing.T) {
		// The six literal characters, not the code point. The byte-scan this
		// replaced rejected it.
		if _, err := storableSnapshot([]byte(`{"a":"\\u0000"}`)); err != nil {
			t.Errorf("a string whose text is the escape is storable: %v", err)
		}
	})

	t.Run("numbers survive exactly", func(t *testing.T) {
		// Decoding into float64 silently rounded this to ...992, corrupting the
		// document this ticket exists to keep true.
		out, err := storableSnapshot([]byte(`{"a":9007199254740993}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "9007199254740993") {
			t.Errorf("output = %s, want the integer preserved exactly", out)
		}
	})

	t.Run("a big-but-valid number is accepted", func(t *testing.T) {
		// 1e400 overflows float64 and is perfectly good PostgreSQL numeric.
		// Rejecting it would turn an additive unknown field into a sale outage.
		if _, err := storableSnapshot([]byte(`{"a":1e400}`)); err != nil {
			t.Errorf("1e400 is storable as numeric: %v", err)
		}
	})

	t.Run("a number past numeric's range is refused", func(t *testing.T) {
		if _, err := storableSnapshot([]byte(`{"a":1e400000}`)); err == nil {
			t.Error("a number beyond numeric's digit limit must fail before the hold")
		}
	})
}

// seatedStack is pricingStack's seated twin: it records which inventory path the
// reservation took and what it sent, and answers with whatever the test wants
// inventory to say. Separate from pricingStack because that helper asserts the
// hold amount by hard-failing at inventory, and the seated cases need to inspect
// the route and the seat set as well.
func seatedStack(t *testing.T, invStatus int, invBody string) (*Server, *string, *[]string, func()) {
	t.Helper()
	var path string
	var seats []string
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(resolutionBody(900, true)))
	}))
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var in struct {
			SeatIdentities []string `json:"seat_identities"`
			UnitAmount     int64    `json:"unit_amount"`
			Quantity       int32    `json:"quantity"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		seats = in.SeatIdentities
		if in.UnitAmount != 900 {
			t.Errorf("inventory got unit_amount=%d, want the catalog-RESOLVED 900 (ADR-036)", in.UnitAmount)
		}
		if in.Quantity != 0 {
			t.Errorf("a seated claim must not carry a quantity, got %d", in.Quantity)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(invStatus)
		_, _ = w.Write([]byte(invBody))
	}))
	s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	return s, &path, &seats, func() { catalog.Close(); inventory.Close() }
}

func reserveSeats(t *testing.T, s *Server, key string, seats string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"organizer_id":"` + pricingOrg + `","ticket_type_id":"` + pricingTT + `","seat_identities":` + seats + `}`
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	return res
}

// TestReserveRoutesSeatedClaimsToTheSeatHold is TKT-173 AC2 + AC3: a seated
// reservation reaches inventory's SEAT claim, not the quantity hold, and carries
// catalog's resolved unit price. Routing it to /holds would be the quiet disaster —
// it would succeed, hold N units of a seated pool's capacity, and hold nobody's seat.
// (It would in fact be refused by ErrPoolKindMismatch today, but relying on the far
// service to catch a routing bug is not a design.)
func TestReserveRoutesSeatedClaimsToTheSeatHold(t *testing.T) {
	s, path, seats, done := seatedStack(t, 409, `{"error":"stop here — the route and the price are what this test is about"}`)
	defer done()

	reserveSeats(t, s, "seated-route", `["B/2/2","A/1/1"]`)

	if *path != "/holds/seats" {
		t.Fatalf("seated reservation hit %q, want /holds/seats", *path)
	}
	if len(*seats) != 2 || (*seats)[0] != "B/2/2" {
		t.Fatalf("inventory got seats %v — commerce forwards the request set verbatim and lets "+
			"inventory canonicalise it", *seats)
	}
}

// TestReserveForwardsContendedSeats is AC4. A bare 409 tells a picker that something
// went wrong; it cannot tell it which seats to re-render. The identities come from
// the inventory transaction that arbitrated — commerce forwards them and never
// invents them.
func TestReserveForwardsContendedSeats(t *testing.T) {
	s, _, _, done := seatedStack(t, 409,
		`{"error":"seat already held by another live claim: A/1/1","code":"seat_taken","seat_identities":["A/1/1"]}`)
	defer done()

	res := reserveSeats(t, s, "seated-conflict", `["A/1/1","A/1/2"]`)

	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409: %s", res.Code, res.Body.String())
	}
	var out struct {
		Code  string   `json:"code"`
		Seats []string `json:"seat_identities"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Code != "seat_taken" {
		t.Fatalf("code = %q want seat_taken — a picker branches on this: %s", out.Code, res.Body.String())
	}
	// Only the contended seat. A/1/2 was free and must not be named: telling a buyer
	// to give up a seat they could have had is its own defect.
	if len(out.Seats) != 1 || out.Seats[0] != "A/1/1" {
		t.Fatalf("seat_identities = %v want [A/1/1] only", out.Seats)
	}
}

// TestReserveRefusesToInventContendedSeats: an inventory `seat_taken` with no usable
// identity list is a broken upstream, not a licence to guess. Echoing the request
// would name seats that were never contended, which is exactly the lie AC4 exists to
// prevent — and it would look completely plausible to the buyer.
func TestReserveRefusesToInventContendedSeats(t *testing.T) {
	for name, body := range map[string]string{
		"no identities":    `{"error":"seat taken","code":"seat_taken"}`,
		"empty identities": `{"error":"seat taken","code":"seat_taken","seat_identities":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, done := seatedStack(t, 409, body)
			defer done()

			res := reserveSeats(t, s, "seated-broken-"+name, `["A/1/1","A/1/2"]`)

			if res.Code != http.StatusBadGateway {
				t.Fatalf("status = %d want 502: %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "A/1/1") {
				t.Fatalf("commerce must not name seats inventory did not name: %s", res.Body.String())
			}
		})
	}
}

// TestReserveFailsClosedOnInconsistentSeatClaim is the ai-review finding: a
// schema-valid but inconsistent inventory response must not be persisted, billed
// from, or later refunded against.
//
// Each case needs an inventory defect or a version skew to occur, which is exactly
// when a cross-service check earns its keep — the failure mode is a wrong-seat sale,
// and no response validator can see it, because every one of these bodies is
// perfectly well-shaped.
//
// The handler is built with a nil database on purpose: every assertion here must be
// refused BEFORE anything is persisted, so reaching the insert would panic rather
// than pass.
func TestReserveFailsClosedOnInconsistentSeatClaim(t *testing.T) {
	const holdID = `"hold_id":"11111111-1111-1111-1111-111111111111"`
	const times = `"expires_at":"2026-11-05T20:00:00Z","server_time":"2026-11-05T19:00:00Z"`
	for name, body := range map[string]string{
		// Same count, different seats: the buyer would be charged the right amount
		// for the wrong seats, and would be issued tickets for them.
		"seat substitution": `{` + holdID + `,` + times + `,"quantity":2,"seats":["Z/9/1","Z/9/2"]}`,
		// The claim's own quantity disagrees with its own seat list. Whichever is
		// right, commerce cannot know, and it drives money from one and inventory
		// reconciliation from the other.
		"quantity disagrees with the seat list": `{` + holdID + `,` + times + `,"quantity":3,"seats":["A/1/1","A/1/2"]}`,
		"no seats at all":                       `{` + holdID + `,` + times + `,"quantity":2,"seats":[]}`,
		"a seat the buyer never asked for":      `{` + holdID + `,` + times + `,"quantity":2,"seats":["A/1/1","Z/9/9"]}`,
		"fewer seats than requested":            `{` + holdID + `,` + times + `,"quantity":1,"seats":["A/1/1"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, done := seatedStack(t, 201, body)
			defer done()

			res := reserveSeats(t, s, "inconsistent-"+name, `["A/1/1","A/1/2"]`)

			if res.Code != http.StatusBadGateway {
				t.Fatalf("status = %d want 502 — an inconsistent claim must never be "+
					"persisted or billed: %s", res.Code, res.Body.String())
			}
		})
	}
}

// TestReserveRefusesUnrequestedContendedSeats: a refusal may only name seats this
// buyer actually asked for. Forwarding verbatim would let a defect or a skew grey
// out somebody else's seats in the picker, and the response schema cannot catch it —
// it is a semantic mismatch, not a shape one.
func TestReserveRefusesUnrequestedContendedSeats(t *testing.T) {
	for name, seats := range map[string]string{
		"an unrelated seat":         `["Z/9/9"]`,
		"one requested one not":     `["A/1/1","Z/9/9"]`,
		"a repeat of one requested": `["A/1/1","A/1/1"]`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, done := seatedStack(t, 409,
				`{"error":"seat taken","code":"seat_taken","seat_identities":`+seats+`}`)
			defer done()

			res := reserveSeats(t, s, "unrequested-"+name, `["A/1/1","A/1/2"]`)

			if res.Code != http.StatusBadGateway {
				t.Fatalf("status = %d want 502: %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "Z/9/9") {
				t.Fatalf("commerce leaked an unrequested identity: %s", res.Body.String())
			}
		})
	}
}

// TestReserveForwardsOrphanedSeats is ADR-041's refusal reaching the buyer. The
// identities here are seats the buyer did NOT request — the ones their selection would
// strand — so commerce's `seat_taken` subset rule must NOT be applied to them. It would
// turn every valid orphan refusal into a 502.
func TestReserveForwardsOrphanedSeats(t *testing.T) {
	s, _, _, done := seatedStack(t, 409,
		`{"error":"would strand","code":"orphaned_seats","seat_identities":["A/1/2"]}`)
	defer done()

	res := reserveSeats(t, s, "orphan-refusal", `["A/1/1","A/1/3"]`)

	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409: %s", res.Code, res.Body.String())
	}
	var out struct {
		Code  string   `json:"code"`
		Seats []string `json:"seat_identities"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Code != "orphaned_seats" {
		t.Fatalf("code = %q want orphaned_seats — a picker branches on this to keep the "+
			"named seats SELECTABLE rather than greying them out: %s", out.Code, res.Body.String())
	}
	// A/1/2 was never requested. Forwarding it is the point.
	if len(out.Seats) != 1 || out.Seats[0] != "A/1/2" {
		t.Fatalf("seat_identities = %v want [A/1/2]", out.Seats)
	}
}

func TestReserveRefusesMalformedOrphanRefusal(t *testing.T) {
	for name, body := range map[string]string{
		"no identities":        `{"error":"x","code":"orphaned_seats"}`,
		"duplicate identities": `{"error":"x","code":"orphaned_seats","seat_identities":["A/1/2","A/1/2"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, done := seatedStack(t, 409, body)
			defer done()
			if res := reserveSeats(t, s, "bad-orphan-"+name, `["A/1/1"]`); res.Code != http.StatusBadGateway {
				t.Fatalf("status = %d want 502: %s", res.Code, res.Body.String())
			}
		})
	}
}

// An orphaned seat is by definition one the buyer did NOT request. Accepting a
// requested identity would have the picker propose an impossible repair — "add the
// seat you already asked for" — so the subset rule that guards seat_taken is exactly
// inverted here (ai-review).
func TestReserveRefusesOrphanIdentitiesThatWereRequested(t *testing.T) {
	for name, seats := range map[string]string{
		"a seat the buyer requested": `["A/1/1"]`,
		"blank identity":             `["   "]`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _, done := seatedStack(t, 409,
				`{"error":"x","code":"orphaned_seats","seat_identities":`+seats+`}`)
			defer done()
			if res := reserveSeats(t, s, "bad-orphan2-"+name, `["A/1/1","A/1/2"]`); res.Code != http.StatusBadGateway {
				t.Fatalf("status = %d want 502: %s", res.Code, res.Body.String())
			}
		})
	}
}
