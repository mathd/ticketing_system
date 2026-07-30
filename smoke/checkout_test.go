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
	venue := created(t, catalog+"/venues", map[string]any{"organizer_id": organizerID, "name": "Checkout " + suffix, "ga_capacity": 5})
	event := created(t, catalog+"/events", map[string]any{"organizer_id": organizerID, "name": map[string]string{"fr": "Paiement " + suffix, "en": "Checkout " + suffix}})
	// starts_at is relative to now, never a fixed date (TKT-93): a hardcoded
	// 2026 date silently ages into the past and would eventually publish a
	// performance that has already started.
	startsAt := time.Now().UTC().AddDate(0, 0, 90).Truncate(time.Second).Format(time.RFC3339)
	perf := created(t, catalog+"/performances", map[string]any{"organizer_id": organizerID, "event_id": event["id"], "venue_id": venue["id"], "starts_at": startsAt, "timezone": "UTC"})
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
	seed, err := base64.RawStdEncoding.DecodeString("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
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
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://access:access@%s/access", pgHostPort))
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
