package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The commerce -> inventory channel seam: STILL OPEN, and pinned open on purpose
// (TKT-240, reverted; TKT-246 owns the closure).
//
// `channel_code` reaches catalog's fee resolution and stops there. Inventory's
// channel_code is what channel_allocations cap consumption against (ADR-024), so a
// reseller-channel sale takes that channel's fees while eating PUBLIC inventory.
// That is a real defect and it is not fixed here.
//
// TKT-240 did close it, by adding `channel` to the GA hold body, and the closure
// was reverted after its own adversarial review. The forward is necessary and NOT
// sufficient: `POST /reservations` is unauthenticated and takes channel_code from
// the request body, so with the forward in place an unauthenticated caller could
// name a reseller`s channel and consume its allocation. That was executed against
// the code, not argued: the probe reached inventory with channel=reseller-acme and
// no credential.
//
// So closing the seam is an AUTHORIZATION change. The allocation must say who may
// sell it and inventory must judge that under the pool row lock -- the shape
// ADR-055 gave requires_code -- and every hold path (first attempt, persisted
// replay, exchange target) must carry the channel or be refused. TKT-246.
//
// THESE TESTS PIN THE PRESENT, NOT THE PAST. They assert the GA hold carries no
// channel today. That is not an endorsement: it is a tripwire, so that re-adding
// the forward on its own -- the tempting one-line "fix" -- fails loudly here and
// sends the author to TKT-246 instead of shipping a bypassable guard. When TKT-246
// lands, these assertions INVERT; do not delete them.
//
// TKT-246 UPDATE -- the tripwire did NOT invert, and that is the ticket's finding.
//
// The closure shipped, and the PUBLIC route still forwards no channel. Inverting
// this test would have meant forwarding a body-supplied channel from an
// unauthenticated route, which is the bypass the revert exists for; a per-allocation
// binding does not save it, because every allocation that exists today is UNBOUND
// and therefore consumable by anyone who names its channel. So the seam is closed
// where a channel can be AUTHORIZED -- the authenticated partner route, which takes
// the channel from its credential and never from a body -- and the public route is
// kept unable to reach the decision at all.
//
// What survives is the fee-attribution half of the original defect: a public sale
// naming a reseller channel still prices under that channel's fee rules while
// consuming public stock. It no longer moves anyone's INVENTORY without a
// credential. See TKT-248 for the remainder and ADR-024 for the reasoning.
// (That pointer read TKT-247 until TKT-248 corrected it; TKT-247 is a
// scanner-device flake and never owned any of this.)
//
// TKT-248 UPDATE -- the remainder is CLOSED, and the closure is an entitlement.
//
// `channel_code` was removed from ReservationCreate entirely (ADR-060): a public
// caller cannot name a channel, so there is no body-supplied channel left to price
// on. The first test below was CONVERTED IN PLACE to that refusal boundary rather
// than replaced, because this file is the record of why TKT-240 was reverted and a
// replacement would lose it. The other two are unchanged and still guard the seam
// itself. TestAPartnerGASaleForwardsItsChannel below is the positive half.

// capturedHold is the inventory hold body commerce sent.
type capturedHold struct {
	path string
	body map[string]any
	// status and catalogCalls are populated by reserveThroughCommerceUnvalidated
	// only; the validated helper discards the response on purpose (its subject is
	// what commerce SENT, not what it answered).
	status       int
	catalogCalls int
}

// reserveThroughCommerceWithScope drives one reserve as an AUTHENTICATED partner,
// whose channel comes from the credential (TKT-248). It is how a test gets a
// channel onto a request now that a public body cannot carry one.
func reserveThroughCommerceWithScope(t *testing.T, channel, requestBody string) capturedHold {
	t.Helper()
	var got capturedHold
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.catalogCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			_, _ = w.Write([]byte(emptyFeeResolutionBody(&channel)))
			return
		}
		_, _ = w.Write([]byte(resolutionBodyFor(2500, false, &channel)))
	}))
	defer catalog.Close()

	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/holds") {
			raw, _ := io.ReadAll(r.Body)
			got.path = r.URL.Path
			got.body = map[string]any{}
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer inventory.Close()

	srv := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "seam-scope-"+t.Name())
	rec := httptest.NewRecorder()
	srv.reserveWithScope(rec, req, &partnerScope{
		OrganizerID: uuid.MustParse(pricingOrg),
		ChannelCode: channel,
		ResellerID:  uuid.MustParse(pricingReseller),
	})
	got.status = rec.Code
	return got
}

// reserveThroughCommerceUnvalidated drives one reserve with response/request
// validation OFF, and reports what commerce ANSWERED as well as what it sent.
//
// Validation off is the whole point (TKT-248). The contract no longer declares
// `channel_code` on ReservationCreate and refuses unknown properties, so with
// validation on this request never reaches the handler and the test would be
// proving the schema. Two mechanisms refuse this field; each needs a case that can
// see it alone, or deleting one stays green behind the other.
func reserveThroughCommerceUnvalidated(t *testing.T, channel *string, requestBody string) capturedHold {
	t.Helper()
	var got capturedHold
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.catalogCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			_, _ = w.Write([]byte(emptyFeeResolutionBody(channel)))
			return
		}
		_, _ = w.Write([]byte(resolutionBodyFor(2500, false, channel)))
	}))
	defer catalog.Close()

	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/holds") {
			raw, _ := io.ReadAll(r.Body)
			got.path = r.URL.Path
			got.body = map[string]any{}
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer inventory.Close()

	srv := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "seam-"+t.Name())
	rec := httptest.NewRecorder()
	// reserveWithScope with a NIL scope IS the public path (server.go:595). Called
	// directly rather than through the router because request validation is
	// unconditional there -- `Router`'s bool is validateRESPONSES -- so the schema
	// would refuse this body first and the handler check would never run. Two
	// mechanisms refuse this field; a test that cannot reach the second one is
	// silent about it, and deleting it would stay green behind the first.
	srv.reserveWithScope(rec, req, nil)
	got.status = rec.Code
	return got
}

// reserveThroughCommerce drives one reserve and returns what commerce sent to
// inventory. The inventory stub answers 201 so the reserve proceeds far enough to
// have sent the hold; what it answers is irrelevant to what it RECEIVED.
func reserveThroughCommerce(t *testing.T, channel *string, requestBody string) capturedHold {
	t.Helper()
	var got capturedHold
	// Catalog ECHOES the channel it was asked about. Commerce validates that echo
	// against what it sent (TKT-237), so a stub answering null would make every
	// channelled reserve fail before the hold — the guard working, not a test bug,
	// and it would make this test green for the wrong reason.
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(r.URL.Path, "/fee-resolution") {
			_, _ = w.Write([]byte(emptyFeeResolutionBody(channel)))
			return
		}
		_, _ = w.Write([]byte(resolutionBodyFor(2500, false, channel)))
	}))
	defer catalog.Close()

	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/holds") {
			raw, _ := io.ReadAll(r.Body)
			got.path = r.URL.Path
			got.body = map[string]any{}
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409) // stop after the hold; this test is about the request
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer inventory.Close()

	srv := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "seam-"+t.Name())
	srv.Router(nil, true).ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// A public GA sale may not name a channel AT ALL (TKT-248).
//
// THIS TEST WAS CONVERTED IN PLACE, NOT REPLACED. Until TKT-248 it was
// TestAChannelledGASaleDoesNotYetForwardItsChannel, and it asserted that a public
// channelled sale reached inventory carrying no channel -- the tripwire against
// re-adding TKT-240's forward without the authorization half. That scenario is now
// unreachable: option A removed `channel_code` from the public request entirely, so
// a public sale CANNOT name a channel and there is no forward left to guard.
//
// The assertion therefore moves UP to the refusal boundary rather than being
// deleted. Its history is the point: this file is the repo's record of why TKT-240
// was reverted, and the two tests below still guard the seam itself.
//
// This is the HANDLER tier, deliberately. The schema refuses the field too
// (ReservationCreate no longer declares it, with additionalProperties:false), and
// that is the real guard -- the field is unsubmittable rather than validated. But a
// test that posts through the router proves the SCHEMA and is silent about the
// handler: request validation is unconditional (Router's bool is
// validateRESPONSES), so the body never gets that far. Delete the handler check and
// such a test stays green behind the schema. This calls the handler directly, which
// is the only way to see it refuse on its own -- and it is not a synthetic path:
// reserveWithScope(nil) is exactly what the public route does.
func TestAPublicGASaleMayNotNameAChannel(t *testing.T) {
	reseller := "reseller-acme"
	got := reserveThroughCommerceUnvalidated(t, &reseller,
		`{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
			`","quantity":2,"channel_code":"reseller-acme"}`)

	if got.status != http.StatusBadRequest {
		t.Fatalf("a public reserve naming channel_code answered %d, want 400. The public route is "+
			"unauthenticated, so a body-supplied channel is a caller choosing their own price "+
			"basis: catalog resolves BOTH fees (ADR-046) and price (TKT-237) on it, and an "+
			"`absorbed` rule means the buyer is charged LESS while the organizer eats the "+
			"difference. TKT-248/ADR-060. The channel comes from a partner credential or from "+
			"nowhere.", got.status)
	}
	if got.body != nil {
		t.Fatalf("commerce called inventory's hold endpoint despite refusing the request: the "+
			"refusal must happen before any downstream call. body=%v", got.body)
	}
	if got.catalogCalls != 0 {
		t.Fatalf("commerce made %d catalog call(s) for a request it refused; the refusal must "+
			"precede price and fee resolution, not follow them", got.catalogCalls)
	}
}

// An unchannelled sale sends NO channel — omitting it is not the same as sending
// an empty one.
//
// `reserveRequest.ChannelCode` is a POINTER precisely so that nil (the
// default/public context) is distinguishable from a caller sending "". Forwarding
// an empty string would make every public sale look channelled to inventory, whose
// allocation lookup would then find no active allocation and refuse the sale — a
// platform-wide outage dressed as a channel feature.
func TestAnUnchannelledSaleForwardsNoChannelAtAll(t *testing.T) {
	got := reserveThroughCommerce(t, nil, `{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
		`","quantity":2}`)

	// Asserted FIRST and separately: this test's claim is an ABSENCE, and an
	// absence assertion passes trivially when the request never reached inventory
	// at all. Without this line the test would stay green against a commerce that
	// stopped calling inventory entirely.
	if got.body == nil {
		t.Fatal("commerce never called inventory's hold endpoint; an absence test that " +
			"never reaches the wire proves nothing")
	}
	if _, hasQty := got.body["quantity"]; !hasQty {
		t.Fatalf("the captured body is not a GA hold (no quantity): %v", got.body)
	}
	if v, present := got.body["channel"]; present {
		t.Fatalf("an unchannelled sale forwarded channel=%#v; omitting a channel is the public "+
			"context, and sending one makes inventory look for an allocation that does not exist", v)
	}
}

// The seated half of the seam stays with TKT-176 and must not move here.
//
// SeatHoldCreate has no `channel` field at all, so a seated claim ignores channel
// allocations entirely. That is a real defect with its own display consequences,
// and it is deliberately NOT fixed here: half-fixing it would mean a seated
// reseller sale silently consuming a GA channel's allocation. This test is the
// guard on the non-goal — if it ever fails, TKT-176 was closed by accident and the
// ADR needs updating, not the test.
// TKT-248 changed only HOW this test gets a channel onto the request, not what it
// asserts. It used to name the channel in a public body; that is no longer
// possible (ADR-060), so the channel now comes from a partner scope -- the only
// place a channel exists at all. The subject is unchanged: the seated hold body
// commerce sends must still carry no channel.
func TestTheSeatedHoldStillCarriesNoChannel(t *testing.T) {
	got := reserveThroughCommerceWithScope(t, "reseller-acme",
		`{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
			`","seat_identities":["A-1","A-2"]}`)

	if got.body == nil {
		t.Fatal("commerce never called inventory's hold endpoint")
	}
	if !strings.Contains(got.path, "/holds/seats") {
		t.Fatalf("a seated request did not take the seated hold path (%s)", got.path)
	}
	if _, present := got.body["channel"]; present {
		t.Fatal("the seated hold body now carries a channel: the seated seam is TKT-176's, and " +
			"forwarding a channel inventory's seat path does not read is a half-fix that reads as done")
	}
}
