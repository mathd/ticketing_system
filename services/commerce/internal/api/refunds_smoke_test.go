//go:build smoke

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

	const token, key = "staff-terminal-token", "staff-terminal-refund-1"
	s := newTestServer(db, payments.Client(), "", "", payments.URL, token)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/refunds", bytes.NewBufferString(
		`{"organizer_id":"`+f.organizer.String()+`","quantity":1,"actor":"support","reason":"terminal provider refusal"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", token)
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
