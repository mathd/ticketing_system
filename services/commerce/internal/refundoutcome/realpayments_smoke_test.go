//go:build smoke

package refundoutcome

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/store"
)

// TKT-525 across the two services: the Commerce coordinator, this package's database, and the
// REAL payments service of the running smoke stack, reached through a recording forwarder.
// scripts/smoke.sh passes the stack's payments URL and credential. The fake PSP behind it fails
// the first refund of a `fake-refund-fail-once` charge and proves that failure, so the leg, the
// proof, the marker and both money ceilings are the production ones.

// refundFailsOnceToken mirrors fakepsp.TokenRefundFailsOnce (shared/go/fakepsp).
const refundFailsOnceToken = "fake-refund-fail-once"

type realPayments struct {
	url, token string
	client     *http.Client
}

func realPaymentsFromEnv(t *testing.T) realPayments {
	t.Helper()
	u, token := os.Getenv("COMMERCE_REFUNDOUTCOME_PAYMENTS_URL"), os.Getenv("COMMERCE_REFUNDOUTCOME_PAYMENTS_TOKEN")
	if u == "" || token == "" {
		t.Skip("COMMERCE_REFUNDOUTCOME_PAYMENTS_URL and _TOKEN are not set")
	}
	return realPayments{url: strings.TrimSuffix(u, "/"), token: token, client: &http.Client{Timeout: 10 * time.Second}}
}

// do sends one internal request to real payments.
func (p realPayments) do(t *testing.T, method, path, idempotencyKey string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, p.url+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", p.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// capture creates the payments charge a checkout would, captured with the fail-once token.
func (p realPayments) capture(t *testing.T, org uuid.UUID, chargeKey string, amount int64) {
	t.Helper()
	code, body := p.do(t, http.MethodPost, "/internal/charges", chargeKey, map[string]any{
		"order_id": uuid.New(), "organizer_id": org, "buyer_id": uuid.New(), "amount": amount, "currency": "EUR",
		"payment_token": refundFailsOnceToken,
		"settlement":    map[string]any{"face_value": amount, "passed_on": 0, "absorbed": 0, "total_amount": amount, "currency": "EUR", "fees": []any{}},
	})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("capture: %d %s", code, body)
	}
}

func (p realPayments) proofStatus(t *testing.T, org uuid.UUID, chargeKey string, refund uuid.UUID) int {
	t.Helper()
	q := url.Values{"organizer_id": {org.String()}, "idempotency_key": {chargeKey}, "refund_key": {refund.String()}}
	code, _ := p.do(t, http.MethodGet, "/internal/psp/partial-refund/failure-proof?"+q.Encode(), "", nil)
	return code
}

func (p realPayments) legStatus(t *testing.T, org uuid.UUID, chargeKey string, refund uuid.UUID) int {
	t.Helper()
	q := url.Values{"organizer_id": {org.String()}, "source_idempotency_key": {chargeKey}, "refund_idempotency_key": {refund.String()}}
	code, _ := p.do(t, http.MethodGet, "/internal/refund-legs?"+q.Encode(), "", nil)
	return code
}

// forwarder sits between Commerce and real payments. It records every call with payments'
// status, and can lose the answer to the next abandon request after payments answered 200.
type forwarder struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
	drop  bool
}

func newForwarder(t *testing.T, p realPayments) *forwarder {
	t.Helper()
	f := &forwarder{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, p.url+r.URL.RequestURI(), bytes.NewReader(raw))
		if err != nil {
			t.Error(err)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := p.client.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+http.StatusText(resp.StatusCode))
		drop := f.drop && r.URL.Path == "/internal/psp/partial-refund/abandon" && resp.StatusCode == http.StatusOK
		if drop {
			f.drop = false
		}
		f.mu.Unlock()
		if drop {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *forwarder) dropNextAbandonAnswer() {
	f.mu.Lock()
	f.drop = true
	f.mu.Unlock()
}

func (f *forwarder) seen(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// caller is the refund service's transport, as production builds it: JSON, the internal
// credential, and the idempotency key when there is one.
func (f *forwarder) caller(token string) refunds.Caller {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, method, u, key string, in any, internal bool) (int, []byte, error) {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if internal {
			req.Header.Set("X-Internal-Token", token)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, body, err
	}
}

// failedStaffRefund refunds `quantity` units of the order through the real refund service.
// Payments binds the leg, the fake PSP fails the refund terminally, and the refund stays pending.
func failedStaffRefund(t *testing.T, ctx context.Context, db *sql.DB, f *forwarder, p realPayments, s seeded, key string, quantity int32) uuid.UUID {
	t.Helper()
	svc := refunds.New(db, f.caller(p.token), f.srv.URL, "http://access.invalid", "http://inventory.invalid")
	_, err := svc.Refund(ctx, store.RefundRequest{OrderID: s.order, OrganizerID: s.org, Quantity: quantity,
		IdempotencyKey: key, Actor: "support@example.test", Reason: "buyer cannot attend"})
	if !errors.Is(err, refunds.ErrProviderRefundFailed) {
		t.Fatalf("staff refund %s: err = %v, want the provider's terminal failure", key, err)
	}
	return store.RefundID(s.org, key)
}

// RA1-2: the recovery through REAL payments. Payments commits its marker and its 200 is lost.
// During the gap: one pending reservation, the marker (the real proof route answers 409), the
// money allowance released in payments, the quantity still reserved in Commerce, and an outside
// completion refused. The retry then fails at the Commerce commit. The original command finishes
// from the marker, with the original attribution, and only that commit releases the quantity.
func TestAbandonedRecoversThroughRealPaymentsAfterALostAnswerAndACommitFailure(t *testing.T) {
	p := realPaymentsFromEnv(t)
	db, ctx := outcomeDB(t)
	f := newForwarder(t, p)
	org := uuid.New()
	s := seedOrderWithChargeKey(t, ctx, db, org, uuid.New(), 2, 1000, "checkout-real-"+uuid.NewString())
	p.capture(t, org, s.chargeKey, 2000)
	refund := failedStaffRefund(t, ctx, db, f, p, s, "staff-real-"+uuid.NewString(), 2)
	in := abandonRequest(s, refund)
	svc := NewAbandon(db, NewHTTPAbandoner(f.srv.URL, p.token, 10*time.Second))

	f.dropNextAbandonAnswer()
	if _, err := svc.RecordAbandoned(ctx, in); !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("lost answer: err = %v, want ErrAbandonUncertain", err)
	}
	if got := f.seen("POST /internal/psp/partial-refund/abandon"); len(got) != 1 || !strings.HasSuffix(got[0], "OK") {
		t.Fatalf("abandon calls = %v, want one that payments answered 200", got)
	}
	if got := readReservation(t, ctx, db, s, refund); got != pendingReservation(in) {
		t.Fatalf("reservation during the gap = %+v, want pending", got)
	}
	if code := p.proofStatus(t, org, s.chargeKey, refund); code != http.StatusConflict {
		t.Fatalf("real failure proof after the marker = %d, want 409", code)
	}
	if quantityFree(t, ctx, db, s, 1) {
		t.Fatal("Commerce released quantity before its outcome committed")
	}
	if _, err := New(db, NewHTTPPayments(f.srv.URL, p.token, 10*time.Second)).RecordRefundedOutside(ctx, outsideRequest(s, refund)); !errors.Is(err, store.ErrAbandonmentReserved) {
		t.Fatalf("outside completion during the gap: err = %v, want ErrAbandonmentReserved", err)
	}
	// The marker released the leg's money allowance: a direct partial refund of the whole
	// captured amount under a fresh refund key now fits the payments ceiling.
	if code, body := p.do(t, http.MethodPost, "/internal/psp/partial-refund", "", map[string]any{
		"organizer_id": org, "idempotency_key": s.chargeKey, "refund_key": uuid.NewString(), "amount": 2000, "currency": "EUR",
	}); code != http.StatusOK {
		t.Fatalf("payments allowance after the marker: %d %s, want the captured amount released", code, body)
	}

	remove := injectCommitFailure(t, ctx, db, "refund_outcomes", "subject_id", refund)
	if _, err := svc.RecordAbandoned(ctx, in); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("retry: err = %v, want the injected commit failure", err)
	}
	if st := readRefundState(t, ctx, db, s, refund); st != untouched {
		t.Fatalf("after the failed commit: %+v", st)
	}
	if got := readReservation(t, ctx, db, s, refund); got != pendingReservation(in) {
		t.Fatalf("reservation after the failed commit = %+v, want pending", got)
	}
	if quantityFree(t, ctx, db, s, 1) {
		t.Fatal("a commit that failed released quantity")
	}
	remove()

	got, err := svc.RecordAbandoned(ctx, in)
	if err != nil || got.Replay || !strings.HasPrefix(got.Outcome.ExternalRef, "re_") ||
		got.Outcome.OperatorName != in.OperatorName || got.Outcome.Reason != in.Reason {
		t.Fatalf("final run = %+v err=%v", got, err)
	}
	if got := readReservation(t, ctx, db, s, refund); got != finalizedReservation(in) {
		t.Fatalf("reservation after recovery = %+v", got)
	}
	if st := readRefundState(t, ctx, db, s, refund); st != (refundState{"pending", sql.NullString{}, 1, 0, 0, 0, "none"}) {
		t.Fatalf("after recovery: %+v", st)
	}
	if !quantityFree(t, ctx, db, s, 2) {
		t.Fatal("the recovered abandonment did not release the quantity")
	}
	if n := len(f.seen("POST /internal/psp/partial-refund/abandon")); n != 3 {
		t.Fatalf("abandon calls = %d, want 3 (lost, commit failure, finish)", n)
	}
}

// RA1-1: two Commerce orders with their own quantity ceilings store the same payments charge
// key (the checkout and exchange alias, TKT-586). The outside completion of order A wins the
// race against an abandonment of the same refund (commit order 2: the outside write commits
// while the abandonment waits before its reservation), so payments never marks A's leg. A fresh
// staff refund of order B then fits B's Commerce ceiling, and payments refuses it at the
// captured-money ceiling: no leg is bound and nothing reaches the provider.
func TestSharedChargeKeepsTheCapturedCeilingAfterAnOutsideCompletionWins(t *testing.T) {
	p := realPaymentsFromEnv(t)
	db, ctx := outcomeDB(t)
	f := newForwarder(t, p)
	org := uuid.New()
	chargeKey := "exchange:" + uuid.NewString()
	a := seedOrderWithChargeKey(t, ctx, db, org, uuid.New(), 2, 1000, chargeKey)
	b := seedOrderWithChargeKey(t, ctx, db, org, uuid.New(), 2, 1000, chargeKey)
	p.capture(t, org, chargeKey, 2000)
	refundA := failedStaffRefund(t, ctx, db, f, p, a, "staff-a-"+uuid.NewString(), 2)

	// Commit order 2 with real payments: the outside write queues first on A's refund row, the
	// reservation second.
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM order_refunds WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, org, refundA); err != nil {
		t.Fatal(err)
	}
	outside, abandon := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := New(db, NewHTTPPayments(f.srv.URL, p.token, 10*time.Second)).RecordRefundedOutside(ctx, outsideRequest(a, refundA))
		outside <- err
	}()
	waitForLockWaiters(t, ctx, db, 1)
	go func() {
		_, err := NewAbandon(db, NewHTTPAbandoner(f.srv.URL, p.token, 10*time.Second)).RecordAbandoned(ctx, abandonRequest(a, refundA))
		abandon <- err
	}()
	waitForLockWaiters(t, ctx, db, 2)
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-outside; err != nil {
		t.Fatalf("outside completion of A: %v", err)
	}
	// The reservation sees the outside outcome and refuses: no reservation, and A completed
	// with the outside outcome as its evidence.
	if err := <-abandon; !errors.Is(err, store.ErrRefundOutcomeConflict) {
		t.Fatalf("abandonment of A after its outside completion: err = %v, want ErrRefundOutcomeConflict", err)
	}
	if got := readReservation(t, ctx, db, a, refundA); got.count != 0 {
		t.Fatalf("A's reservation = %+v, want none", got)
	}
	if st := readRefundState(t, ctx, db, a, refundA); st.status != "completed" || st.outside.String != store.OutcomeRefundedOutside || st.qty != 2 {
		t.Fatalf("A after the outside completion: %+v", st)
	}

	keyB := "staff-b-" + uuid.NewString()
	svc := refunds.New(db, f.caller(p.token), f.srv.URL, "http://access.invalid", "http://inventory.invalid")
	_, err = svc.Refund(ctx, store.RefundRequest{OrderID: b.order, OrganizerID: org, Quantity: 2,
		IdempotencyKey: keyB, Actor: "support@example.test", Reason: "replacement order refund"})
	if !errors.Is(err, refunds.ErrPaymentsRefused) {
		t.Fatalf("staff refund of B: err = %v, want payments' captured-money refusal", err)
	}
	if got := f.seen("POST /internal/psp/partial-refund "); len(got) != 2 || got[1] != "POST /internal/psp/partial-refund Conflict" {
		t.Fatalf("partial refund calls = %v, want A's failed attempt and B's 409", got)
	}
	if code := p.legStatus(t, org, chargeKey, store.RefundID(org, keyB)); code != http.StatusNotFound {
		t.Fatalf("B's leg lookup = %d, want 404: payments bound no leg, so nothing reached the provider", code)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, store.RefundID(org, keyB)).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("B's refund status = %q err=%v, want pending", status, err)
	}
	// Asserted after B, so a split that let the abandonment reach payments is observed as the
	// money it releases, not only as the call.
	if got := f.seen("POST /internal/psp/partial-refund/abandon"); len(got) != 0 {
		t.Fatalf("abandon calls = %v, want none", got)
	}
	if code := p.proofStatus(t, org, chargeKey, refundA); code != http.StatusOK {
		t.Fatalf("A's leg after the outside completion: proof %d, want 200 (bound, unmarked)", code)
	}
}
