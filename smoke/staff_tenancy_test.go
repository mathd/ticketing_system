//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-287: commerce's three staff operations — refund, void, order read — take the
// organizer from a VERIFIED organizer assertion, never from the request, and the shared
// internal token no longer opens them. The cross-tenant refusals are EXECUTED here
// against the running services (AGENTS.md: a security claim is a hypothesis until it is
// executed). One case per predicate, each passing the others.

// staffHeaders is what the back office sends: its commerce credential and an assertion
// for the given organizer. Built per call and passed explicitly, so a negative case can
// never inherit a credential from a helper (the plan's rule for this suite).
func staffHeaders(t *testing.T, organizer string) map[string]string {
	t.Helper()
	return map[string]string{
		"X-Commerce-Staff-Write-Token": os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"),
		organizerAssertionHeader:       organizerAssertionFor(t, organizer),
	}
}

// commerceStaffCall sends exactly the headers given, through the suite's contract
// chokepoint (checkDirectServiceResponse), like every other direct-service call.
func commerceStaffCall(t *testing.T, method, url, key string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if service := directService(url); service != "" {
		if err := checkDirectServiceResponse(service, resp.Request, resp.StatusCode, resp.Header, out); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return resp.StatusCode, out
}

// staffRefusals are the predicate cases every staff operation must refuse with 404:
// each is otherwise well-formed and differs from the owner's request in ONE respect.
func staffRefusals(t *testing.T) []struct {
	name    string
	headers map[string]string
} {
	owner := staffHeaders(t, organizerID)
	return []struct {
		name    string
		headers map[string]string
	}{
		{"no staff credential", map[string]string{organizerAssertionHeader: owner[organizerAssertionHeader]}},
		{"no assertion", map[string]string{"X-Commerce-Staff-Write-Token": owner["X-Commerce-Staff-Write-Token"]}},
		// The shared internal token used to open these operations on its own.
		{"the internal token with the owner's assertion", map[string]string{
			"X-Internal-Token": internalTokenFor(commerceURL), organizerAssertionHeader: owner[organizerAssertionHeader]}},
		// The tenancy case the ticket is about: a valid credential and a VALID assertion,
		// for another organizer.
		{"a valid assertion for another organizer", staffHeaders(t, uuid.NewString())},
	}
}

// TestACrossTenantRefundIsRefusedAndMovesNoMoney is TKT-287 COS2, executed. The order
// is a real paid order (two tickets at 1250). After every refusal, the owner's own
// refund of ONE ticket must come back as a fresh (not replayed) partial refund of
// exactly 1250 with refunded_quantity 1 — which is only true if no refused attempt
// bound a refund, moved money, or consumed refundable quantity.
func TestACrossTenantRefundIsRefusedAndMovesNoMoney(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "tenancy-refund")
	url := fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, orderID)
	body := map[string]any{"quantity": 1, "actor": "staff:tenancy", "reason": "cross-tenant probe"}

	for i, tc := range staffRefusals(t) {
		t.Run(tc.name, func(t *testing.T) {
			// A distinct key per case: a shared key would let a refusal hide behind a
			// replay of an earlier attempt.
			code, out := commerceStaffCall(t, http.MethodPost, url, fmt.Sprintf("tenancy-refund-%d-%s", i, orderID), tc.headers, body)
			if code != http.StatusNotFound {
				t.Fatalf("status=%d want 404; body=%.300s", code, out)
			}
		})
	}

	code, out := commerceStaffCall(t, http.MethodPost, url, "tenancy-refund-owner-"+orderID, staffHeaders(t, organizerID), body)
	if code != http.StatusOK {
		t.Fatalf("the owner's refund: %d %s", code, out)
	}
	var refund struct {
		Amount           int64  `json:"amount"`
		RefundedQuantity int    `json:"refunded_quantity"`
		RefundStatus     string `json:"refund_status"`
		Replay           bool   `json:"replay"`
	}
	if err := json.Unmarshal(out, &refund); err != nil {
		t.Fatal(err)
	}
	if refund.Replay || refund.Amount != 1250 || refund.RefundedQuantity != 1 || refund.RefundStatus != "partial" {
		t.Fatalf("owner refund = %+v, want a fresh partial refund of 1250 with refunded_quantity 1 — "+
			"anything else means a refused attempt moved money or consumed quantity", refund)
	}
}

// A body that names an organizer is refused (400) even with valid credentials: the
// field is unsubmittable, not validated against the assertion (TKT-287 D2).
func TestARefundBodyNamingAnOrganizerIsRefused(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "tenancy-body")
	code, out := commerceStaffCall(t, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, orderID), "tenancy-body-"+orderID,
		staffHeaders(t, organizerID),
		map[string]any{"organizer_id": organizerID, "quantity": 1, "actor": "staff:tenancy", "reason": "named organizer"})
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%.300s", code, out)
	}
}
