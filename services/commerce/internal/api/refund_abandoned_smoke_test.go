//go:build smoke

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	commercestore "ticketing/services/commerce/internal/store"
)

// TKT-525 S2 (ADR-087, D12): a valid staff replay of a refund that an operator recorded as
// abandoned answers 409 with `code: refund_abandoned`, and reaches no payments route. Generic
// conflicts keep their generic body: a reused key with a different request, and an ordinary
// payments 409. The Back Office retires a saved retry only on the specific code.
func TestStaffReplayOfAnAbandonedRefundAnswersRefundAbandoned(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "staff-abandoned-replay", 2, 1000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_refunds WHERE organizer_id=$1`, f.organizer) })

	var mu sync.Mutex
	var paths []string
	answers := []struct {
		status int
		body   string
	}{
		{http.StatusUnprocessableEntity, `{"error":"provider refund failed","code":"provider_refund_failed","provider_ref":"re_terminal"}`},
		{http.StatusUnprocessableEntity, `{"error":"provider refund failed","code":"provider_refund_failed","provider_ref":"re_terminal"}`},
		{http.StatusConflict, `{"error":"refund legs would exceed the captured amount"}`},
	}
	payments := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		n := len(paths)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n > len(answers) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"peer: unexpected call"}`))
			return
		}
		w.WriteHeader(answers[n-1].status)
		_, _ = w.Write([]byte(answers[n-1].body))
	}))
	defer payments.Close()
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}

	s := newTestServer(db, payments.Client(), "", "", payments.URL, internalTok).
		WithStaffWriteCredential(staffTok).WithOrganizerAssertionVerifier(testOrgVerifier(t))
	post := func(key, body string) (int, map[string]string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/refunds", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Commerce-Staff-Write-Token", staffTok)
		req.Header.Set(organizerAssertionHeader, mintTestAssertion(t, testOrgKey(), f.organizer.String(), time.Now().Add(time.Hour)))
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		s.Router(nil, true).ServeHTTP(rec, req)
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("invalid JSON body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, out
	}

	const key, original = "staff-abandoned-1", `{"quantity":1,"actor":"support","reason":"buyer cannot attend"}`
	if code, body := post(key, original); code != http.StatusUnprocessableEntity || body["code"] != "provider_refund_failed" {
		t.Fatalf("first attempt: %d %v, want the terminal 422", code, body)
	}
	refundID := commercestore.RefundID(f.organizer, key)
	// A PENDING reservation is not a local abandonment: the staff replay still reaches
	// payments, so an ordinary completion can still settle the refund.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refund_abandonment_dispositions(organizer_id,refund_id,state,charge_key,amount,currency,operator_name,reason)
		SELECT organizer_id, id, 'pending', 'checkout-key', amount, currency, 'Ada Operator', 'nobody will refund this attempt'
		FROM order_refunds WHERE organizer_id=$1 AND id=$2`, f.organizer, refundID); err != nil {
		t.Fatal(err)
	}
	if code, body := post(key, original); code != http.StatusUnprocessableEntity || body["code"] != "provider_refund_failed" {
		t.Fatalf("replay while the reservation is pending: %d %v, want the payments answer", code, body)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO refund_outcomes(organizer_id,subject_kind,subject_id,kind,amount,currency,external_ref,operator_name,session_user_name,reason)
		VALUES($1,'order_refund',$2,'abandoned',1000,'EUR','re_terminal','Ada Operator',session_user,'nobody will refund this attempt')`,
		f.organizer, refundID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE order_refund_abandonment_dispositions SET state='finalized' WHERE organizer_id=$1 AND refund_id=$2`, f.organizer, refundID); err != nil {
		t.Fatal(err)
	}

	// The exact original request: the specific code, and no payments call.
	code, body := post(key, original)
	if code != http.StatusConflict || body["error"] != "refund abandoned" || body["code"] != "refund_abandoned" || len(body) != 2 {
		t.Fatalf("replay of the abandoned refund: %d %v, want 409 refund_abandoned", code, body)
	}
	if got := seen(); len(got) != 2 {
		t.Fatalf("payments calls = %v, want only the first attempt's and the pending replay's", got)
	}

	// The same key with a different request is the generic idempotency conflict.
	code, body = post(key, `{"quantity":1,"actor":"support","reason":"a different reason"}`)
	if code != http.StatusConflict || body["code"] != "" || body["error"] != "refund conflicts with an existing request" {
		t.Fatalf("mismatched fingerprint: %d %v, want the generic 409", code, body)
	}
	// A fresh key fits (the abandoned unit is released) and an ordinary payments 409 stays
	// generic.
	code, body = post("staff-abandoned-2", original)
	if code != http.StatusConflict || body["code"] != "" || body["error"] != "refund unresolved" {
		t.Fatalf("ordinary payments 409: %d %v, want the generic 409", code, body)
	}
	if got := seen(); len(got) != 3 || got[2] != "POST /internal/psp/partial-refund" {
		t.Fatalf("payments calls = %v, want the fresh key's one partial refund", got)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, f.organizer, refundID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("abandoned refund status = %q err=%v, want pending", status, err)
	}
}
