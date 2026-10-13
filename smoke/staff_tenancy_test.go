//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// foreignAssertionFor signs a well-formed v2 assertion with a key catalog does NOT
// hold, under catalog's own kid: a forgery that differs from a real one only in the
// signature.
func foreignAssertionFor(t *testing.T, organizer string) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Join([]string{"v2", os.Getenv("SMOKE_CATALOG_ORGANIZER_ASSERTION_KID"),
		"00000000-0000-0000-0000-0000000000aa", organizer, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(payload)))
}

// assertStaffRefusal checks the exact D3 refusal: 404 with the same body as an
// absent route, so no refusal says which predicate failed.
func assertStaffRefusal(t *testing.T, what string, code int, body []byte) {
	t.Helper()
	if code != http.StatusNotFound || strings.TrimSpace(string(body)) != `{"error":"not found"}` {
		t.Fatalf("%s: status=%d body=%.300s, want 404 {\"error\":\"not found\"}", what, code, body)
	}
}

// allRefundRows counts EVERY order_refunds row for the order, under ANY organizer,
// straight from commerce's database. The owner's detail read filters by organizer,
// so it cannot see a pending refund bound under an attacker's organizer — exactly
// the row a broken guard would write.
func allRefundRows(t *testing.T, orderID string) int {
	t.Helper()
	ctx := context.Background()
	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce db: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1`, orderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// staffOrderRefunds reads the order as its OWNER and returns its refunded quantity
// and refund rows — what a refused attempt must not have changed.
func staffOrderRefunds(t *testing.T, orderID string) (refundedQty int, refunds int) {
	t.Helper()
	code, out := commerceStaffCall(t, http.MethodGet, fmt.Sprintf("%s/internal/orders/%s", commerceURL, orderID), "",
		staffHeaders(t, organizerID), nil)
	if code != http.StatusOK {
		t.Fatalf("owner read: %d %s", code, out)
	}
	var detail struct {
		Totals struct {
			RefundedQuantity int `json:"refunded_quantity"`
		} `json:"totals"`
		Refunds []json.RawMessage `json:"refunds"`
	}
	if err := json.Unmarshal(out, &detail); err != nil {
		t.Fatal(err)
	}
	return detail.Totals.RefundedQuantity, len(detail.Refunds)
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
		{"a wrong staff credential", map[string]string{
			"X-Commerce-Staff-Write-Token": uuid.NewString() + uuid.NewString(), organizerAssertionHeader: owner[organizerAssertionHeader]}},
		{"an assertion signed by a key catalog does not hold", map[string]string{
			"X-Commerce-Staff-Write-Token": owner["X-Commerce-Staff-Write-Token"], organizerAssertionHeader: foreignAssertionFor(t, organizerID)}},
		{"no assertion", map[string]string{"X-Commerce-Staff-Write-Token": owner["X-Commerce-Staff-Write-Token"]}},
		// The shared internal token used to open these operations on its own.
		{"the internal token with the owner's assertion", map[string]string{
			"X-Internal-Token": internalTokenFor(commerceURL), organizerAssertionHeader: owner[organizerAssertionHeader]}},
		// The tenancy case the ticket is about: a valid credential and a VALID assertion,
		// for another organizer.
		{"a valid assertion for another organizer", staffHeaders(t, uuid.NewString())},
	}
}

// TestACrossTenantRefundIsRefusedAndMovesNoMoney is TKT-287 COS2, executed against a
// real paid order (two tickets at 1250). After EACH refusal the owner reads the order
// and finds no refund row and nothing refunded. Then the owner refunds one ticket, and
// a replay of that exact request under another organizer's valid assertion is refused
// too and leaves exactly the owner's one refund.
func TestACrossTenantRefundIsRefusedAndMovesNoMoney(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "tenancy-refund")
	url := fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, orderID)
	body := map[string]any{"quantity": 1, "actor": "staff:tenancy", "reason": "cross-tenant probe"}

	for i, tc := range staffRefusals(t) {
		t.Run(tc.name, func(t *testing.T) {
			// A distinct key per case: a shared key would let a refusal hide behind a
			// replay of an earlier attempt.
			code, out := commerceStaffCall(t, http.MethodPost, url, fmt.Sprintf("tenancy-refund-%d-%s", i, orderID), tc.headers, body)
			assertStaffRefusal(t, tc.name, code, out)
			if qty, rows := staffOrderRefunds(t, orderID); qty != 0 || rows != 0 {
				t.Fatalf("after a refused refund the order shows refunded_quantity=%d and %d refund row(s), want 0 and 0", qty, rows)
			}
			if n := allRefundRows(t, orderID); n != 0 {
				t.Fatalf("after a refused refund the database holds %d refund row(s) for the order under any organizer, want 0", n)
			}
		})
	}

	ownerKey := "tenancy-refund-owner-" + orderID
	code, out := commerceStaffCall(t, http.MethodPost, url, ownerKey, staffHeaders(t, organizerID), body)
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
		t.Fatalf("owner refund = %+v, want a fresh partial refund of 1250 with refunded_quantity 1", refund)
	}

	// The replay of an EXISTING result under another tenant must not become a way
	// around scope: same key, same body, another organizer's valid assertion.
	code, out = commerceStaffCall(t, http.MethodPost, url, ownerKey, staffHeaders(t, uuid.NewString()), body)
	assertStaffRefusal(t, "a cross-tenant replay of the owner's refund", code, out)
	if qty, rows := staffOrderRefunds(t, orderID); qty != 1 || rows != 1 {
		t.Fatalf("after the cross-tenant replay the order shows refunded_quantity=%d and %d refund row(s), want 1 and 1", qty, rows)
	}
	if n := allRefundRows(t, orderID); n != 1 {
		t.Fatalf("after the cross-tenant replay the database holds %d refund row(s) for the order, want exactly the owner's 1", n)
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
