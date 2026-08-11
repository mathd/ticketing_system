//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The staff/internal surface is deliberately off the gateway; the smoke stack exposes
// the services on localhost (compose.yaml ports) for box-office ops and this suite.
var (
	inventoryURL = env("SMOKE_INVENTORY_URL", "http://localhost:8091")
	commerceURL  = env("SMOKE_COMMERCE_URL", "http://localhost:8092")
	paymentsURL  = env("SMOKE_PAYMENTS_URL", "http://localhost:8093")
	// TKT-157: access gained an internal refund surface, so the suite needs to reach
	// it off the gateway like the other three.
	accessURL = env("SMOKE_ACCESS_URL", "http://localhost:8094")
	// No fallback: the harness (scripts/smoke.sh) generates and exports the
	// credential per invocation (TKT-83).
	internalToken = os.Getenv("SMOKE_INTERNAL_TOKEN")
	// Payments has its own credential (ai-review S8): the shared token no longer
	// opens charge, void or refund.
	paymentsInternalToken = os.Getenv("SMOKE_PAYMENTS_INTERNAL_TOKEN")
)

// internalJSON performs an internal (off-gateway) service request and contract-validates
// the response against that service's committed contract. It is the single internal-surface
// helper for the suite: TKT-95 folded group_reservations_test.go's identical
// internalValidated into it (that one took the service name explicitly; directService
// resolves it from the base URL, and it was only ever called with inventory and commerce).
// internalTokenFor picks the credential the destination accepts, from the URL —
// the same rule commerce applies in production (ai-review S8). Chosen here rather
// than at each call site for the same reason: the call site that forgets sends the
// shared token to the money surface and gets a 401 that reads as a broken stack.
func internalTokenFor(url string) string {
	if strings.HasPrefix(url, paymentsURL) {
		return paymentsInternalToken
	}
	return internalToken
}

func internalJSON(t *testing.T, method, url, key string, body any) (int, []byte) {
	t.Helper()
	return internalRequest(t, t.Fatalf, method, url, key, body)
}

// internalJSONAsync is internalJSON for callers inside a `go func`: it reports with
// t.Error, since T.FailNow may only be called on the test goroutine (TKT-95). Before
// TKT-95 the two goroutine call sites used internalJSON and so carried a latent illegal
// t.Fatal — never triggered, because it only fires on a contract violation.
func internalJSONAsync(t *testing.T, method, url, key string, body any) (int, []byte) {
	return internalRequest(t, t.Errorf, method, url, key, body)
}

func internalRequest(t *testing.T, fail func(string, ...any), method, url, key string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		fail("bad request: %v", err)
		return 0, []byte(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", internalTokenFor(url))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fail("%s %s: %v", method, url, err)
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if service := directService(url); service != "" {
		if err := checkDirectServiceResponse(service, resp.Request, resp.StatusCode, resp.Header, out); err != nil {
			fail("%v", err)
		}
	}
	return resp.StatusCode, out
}

// directService maps a direct (non-gateway) base URL to its contract; the
// internal surface is off the gateway, so gateway-path parsing never sees it.
func directService(url string) string {
	switch {
	case strings.HasPrefix(url, inventoryURL):
		return "inventory"
	case strings.HasPrefix(url, commerceURL):
		return "commerce"
	case strings.HasPrefix(url, paymentsURL):
		return "payments"
	case strings.HasPrefix(url, accessURL):
		return "access"
	}
	return ""
}

// publishedSlot creates and publishes a GA performance of the given capacity and waits
// for inventory to provision its pool. Returns (slotID, ticketTypeID).
func publishedSlot(t *testing.T, name string, capacity int) (string, string) {
	t.Helper()
	catalog := gatewayURL + "/api/catalog"
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": name, "ga_capacity": capacity})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": name, "en": name}})
	perf := created(t, catalog+"/performances", map[string]any{"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"], "starts_at": "2026-11-01T20:00:00Z", "timezone": "UTC"})
	tt := created(t, catalog+"/ticket-types", map[string]any{"organizer_id": organizerID, "performance_id": perf["id"], "name": map[string]string{"fr": "GA", "en": "GA"}, "price": map[string]any{"amount": 2500, "currency": "EUR"}})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != 200 {
		t.Fatalf("publish %d %s", code, body)
	}
	slot := fmt.Sprint(perf["id"])
	retry(t, 20*time.Second, func() error {
		code, body := get(t, fmt.Sprintf("%s/api/inventory/slots/%s/availability?organizer_id=%s", gatewayURL, slot, organizerID), nil)
		if code != 200 {
			return fmt.Errorf("not provisioned: %d %s", code, body)
		}
		return nil
	})
	return slot, fmt.Sprint(tt["id"])
}

func staffAvailability(t *testing.T, slot string) (buyerHeld, operationalHeld, confirmed, available int32) {
	t.Helper()
	code, body := internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/slots/%s/availability?organizer_id=%s", inventoryURL, slot, organizerID), "", nil)
	if code != 200 {
		t.Fatalf("staff availability %d %s", code, body)
	}
	var a struct {
		BuyerHeld       int32 `json:"buyer_held"`
		OperationalHeld int32 `json:"operational_held"`
		Confirmed       int32 `json:"confirmed"`
		Available       int32 `json:"available"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	return a.BuyerHeld, a.OperationalHeld, a.Confirmed, a.Available
}

// TestOperationalConvertNeverLeaksCapacityToPublicHolds fills a pool with an operational
// hold, then races a partial staff conversion against a burst of public holds. The
// conversion swaps 3 operational for 3 buyer units under one pool lock, so at no point —
// before, during, after — is any quantity publicly claimable (AC2). An implementation
// that releases before re-claiming fails this: a public hold would land in the gap.
func TestOperationalConvertNeverLeaksCapacityToPublicHolds(t *testing.T) {
	slot, tt := publishedSlot(t, "Operational Race Hall", 10)

	code, body := internalJSON(t, http.MethodPost, inventoryURL+"/internal/operational-holds", "op-place-"+slot,
		map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 10, "purpose": "house", "label": "full house hold", "actor": "staff:smoke", "reason": "race test"})
	if code != 201 {
		t.Fatalf("place operational hold: %d %s", code, body)
	}
	var op struct {
		ID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}

	var publicGrants atomic.Int32
	var wg sync.WaitGroup
	// The convert call below is a t.Fatal path (transport error or contract violation) and
	// it sits between spawning these workers and joining them. The workers report on t, and
	// t.Error after the test completed panics — so join them in a defer too. The explicit
	// wg.Wait() after convert stays; Wait is idempotent.
	defer wg.Wait()
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postWithKeyAsync(t, gatewayURL+"/api/inventory/holds", fmt.Sprintf("race-%s-%d", slot, i),
				map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
			if code == 201 {
				publicGrants.Add(1)
			} else if code != 409 {
				t.Errorf("public hold %d unexpected status %d", i, code)
			}
		}(i)
	}
	convertURL := fmt.Sprintf("%s/internal/operational-holds/%s/convert", commerceURL, op.ID)
	convCode, convBody := internalJSON(t, http.MethodPost, convertURL, "op-convert-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 3, "actor": "staff:smoke", "reason": "walk-up sale"})
	wg.Wait()

	if convCode != 201 {
		t.Fatalf("convert: %d %s", convCode, convBody)
	}
	if got := publicGrants.Load(); got != 0 {
		t.Fatalf("public holds granted during conversion: %d, want 0", got)
	}
	buyerHeld, opHeld, confirmed, available := staffAvailability(t, slot)
	if opHeld != 7 || buyerHeld != 3 || confirmed != 0 || available != 0 {
		t.Fatalf("accounting after convert: buyer=%d operational=%d confirmed=%d available=%d, want 3/7/0/0", buyerHeld, opHeld, confirmed, available)
	}

	// Replaying the same conversion returns the original outcome, not a second carve.
	if code, body := internalJSON(t, http.MethodPost, convertURL, "op-convert-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 3, "actor": "staff:smoke", "reason": "walk-up sale"}); code != 200 {
		t.Fatalf("convert replay: %d %s", code, body)
	}

	// The staff surface must not exist at the public edge.
	for _, url := range []string{
		fmt.Sprintf("%s/api/inventory/internal/slots/%s/availability?organizer_id=%s", gatewayURL, slot, organizerID),
		fmt.Sprintf("%s/api/inventory/internal/operational-holds/%s/history?organizer_id=%s", gatewayURL, op.ID, organizerID),
	} {
		if code, _ := get(t, url, map[string]string{"X-Internal-Token": internalToken}); code != 404 {
			t.Fatalf("gateway must 404 %s, got %d", url, code)
		}
	}
	if code, _ := internalJSON(t, http.MethodPost, fmt.Sprintf("%s/api/commerce/internal/operational-holds/%s/convert", gatewayURL, op.ID), "k",
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 1, "actor": "x", "reason": "y"}); code != 404 {
		t.Fatalf("gateway must 404 the commerce conversion route, got %d", code)
	}
}

// TestConvertedHoldCompletesPublicCheckout proves the converted claim is a normal buyer
// reservation: the existing checkout finalizes, charges and confirms it (AC2
// "buyer-purchasable"), and the audit history records the full journey.
func TestConvertedHoldCompletesPublicCheckout(t *testing.T) {
	slot, tt := publishedSlot(t, "Operational Checkout Hall", 5)

	code, body := internalJSON(t, http.MethodPost, inventoryURL+"/internal/operational-holds", "op-place-co-"+slot,
		map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 5, "purpose": "artist", "label": "band allotment", "actor": "staff:smoke", "reason": "contract"})
	if code != 201 {
		t.Fatalf("place operational hold: %d %s", code, body)
	}
	var op struct {
		ID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/operational-holds/%s/convert", commerceURL, op.ID), "op-convert-co-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 2, "actor": "staff:smoke", "reason": "artist guest purchase"})
	if code != 201 {
		t.Fatalf("convert: %d %s", code, body)
	}
	var conv struct {
		ReservationID   string `json:"reservation_id"`
		Amount          int64  `json:"amount"`
		SourceRemaining int32  `json:"source_remaining"`
	}
	if err := json.Unmarshal(body, &conv); err != nil {
		t.Fatal(err)
	}
	if conv.Amount != 5000 || conv.SourceRemaining != 3 {
		t.Fatalf("conversion result %+v, want amount 5000 (2 x 2500) remaining 3", conv)
	}

	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "op-order-"+slot,
		map[string]any{"reservation_id": conv.ReservationID, "name": "Guest Of Band", "email": "guest@example.test", "payment_token": "fake-ok"})
	if code != 200 || !bytes.Contains(body, []byte(`"completed"`)) {
		t.Fatalf("checkout of converted reservation: %d %s", code, body)
	}
	buyerHeld, opHeld, confirmed, available := staffAvailability(t, slot)
	if confirmed != 2 || opHeld != 3 || buyerHeld != 0 || available != 0 {
		t.Fatalf("accounting after checkout: buyer=%d operational=%d confirmed=%d available=%d, want 0/3/2/0", buyerHeld, opHeld, confirmed, available)
	}

	// Replaying the conversion after checkout must succeed: the child is confirmed, and
	// a guard that rejects replays by anything but lifecycle status would 409 here and
	// instruct a second carve of already-sold seats (second-review-pass regression).
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/operational-holds/%s/convert", commerceURL, op.ID), "op-convert-co-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 2, "actor": "staff:smoke", "reason": "artist guest purchase"}); code != 200 {
		t.Fatalf("convert replay after checkout: %d %s", code, body)
	}

	code, body = internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/operational-holds/%s/history?organizer_id=%s", inventoryURL, op.ID, organizerID), "", nil)
	if code != 200 {
		t.Fatalf("history: %d %s", code, body)
	}
	var entries []struct {
		Action string `json:"action"`
		Actor  string `json:"actor"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != "place" || entries[1].Action != "convert" || entries[0].Actor != "staff:smoke" {
		t.Fatalf("history = %+v, want place then convert by staff:smoke", entries)
	}
}
