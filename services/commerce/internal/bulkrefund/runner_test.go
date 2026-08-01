package bulkrefund

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/store"
)

// Event-cancellation bulk refund runner (TKT-159, ADR-040).
//
// The store is a port rather than a *sql.DB for the same reason recovery's is: the question
// these tests answer is "which outcome did the runner choose for this evidence", and that
// is unreadable through SQL side effects alone. The transitions themselves are covered
// against real PostgreSQL by the store's smoke tests.

type fakeOrder struct {
	state    store.OrderCancellationState
	refunded bool // a cancellation refund has been bound for this order
	moved    int  // how many times money ACTUALLY moved — the double-refund detector
	refuse   error
	failVoid bool
	quantity int32
}

type fakeStore struct {
	runs     []store.CancellationRun
	work     []store.CancellationWork
	orders   map[uuid.UUID]*fakeOrder
	fixed    map[uuid.UUID]int32
	final    map[uuid.UUID]store.CancellationOutcome
	abandon  map[uuid.UUID]int
	claims   int
	enumDone bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		orders: map[uuid.UUID]*fakeOrder{}, fixed: map[uuid.UUID]int32{},
		final: map[uuid.UUID]store.CancellationOutcome{}, abandon: map[uuid.UUID]int{},
		enumDone: true,
	}
}

func (f *fakeStore) Runs(context.Context, int) ([]store.CancellationRun, error) { return f.runs, nil }
func (f *fakeStore) Enumerate(context.Context, uuid.UUID, uuid.UUID, int) (bool, error) {
	return f.enumDone, nil
}

func (f *fakeStore) Claim(_ context.Context, limit int, _ time.Duration) ([]store.CancellationWork, error) {
	f.claims++
	var out []store.CancellationWork
	for _, w := range f.work {
		if _, done := f.final[w.OrderID]; done {
			continue
		}
		if len(out) == limit {
			break
		}
		if q, ok := f.fixed[w.OrderID]; ok {
			w.RequestedQuantity = sql.NullInt32{Int32: q, Valid: true}
		}
		w.ClaimID = uuid.New()
		out = append(out, w)
	}
	return out, nil
}

func (f *fakeStore) OrderState(_ context.Context, _, order uuid.UUID) (store.OrderCancellationState, error) {
	o, ok := f.orders[order]
	if !ok {
		return store.OrderCancellationState{}, errors.New("no such order")
	}
	return o.state, nil
}

func (f *fakeStore) LookupRefund(_ context.Context, _, _ uuid.UUID) (store.Refund, bool, error) {
	return store.Refund{}, false, nil
}

func (f *fakeStore) FixQuantity(_ context.Context, w store.CancellationWork, q int32) error {
	f.fixed[w.OrderID] = q
	return nil
}

func (f *fakeStore) Finalize(_ context.Context, w store.CancellationWork, out store.CancellationOutcome) error {
	if _, done := f.final[w.OrderID]; done {
		return store.ErrCancellationClaimLost
	}
	f.final[w.OrderID] = out
	return nil
}

func (f *fakeStore) Abandon(_ context.Context, w store.CancellationWork) error {
	f.abandon[w.OrderID]++
	return nil
}
func (f *fakeStore) CompleteRuns(context.Context) (int, error) { return 0, nil }

// fakeRefunder records the idempotency key every attempt used, per order — the thing that
// decides whether a resumed run refunds twice or converges.
type fakeRefunder struct {
	store *fakeStore
	keys  map[uuid.UUID][]string
}

func newFakeRefunder(f *fakeStore) *fakeRefunder {
	return &fakeRefunder{store: f, keys: map[uuid.UUID][]string{}}
}

func (f *fakeRefunder) Refund(_ context.Context, in store.RefundRequest) (refunds.Result, error) {
	f.keys[in.OrderID] = append(f.keys[in.OrderID], in.IdempotencyKey)
	o, ok := f.store.orders[in.OrderID]
	if !ok {
		return refunds.Result{}, errors.New("no such order")
	}
	if o.refuse != nil {
		return refunds.Result{}, o.refuse
	}
	replay := o.refunded
	if !o.refunded {
		o.refunded = true
		o.moved++
		o.state.RefundedQuantity += in.Quantity
		o.state.RefundStatus = "full"
		o.quantity = in.Quantity
	}
	if o.failVoid {
		o.state.OutstandingRefunds = []uuid.UUID{uuid.New()}
	} else {
		o.state.OutstandingRefunds = nil
	}
	return refunds.Result{Refund: store.Refund{OrderID: in.OrderID, Quantity: in.Quantity}, Replay: replay}, nil
}

func (f *fakeRefunder) DriveReversal(_ context.Context, r store.Refund) store.Refund { return r }

func work(order uuid.UUID) store.CancellationWork {
	return store.CancellationWork{
		OrganizerID: uuid.New(), RunID: uuid.New(), OrderID: order,
		SlotID: uuid.New(), ClaimID: uuid.New(), Currency: "EUR",
	}
}

func completedOrder(sold int32, unit int64) store.OrderCancellationState {
	return store.OrderCancellationState{
		SoldQuantity: sold, UnitAmount: unit, Currency: "EUR",
		OrderStatus: "completed", RefundStatus: "none",
	}
}

func runnerFor(f *fakeStore, r *fakeRefunder) *Runner {
	return New(f, r, time.Minute, 10, time.Minute)
}

// AC 1 + AC 2: every non-full order is refunded for exactly its remaining quantity, and an
// order that is already full makes NO provider call and reports already_refunded.
func TestRunOnceRefundsEveryNonFullOrderExactlyOnce(t *testing.T) {
	f := newFakeStore()
	fresh, partial, full := uuid.New(), uuid.New(), uuid.New()

	f.orders[fresh] = &fakeOrder{state: completedOrder(2, 1000)}
	partialState := completedOrder(3, 1000)
	partialState.RefundedQuantity, partialState.RefundStatus = 1, "partial"
	f.orders[partial] = &fakeOrder{state: partialState}
	fullState := completedOrder(2, 1000)
	fullState.RefundedQuantity, fullState.RefundStatus = 2, "full"
	f.orders[full] = &fakeOrder{state: fullState}

	for _, id := range []uuid.UUID{fresh, partial, full} {
		f.work = append(f.work, work(id))
	}
	r := newFakeRefunder(f)
	runnerFor(f, r).RunOnce(context.Background())

	if got := f.final[fresh].Outcome; got != "refunded" {
		t.Fatalf("fresh order outcome = %q, want refunded", got)
	}
	if got := f.final[partial].Outcome; got != "refunded" {
		t.Fatalf("partially refunded order outcome = %q, want refunded", got)
	}
	if got := f.final[full].Outcome; got != "already_refunded" {
		t.Fatalf("already-full order outcome = %q, want already_refunded", got)
	}
	if f.orders[full].moved != 0 {
		t.Fatal("an already fully refunded order must not reach the provider")
	}
	// The remaining quantity, not the sold quantity: 3 sold with 1 already refunded is 2.
	if f.orders[partial].quantity != 2 {
		t.Fatalf("partial order refunded %d, want its remaining 2", f.orders[partial].quantity)
	}
	if f.fixed[fresh] != 2 {
		t.Fatalf("fixed quantity = %d, want 2 persisted BEFORE the provider call", f.fixed[fresh])
	}
}

// AC 3: one order failing does not abort the run. The failure is recorded with a bounded
// reason and every later order still runs.
func TestRunOnceRecordsFailureAndContinuesTheBatch(t *testing.T) {
	f := newFakeStore()
	first, bad, last := uuid.New(), uuid.New(), uuid.New()
	f.orders[first] = &fakeOrder{state: completedOrder(1, 1000)}
	f.orders[bad] = &fakeOrder{state: completedOrder(1, 1000), refuse: refunds.ErrPaymentsRefused}
	f.orders[last] = &fakeOrder{state: completedOrder(1, 1000)}
	for _, id := range []uuid.UUID{first, bad, last} {
		f.work = append(f.work, work(id))
	}
	r := newFakeRefunder(f)
	runnerFor(f, r).RunOnce(context.Background())

	if f.final[first].Outcome != "refunded" || f.final[last].Outcome != "refunded" {
		t.Fatalf("a failure aborted the batch: first=%q last=%q", f.final[first].Outcome, f.final[last].Outcome)
	}
	got := f.final[bad]
	if got.Outcome != "failed" || got.FailureCode != "refund_refused" {
		t.Fatalf("refused order = %+v, want failed/refund_refused", got)
	}
	if got.FailureReason == "" || len(got.FailureReason) > 500 {
		t.Fatalf("failure reason %q must be present and bounded", got.FailureReason)
	}
}

// ADR-039: money back with the reversal still outstanding is NOT a success. It is `failed`
// with the truth about which half is missing — never rounded up to `refunded`.
func TestMoneyBackWithOutstandingReversalIsNotASuccess(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), failVoid: true}
	f.work = append(f.work, work(order))
	r := newFakeRefunder(f)
	runnerFor(f, r).RunOnce(context.Background())

	got := f.final[order]
	if got.Outcome != "failed" || got.FailureCode != "reversal_outstanding" {
		t.Fatalf("outcome = %+v, want failed/reversal_outstanding", got)
	}
	if !got.MoneyRefunded {
		t.Fatal("the money DID move — the report must say so even though the outcome failed")
	}
}

// A5: a zero-price (comped) order has no money leg to refund. It is recorded visibly
// rather than skipped, because it also receives no reversal at all.
func TestZeroPriceOrderIsRecordedRatherThanSkipped(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 0)}
	f.work = append(f.work, work(order))
	r := newFakeRefunder(f)
	runnerFor(f, r).RunOnce(context.Background())

	got := f.final[order]
	if got.Outcome != "failed" || got.FailureCode != "no_captured_money" {
		t.Fatalf("comped order = %+v, want failed/no_captured_money", got)
	}
	if f.orders[order].moved != 0 {
		t.Fatal("a zero-price order must not reach the provider")
	}
}

// AC 4: an interrupted run resumes without skipping an order and without refunding one
// twice. The interrupted order's claim is abandoned rather than finalized — context
// cancellation is an interruption, never a business verdict — and the successor reuses the
// SAME idempotency key, which is what makes the retry converge instead of double-refunding.
func TestInterruptedRunResumesWithoutSkipOrDuplicate(t *testing.T) {
	f := newFakeStore()
	orders := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for _, id := range orders {
		f.orders[id] = &fakeOrder{state: completedOrder(1, 1000)}
		f.work = append(f.work, work(id))
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := newFakeRefunder(f)
	// Cancel after the first order is done: the second is claimed but must not be
	// finalized from a cancelled context.
	stop := &cancelAfter{n: 1, cancel: cancel, inner: r}
	New(f, stop, time.Minute, 10, time.Minute).RunOnce(ctx)

	if len(f.final) == len(orders) {
		t.Fatal("the run finished despite being cancelled — the interruption was not honoured")
	}
	interrupted := len(f.final)
	for id := range f.final {
		if f.final[id].Outcome == "failed" && f.final[id].FailureCode == "internal" {
			t.Fatal("an interruption was finalized as a business failure; it must stay reclaimable")
		}
	}
	if f.abandon[orders[1]] == 0 && interrupted > 1 {
		t.Fatal("the interrupted order was neither abandoned nor left unclaimed")
	}

	// A successor runner picks up what is left. It shares the fake store, so it sees the
	// same durable state a restarted process would.
	successor := newFakeRefunder(f)
	New(f, successor, time.Minute, 10, time.Minute).RunOnce(context.Background())

	if len(f.final) != len(orders) {
		t.Fatalf("after resume %d/%d orders are terminal — one was skipped", len(f.final), len(orders))
	}
	for _, id := range orders {
		if f.orders[id].moved != 1 {
			t.Fatalf("order %s moved money %d times, want exactly 1", id, f.orders[id].moved)
		}
		// The convergence property itself: whichever runner attempted this order, and
		// however many times, every attempt used ONE key — because the key derives from
		// (slot, order) and carries no run and no attempt counter in it. A run-scoped key
		// would show two distinct keys here and would bind a second refund in production.
		attempts := append(append([]string{}, r.keys[id]...), successor.keys[id]...)
		for _, k := range attempts {
			if k != attempts[0] {
				t.Fatalf("order %s was attempted under two different keys (%q, %q): a retry would bind a second refund", id, attempts[0], k)
			}
		}
	}
}

// cancelAfter cancels the run's context once n orders have been refunded, so the runner is
// interrupted mid-book rather than between books.
type cancelAfter struct {
	n      int
	done   int
	cancel context.CancelFunc
	inner  *fakeRefunder
}

func (c *cancelAfter) Refund(ctx context.Context, in store.RefundRequest) (refunds.Result, error) {
	if c.done >= c.n {
		c.cancel()
		return refunds.Result{}, context.Canceled
	}
	c.done++
	return c.inner.Refund(ctx, in)
}

func (c *cancelAfter) DriveReversal(ctx context.Context, r store.Refund) store.Refund {
	return c.inner.DriveReversal(ctx, r)
}

// AC 6: the claim is batch-bounded, and the runner keeps claiming until the book is drained
// rather than processing one batch and stopping.
func TestRunOnceDrainsTheBookInBatches(t *testing.T) {
	f := newFakeStore()
	for range 5 {
		id := uuid.New()
		f.orders[id] = &fakeOrder{state: completedOrder(1, 1000)}
		f.work = append(f.work, work(id))
	}
	r := newFakeRefunder(f)
	New(f, r, time.Minute, 2, time.Minute).RunOnce(context.Background())

	if len(f.final) != 5 {
		t.Fatalf("%d/5 orders terminal — the runner stopped after one batch", len(f.final))
	}
	if f.claims < 3 {
		t.Fatalf("%d claims for 5 orders at a batch of 2 — the claim is not batch-bounded", f.claims)
	}
}
