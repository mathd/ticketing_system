//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func postWithKey(t *testing.T, url, key string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func setupCheckoutOffer(t *testing.T, suffix string) (string, string) {
	t.Helper()
	catalog := gatewayURL + "/api/catalog"
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": "Checkout " + suffix, "ga_capacity": 5})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": "Paiement " + suffix, "en": "Checkout " + suffix}})
	perf := created(t, catalog+"/performances", map[string]any{"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"], "starts_at": "2026-11-01T20:00:00Z", "timezone": "UTC"})
	tt := created(t, catalog+"/ticket-types", map[string]any{"organizer_id": organizerID, "performance_id": perf["id"], "name": map[string]string{"fr": "GA", "en": "GA"}, "price": map[string]any{"amount": 1250, "currency": "EUR"}})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != 200 {
		t.Fatalf("publish %d %s", code, body)
	}
	retry(t, 20*time.Second, func() error {
		code, body, _ := getWithHeaders(t, fmt.Sprintf("%s/api/inventory/slots/%v/availability?organizer_id=%s", gatewayURL, perf["id"], organizerID))
		if code != 200 {
			return fmt.Errorf("inventory %d %s", code, body)
		}
		return nil
	})
	return fmt.Sprint(perf["id"]), fmt.Sprint(tt["id"])
}

func reserveCheckout(t *testing.T, ticketType, key string) map[string]any {
	t.Helper()
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", key, map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != 201 {
		t.Fatalf("reserve %d %s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["amount"] != float64(2500) || out["currency"] != "EUR" {
		t.Fatalf("authoritative total: %v", out)
	}
	return out
}

func expireInventoryHold(t *testing.T, holdID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://inventory:inventory@%s/inventory", pgHostPort))
	if err != nil {
		t.Fatalf("connect inventory db: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	tag, err := db.Exec(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1`, holdID)
	if err != nil {
		t.Fatalf("expire hold: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expired claims = %d, want 1", tag.RowsAffected())
	}
}

func TestUS004CheckoutSuccessAndDecline(t *testing.T) {
	_, ticketType := setupCheckoutOffer(t, "flow")
	if code, _, _ := getWithHeaders(t, gatewayURL+"/api/catalog/internal/ticket-types/"+ticketType); code != http.StatusNotFound {
		t.Fatalf("public internal catalog route status = %d, want 404", code)
	}
	reservation := reserveCheckout(t, ticketType, "reserve-success")
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-success", map[string]any{"reservation_id": reservation["reservation_id"], "name": "Buyer One", "email": "buyer1@example.test", "payment_token": "fake-ok"})
	if code != 200 {
		t.Fatalf("checkout success %d %s", code, body)
	}
	var success map[string]any
	_ = json.Unmarshal(body, &success)
	if success["status"] != "completed" {
		t.Fatalf("success: %v", success)
	}
	if replayCode, replayBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-success", map[string]any{"reservation_id": reservation["reservation_id"], "name": "Buyer One", "email": "buyer1@example.test", "payment_token": "fake-ok"}); replayCode != 200 {
		t.Fatalf("completed replay %d %s", replayCode, replayBody)
	}

	_, invalidTokenTicketType := setupCheckoutOffer(t, "invalid-token")
	invalidTokenReservation := reserveCheckout(t, invalidTokenTicketType, "reserve-invalid-token")
	invalidTokenRequest := map[string]any{"reservation_id": invalidTokenReservation["reservation_id"], "name": "Buyer Invalid", "email": "invalid@example.test", "payment_token": "not-a-token"}
	if invalidCode, invalidBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-invalid-token", invalidTokenRequest); invalidCode != http.StatusBadRequest {
		t.Fatalf("invalid payment token %d %s", invalidCode, invalidBody)
	}
	// Validation happens before finalization, so the original live hold can
	// still be completed with a valid token.
	if validCode, validBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-invalid-token", map[string]any{"reservation_id": invalidTokenReservation["reservation_id"], "name": "Buyer Invalid", "email": "invalid@example.test", "payment_token": "fake-ok"}); validCode != http.StatusOK {
		t.Fatalf("valid retry after invalid token %d %s", validCode, validBody)
	}

	_, concurrentTicketType := setupCheckoutOffer(t, "same-key-race")
	concurrentReservation := reserveCheckout(t, concurrentTicketType, "reserve-same-key-race")
	concurrentBody := map[string]any{"reservation_id": concurrentReservation["reservation_id"], "name": "Buyer Concurrent", "email": "concurrent@example.test", "payment_token": "fake-ok"}
	type checkoutResult struct {
		code int
		body []byte
	}
	results := make(chan checkoutResult, 2)
	for range 2 {
		go func() {
			code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-same-key-race", concurrentBody)
			results <- checkoutResult{code, body}
		}()
	}
	var concurrentOrder map[string]any
	for range 2 {
		result := <-results
		if result.code != http.StatusOK && result.code != http.StatusConflict {
			t.Fatalf("concurrent checkout status = %d %s", result.code, result.body)
		}
		if result.code == http.StatusOK {
			_ = json.Unmarshal(result.body, &concurrentOrder)
		}
	}
	orderID := fmt.Sprint(concurrentOrder["order_id"])
	if orderCode, orderBody, _ := getWithHeaders(t, gatewayURL+"/api/commerce/orders/"+orderID); orderCode != http.StatusOK || !bytes.Contains(orderBody, []byte(`"completed"`)) {
		t.Fatalf("concurrent checkout order = %d %s", orderCode, orderBody)
	}

	expiredSlot, expiredTicketType := setupCheckoutOffer(t, "expired-finalize")
	expiredReservation := reserveCheckout(t, expiredTicketType, "reserve-expired-finalize")
	expireInventoryHold(t, fmt.Sprint(expiredReservation["hold_id"]))
	if expiredCode, expiredBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-expired-finalize", map[string]any{"reservation_id": expiredReservation["reservation_id"], "name": "Buyer Expired", "email": "expired@example.test", "payment_token": "fake-ok"}); expiredCode != http.StatusConflict {
		t.Fatalf("expired checkout %d %s", expiredCode, expiredBody)
	}
	availabilityCode, availabilityBody, _ := getWithHeaders(t, fmt.Sprintf("%s/api/inventory/slots/%s/availability?organizer_id=%s", gatewayURL, expiredSlot, organizerID))
	if availabilityCode != http.StatusOK {
		t.Fatalf("expired availability %d %s", availabilityCode, availabilityBody)
	}
	var availability struct{ Available, Held, Confirmed int32 }
	if err := json.Unmarshal(availabilityBody, &availability); err != nil {
		t.Fatal(err)
	}
	if availability.Available != 5 || availability.Held != 0 || availability.Confirmed != 0 {
		t.Fatalf("expired hold accounting %+v", availability)
	}

	declined := reserveCheckout(t, ticketType, "reserve-decline")
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "order-decline", map[string]any{"reservation_id": declined["reservation_id"], "name": "Buyer Two", "email": "buyer2@example.test", "payment_token": "fake-decline"})
	if code != 402 {
		t.Fatalf("checkout decline %d %s", code, body)
	}
	if replayCode, replayBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-decline", map[string]any{"reservation_id": declined["reservation_id"], "name": "Buyer Two", "email": "buyer2@example.test", "payment_token": "fake-decline"}); replayCode != 402 {
		t.Fatalf("declined replay %d %s", replayCode, replayBody)
	}
	if conflictCode, conflictBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-decline", map[string]any{"reservation_id": declined["reservation_id"], "name": "Buyer Two", "email": "different@example.test", "payment_token": "fake-decline"}); conflictCode != http.StatusConflict {
		t.Fatalf("checkout fingerprint conflict %d %s", conflictCode, conflictBody)
	}

	_, timeoutTicketType := setupCheckoutOffer(t, "timeout")
	timedOut := reserveCheckout(t, timeoutTicketType, "reserve-timeout")
	if timeoutCode, timeoutBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-timeout", map[string]any{"reservation_id": timedOut["reservation_id"], "name": "Buyer Timeout", "email": "timeout@example.test", "payment_token": "fake-timeout"}); timeoutCode != http.StatusRequestTimeout {
		t.Fatalf("checkout timeout %d %s", timeoutCode, timeoutBody)
	}
	// The deterministic fake timeout proves no PSP side effect, so its hold is
	// released and the full capacity can be reacquired.
	_ = reserveCheckout(t, timeoutTicketType, "reserve-after-timeout")

	if finalizeCode, _ := postWithKey(t, gatewayURL+"/api/inventory/holds/"+fmt.Sprint(reservation["hold_id"])+"/finalize?organizer_id="+organizerID, "", nil); finalizeCode != http.StatusNotFound {
		t.Fatalf("public inventory finalize status = %d, want 404", finalizeCode)
	}
	// Released claims are terminal, so retry means reacquiring a fresh hold.
	_ = reserveCheckout(t, ticketType, "reserve-retry")
}
