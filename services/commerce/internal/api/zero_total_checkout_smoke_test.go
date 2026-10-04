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
	z := &zeroStack{}
	payments := newCountingStub(t, func(_ *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/internal/facts") {
			entry = "/internal/facts:" + bodyField(r, "fact_type")
		}
		z.mu.Lock()
		z.payments = append(z.payments, entry)
		z.mu.Unlock()
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
		z.mu.Unlock()
		if action == "confirm" && confirmStatus != http.StatusOK {
			w.WriteHeader(confirmStatus)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := newTestServer(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)
	z.h = r
	return z
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
