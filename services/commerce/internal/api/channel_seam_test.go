package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/commerce/api"
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
// ADR-064 gave requires_code -- and every hold path (first attempt, persisted
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
// TKT-248 UPDATE: a CHANNELLED seated request is now UNREACHABLE, and this test
// says so instead of pretending otherwise (ai-review [medium]).
//
// The original asserted that a seated hold carries no channel, driving it with a
// public body that named one. Both vehicles for that request are gone:
//
//   - the PUBLIC contract no longer has `channel_code` (ADR-060);
//   - the PARTNER contract is GA-only -- PartnerReservationCreate has no
//     `seat_identities` at all, and openapi.yaml:325-327 says why: "seated pools do
//     not consult channel allocations at all (TKT-176 owns that seam), so a seated
//     partner sale would claim an authorization this contract cannot deliver".
//
// So there is no route by which a seated claim can carry a channel. An earlier
// version of this test constructed one anyway by calling reserveWithScope directly
// with seat_identities -- a request no client can send, which would have stayed
// green regardless of the real wiring. That is the fixture-that-proves-nothing
// shape, so it is replaced by asserting the two contracts that make it impossible.
//
// TKT-176 still owns the seated seam. What changed is that the way in is now shut
// at the contract rather than open and forwarding nothing.
func TestASeatedSaleCannotCarryAChannelAtAll(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}

	// WHICH SCHEMA EACH OPERATION ACTUALLY USES, asserted first (ai-review pass 2
	// [high]). Inspecting the two component definitions alone does not pin the
	// boundary: repointing /partners/reservations at ReservationCreate would admit
	// seat_identities, authentication would then inject the credential's channel,
	// and the seated path would carry one -- while every property assertion below
	// stayed green. The wiring is half the claim.
	bodySchemaRef := func(path string) string {
		t.Helper()
		item := doc.Paths.Find(path)
		if item == nil || item.Post == nil {
			t.Fatalf("no POST %s in the contract", path)
		}
		body := item.Post.RequestBody
		if body == nil || body.Value == nil {
			t.Fatalf("POST %s declares no request body", path)
		}
		media := body.Value.Content.Get("application/json")
		if media == nil || media.Schema == nil {
			t.Fatalf("POST %s declares no application/json schema", path)
		}
		return media.Schema.Ref
	}
	if ref := bodySchemaRef("/reservations"); ref != "#/components/schemas/ReservationCreate" {
		t.Fatalf("POST /reservations takes %q, want ReservationCreate -- the public route must "+
			"not be repointed at a schema that carries a channel (TKT-248/ADR-060)", ref)
	}
	if ref := bodySchemaRef("/partners/reservations"); ref != "#/components/schemas/PartnerReservationCreate" {
		t.Fatalf("POST /partners/reservations takes %q, want PartnerReservationCreate -- "+
			"repointing it at a schema with seat_identities would let a credentialled seated "+
			"sale carry the credential's channel into a claim path that ignores allocations "+
			"entirely (TKT-176)", ref)
	}

	public, ok := doc.Components.Schemas["ReservationCreate"]
	if !ok {
		t.Fatal("no ReservationCreate schema")
	}
	if _, present := public.Value.Properties["channel_code"]; present {
		t.Fatal("ReservationCreate declares channel_code again: a public seated request could " +
			"then name a channel, and inventory's seat path does not read one -- a forward that " +
			"consults no allocation and silently succeeds. TKT-248/ADR-060, TKT-176.")
	}

	partner, ok := doc.Components.Schemas["PartnerReservationCreate"]
	if !ok {
		t.Fatal("no PartnerReservationCreate schema")
	}
	if _, present := partner.Value.Properties["seat_identities"]; present {
		t.Fatal("PartnerReservationCreate declares seat_identities: a credentialled seated sale " +
			"would carry the credential's channel into a claim path that ignores allocations " +
			"entirely, which is the half-fix TKT-176 exists to prevent. If seated partner sales " +
			"are wanted, that is TKT-176's design, not a schema addition here.")
	}
	// And the partner body still cannot name one directly either.
	if _, present := partner.Value.Properties["channel_code"]; present {
		t.Fatal("PartnerReservationCreate declares channel_code: a partner's channel must come " +
			"from its credential and nothing else (ADR-056)")
	}
}
