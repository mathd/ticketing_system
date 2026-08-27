//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestServiceContractsAreServedByteIdentically(t *testing.T) {
	for _, service := range []string{"inventory", "commerce", "payments", "access"} {
		t.Run(service, func(t *testing.T) {
			want, err := os.ReadFile("../services/" + service + "/api/openapi.yaml")
			if err != nil {
				t.Fatal(err)
			}
			code, got, _ := getWithHeaders(t, gatewayURL+"/api/"+service+"/openapi.yaml")
			if code != http.StatusOK || !bytes.Equal(got, want) {
				t.Fatalf("served %s contract differs (status %d, %d vs %d bytes)", service, code, len(got), len(want))
			}
		})
	}
}

func postWithKey(t *testing.T, url, key string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	// TKT-191: catalog writes need the staff-write credential; other services
	// must never see it.
	//
	// TKT-251: and since the path-id transitions take the organizer from the
	// verified assertion, an unsafe catalog write needs BOTH headers.
	if isCatalogURL(url) {
		req.Header.Set(staffWriteHeader, staffWriteToken())
		req.Header.Set(organizerAssertionHeader, organizerAssertion(t))
	}
	// ai-review S1: the scan routes admit only enrolled devices.
	if isScanURL(url) {
		req.Header.Set("X-Scanner-Token", scannerToken())
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, out)
	return resp.StatusCode, out
}

// postWithKeyAsync is postWithKey for callers inside a `go func` (TKT-95, was
// holdRequest): same transport, same timing, but it reports a transport failure or a
// contract violation with t.Error instead of t.Fatal, because T.FailNow may only be
// called on the test goroutine. Before TKT-95 the concurrent hold path stayed legal by
// skipping validation altogether, which also hid it from the coverage gate.
//
// A transport error still returns (0, message) rather than aborting: the contention
// tests classify a zero status themselves, and the goroutine must reach its
// WaitGroup/channel so the test can join it.
func postWithKeyAsync(t *testing.T, url, key string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	// TKT-191: catalog writes need the staff-write credential; other services
	// must never see it.
	//
	// TKT-251: and since the path-id transitions take the organizer from the
	// verified assertion, an unsafe catalog write needs BOTH headers. The
	// credential says who is calling; the assertion says which organizer for.
	if isCatalogURL(url) {
		req.Header.Set(staffWriteHeader, staffWriteToken())
		req.Header.Set(organizerAssertionHeader, organizerAssertion(t))
	}
	// ai-review S1: the scan routes admit only enrolled devices.
	if isScanURL(url) {
		req.Header.Set("X-Scanner-Token", scannerToken())
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	validateServiceResponseAsync(t, resp.Request, resp.StatusCode, resp.Header, out)
	return resp.StatusCode, out
}

func setupCheckoutOffer(t *testing.T, suffix string) (string, string) {
	t.Helper()
	catalog := gatewayURL + "/api/catalog"
	venue := created(t, catalog+"/venues", map[string]any{"name": "Checkout " + suffix, "ga_capacity": 5})
	event := created(t, catalog+"/events", map[string]any{"name": map[string]string{"fr": "Paiement " + suffix, "en": "Checkout " + suffix}})
	// starts_at is relative to now, never a fixed date (TKT-93): a hardcoded
	// 2026 date silently ages into the past and would eventually publish a
	// performance that has already started.
	startsAt := time.Now().UTC().AddDate(0, 0, 90).Truncate(time.Second).Format(time.RFC3339)
	perf := created(t, catalog+"/performances", map[string]any{"event_id": event["id"], "venue_id": venue["id"], "starts_at": startsAt, "timezone": "UTC"})
	tt := created(t, catalog+"/ticket-types", map[string]any{"performance_id": perf["id"], "name": map[string]string{"fr": "GA", "en": "GA"}, "price": map[string]any{"amount": 1250, "currency": "EUR"}})
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
	db, err := pgx.Connect(ctx, dsn("inventory", "inventory"))
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

func TestCheckoutSuccessDeclineAndRecovery(t *testing.T) {
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
	guestRef := fmt.Sprint(success["guest_order_ref"])
	if guestRef == "" || guestRef == fmt.Sprint(success["order_id"]) {
		t.Fatalf("guest order reference must be independent from the order ID: %v", success)
	}
	var issuedBundle struct {
		Tickets []struct {
			QRPayload string `json:"qr_payload"`
			QRURL     string `json:"qr_url"`
			History   []struct {
				Type       string    `json:"type"`
				Sequence   *int64    `json:"sequence"`
				OccurredAt time.Time `json:"occurred_at"`
			} `json:"history"`
		} `json:"tickets"`
	}
	retry(t, 10*time.Second, func() error {
		bundleCode, bundleBody, bundleHeaders := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestRef+"/tickets")
		if bundleCode != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", bundleCode, bundleBody)
		}
		if bundleHeaders.Get("Cache-Control") != "no-store" {
			return fmt.Errorf("ticket bundle Cache-Control = %q", bundleHeaders.Get("Cache-Control"))
		}
		if err := json.Unmarshal(bundleBody, &issuedBundle); err != nil {
			return err
		}
		if len(issuedBundle.Tickets) != 2 {
			return fmt.Errorf("issued tickets = %d, want 2", len(issuedBundle.Tickets))
		}
		for _, ticket := range issuedBundle.Tickets {
			if len(ticket.History) != 2 || ticket.History[0].Type != "issued" || ticket.History[1].Type != "delivered" {
				return fmt.Errorf("ticket lifecycle = %#v", ticket.History)
			}
			// The integrity sequence is the API's authoritative order (ADR-025 §D5).
			for i, e := range ticket.History {
				if e.Sequence == nil || *e.Sequence != int64(i+1) {
					return fmt.Errorf("history[%d].sequence = %v, want %d", i, e.Sequence, i+1)
				}
			}
			qrCode, _, qrHeaders := getWithHeaders(t, gatewayURL+ticket.QRURL)
			if qrCode != http.StatusOK || qrHeaders.Get("Content-Type") != "image/png" {
				return fmt.Errorf("ticket QR = %d %q", qrCode, qrHeaders.Get("Content-Type"))
			}
		}
		return nil
	})
	first, second := issuedBundle.Tickets[0], issuedBundle.Tickets[1]
	acceptedCode, acceptedBody := postJSON(t, gatewayURL+"/api/access/scans", map[string]string{"qr_payload": first.QRPayload})
	if acceptedCode != http.StatusOK {
		t.Fatalf("first scan = %d %s", acceptedCode, acceptedBody)
	}
	var accepted struct {
		Decision  string    `json:"decision"`
		ScannedAt time.Time `json:"scanned_at"`
	}
	if err := json.Unmarshal(acceptedBody, &accepted); err != nil || accepted.Decision != "accepted" || accepted.ScannedAt.IsZero() {
		t.Fatalf("accepted scan = %s: %v", acceptedBody, err)
	}
	duplicateCode, duplicateBody := postJSON(t, gatewayURL+"/api/access/scans", map[string]string{"qr_payload": first.QRPayload})
	if duplicateCode != http.StatusConflict {
		t.Fatalf("duplicate scan = %d %s", duplicateCode, duplicateBody)
	}
	var duplicate struct {
		Decision       string    `json:"decision"`
		Reason         string    `json:"reason"`
		OriginalScanAt time.Time `json:"original_scan_at"`
	}
	if err := json.Unmarshal(duplicateBody, &duplicate); err != nil || duplicate.Decision != "rejected" || duplicate.Reason != "already_redeemed" || !duplicate.OriginalScanAt.Equal(accepted.ScannedAt) {
		t.Fatalf("duplicate result = %s: %v", duplicateBody, err)
	}
	forged := corruptSignature(t, first.QRPayload)
	if code, body := postJSON(t, gatewayURL+"/api/access/scans", map[string]string{"qr_payload": forged}); code != http.StatusUnprocessableEntity {
		t.Fatalf("forged scan = %d %s", code, body)
	}
	parts := strings.Split(second.QRPayload, ".")
	if len(parts) != 3 {
		t.Fatal("issued credential has invalid shape")
	}
	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err = json.Unmarshal(claimBytes, &claims); err != nil {
		t.Fatal(err)
	}
	claims["org"] = uuid.NewString()
	claimBytes, err = json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded := parts[0] + "." + base64.RawURLEncoding.EncodeToString(claimBytes)
	// The stack's own issuing seed, minted per run by scripts/stack-env.sh. It was
	// the all-zero literal until ai-review S5 removed the checked-in compose
	// defaults — and a hard-coded seed here would now sign under a key the gate
	// does not know, turning this claim-mismatch case into a signature-failure
	// case that passes for the wrong reason.
	seed, err := base64.RawStdEncoding.DecodeString(os.Getenv("SMOKE_ACCESS_QR_SEED"))
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("SMOKE_ACCESS_QR_SEED must be the stack's raw-base64 Ed25519 seed: %v", err)
	}
	mismatched := encoded + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), []byte(encoded)))
	if code, body := postJSON(t, gatewayURL+"/api/access/scans", map[string]string{"qr_payload": mismatched}); code != http.StatusUnprocessableEntity {
		t.Fatalf("claim-mismatch scan = %d %s", code, body)
	}

	// A real-Postgres race proves the lock -> trace read -> insert ordering:
	// one gate accepts and the other obtains that exact stored timestamp.
	// TKT-88: each gate carries a DISTINCT occurrence id (two physical gates
	// admitting the same single-entry ticket at once), so the race also proves
	// the occurrence-keyed redemption: exactly one occurrence becomes the
	// stored `redeemed`, and the loser sees its already-redeemed time.
	type scanResult struct {
		code       int
		body       []byte
		occurrence string
		err        error
	}
	firstOcc, secondOcc := uuid.NewString(), uuid.NewString()
	raceOccurrences := []string{firstOcc, secondOcc}
	raceClaimedAt := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Microsecond)
	start := make(chan struct{})
	scanResults := make(chan scanResult, 2)
	var scans sync.WaitGroup
	for i := range 2 {
		occurrence := raceOccurrences[i]
		scans.Add(1)
		go func() {
			defer scans.Done()
			<-start
			code, body := postWithKeyAsync(t, gatewayURL+"/api/access/scans", "concurrent-scan-"+occurrence,
				map[string]any{"qr_payload": second.QRPayload, "occurrence_id": occurrence, "occurred_at": raceClaimedAt.Format(time.RFC3339Nano)})
			scanResults <- scanResult{code: code, body: body, occurrence: occurrence}
		}()
	}
	close(start)
	scans.Wait()
	close(scanResults)
	var concurrentAccepted time.Time
	var concurrentDuplicate time.Time
	var winningOccurrence string
	acceptedCount, duplicateCount := 0, 0
	for result := range scanResults {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.code == http.StatusOK {
			var value struct {
				ScannedAt time.Time `json:"scanned_at"`
			}
			if err := json.Unmarshal(result.body, &value); err != nil || value.ScannedAt.IsZero() {
				t.Fatalf("concurrent accepted result = %s: %v", result.body, err)
			}
			concurrentAccepted = value.ScannedAt
			winningOccurrence = result.occurrence
			acceptedCount++
			continue
		}
		if result.code == http.StatusConflict {
			var value struct {
				OriginalScanAt time.Time `json:"original_scan_at"`
			}
			if err := json.Unmarshal(result.body, &value); err != nil || value.OriginalScanAt.IsZero() {
				t.Fatalf("concurrent duplicate result = %s: %v", result.body, err)
			}
			concurrentDuplicate = value.OriginalScanAt
			duplicateCount++
			continue
		}
		t.Fatalf("concurrent scan = %d %s", result.code, result.body)
	}
	if acceptedCount != 1 || duplicateCount != 1 {
		t.Fatalf("concurrent outcomes accepted=%d duplicate=%d, want exactly 1 each", acceptedCount, duplicateCount)
	}
	if concurrentAccepted.IsZero() || concurrentDuplicate.IsZero() || !concurrentAccepted.Equal(concurrentDuplicate) {
		t.Fatalf("concurrent outcomes have timestamps accepted=%s duplicate=%s", concurrentAccepted, concurrentDuplicate)
	}
	// The winning occurrence is the one and only stored redeemed event; the
	// loser appended nothing.
	assertRedeemedOccurrence(t, second.QRPayload, winningOccurrence, concurrentAccepted)
	retry(t, 10*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket history after scan = %d %s", code, body)
		}
		var bundle struct {
			Tickets []struct {
				History []struct {
					Type     string `json:"type"`
					Sequence *int64 `json:"sequence"`
				} `json:"history"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 2 || len(bundle.Tickets[0].History) != 3 || len(bundle.Tickets[1].History) != 3 {
			return fmt.Errorf("redemption lifecycle = %#v", bundle.Tickets)
		}
		for _, ticket := range bundle.Tickets {
			for i, e := range ticket.History {
				if e.Sequence == nil || *e.Sequence != int64(i+1) {
					return fmt.Errorf("post-redemption history[%d].sequence = %v, want %d", i, e.Sequence, i+1)
				}
			}
		}
		return nil
	})
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
			code, body := postWithKeyAsync(t, gatewayURL+"/api/commerce/orders", "order-same-key-race", concurrentBody)
			results <- checkoutResult{code, body}
		}()
	}
	concurrentOrders := make([]map[string]any, 0, 2)
	for range 2 {
		result := <-results
		if result.code != http.StatusOK && result.code != http.StatusConflict {
			t.Fatalf("concurrent checkout status = %d %s", result.code, result.body)
		}
		if result.code == http.StatusOK {
			var completed map[string]any
			if err := json.Unmarshal(result.body, &completed); err != nil {
				t.Fatal(err)
			}
			concurrentOrders = append(concurrentOrders, completed)
		}
	}
	// A request that observed an in-progress payment may return 409. Its
	// idempotent replay must converge on the same completed order/reference.
	for len(concurrentOrders) < 2 {
		code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-same-key-race", concurrentBody)
		if code != http.StatusOK {
			t.Fatalf("concurrent checkout replay = %d %s", code, body)
		}
		var completed map[string]any
		if err := json.Unmarshal(body, &completed); err != nil {
			t.Fatal(err)
		}
		concurrentOrders = append(concurrentOrders, completed)
	}
	orderID := fmt.Sprint(concurrentOrders[0]["order_id"])
	guestOrderRef := fmt.Sprint(concurrentOrders[0]["guest_order_ref"])
	if guestOrderRef == "" {
		t.Fatalf("concurrent checkout omitted guest_order_ref: %v", concurrentOrders[0])
	}
	for _, completed := range concurrentOrders[1:] {
		if fmt.Sprint(completed["order_id"]) != orderID || fmt.Sprint(completed["guest_order_ref"]) != guestOrderRef {
			t.Fatalf("concurrent checkout did not converge: %v", concurrentOrders)
		}
	}
	if orderCode, orderBody, _ := getWithHeaders(t, gatewayURL+"/api/commerce/orders/"+orderID); orderCode != http.StatusOK || !bytes.Contains(orderBody, []byte(`"completed"`)) {
		t.Fatalf("concurrent checkout order = %d %s", orderCode, orderBody)
	}
	retry(t, 10*time.Second, func() error {
		bundleCode, bundleBody, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestOrderRef+"/tickets")
		if bundleCode != http.StatusOK {
			return fmt.Errorf("concurrent ticket bundle %d %s", bundleCode, bundleBody)
		}
		var bundle struct {
			Tickets []json.RawMessage `json:"tickets"`
		}
		if err := json.Unmarshal(bundleBody, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 2 {
			return fmt.Errorf("concurrent issued tickets = %d, want 2", len(bundle.Tickets))
		}
		return nil
	})

	expiredSlot, expiredTicketType := setupCheckoutOffer(t, "expired-finalize")
	expiredReservation := reserveCheckout(t, expiredTicketType, "reserve-expired-finalize")
	expireInventoryHold(t, fmt.Sprint(expiredReservation["hold_id"]))
	if replayCode, replayBody := postWithKey(t, gatewayURL+"/api/commerce/reservations", "reserve-expired-finalize", map[string]any{"organizer_id": organizerID, "ticket_type_id": expiredTicketType, "quantity": 2}); replayCode != http.StatusConflict {
		t.Fatalf("expired reservation replay %d %s", replayCode, replayBody)
	}
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
	// A terminal replay re-delivers the idempotent order.failed journal fact
	// before returning the buyer-visible decline.
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

	// The public-denial assertion for the hold transitions lives in
	// TestGatewayDeniesGenericInternalRoutes (TKT-124): it needs raw http.Client,
	// because postWithKey contract-validates every /api/<svc>/ response and the
	// retired paths have no operation left in inventory's spec to validate against.
	// Released claims are terminal, so retry means reacquiring a fresh hold.
	_ = reserveCheckout(t, ticketType, "reserve-retry")
}

// assertRedeemedOccurrence asserts that the winning occurrence of a concurrent
// scan race is the single stored `redeemed` lifecycle event, with the timestamp
// the accepted response reported. The redemption event's id IS the occurrence
// id (ADR-025 §D3), so the occurrence uniquely identifies the stored row.
//
// Isolation on the shared smoke stack comes from the freshly-minted UUIDv4
// occurrence and ticket id unique to this run, NOT from any DB partitioning — a
// future refactor that reuses a fixture ticket would silently break the
// exactly-one-redeemed/zero-duplicate_admit counts below.
func assertRedeemedOccurrence(t *testing.T, qrPayload, winningOccurrence string, acceptedAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn("access", "access"))
	if err != nil {
		t.Fatalf("connect access db: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	var eventType string
	var occurredAt time.Time
	if err := db.QueryRow(ctx, `SELECT event_type, occurred_at FROM lifecycle_events WHERE id=$1`, winningOccurrence).Scan(&eventType, &occurredAt); err != nil {
		t.Fatalf("winning occurrence %s has no lifecycle event: %v", winningOccurrence, err)
	}
	if eventType != "redeemed" {
		t.Fatalf("winning occurrence event_type = %q, want redeemed", eventType)
	}
	if !occurredAt.UTC().Equal(acceptedAt.UTC()) {
		t.Fatalf("redeemed occurred_at = %s, want accepted scanned_at %s", occurredAt.UTC(), acceptedAt.UTC())
	}
	// The losing occurrence never became an event: exactly one redeemed for
	// this ticket, and no duplicate_admit from a live concurrent scan.
	var redeemed, dupes int
	if err := db.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE event_type='redeemed'),
			count(*) FILTER (WHERE event_type='duplicate_admit')
		FROM lifecycle_events
		WHERE ticket_id = (SELECT ticket_id FROM lifecycle_events WHERE id=$1)`, winningOccurrence).Scan(&redeemed, &dupes); err != nil {
		t.Fatalf("count redemptions: %v", err)
	}
	if redeemed != 1 || dupes != 0 {
		t.Fatalf("concurrent race left redeemed=%d duplicate_admit=%d, want 1 and 0", redeemed, dupes)
	}
}

func corruptSignature(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) == 0 {
		t.Fatalf("decode signature: %v", err)
	}
	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	return strings.Join(parts, ".")
}

// TKT-153: the sale is priced by catalog's rule hierarchy, and the quote a buyer
// was given is the quote they are charged — proven through the running stack,
// not at a fake seam.
//
// Rules are seeded directly into catalog's database because this epic ships no
// HTTP rule-authoring surface (a deliberate scoping decision, recorded on
// TKT-151). Everything AFTER the seed goes through the gateway.
func TestReserveUsesRuleResolvedPriceAndPinsTheQuote(t *testing.T) {
	_, ticketType := setupCheckoutOffer(t, "tkt153")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cat.Close(ctx) }()

	// The event this ticket type hangs off — an event-scoped rule beats nothing
	// else here, and proves resolution walked the hierarchy rather than reading
	// the ticket type's own column.
	var eventID string
	if err = cat.QueryRow(ctx, `SELECT p.event_id FROM ticket_types t
		JOIN performances p ON p.id = t.performance_id WHERE t.id = $1`, ticketType).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	seedRule := func(amount int64, priority int32) {
		t.Helper()
		if _, err := cat.Exec(ctx, `INSERT INTO price_rules
			(organizer_id, scope_level, scope_id, action_kind, amount, currency, priority)
			VALUES($1,'event',$2,'absolute',$3,'EUR',$4)`, organizerID, eventID, amount, priority); err != nil {
			t.Fatal(err)
		}
	}

	// Base price is 2500; the rule says 1800.
	seedRule(1800, 0)

	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt153-pin",
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != 201 {
		t.Fatalf("reserve %d %s", code, body)
	}
	var first map[string]any
	if err = json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if first["amount"] != float64(3600) {
		t.Fatalf("reserved total = %v, want 2 x the RULE price 1800 — not the base 2500", first["amount"])
	}

	// The reservation records WHY it was priced that way, as a snapshot rather
	// than a pointer: closing the rule below must not rewrite it.
	com, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = com.Close(ctx) }()
	var scopeLevel string
	var snapshotAmount int64
	if err = com.QueryRow(ctx, `SELECT price_rule_scope_level,
		(price_resolution_snapshot #>> '{resolved_price,amount}')::bigint
		FROM reservations WHERE id = $1`, first["reservation_id"]).Scan(&scopeLevel, &snapshotAmount); err != nil {
		t.Fatalf("the reservation must carry its price provenance: %v", err)
	}
	if scopeLevel != "event" || snapshotAmount != 1800 {
		t.Fatalf("provenance = %q/%d, want the event rule at 1800", scopeLevel, snapshotAmount)
	}

	// Now change the price under the buyer: a higher-priority rule wins from here on.
	seedRule(9900, 100)
	// TKT-155 moved price resolution onto catalog's /internal/ surface. The
	// RETIRED public path is asserted here, against THIS ticket type, and the
	// fixture is the whole point: `ticketType` is seeded, priced, and carries the
	// 9900 rule just written above, so a restored public route would answer 200
	// with a real resolution.
	//
	// Asserted here rather than in contract_validation_test.go's table for exactly
	// that reason. That table uses fixed all-zero UUIDs, which is right for its
	// job (proving WHICH layer refuses an internal path) and fatal for this one: a
	// restored route handed a ticket type that does not exist answers 404 from the
	// store, so every assertion would stay green while the exposure was back. The
	// test must be able to see the payload it forbids. (Found by the adversarial
	// review; the first version of this assertion had exactly that defect.)
	// A RAW client, not getWithHeaders: that helper validates the response against
	// the service contract, and a retired path has no operation to validate
	// against — it would fail the request before this assertion could make its
	// point, which is a fact about the helper rather than about the route.
	retired, err := (&http.Client{Timeout: 10 * time.Second}).Get(
		fmt.Sprintf("%s/api/catalog/ticket-types/%s/price-resolution", gatewayURL, ticketType))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(retired.Body)
	_ = retired.Body.Close()
	if retired.StatusCode == 200 {
		t.Fatalf("the retired public price-resolution path still answers: %s", body)
	}
	if strings.Contains(string(body), `"amount":9900`) || strings.Contains(string(body), "candidates") {
		t.Fatalf("the retired public path is still returning a price resolution: %s", body)
	}

	// COS-2: the ANSWER is unchanged for a credentialed caller. The ticket closes
	// an audience; it does not change what catalog computes, so this read has to
	// survive the move rather than be replaced by refusal assertions.
	code, body = internalJSON(t, "GET",
		fmt.Sprintf("%s/internal/ticket-types/%s/price-resolution", catalogURL, ticketType), "", nil)
	if code != 200 {
		t.Fatalf("resolution %d %s", code, body)
	}
	if !strings.Contains(string(body), `"amount":9900`) {
		t.Fatalf("catalog should now resolve 9900 for a NEW quote: %s", body)
	}

	// The replay must answer with the ORIGINAL quote. Re-resolving would also
	// hand inventory a different unit_amount, and inventory fingerprints its
	// idempotency on that amount — so this would 409 rather than merely
	// repricing.
	code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt153-pin",
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != 201 {
		t.Fatalf("replay %d %s — a replay answers like a first call", code, body)
	}
	var replay map[string]any
	if err = json.Unmarshal(body, &replay); err != nil {
		t.Fatal(err)
	}
	if replay["amount"] != float64(3600) {
		t.Fatalf("replayed total = %v, want the PINNED 3600 — a rule change must not move a quote already given",
			replay["amount"])
	}
}

// TestSeatedReservationAndCheckout is TKT-173 end to end through the gateway: a
// buyer names seats, pays for exactly the seats they got, and a competitor naming
// one of them is refused by name.
//
// It gets its own fixture and its own test rather than joining
// TestSeatedPublicationCoexistsWithGA, which already carries publication, the
// schema-4 fork, occupancy (TKT-172), direct seat holding and pinning. The buyer
// WRITE path deserves its own failure boundary — when this breaks, the message
// should say "seated checkout", not "seated publication".
func TestSeatedReservationAndCheckout(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	suffix := uuid.NewString()[:8]

	venue := created(t, catalog+"/venues", map[string]any{
		"name": "Seated Checkout " + suffix, "ga_capacity": 50})
	event := created(t, catalog+"/events", map[string]any{
		"name": map[string]string{"fr": "Concert " + suffix, "en": "Concert " + suffix}})

	// Three seats in one row, so a partial collision is expressible.
	seatMap := created(t, catalog+"/venues/"+fmt.Sprint(venue["id"])+"/seat-maps", map[string]any{
		"name": "Stalls " + suffix})
	section := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/sections", map[string]any{
		"name": "Stalls", "position": 1})
	row := created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/rows", map[string]any{
		"section_id": section["id"], "label": "A", "position": 1})
	for i := 1; i <= 3; i++ {
		created(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/seats", map[string]any{
			"row_id": row["id"], "label": fmt.Sprint(i), "position": i})
	}
	if code, body := postJSON(t, catalog+"/seat-maps/"+fmt.Sprint(seatMap["id"])+"/publish", nil); code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", code, body)
	}

	perf := created(t, catalog+"/performances", map[string]any{
		"event_id": event["id"], "venue_id": venue["id"],
		"starts_at": "2026-11-05T20:00:00Z", "timezone": "UTC", "seat_map_id": seatMap["id"]})
	tt := created(t, catalog+"/ticket-types", map[string]any{
		"performance_id": perf["id"],
		"name":           map[string]string{"fr": "Place", "en": "Seat"},
		"price":          map[string]any{"amount": 3000, "currency": "EUR"}})
	if code, body := postJSON(t, fmt.Sprintf("%s/performances/%v/publish", catalog, perf["id"]), nil); code != http.StatusOK {
		t.Fatalf("publish performance: %d %s", code, body)
	}

	// Inventory provisions the seated pool asynchronously off the publication event.
	reservations := gatewayURL + "/api/commerce/reservations"
	seatOf := func(n int) string { return "Stalls/A/" + fmt.Sprint(n) }
	var code int
	var body []byte
	for i := 0; i < 40; i++ {
		code, body = postWithKey(t, reservations, "seated-probe-"+suffix, map[string]any{
			"organizer_id": organizerID, "ticket_type_id": tt["id"],
			"seat_identities": []string{seatOf(1)}})
		if code == http.StatusCreated {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if code != http.StatusCreated {
		t.Fatalf("seated reservation never became possible: %d %s", code, body)
	}

	// -- the money follows the CLAIM, not the request --
	// [2,3,3] is three identities and two seats. Charging for three would be charging
	// for a seat nobody holds; inventory canonicalises and commerce must follow it.
	code, body = postWithKey(t, reservations, "seated-buy-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": tt["id"],
		"seat_identities": []string{seatOf(3), seatOf(2), seatOf(3)}})
	if code != http.StatusCreated {
		t.Fatalf("seated reservation = %d %s", code, body)
	}
	var res struct {
		ReservationID string   `json:"reservation_id"`
		HoldID        string   `json:"hold_id"`
		Amount        int64    `json:"amount"`
		Currency      string   `json:"currency"`
		Seats         []string `json:"seats"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("reservation decode: %v (%s)", err, body)
	}
	if len(res.Seats) != 2 || res.Seats[0] != seatOf(2) || res.Seats[1] != seatOf(3) {
		t.Fatalf("seats = %v want [%s %s] — sorted and de-duplicated by inventory",
			res.Seats, seatOf(2), seatOf(3))
	}
	if res.Amount != 6000 || res.Currency != "EUR" {
		t.Fatalf("amount = %d %s want 6000 EUR (2 claimed seats x 3000), not 9000 for three identities",
			res.Amount, res.Currency)
	}

	// -- replay: same key, reordered set, one reservation --
	replayCode, replayBody := postWithKey(t, reservations, "seated-buy-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": tt["id"],
		"seat_identities": []string{seatOf(2), seatOf(3)}})
	if replayCode != http.StatusCreated {
		t.Fatalf("replay = %d %s", replayCode, replayBody)
	}
	var replay struct {
		ReservationID string   `json:"reservation_id"`
		HoldID        string   `json:"hold_id"`
		Amount        int64    `json:"amount"`
		Seats         []string `json:"seats"`
	}
	if err := json.Unmarshal(replayBody, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ReservationID != res.ReservationID || replay.HoldID != res.HoldID || replay.Amount != res.Amount {
		t.Fatalf("replay produced a different reservation: %+v vs %+v — the persisted seat set is "+
			"what makes a retry land on the original claim", replay, res)
	}
	if len(replay.Seats) != 2 {
		t.Fatalf("replay seats = %v want the persisted pair", replay.Seats)
	}

	// -- same key, a genuinely different set: refused, not a second claim --
	if c, b := postWithKey(t, reservations, "seated-buy-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": tt["id"],
		"seat_identities": []string{seatOf(1), seatOf(2)}}); c != http.StatusConflict {
		t.Fatalf("key reuse with different terms = %d %s want 409", c, b)
	}

	// -- a competitor is refused BY NAME, and only for the seat actually contended --
	c, b := postWithKey(t, reservations, "seated-rival-"+suffix, map[string]any{
		"organizer_id": organizerID, "ticket_type_id": tt["id"],
		"seat_identities": []string{seatOf(2)}})
	if c != http.StatusConflict {
		t.Fatalf("contended reservation = %d %s want 409", c, b)
	}
	var conflict struct {
		Code  string   `json:"code"`
		Seats []string `json:"seat_identities"`
	}
	if err := json.Unmarshal(b, &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Code != "seat_taken" || len(conflict.Seats) != 1 || conflict.Seats[0] != seatOf(2) {
		t.Fatalf("conflict = %s — a picker re-renders from seat_identities, so it must name "+
			"exactly the seats lost", b)
	}

	// -- the occupancy read (TKT-172) agrees with what commerce just claimed --
	occCode, occBody, _ := getWithHeaders(t, fmt.Sprintf(
		"%s/api/inventory/slots/%v/seat-occupancy?organizer_id=%s", gatewayURL, perf["id"], organizerID))
	if occCode != http.StatusOK {
		t.Fatalf("occupancy = %d %s", occCode, occBody)
	}
	var occ struct {
		Unavailable []string `json:"unavailable_seat_identities"`
	}
	if err := json.Unmarshal(occBody, &occ); err != nil {
		t.Fatal(err)
	}
	if len(occ.Unavailable) != 3 {
		t.Fatalf("occupancy = %v want all three seats held (1 by the probe, 2 and 3 by the buyer) — "+
			"the read and the claim path must agree", occ.Unavailable)
	}

	// -- checkout completes and issues one ticket per claimed seat --
	orderCode, orderBody := postWithKey(t, gatewayURL+"/api/commerce/orders", "seated-order-"+suffix, map[string]any{
		"reservation_id": res.ReservationID, "name": "Seated Buyer",
		"email": "seated-" + suffix + "@example.test", "payment_token": "fake-ok"})
	if orderCode != http.StatusOK {
		t.Fatalf("seated checkout = %d %s", orderCode, orderBody)
	}
	var order struct {
		Status        string `json:"status"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(orderBody, &order); err != nil {
		t.Fatal(err)
	}
	if order.Status != "completed" || order.GuestOrderRef == "" {
		t.Fatalf("seated order = %s", orderBody)
	}
}

// TKT-215 AC4 + AC8, end to end through the real stack: two reservations that
// differ ONLY by sales channel are charged different totals while recording the
// same fee amount, and checkout captures the stored fee-inclusive total.
//
// This is the assertion the whole ticket exists for, and it is the one that
// cannot be made at unit level: it needs catalog resolving real rules, commerce
// composing them, and payments capturing the result.
//
// FIXTURE NOTE: the two rules carry the SAME amount and OPPOSITE incidence. A
// fixture where the two arms also differed in amount could not tell "the
// incidence changed what the buyer pays" from "the fee itself was different" —
// which is precisely the confusion a build that treats incidence as a display
// flag would produce.
//
// TKT-248 changed WHICH TWO ARMS, and nothing else. It used to be two channels,
// both named by a public request body. Public reservations may no longer name a
// channel (ADR-060), and the smoke stack enrols exactly one partner credential —
// so two channelled sales are no longer constructible here. The arms are now the
// PARTNER's channel (absorbed) and the PUBLIC context (passed_on), which differ
// in exactly the one dimension the test is about while still being two real
// end-to-end sales through the whole stack. The claim, the amounts and every
// assertion below are untouched.
func TestFeeIncidenceChangesTheChargedTotalButNotTheFee(t *testing.T) {
	slot, ticketType := setupCheckoutOffer(t, "tkt215")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cat.Close(ctx) }()

	var eventID string
	if err = cat.QueryRow(ctx, `SELECT p.event_id FROM ticket_types t
		JOIN performances p ON p.id = t.performance_id WHERE t.id = $1`, ticketType).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	// Same code, same amount, same scope — only the channel and the incidence
	// differ, so nothing else can explain a difference in the charged total.
	//
	// One rule is channel-AGNOSTIC (channel_code NULL), which is what a public sale
	// resolves; the other is scoped to the partner's channel. Omitting a channel is
	// the default/public context and never matches a channel-specific rule
	// (ADR-046 §4), so the two arms cannot cross-contaminate.
	partnerCh := partnerChannel()
	if partnerCh == "" {
		t.Fatal("SMOKE_PARTNER_CHANNEL is not set: a channelled sale needs the credential's " +
			"channel, and after ADR-060 there is no other way to make one")
	}
	for _, r := range []struct {
		channel   *string
		incidence string
	}{
		{nil, "passed_on"},       // public: the buyer pays the fee
		{&partnerCh, "absorbed"}, // partner channel: the organizer bears it
	} {
		if _, err = cat.Exec(ctx, `INSERT INTO fee_rules
			(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
			VALUES($1,'event',$2,'service','per_ticket_fixed',300,'EUR',$3,$4)`,
			organizerID, eventID, r.incidence, r.channel); err != nil {
			t.Fatal(err)
		}
	}

	// Both channels need an INVENTORY ALLOCATION, not only a fee rule (TKT-240).
	// Before the commerce->inventory seam was closed, a channelled sale never told
	// inventory which channel it was for, so a channel that existed purely for fee
	// rules sold out of public stock. It no longer does: CreateHold refuses a
	// channel with no active allocation outright (`!haveAllocation` ->
	// ErrUnavailable), which is the intended breaking change and a WIDER rule than
	// "an exhausted allocation is refused".
	//
	// So this fixture now has to describe a channel that is genuinely set up to
	// sell. That is not a workaround for the test — it is the new requirement, and
	// the same one every fee-only channel in a real deployment must satisfy before
	// this ships.
	//
	// TKT-246 added the second half: the allocation must also say WHO may consume
	// it (`sold_by`), because the partner arm below sells with a credential and
	// inventory judges seller before capacity, under the pool row lock.
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			// cap EXACTLY the partner arm's 2. The slot's GA capacity is 5
			// (setupCheckoutOffer), an allocation reserves its cap away from the
			// public pool, and the public arm below also buys 2 -- so a larger cap
			// starves the public arm and it fails with "inventory unavailable"
			// while looking like a pricing defect.
			{"channel": partnerCh, "cap": 2, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate the partner channel: %d %s", code, body)
	}

	// `partner` false is the PUBLIC arm: no channel is named, which is the only
	// shape a public request has after ADR-060 and which resolves the
	// channel-agnostic rule. `partner` true goes through the credentialled route,
	// whose channel comes from the credential and never from a body.
	reserveIn := func(partner bool, key string) map[string]any {
		t.Helper()
		var code int
		var body []byte
		req := map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2}
		if partner {
			code, body = partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations", key, req)
		} else {
			code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", key, req)
		}
		if code != 201 {
			t.Fatalf("reserve (partner=%v): %d %s", partner, code, body)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The fee is 300 per ticket at quantity 2 → 600. Everything below is asserted
	// RELATIVE to the observed face value rather than against a hard-coded price:
	// the claim under test is "incidence changes the charged total by exactly the
	// fee", and pinning the fixture's unit price would test the seed data instead
	// — which is how the first version of this test failed for the wrong reason.
	passed := reserveIn(false, "tkt215-passed")
	absorbed := reserveIn(true, "tkt215-absorbed")

	const feeTotal = float64(600)
	face := passed["face_value"].(float64)
	if face <= 0 {
		t.Fatalf("face_value = %v, want the priced face of the reservation", passed["face_value"])
	}
	if absorbed["face_value"] != face {
		t.Errorf("face values differ (%v vs %v) — only the incidence was supposed to change",
			face, absorbed["face_value"])
	}
	if passed["amount"] != face+feeTotal {
		t.Errorf("passed-on total = %v, want face %v + %v of fees the buyer pays",
			passed["amount"], face, feeTotal)
	}
	if absorbed["amount"] != face {
		t.Errorf("absorbed total = %v, want the bare face %v — an absorbed fee must NOT reach "+
			"the buyer's total", absorbed["amount"], face)
	}

	// The SAME fee amount is recorded on both, which is the half that stops
	// incidence from being read as "the fee did not apply".
	feeAmount := func(res map[string]any) float64 {
		t.Helper()
		items, ok := res["fee_breakdown"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("want one fee in the breakdown, got %v", res["fee_breakdown"])
		}
		item := items[0].(map[string]any)
		if item["fee_code"] != "service" {
			t.Errorf("fee_code = %v", item["fee_code"])
		}
		return item["amount"].(float64)
	}
	if a, b := feeAmount(passed), feeAmount(absorbed); a != 600 || b != 600 {
		t.Errorf("fee amounts = %v and %v, want 600 both — the absorbed fee is still OWED to a "+
			"payee (TKT-217), it is simply not charged to the buyer", a, b)
	}

	// AC8: checkout captures the STORED total, not a recomputation.
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "tkt215-order",
		map[string]any{"reservation_id": passed["reservation_id"], "name": "Fee Buyer",
			"email": "fees@example.test", "payment_token": "fake-ok"})
	if code != 200 {
		t.Fatalf("checkout %d %s", code, body)
	}
	var order map[string]any
	if err = json.Unmarshal(body, &order); err != nil {
		t.Fatal(err)
	}

	pay, err := pgx.Connect(ctx, dsn("payments", "payments"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pay.Close(context.Background()) }()
	var captured int64
	if err = pay.QueryRow(ctx, `SELECT amount FROM journal_entries
		WHERE fact_type='order.created' AND payload->>'order_id' = $1`,
		fmt.Sprint(order["order_id"])).Scan(&captured); err != nil {
		t.Fatalf("the money journal must carry the captured amount: %v", err)
	}
	if captured != int64(face+feeTotal) {
		t.Errorf("captured = %d, want the stored fee-inclusive %v. The bare face %v would mean "+
			"the buyer was charged without their fees; anything else means checkout recomputed "+
			"instead of charging what was quoted", captured, face+feeTotal, face)
	}

	// And the commerce row agrees with what was captured.
	com, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = com.Close(context.Background()) }()
	var total, storedFace, passedOn int64
	if err = com.QueryRow(ctx, `SELECT total_amount, face_value_amount,
		(fee_resolution_snapshot->>'passed_on_fees')::bigint
		FROM reservations WHERE id = $1`, passed["reservation_id"]).Scan(&total, &storedFace, &passedOn); err != nil {
		t.Fatalf("the reservation must carry its fee composition: %v", err)
	}
	if total != int64(face+feeTotal) || storedFace != int64(face) || passedOn != int64(feeTotal) {
		t.Errorf("stored total=%d face=%d passed_on=%d, want %v/%v/%v",
			total, storedFace, passedOn, face+feeTotal, face, feeTotal)
	}

	// TKT-217: the ledger says who that money is owed to, and it balances against
	// what the provider took. This is the epic's second condition of success,
	// asserted end to end rather than at a seam.
	settlementURL := fmt.Sprintf("%s/internal/orders/%v/settlement?organizer_id=%s",
		paymentsURL, order["order_id"], organizerID)
	code, body = internalJSON(t, http.MethodGet, settlementURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("settlement read %d %s", code, body)
	}
	var ledger struct {
		Total   int64 `json:"total"`
		Entries []struct {
			EntryKind string  `json:"entry_kind"`
			Amount    int64   `json:"amount"`
			FeeCode   *string `json:"fee_code"`
			PayeeID   *string `json:"payee_id"`
		} `json:"entries"`
	}
	if err = json.Unmarshal(body, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.Total != int64(face+feeTotal) {
		t.Errorf("ledger totals %d, want the captured %v — every cent of a capture must be "+
			"attributed", ledger.Total, face+feeTotal)
	}
	var organizerLine, feeLines int64
	var sawFee bool
	for _, e := range ledger.Entries {
		switch e.EntryKind {
		case "face_value":
			organizerLine += e.Amount
		case "fee":
			feeLines += e.Amount
			if e.FeeCode != nil && *e.FeeCode == "service" {
				sawFee = true
			}
		}
	}
	if organizerLine != int64(face) {
		t.Errorf("organizer line = %d, want the face %v (no absorbed fee on this order)",
			organizerLine, face)
	}
	if feeLines != int64(feeTotal) || !sawFee {
		t.Errorf("fee lines total %d (service seen: %v), want %v — the fee the buyer paid must be "+
			"owed to somebody", feeLines, sawFee, feeTotal)
	}
}

// Incidence decides the charge with the CALLER HELD CONSTANT (TKT-248, ai-review
// pass 2 [medium]).
//
// WHY THIS EXISTS. TKT-248 had to change the test above from two-channels to
// partner-vs-public, because a public request can no longer name a channel and the
// smoke stack enrols exactly one partner credential. That left its arms differing
// in THREE things at once — incidence, channel presence, and whether the caller
// was authenticated — so a build that ignored `incidence` entirely and instead
// treated channel-less fees as passed-on and channel-scoped fees as absorbed would
// satisfy every assertion up there.
//
// This test removes the confound rather than balancing it: ONE public reservation,
// ONE caller, carrying TWO fee codes that differ only in incidence. Nothing about
// the caller, the channel or the credential varies, so `incidence` is the only
// thing that can explain the two fees landing differently.
//
// (Two rules for the SAME code would not work: ADR-046 resolves one winner per fee
// code, so the second could never apply. Two codes is what makes a single-caller
// fixture possible at all.)
func TestIncidenceDecidesTheChargeForOneCaller(t *testing.T) {
	_, ticketType := setupCheckoutOffer(t, "tkt248inc")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cat.Close(ctx) }()

	var eventID string
	if err = cat.QueryRow(ctx, `SELECT p.event_id FROM ticket_types t
		JOIN performances p ON p.id = t.performance_id WHERE t.id = $1`, ticketType).Scan(&eventID); err != nil {
		t.Fatal(err)
	}

	// Both channel-AGNOSTIC, so both apply to the same public sale. Same amount,
	// same basis, same scope — only the fee code and the incidence differ.
	for _, r := range []struct{ feeCode, incidence string }{
		{"service", "passed_on"}, // the buyer pays this one
		{"facility", "absorbed"}, // the organizer bears this one
	} {
		if _, err = cat.Exec(ctx, `INSERT INTO fee_rules
			(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
			VALUES($1,'event',$2,$3,'per_ticket_fixed',300,'EUR',$4,NULL)`,
			organizerID, eventID, r.feeCode, r.incidence); err != nil {
			t.Fatal(err)
		}
	}

	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "tkt248inc",
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != 201 {
		t.Fatalf("reserve: %d %s", code, body)
	}
	var res map[string]any
	if err = json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}

	// 300 per ticket x 2 = 600 for EACH code. Asserted relative to the observed
	// face value rather than a hardcoded price, so this tests the rule and not the
	// seed data.
	const perCode = float64(600)
	face := res["face_value"].(float64)
	if face <= 0 {
		t.Fatalf("face_value = %v", res["face_value"])
	}
	if res["amount"] != face+perCode {
		t.Errorf("charged total = %v, want face %v + %v: exactly ONE of the two fees is "+
			"passed_on. face+1200 means an absorbed fee reached the buyer; a bare face means a "+
			"passed_on fee did not (ADR-046 §3).", res["amount"], face, perCode)
	}

	// BOTH fees are recorded, which is the half that stops incidence being read as
	// "the absorbed fee did not apply" — TKT-217 still owes it to a payee.
	items, ok := res["fee_breakdown"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("want both fee codes in the breakdown, got %v", res["fee_breakdown"])
	}
	seen := map[string]float64{}
	for _, it := range items {
		item := it.(map[string]any)
		seen[item["fee_code"].(string)] = item["amount"].(float64)
	}
	if seen["service"] != perCode || seen["facility"] != perCode {
		t.Errorf("fee amounts = %v, want %v for both codes — an absorbed fee is still OWED, "+
			"it is simply not charged to the buyer", seen, perCode)
	}
}
