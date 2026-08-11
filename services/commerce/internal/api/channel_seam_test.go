package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The commerce -> inventory channel seam, closed for GA pools (TKT-240).
//
// Before this ticket, `channel_code` reached catalog's fee resolution and stopped
// there: a reseller-channel sale took reseller fees while consuming PUBLIC
// inventory, because inventory's channel_code is what channel_allocations cap
// consumption against (ADR-024). Closing the seam is the ticket's intended
// breaking change — a channelled sale now consumes its own channel's allocation
// and starts returning 409 when that allocation is exhausted, on ticket types
// this ticket never touched.
//
// WHY THIS TEST ASSERTS THE OUTBOUND BODY AND NOT THE STATUS. The failure mode
// this closure invites is silent and green: inventory's hold takes a field named
// `channel` (HoldCreate in services/inventory/api/openapi.yaml), while commerce's
// request field is `channel_code`. Forwarding the wrong key makes inventory ignore
// it, consult no allocation, and succeed — the seam still open, every test still
// passing, the 409 never observed because it never fires. A status assertion
// against a stub cannot tell that apart from a correct forward. So the assertion
// is on the KEY commerce actually puts on the wire.
//
// The end-to-end proof that the forwarded channel really engages allocation
// capacity lives in the smoke suite against real inventory SQL; this test pins the
// wire contract that the smoke test would otherwise be the only thing protecting.

// capturedHold is the inventory hold body commerce sent.
type capturedHold struct {
	path string
	body map[string]any
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

// A channelled GA sale forwards its channel to inventory, under the key inventory
// actually reads.
//
// The `channel` spelling is asserted explicitly, and `channel_code` is asserted
// ABSENT, because sending the latter is the silent no-op described above: it looks
// identical in a diff and leaves the seam open.
func TestAChannelledGASaleForwardsItsChannelToInventory(t *testing.T) {
	reseller := "reseller-acme"
	got := reserveThroughCommerce(t, &reseller, `{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
		`","quantity":2,"channel_code":"reseller-acme"}`)

	if got.body == nil {
		t.Fatal("commerce never called inventory's hold endpoint")
	}
	if strings.Contains(got.path, "/holds/seats") {
		t.Fatalf("a quantity sale took the seated hold path (%s)", got.path)
	}
	v, ok := got.body["channel"]
	if !ok {
		t.Fatalf("the inventory hold body carries no `channel`: the seam is still open and a "+
			"reseller-channel sale is consuming public inventory. body=%v", got.body)
	}
	if v != "reseller-acme" {
		t.Fatalf("forwarded channel = %v, want %q", v, "reseller-acme")
	}
	if _, wrong := got.body["channel_code"]; wrong {
		t.Fatalf("the hold body uses `channel_code`; inventory reads `channel` and ignores this, "+
			"so no allocation is consulted and the sale succeeds against public stock. body=%v", got.body)
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
func TestTheSeatedHoldStillCarriesNoChannel(t *testing.T) {
	reseller := "reseller-acme"
	got := reserveThroughCommerce(t, &reseller, `{"organizer_id":"`+pricingOrg+`","ticket_type_id":"`+pricingTT+
		`","seat_identities":["A-1","A-2"],"channel_code":"reseller-acme"}`)

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
