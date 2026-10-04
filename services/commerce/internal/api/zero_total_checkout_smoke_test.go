//go:build smoke

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TKT-285. A checkout whose persisted GROSS total is zero skips the provider: no charge, no
// payment operation, and the order completes through the same tail a paid checkout uses.
//
// Everything here is asserted at the seam the buyer's request crosses, against a real
// database. The payments stub RECORDS every request path (and the fact type for /internal/facts),
// because "the provider was not called" can only be observed from the caller's side: a charge
// that payments refused or ignored leaves no row anywhere, so the operation table cannot see it.

// zeroStack is one checkout handler wired to recording stubs.
type zeroStack struct {
	h        http.Handler
	mu       sync.Mutex
	payments []string // "<path>" or "/internal/facts:<fact_type>"
	hold     []string // inventory action: finalize | confirm | release

	// Behaviour switches, read per request so a test can change them between a first checkout
	// and its replay. operation is the status payments answers to GET /internal/operations
	// (404 = no operation bound, 200 = one is, 500 = the lookup fails); confirm is what
	// inventory answers to confirm; factStatus overrides the answer to /internal/facts for one
	// fact type.
	operation  int
	confirm    int
	factStatus map[string]int
}

func (z *zeroStack) set(f func()) {
	z.mu.Lock()
	defer z.mu.Unlock()
	f()
}

// reset forgets the recorded calls, so a replay's assertions are about the replay alone.
func (z *zeroStack) reset() {
	z.set(func() { z.payments, z.hold = nil, nil })
}

func (z *zeroStack) paymentsCalls() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.payments...)
}

func (z *zeroStack) holdCalls() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.hold...)
}

// factTypes lists the fact types commerce submitted to the journal, in order.
func (z *zeroStack) factTypes() []string {
	var out []string
	for _, c := range z.paymentsCalls() {
		if t, ok := strings.CutPrefix(c, "/internal/facts:"); ok {
			out = append(out, t)
		}
	}
	return out
}

func (z *zeroStack) charges() int {
	n := 0
	for _, c := range z.paymentsCalls() {
		if strings.HasSuffix(c, "/internal/charges") {
			n++
		}
	}
	return n
}

// newZeroStack wires the real checkout handler. confirmStatus is what inventory answers to
// the confirm call, so a case can make the completion tail's first step fail.
func newZeroStack(t *testing.T, db *sql.DB, confirmStatus int) *zeroStack {
	t.Helper()
	z := &zeroStack{operation: http.StatusNotFound, confirm: confirmStatus}
	payments := newCountingStub(t, func(_ *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/internal/facts") {
			entry = "/internal/facts:" + bodyField(r, "fact_type")
		}
		z.mu.Lock()
		z.payments = append(z.payments, entry)
		operation, factCode := z.operation, z.factStatus[strings.TrimPrefix(entry, "/internal/facts:")]
		z.mu.Unlock()
		if strings.HasPrefix(entry, "/internal/facts:") && factCode != 0 {
			w.WriteHeader(factCode)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/internal/operations") {
			w.WriteHeader(operation)
			_, _ = w.Write([]byte(`{"resolved":false}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/internal/charges") {
			_, _ = w.Write([]byte(`{"status":"captured","fact_id":"` + uuid.NewString() + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	inventory := newCountingStub(t, func(_ *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		z.mu.Lock()
		z.hold = append(z.hold, action)
		confirmCode := z.confirm
		z.mu.Unlock()
		if action == "confirm" && confirmCode != http.StatusOK {
			w.WriteHeader(confirmCode)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := newTestServer(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)
	z.h = r
	return z
}

func (z *zeroStack) lookups() int {
	n := 0
	for _, c := range z.paymentsCalls() {
		if strings.HasSuffix(c, "/internal/operations") {
			n++
		}
	}
	return n
}

func (z *zeroStack) checkout(t *testing.T, reservation uuid.UUID, key string) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"reservation_id":%q,"name":"Free Buyer","email":"free@example.test","payment_token":"fake-ok"}`, reservation)
	req := httptest.NewRequest(http.MethodPost, "/reservations/"+reservation.String()+"/checkout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	z.h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	return rec.Code, out
}

func orderStatus(t *testing.T, ctx context.Context, db *sql.DB, reservation uuid.UUID) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM orders WHERE reservation_id=$1`, reservation).Scan(&status); err != nil {
		t.Fatalf("read the order of reservation %s: %v", reservation, err)
	}
	return status
}

func owedCompletions(t *testing.T, ctx context.Context, db *sql.DB, reservation uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM completion_outbox c JOIN orders o ON o.id = c.order_id
		WHERE o.reservation_id=$1`, reservation).Scan(&n); err != nil {
		t.Fatalf("count owed completions: %v", err)
	}
	return n
}

// COS1 / D1 / D3, at the seam: a zero-total checkout completes, owes its completion event,
// journals exactly the two order facts and NEVER asks payments to charge.
//
// The charge count is the load-bearing assertion. A mutant that still calls /internal/charges
// and ignores the answer completes the order just the same, and leaves no operation behind
// if payments refuses it — so only a recording stub can tell it from the real thing.
func TestAZeroTotalCheckoutCompletesWithoutACharge(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	_, reservation := seedCheckoutableAt(t, db, ctx, 0)
	z := newZeroStack(t, db, http.StatusOK)

	code, body := z.checkout(t, reservation, "zero-"+uuid.NewString())
	if code != http.StatusOK || body["status"] != "completed" {
		t.Fatalf("checkout answered %d %v, want 200 completed", code, body)
	}
	if n := z.charges(); n != 0 {
		t.Fatalf("payments was asked to charge %d time(s) for a zero total, want 0 (calls: %v)", n, z.paymentsCalls())
	}
	if got := z.factTypes(); fmt.Sprint(got) != "[order.created order.completed]" {
		t.Fatalf("journal facts = %v, want exactly [order.created order.completed] in that order", got)
	}
	if n := z.lookups(); n != 0 {
		t.Fatalf("a NEW zero-total order made %d operation lookup(s), want 0: a newly inserted order cannot have an operation (D20)", n)
	}
	// Every payments call is a journal submission: nothing else is asked of payments.
	for _, c := range z.paymentsCalls() {
		if !strings.HasPrefix(c, "/internal/facts:") {
			t.Fatalf("payments received %q; a zero-total checkout may only submit facts (calls: %v)", c, z.paymentsCalls())
		}
	}
	// The same completion tail as a paid checkout: the claim is finalized then confirmed.
	if got := z.holdCalls(); fmt.Sprint(got) != "[finalize confirm]" {
		t.Fatalf("inventory calls = %v, want [finalize confirm]", got)
	}
	if got := orderStatus(t, ctx, db, reservation); got != "completed" {
		t.Fatalf("order status = %q, want completed", got)
	}
	if n := owedCompletions(t, ctx, db, reservation); n != 1 {
		t.Fatalf("completion outbox rows = %d, want 1: issuance depends on the event the completion owes", n)
	}
	var amount int64
	var currency string
	if err := db.QueryRowContext(ctx, `
		SELECT f.amount, f.currency FROM order_facts f JOIN orders o ON o.id = f.order_id
		WHERE o.reservation_id=$1 AND f.fact_type='order.completed'`, reservation).Scan(&amount, &currency); err != nil {
		t.Fatal(err)
	}
	if amount != 0 || currency != "EUR" {
		t.Fatalf("order.completed fact carries %d %s, want 0 EUR", amount, currency)
	}
}

// D3. The extracted completion tail keeps its failure arm for BOTH callers: an inventory
// confirm that fails leaves the order `confirmation_pending`, answers 202, and journals NO
// order.completed — the order is not complete, and the recovery runner will finish it.
//
// The paid case is the control the extraction owes: the tail was one block inside
// executeCheckout, existing paid tests exercise the recovery runner and not this arm, and an
// extraction can silently reorder confirm and the fact.
func TestAConfirmFailureLeavesAnOrderPendingWithoutACompletionFact(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total       int64
		wantCharges int
	}{
		{"paid", 2500, 1},
		{"zero total", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			_, reservation := seedCheckoutableAt(t, db, ctx, tc.total)
			z := newZeroStack(t, db, http.StatusInternalServerError)

			code, body := z.checkout(t, reservation, "confirm-fails-"+uuid.NewString())
			if code != http.StatusAccepted || body["status"] != "confirmation_pending" {
				t.Fatalf("checkout answered %d %v, want 202 confirmation_pending", code, body)
			}
			if n := z.charges(); n != tc.wantCharges {
				t.Fatalf("charges = %d, want %d", n, tc.wantCharges)
			}
			for _, f := range z.factTypes() {
				if f == "order.completed" {
					t.Fatalf("order.completed was journalled although inventory never confirmed (facts: %v)", z.factTypes())
				}
			}
			if got := orderStatus(t, ctx, db, reservation); got != "confirmation_pending" {
				t.Fatalf("order status = %q, want confirmation_pending", got)
			}
			if n := owedCompletions(t, ctx, db, reservation); n != 0 {
				t.Fatalf("completion outbox rows = %d, want 0 for an order that is not complete", n)
			}
		})
	}
}

// D20 (TKT-285). A zero-total checkout that RESUMES an existing order is classified by payments'
// operation lookup, exactly as recovery classifies it. The order may be a legacy one whose old
// zero charge bound an operation; skipping the PSP for it would bypass operation evidence.
//
// resumedZeroOrder builds that state through the real handler: a first zero checkout whose confirm
// fails leaves an order behind (confirmation_pending), which the test then moves to `status` (the
// legacy shapes are payment_unknown). The stub's calls are reset before the replay, so every
// assertion below is about the replay alone.
func resumedZeroOrder(t *testing.T, status string) (*zeroStack, *sql.DB, context.Context, uuid.UUID, string) {
	t.Helper()
	db, ctx := exchangeAPIDB(t)
	_, reservation := seedCheckoutableAt(t, db, ctx, 0)
	z := newZeroStack(t, db, http.StatusInternalServerError)
	key := "resume-" + uuid.NewString()
	if code, body := z.checkout(t, reservation, key); code != http.StatusAccepted || body["status"] != "confirmation_pending" {
		t.Fatalf("first checkout answered %d %v, want 202 confirmation_pending", code, body)
	}
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status=$2 WHERE reservation_id=$1`, reservation, status); err != nil {
		t.Fatal(err)
	}
	z.reset()
	return z, db, ctx, reservation, key
}

func TestAResumedZeroOrderWithAnOperationIsNotSkippedOrCompleted(t *testing.T) {
	for _, status := range []string{"payment_unknown", "confirmation_pending"} {
		t.Run(status, func(t *testing.T) {
			z, db, ctx, reservation, key := resumedZeroOrder(t, status)
			z.set(func() { z.operation, z.confirm = http.StatusOK, http.StatusOK })

			code, body := z.checkout(t, reservation, key)
			if code != http.StatusAccepted || body["status"] != "payment_unknown" {
				t.Fatalf("replay answered %d %v, want 202 payment_unknown", code, body)
			}
			if got := z.paymentsCalls(); fmt.Sprint(got) != "[/internal/operations]" {
				t.Fatalf("payments calls = %v, want exactly one operation lookup: no charge, no fact", got)
			}
			if got := z.holdCalls(); len(got) != 0 {
				t.Fatalf("inventory calls = %v, want none: no finalize, no confirm", got)
			}
			if got := orderStatus(t, ctx, db, reservation); got != status {
				t.Fatalf("order status = %q, want it untouched (%q)", got, status)
			}
			if n := owedCompletions(t, ctx, db, reservation); n != 0 {
				t.Fatalf("completion outbox rows = %d, want 0", n)
			}
		})
	}
}

func TestAResumedZeroOrderWithoutAnOperationCompletes(t *testing.T) {
	z, db, ctx, reservation, key := resumedZeroOrder(t, "payment_unknown")
	z.set(func() { z.operation, z.confirm = http.StatusNotFound, http.StatusOK })

	code, body := z.checkout(t, reservation, key)
	if code != http.StatusOK || body["status"] != "completed" {
		t.Fatalf("replay answered %d %v, want 200 completed", code, body)
	}
	if n := z.lookups(); n != 1 {
		t.Fatalf("operation lookups = %d, want 1", n)
	}
	if n := z.charges(); n != 0 {
		t.Fatalf("charges = %d, want 0", n)
	}
	if got := orderStatus(t, ctx, db, reservation); got != "completed" {
		t.Fatalf("order status = %q, want completed", got)
	}
	if n := owedCompletions(t, ctx, db, reservation); n != 1 {
		t.Fatalf("completion outbox rows = %d, want 1", n)
	}
}

// A lookup that fails proves nothing. Not "found", not "not found": the buyer gets the same
// 202 and the row is left exactly as it was.
func TestAResumedZeroOrderWhoseLookupFailsAnswers202AndChangesNothing(t *testing.T) {
	for name, status := range map[string]int{"5xx": http.StatusInternalServerError, "unexpected 4xx": http.StatusTeapot} {
		t.Run(name, func(t *testing.T) {
			z, db, ctx, reservation, key := resumedZeroOrder(t, "payment_unknown")
			z.set(func() { z.operation, z.confirm = status, http.StatusOK })

			code, body := z.checkout(t, reservation, key)
			if code != http.StatusAccepted || body["status"] != "payment_unknown" {
				t.Fatalf("replay answered %d %v, want 202 payment_unknown", code, body)
			}
			if z.charges() != 0 || len(z.holdCalls()) != 0 || len(z.factTypes()) != 0 {
				t.Fatalf("a failed lookup was acted on: payments=%v inventory=%v", z.paymentsCalls(), z.holdCalls())
			}
			if got := orderStatus(t, ctx, db, reservation); got != "payment_unknown" {
				t.Fatalf("order status = %q, want it untouched", got)
			}
		})
	}
}

// A5. The journal refusing a fact aborts the checkout with 503 on BOTH paths, and the same way:
// the order stays `created` and recoverable, never completed. order.created is refused before
// the finalize, so inventory is never called; order.completed is refused after the confirm, so
// the claim is confirmed but the order is not completed. A zero order behaves exactly as a paid
// one does for the same failure.
func TestAJournalRefusingAFactAbortsCheckoutAndLeavesTheOrderRecoverable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int64
		fact  string
		holds string
	}{
		{"paid order.created", 2500, "order.created", "[]"},
		{"zero order.created", 0, "order.created", "[]"},
		{"paid order.completed", 2500, "order.completed", "[finalize confirm]"},
		{"zero order.completed", 0, "order.completed", "[finalize confirm]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			_, reservation := seedCheckoutableAt(t, db, ctx, tc.total)
			z := newZeroStack(t, db, http.StatusOK)
			z.set(func() { z.factStatus = map[string]int{tc.fact: http.StatusServiceUnavailable} })

			code, body := z.checkout(t, reservation, "journal-down-"+uuid.NewString())
			if code != http.StatusServiceUnavailable || body["error"] != "journal unavailable" {
				t.Fatalf("checkout answered %d %v, want 503 journal unavailable", code, body)
			}
			if got := fmt.Sprint(z.holdCalls()); got != tc.holds {
				t.Fatalf("inventory calls = %s, want %s", got, tc.holds)
			}
			if got := orderStatus(t, ctx, db, reservation); got != "created" {
				t.Fatalf("order status = %q, want created (recoverable)", got)
			}
			if n := owedCompletions(t, ctx, db, reservation); n != 0 {
				t.Fatalf("completion outbox rows = %d, want 0", n)
			}
		})
	}
}
