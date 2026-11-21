//go:build smoke

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	commercestore "ticketing/services/commerce/internal/store"
)

func TestRefundOrderProviderTerminalFailureAnswers422AndStaysPending(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "staff-terminal-refund", 2, 1000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_refunds WHERE organizer_id=$1`, f.organizer) })
	payments := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/psp/partial-refund" {
			t.Errorf("payments path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"provider refund failed","code":"provider_refund_failed","provider_ref":"re_terminal"}`))
	}))
	defer payments.Close()

	const key = "staff-terminal-refund-1"
	// The staff credential and an assertion for the fixture's organizer (TKT-287).
	s := newTestServer(db, payments.Client(), "", "", payments.URL, internalTok).
		WithStaffWriteCredential(staffTok).WithOrganizerAssertionVerifier(testOrgVerifier(t))
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/refunds", bytes.NewBufferString(
		`{"quantity":1,"actor":"support","reason":"terminal provider refusal"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
	req.Header.Set(organizerAssertionHeader, mintTestAssertion(t, testOrgKey(), f.organizer.String(), time.Now().Add(time.Hour)))
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body %s: %v", rec.Body.String(), err)
	}
	if body["error"] != "provider refund failed" || body["code"] != "provider_refund_failed" {
		t.Fatalf("body=%v, want stable terminal refusal", body)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`,
		f.organizer, commercestore.RefundID(f.organizer, key)).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("refund row status = %q, want pending", status)
	}
}

// Same-key pin 1, staff half (TKT-525 COS 5). After a terminal 422 the staff repeats the
// ORIGINAL request. The repeat must reach payments with the same charge key and the same
// refund key the first attempt used, so payments resolves the same bound leg instead of binding
// a second one, and Commerce must answer the declared 422 again with one binding and no
// compensating fact. The keys are recorded by the peer BEFORE it answers, so a second attempt
// under any other key is observed rather than inferred.
func TestStaffRefundRepeatAfterTerminal422ReusesTheOriginalKeys(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	const chargeKey = "staff-terminal-repeat"
	f := seedExchangeSource(t, db, ctx, chargeKey, 2, 1000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_refunds WHERE organizer_id=$1`, f.organizer) })
	type sent struct{ path, chargeKey, refundKey string }
	var mu sync.Mutex
	var requests []sent
	payments := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			IdempotencyKey string `json:"idempotency_key"`
			RefundKey      string `json:"refund_key"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		requests = append(requests, sent{r.URL.Path, body.IdempotencyKey, body.RefundKey})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"provider refund failed","code":"provider_refund_failed","provider_ref":"re_terminal"}`))
	}))
	defer payments.Close()

	const key = "staff-terminal-repeat-1"
	s := newTestServer(db, payments.Client(), "", "", payments.URL, internalTok).
		WithStaffWriteCredential(staffTok).WithOrganizerAssertionVerifier(testOrgVerifier(t))
	for attempt := 1; attempt <= 2; attempt++ {
		req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/refunds", bytes.NewBufferString(
			`{"quantity":1,"actor":"support","reason":"terminal provider refusal"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
		req.Header.Set(organizerAssertionHeader, mintTestAssertion(t, testOrgKey(), f.organizer.String(), time.Now().Add(time.Hour)))
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		s.Router(nil, true).ServeHTTP(rec, req)
		var body map[string]string
		if rec.Code != http.StatusUnprocessableEntity || json.Unmarshal(rec.Body.Bytes(), &body) != nil ||
			body["code"] != "provider_refund_failed" {
			t.Fatalf("attempt %d: status=%d body=%s, want the declared terminal 422", attempt, rec.Code, rec.Body.String())
		}
	}

	refundID := commercestore.RefundID(f.organizer, key)
	want := sent{"/internal/psp/partial-refund", chargeKey, refundID.String()}
	if len(requests) != 2 || requests[0] != want || requests[1] != want {
		t.Fatalf("payments requests = %+v, want two partial refunds under charge key %q and refund key %s", requests, chargeKey, refundID)
	}
	if n := countCommerceRows(t, ctx, db, `SELECT count(*) FROM order_refunds WHERE organizer_id=$1`, f.organizer); n != 1 {
		t.Fatalf("refund bindings = %d, want 1", n)
	}
	if n := countCommerceRows(t, ctx, db, `SELECT count(*) FROM order_facts WHERE organizer_id=$1 AND fact_type='order.refunded'`, f.organizer); n != 0 {
		t.Fatalf("order.refunded facts = %d, want none after two terminal refusals", n)
	}
}
