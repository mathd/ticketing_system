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
	state        store.OrderCancellationState
	refunded     bool // a cancellation refund has been bound for this order
	moved        int  // how many times money ACTUALLY moved — the double-refund detector
	voided       bool // a comped void has been bound for this order (TKT-171)
	voidCalls    int  // how many times Void was called — the double-void detector
	voidRefuse   error
	refuse       error
	failVoid     bool
	failCapacity bool
	quantity     int32
}

type fakeStore struct {
	runs                []store.CancellationRun
	work                []store.CancellationWork
	orders              map[uuid.UUID]*fakeOrder
	fixed               map[uuid.UUID]int32
	final               map[uuid.UUID]store.CancellationOutcome
	abandon             map[uuid.UUID]int
	cleared             map[uuid.UUID]int
	prior               map[uuid.UUID]bool
	chargeFailures      int
	chargeCommitsAnyway bool
	attempts            map[uuid.UUID]int
	// leased models the real lease: a claimed row is NOT claimable again until it is
	// abandoned or its lease expires. Without this the fake re-claims instantly and a
	// retry budget meant to be spread over lease-length intervals burns inside one pass —
	// which is exactly the bug the first version of the retry fix had.
	leased   map[uuid.UUID]bool
	claims   int
	enumDone bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		orders: map[uuid.UUID]*fakeOrder{}, fixed: map[uuid.UUID]int32{},
		final: map[uuid.UUID]store.CancellationOutcome{}, abandon: map[uuid.UUID]int{},
		cleared: map[uuid.UUID]int{}, attempts: map[uuid.UUID]int{}, leased: map[uuid.UUID]bool{},
		prior:    map[uuid.UUID]bool{},
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
		if f.leased[w.OrderID] {
			continue
		}
		if len(out) == limit {
			break
		}
		if q, ok := f.fixed[w.OrderID]; ok {
			w.RequestedQuantity = sql.NullInt32{Int32: q, Valid: true}
		}
		w.PriorRun = f.prior[w.OrderID]
		// The claim does NOT charge (TKT-300), and Attempts is the count already charged,
		// i.e. not counting the attempt this claim is about to make. Mirroring the store
		// matters here: a fake that still charged at claim would make every post-drive
		// assertion agree with itself and prove nothing about the shipped SQL.
		w.Attempts = f.attempts[w.OrderID]
		w.ClaimID = uuid.New()
		f.leased[w.OrderID] = true
		out = append(out, w)
	}
	return out, nil
}

func (f *fakeStore) OrderState(_ context.Context, _, order, _ uuid.UUID) (store.OrderCancellationState, error) {
	o, ok := f.orders[order]
	if !ok {
		return store.OrderCancellationState{}, errors.New("no such order")
	}
	return o.state, nil
}

// LookupRefund answers from the refunds the fake refunder has actually bound, keyed the
// way the real store keys them. The first version of this fake always answered "not
// found", which made the SECOND-run path unreachable — and that path shipped two defects
// the whole-stack suite had to find: a `refunded` verdict with no persisted quantity (the
// database rejects it, the finalize fails, and the run never completes) and a repeat run
// reporting `refunded` instead of `already_refunded`. A fake that cannot express a state
// cannot test it.
func (f *fakeStore) LookupRefund(_ context.Context, org, refundID uuid.UUID) (store.Refund, bool, error) {
	for order, o := range f.orders {
		if !o.refunded {
			continue
		}
		if store.RefundID(org, store.CancellationRefundKey(f.slotOf(order), order)) == refundID {
			return store.Refund{ID: refundID, OrderID: order, Quantity: o.quantity}, true, nil
		}
	}
	return store.Refund{}, false, nil
}

func (f *fakeStore) slotOf(order uuid.UUID) uuid.UUID {
	for _, w := range f.work {
		if w.OrderID == order {
			return w.SlotID
		}
	}
	return uuid.Nil
}

func (f *fakeStore) FixQuantity(_ context.Context, w store.CancellationWork, q int32, priorRun bool) error {
	f.fixed[w.OrderID] = q
	f.prior[w.OrderID] = priorRun
	return nil
}

func (f *fakeStore) ClearQuantity(_ context.Context, w store.CancellationWork) error {
	delete(f.fixed, w.OrderID)
	delete(f.prior, w.OrderID)
	f.cleared[w.OrderID]++
	return nil
}

func (f *fakeStore) Finalize(_ context.Context, w store.CancellationWork, out store.CancellationOutcome, charge bool) error {
	if _, done := f.final[w.OrderID]; done {
		return store.ErrCancellationClaimLost
	}
	f.final[w.OrderID] = out
	delete(f.leased, w.OrderID)
	// Mirrors the store: a verdict that ended a RETRYABLE attempt is charged; a definite
	// refusal is terminal on its first attempt and never consumes the budget.
	if charge {
		f.attempts[w.OrderID]++
	}
	return nil
}

// Abandon REFUSES on a dead context, as a driver does. Without that the fake cannot tell
// whether the hand-back ran on the caller's dying context or the detached one, and a test
// asserting the attempt survived a shutdown would pass either way.
func (f *fakeStore) Abandon(ctx context.Context, w store.CancellationWork, charge bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.abandon[w.OrderID]++
	delete(f.leased, w.OrderID)
	// Mirrors the store: only a DRIVEN claim costs an attempt.
	if charge {
		f.attempts[w.OrderID]++
	}
	return nil
}

// ChargeAttempt mirrors the store's charge-without-releasing: a retryable failure keeps
// its lease, because the lease is the backoff.
// ChargeAttempt mirrors a real driver in two ways that matter: it REFUSES on a dead
// context, so the fake can tell whether the runner charged on the caller's context or a
// detached one, and it can be made to fail a bounded number of times, which is how a
// transient database error is expressed.
func (f *fakeStore) ChargeAttempt(ctx context.Context, w store.CancellationWork) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Mirrors the store's COMPARE-AND-SET: the write applies only while the row still
	// holds the count this claimant observed. A fake that incremented unconditionally
	// would make the retry look safe whatever the SQL did.
	if f.attempts[w.OrderID] != w.Attempts {
		return nil
	}
	if f.chargeFailures > 0 {
		f.chargeFailures--
		if f.chargeCommitsAnyway {
			// The write COMMITTED and its acknowledgement was lost -- the case the
			// compare-and-set exists for.
			f.attempts[w.OrderID]++
		}
		return errors.New("charge write failed")
	}
	f.attempts[w.OrderID]++
	return nil
}

// expireLeases is the passage of time between runner passes.
func (f *fakeStore) expireLeases()                             { f.leased = map[uuid.UUID]bool{} }
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
		// Voiding failed, so the capacity return never runs either (ADR-038 §1 ordering).
		o.state.VoidingOutstanding, o.state.CapacityOutstanding = 1, 1
	} else if o.failCapacity {
		// The seated case: the tickets ARE void, only the seat did not come back. The
		// report has to distinguish this from "we did not get to either".
		o.state.VoidingOutstanding, o.state.CapacityOutstanding = 0, 1
	} else {
		o.state.VoidingOutstanding, o.state.CapacityOutstanding = 0, 0
	}
	return refunds.Result{Refund: store.Refund{OrderID: in.OrderID, Quantity: in.Quantity}, Replay: replay}, nil
}

func (f *fakeRefunder) DriveReversal(_ context.Context, r store.Refund) store.Refund { return r }

// Void is the comped reversal (TKT-171). It records that it ran and, critically,
// NEVER touches `moved` — so the double-refund detector every money assertion in
// this file already rests on doubles as the proof that a void moved no money.
func (f *fakeRefunder) Void(_ context.Context, in store.VoidRequest) (refunds.VoidResult, error) {
	f.keys[in.OrderID] = append(f.keys[in.OrderID], in.IdempotencyKey)
	o, ok := f.store.orders[in.OrderID]
	if !ok {
		return refunds.VoidResult{}, errors.New("no such order")
	}
	if o.voidRefuse != nil {
		return refunds.VoidResult{}, o.voidRefuse
	}
	replay := o.voided
	o.voided = true
	o.voidCalls++
	return refunds.VoidResult{
		Void: store.OrderVoid{
			ID: store.VoidID(in.OrganizerID, in.OrderID), OrderID: in.OrderID,
			OrganizerID: in.OrganizerID, Quantity: o.state.SoldQuantity,
			TicketsVoided: true, CapacityReturned: true,
		},
		Replay: replay,
	}, nil
}

func work(order uuid.UUID) store.CancellationWork {
	return store.CancellationWork{
		OrganizerID: uuid.New(), RunID: uuid.New(), OrderID: order,
		SlotID: uuid.New(), ClaimID: uuid.New(), Currency: "EUR",
	}
}

// completedOrder builds a COHERENT order state: total = quantity × unit, which is
// what a sale with no passed-on fees produces. Coherent on purpose — the void's
// eligibility turns on BOTH numbers since ai-review F1, so a helper that left
// TotalAmount at its zero value would make every paid fixture look like it
// captured nothing and route it to the void.
//
// compedWithFees below is the incoherent-looking-but-real case that must NOT be
// voided; it is built explicitly so the difference is visible at the call site.
func completedOrder(sold int32, unit int64) store.OrderCancellationState {
	return store.OrderCancellationState{
		SoldQuantity: sold, UnitAmount: unit, TotalAmount: int64(sold) * unit, Currency: "EUR",
		OrderStatus: "completed", RefundStatus: "none",
	}
}

// compedWithFees is a zero-FACE order that nonetheless captured money — a comped
// ticket carrying a passed-on fee (`total = face + passed_on`, migration 0014).
func compedWithFees(sold int32, total int64) store.OrderCancellationState {
	s := completedOrder(sold, 0)
	s.TotalAmount = total
	return s
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
	// An outstanding reversal is RETRYABLE — the obligation may still discharge — so the
	// verdict that sticks is the one after the attempt budget is spent.
	for range maxAttempts + 1 {
		f.expireLeases()
		runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())
	}

	got := f.final[order]
	if got.Outcome != "failed" || got.FailureCode != "reversal_outstanding" {
		t.Fatalf("outcome = %+v, want failed/reversal_outstanding", got)
	}
	if !got.MoneyRefunded {
		t.Fatal("the money DID move — the report must say so even though the outcome failed")
	}
}

// TKT-171: a zero-price (comped) order has no money leg to refund — and is
// REVERSED anyway, through the void.
//
// This test used to assert `failed/no_captured_money`, which was the honest report
// of a real gap: the order got no reversal at all, so its tickets kept admitting
// and its seat stayed sold. That gap is what TKT-171 closes, so the assertion is
// reconciled here rather than deleted — a deleted test would have left nothing
// saying what a comped order in a cancellation run is supposed to do.
//
// `moved` is the file's existing double-refund detector, and it does the second
// job here: a void that reached the provider would move it. Zero is the assertion
// that a void moves no money, and it is derived from the rule ("a void moves
// tickets and capacity and never money") rather than from what the code did.
func TestZeroPriceOrderIsVoidedNotRefunded(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 0)}
	f.work = append(f.work, work(order))
	r := newFakeRefunder(f)
	runnerFor(f, r).RunOnce(context.Background())

	got := f.final[order]
	if got.Outcome != "voided" {
		t.Fatalf("comped order = %+v, want outcome voided", got)
	}
	// The three flags separately, because that is the whole reason the outcome
	// carries three of them: `voided` is a COMPLETE reversal that moved no money,
	// and reporting it as `refunded` would tell an operator money went back.
	if got.MoneyRefunded {
		t.Error("a void must not report money as refunded")
	}
	if !got.TicketsVoided {
		t.Error("a void must void the tickets — that is the half that stops admission")
	}
	if !got.CapacityReturned {
		t.Error("a void must return the capacity — that is the half that gives the seat back")
	}
	if f.orders[order].moved != 0 {
		t.Fatal("a zero-price order must not reach the provider")
	}
	if f.orders[order].voidCalls != 1 {
		t.Fatalf("void calls = %d, want exactly 1", f.orders[order].voidCalls)
	}
}

// The other side of the branch: two shapes that are NOT comped orders and must not
// be voided, each with its own case because each would be admitted by a different
// sloppy predicate.
//
// Without these the branch could be written on UnitAmount alone, or as `<= 0`, and
// every test above would still pass — which is exactly how a void comes to reverse
// an order that captured money, reporting success.
func TestNonCompedOrdersAreReportedNotVoided(t *testing.T) {
	for name, tc := range map[string]struct {
		state store.OrderCancellationState
		why   string
	}{
		// ai-review F1. Face 0, total 600: a comped ticket with a passed-on fee.
		// Real money the buyer paid. Voiding it would return the tickets and the
		// seat and keep the fee. Delete `&& state.TotalAmount == 0` from the void
		// branch and this is the case that goes red.
		"zero face with captured fees": {compedWithFees(2, 600),
			"a zero FACE with a captured TOTAL is not a comped order"},
		// Corrupt data — unreachable through any supported write. Voiding it would
		// hide the corruption behind a successful-looking reversal.
		"negative unit amount": {completedOrder(1, -100),
			"corrupt data must not be reversed as though it were comped"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeStore()
			order := uuid.New()
			f.orders[order] = &fakeOrder{state: tc.state}
			f.work = append(f.work, work(order))
			r := newFakeRefunder(f)
			runnerFor(f, r).RunOnce(context.Background())

			got := f.final[order]
			if got.Outcome != "failed" || got.FailureCode != "no_captured_money" {
				t.Fatalf("%s: outcome = %+v, want failed/no_captured_money", tc.why, got)
			}
			if f.orders[order].voidCalls != 0 {
				t.Fatalf("%s: it was voided anyway", tc.why)
			}
			if f.orders[order].moved != 0 {
				t.Fatalf("%s: it reached the provider", tc.why)
			}
		})
	}
}

// A paid order must still take the REFUND path. Without this, routing everything
// through the void would pass every comped assertion above.
func TestPaidOrderStillTakesTheRefundPath(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(2, 1500)}
	f.work = append(f.work, work(order))
	r := newFakeRefunder(f)
	runnerFor(f, r).RunOnce(context.Background())

	got := f.final[order]
	if got.Outcome != "refunded" {
		t.Fatalf("paid order = %+v, want outcome refunded", got)
	}
	if !got.MoneyRefunded {
		t.Error("a paid order's reversal moves money and must report it")
	}
	if f.orders[order].moved != 1 {
		t.Fatalf("money movements = %d, want exactly 1", f.orders[order].moved)
	}
	if f.orders[order].voidCalls != 0 {
		t.Fatal("a paid order must not be voided — it has money to return")
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

func (c *cancelAfter) Void(ctx context.Context, in store.VoidRequest) (refunds.VoidResult, error) {
	return c.inner.Void(ctx, in)
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

// AC 2 at the unit seam: a SECOND run over a book the first run already refunded must
// report every order `already_refunded`, move no money, and — the part the database cares
// about — persist a requested quantity, because a `refunded`/`already_refunded` verdict
// with none is rejected by cancellation_refund_orders_refunded_has_refund. That rejection
// fails the FINALIZE, not the refund: the row stays pending and its run never completes,
// which is how this reached the whole-stack suite instead of dying here.
func TestSecondRunOverARefundedBookReportsAlreadyRefunded(t *testing.T) {
	f := newFakeStore()
	orders := []uuid.UUID{uuid.New(), uuid.New()}
	for _, id := range orders {
		f.orders[id] = &fakeOrder{state: completedOrder(2, 1000)}
		f.work = append(f.work, work(id))
	}
	first := newFakeRefunder(f)
	runnerFor(f, first).RunOnce(context.Background())
	for _, id := range orders {
		if f.final[id].Outcome != "refunded" {
			t.Fatalf("first run outcome for %s = %q, want refunded", id, f.final[id].Outcome)
		}
	}

	// A second run means fresh ledger rows: no outcome, no fixed quantity. The orders and
	// their refunds are the durable state that carries over.
	f.final = map[uuid.UUID]store.CancellationOutcome{}
	f.fixed = map[uuid.UUID]int32{}
	f.prior = map[uuid.UUID]bool{}
	second := newFakeRefunder(f)
	runnerFor(f, second).RunOnce(context.Background())

	for _, id := range orders {
		got := f.final[id]
		if got.Outcome != "already_refunded" {
			t.Fatalf("second run outcome for %s = %q (%s/%s), want already_refunded",
				id, got.Outcome, got.FailureCode, got.FailureReason)
		}
		if f.orders[id].moved != 1 {
			t.Fatalf("order %s moved money %d times across two runs, want exactly 1", id, f.orders[id].moved)
		}
		if f.fixed[id] == 0 {
			t.Fatalf("order %s reached a terminal success with no persisted quantity: the database refuses that row and the run never completes", id)
		}
		// And it converged on ONE refund identity, which is the whole point of a
		// run-independent key.
		if len(second.keys[id]) > 0 && len(first.keys[id]) > 0 && second.keys[id][0] != first.keys[id][0] {
			t.Fatalf("order %s used key %q on the second run and %q on the first", id, second.keys[id][0], first.keys[id][0])
		}
	}
}

// Review finding 1: an AMBIGUOUS failure — the money may or may not have moved — must not be
// terminal on the first attempt. Finalizing it strands money with the tickets still valid and
// nothing driving the reversal, and the run then completes over the top of it.
func TestAmbiguousFailureIsRetriedNotFinalized(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), refuse: refunds.ErrPaymentsUnresolved}
	f.work = append(f.work, work(order))

	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())

	if _, terminal := f.final[order]; terminal {
		t.Fatalf("an ambiguous payments failure was finalized on attempt 1: %+v", f.final[order])
	}
	if f.abandon[order] != 0 {
		t.Fatal("a retryable failure released its lease: the next claim re-drives it immediately, " +
			"burning the whole attempt budget in one pass instead of spacing it over the lease")
	}
	// But it does not retry forever: the run has to be able to complete.
	for range maxAttempts + 2 {
		f.expireLeases()
		runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())
	}
	got, terminal := f.final[order]
	if !terminal {
		t.Fatalf("still not terminal after %d attempts — the run can never complete", f.attempts[order])
	}
	if got.Outcome != "failed" || got.FailureCode != "unavailable" {
		t.Fatalf("final outcome = %+v, want failed/unavailable", got)
	}
}

// A DEFINITE refusal is terminal immediately and never consumes a retry: retrying it just
// burns the book.
func TestDefiniteRefusalIsTerminalOnTheFirstAttempt(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), refuse: refunds.ErrPaymentsRefused}
	f.work = append(f.work, work(order))

	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())

	got, terminal := f.final[order]
	if !terminal || got.FailureCode != "refund_refused" {
		t.Fatalf("outcome = %+v (terminal=%v), want an immediate failed/refund_refused", got, terminal)
	}
	if f.attempts[order] != 1 {
		t.Fatalf("a definite refusal consumed %d attempts, want 1", f.attempts[order])
	}
}

// Review finding 2: the ceiling can move between reading the remainder and binding — a staff
// refund lands in between. The fixed quantity is then wrong forever, so it must be CLEARED
// and recomputed rather than stranding a refundable order on a stale number.
func TestCeilingMovingUnderTheRunnerClearsTheFixedQuantity(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(10, 1000), refuse: store.ErrRefundExceedsOrder}
	f.work = append(f.work, work(order))

	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())

	if f.cleared[order] == 0 {
		t.Fatal("the stale quantity was not cleared — every retry would fail the same way")
	}
	if q, ok := f.fixed[order]; ok {
		t.Fatalf("quantity still fixed at %d after the ceiling moved", q)
	}
	// And the row must NOT be terminal: clearing the quantity only helps if something
	// recomputes it. The first version of this fix cleared it and finalized anyway, which
	// stranded exactly the refundable tickets the clear was meant to rescue.
	if got, terminal := f.final[order]; terminal {
		t.Fatalf("a moved ceiling was finalized as %+v — nothing will ever recompute the quantity", got)
	}

	// Once the ceiling stops moving, the recomputed attempt succeeds.
	f.orders[order].refuse = nil
	f.expireLeases()
	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())
	if got := f.final[order].Outcome; got != "refunded" {
		t.Fatalf("after the ceiling settled the order = %q, want refunded", got)
	}
}

// Only a DRIVEN claim costs an attempt. Since TKT-300 the charge follows the drive rather
// than the claim, so this is an omission rather than the refund it used to be — but the
// distinction it protects is unchanged and still matters in both directions. A claim
// released without the refund unit ever running must cost nothing, or a shutdown between
// claim and work leaves a row arriving at its first real ambiguous failure with budget
// spent on work that never happened. And a claim released AFTER the refund unit ran must
// cost its attempt, or a cancellation window recurring at exactly that point holds the row
// below the cap forever, so the run never completes and its report never becomes readable.
//
// The name is kept: what it asserts is the same partition, read from the other side.
func TestOnlyAnUndrivenClaimGetsItsAttemptBack(t *testing.T) {
	f := newFakeStore()
	driven, undriven := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{driven, undriven} {
		f.orders[id] = &fakeOrder{state: completedOrder(1, 1000)}
		f.work = append(f.work, work(id))
	}

	// Cancel the instant the FIRST order's refund succeeds: that claim was driven, and the
	// second is then released at the top of the loop, before the refund unit is ever called
	// for it.
	ctx, cancel := context.WithCancel(context.Background())
	stop := &cancelAfterSuccess{cancel: cancel, inner: newFakeRefunder(f)}
	New(f, stop, time.Minute, 10, time.Minute).RunOnce(ctx)

	if f.abandon[undriven] == 0 {
		t.Fatal("the undriven claim was not released")
	}
	if f.attempts[undriven] != 0 {
		t.Fatalf("undriven claim kept %d attempt(s); the charge must come back", f.attempts[undriven])
	}
	if f.attempts[driven] == 0 && f.abandon[driven] > 0 {
		t.Fatal("a claim released after the refund unit ran got its charge back; a recurring " +
			"cancellation there would hold the row below the cap forever")
	}
}

// Review pass 2, finding 8: an outstanding reversal on a refund this run does NOT own
// cannot be repaired by retrying — the row does not carry that refund's idempotency key.
// Retrying it is five reads and a wait, so it is terminal at once.
func TestForeignOutstandingReversalIsTerminalNotRetried(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	// Fully refunded by staff already, with an obligation still outstanding.
	st := completedOrder(2, 1000)
	st.RefundedQuantity, st.RefundStatus = 2, "full"
	st.CapacityOutstanding = 1
	f.orders[order] = &fakeOrder{state: st}
	f.work = append(f.work, work(order))

	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())

	got, terminal := f.final[order]
	if !terminal {
		t.Fatal("a foreign outstanding reversal was left for retry; nothing will ever repair it")
	}
	if got.FailureCode != "reversal_outstanding" {
		t.Fatalf("failure code = %q, want reversal_outstanding", got.FailureCode)
	}
	if f.attempts[order] != 1 {
		t.Fatalf("it consumed %d attempts, want 1", f.attempts[order])
	}
}

// Review finding 4: the two obligations are independent. Voiding can succeed while the
// capacity return fails — the ordinary seated case — and a report that says both are
// outstanding sends the operator after work that is already done.
func TestObligationsAreReportedSeparately(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), failCapacity: true}
	f.work = append(f.work, work(order))

	// Drive past the retry budget: reversal_outstanding is retryable, and the verdict that
	// sticks is the one after the attempts are spent.
	for range maxAttempts + 1 {
		f.expireLeases()
		runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())
	}
	got := f.final[order]
	if got.Outcome != "failed" || got.FailureCode != "reversal_outstanding" {
		t.Fatalf("outcome = %+v, want failed/reversal_outstanding", got)
	}
	if !got.TicketsVoided {
		t.Fatal("tickets WERE voided; reporting otherwise sends the operator after work already done")
	}
	if got.CapacityReturned {
		t.Fatal("the capacity did not come back; the report must say so")
	}
}

// Review finding 6: a shutdown between a successful refund and its classification must not
// commit a permanent `failed` for an order that is in fact fully discharged.
func TestCancellationDuringClassificationIsNotAVerdict(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(1, 1000)}
	f.work = append(f.work, work(order))

	ctx, cancel := context.WithCancel(context.Background())
	r := newFakeRefunder(f)
	// Cancel the moment the refund itself has succeeded, i.e. before classification.
	New(f, &cancelAfterSuccess{cancel: cancel, inner: r}, time.Minute, 10, time.Minute).RunOnce(ctx)

	if got, terminal := f.final[order]; terminal && got.Outcome == "failed" {
		t.Fatalf("a shutdown after a successful refund was committed as %+v", got)
	}
	if f.orders[order].moved != 1 {
		t.Fatalf("money moved %d times, want 1", f.orders[order].moved)
	}
}

type cancelAfterSuccess struct {
	cancel context.CancelFunc
	inner  *fakeRefunder
}

func (c *cancelAfterSuccess) Refund(ctx context.Context, in store.RefundRequest) (refunds.Result, error) {
	out, err := c.inner.Refund(ctx, in)
	c.cancel()
	return out, err
}

func (c *cancelAfterSuccess) DriveReversal(ctx context.Context, r store.Refund) store.Refund {
	return c.inner.DriveReversal(ctx, r)
}

func (c *cancelAfterSuccess) Void(ctx context.Context, in store.VoidRequest) (refunds.VoidResult, error) {
	return c.inner.Void(ctx, in)
}

// A second run whose FIRST attempt was interrupted must still report already_refunded when it
// resumes. The attribution is read back from the row, because a resumed attempt cannot
// re-derive it: a lookup then cannot tell "a previous run refunded this" from "this run did,
// before it was interrupted".
func TestResumedSecondRunStillReportsAlreadyRefunded(t *testing.T) {
	f := newFakeStore()
	order := uuid.New()
	f.orders[order] = &fakeOrder{state: completedOrder(2, 1000)}
	f.work = append(f.work, work(order))

	// Run 1 refunds it.
	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())
	if f.final[order].Outcome != "refunded" {
		t.Fatalf("run 1 outcome = %q, want refunded", f.final[order].Outcome)
	}

	// Run 2: fresh ledger row. Its first attempt fixes the quantity and records the
	// attribution, then fails ambiguously, so the row is retried rather than finalized.
	f.final = map[uuid.UUID]store.CancellationOutcome{}
	f.fixed = map[uuid.UUID]int32{}
	f.prior = map[uuid.UUID]bool{}
	f.orders[order].refuse = refunds.ErrPaymentsUnresolved
	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())
	if _, terminal := f.final[order]; terminal {
		t.Fatalf("an ambiguous failure was finalized: %+v", f.final[order])
	}
	if !f.prior[order] {
		t.Fatal("the attribution was not recorded when the quantity was fixed")
	}

	// The downstream recovers and the row resumes.
	f.orders[order].refuse = nil
	f.expireLeases()
	runnerFor(f, newFakeRefunder(f)).RunOnce(context.Background())

	if got := f.final[order].Outcome; got != "already_refunded" {
		t.Fatalf("resumed second run reported %q, want already_refunded — the attribution was lost across the retry", got)
	}
	if f.orders[order].moved != 1 {
		t.Fatalf("money moved %d times, want 1", f.orders[order].moved)
	}
}

// A driven attempt is charged on a context that outlives the caller's, so a shutdown
// landing between the drive and the charge cannot lose it. Found by ai-review pass 1
// [high]; narrowed twice.
//
// What this does NOT claim: that no charge is ever lost. A database error still loses one,
// and pass 2 established that retrying to close that would be worse — a write that commits
// and then reports a timeout would be counted twice, parking a recoverable row early. See
// `chargeDriven`. This test pins the property that actually holds.
//
// The fake's ChargeAttempt refuses on a dead context, exactly as a driver does; without
// that this test passes whichever context the runner uses.
//
// Mutation that must make this RED: give `worklease.HandBackUndriven` a dead context
// instead of its detached one. NOT `chargeDriven` -- this row takes the ABANDON path,
// because `record` sees the cancellation before reaching the retryable branch, and the
// charge rides on the hand-back. Naming the wrong mutation is how a test like this ends
// up believed: the first two mutations tried here both stayed green, once because the
// path was wrong and once because the fake's Abandon ignored its context.
func TestADrivenAttemptIsChargedOnAContextThatOutlivesTheCaller(t *testing.T) {
	order := uuid.New()
	f := newFakeStore()
	// The ambiguous failure: retryable, and the class whose budget actually matters,
	// because a terminal verdict on it leaves money possibly gone.
	f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), refuse: refunds.ErrPaymentsUnresolved}
	f.work = append(f.work, work(order))

	// A caller context that is already dead by the time the charge runs. Cancelling it
	// before RunOnce would short-circuit the pass, so this cancels it during the drive --
	// the ordering the detached charge exists for.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drive := &cancelDuringDrive{cancel: cancel, inner: newFakeRefunder(f)}

	New(f, drive, time.Minute, 4, time.Minute).RunOnce(ctx)

	// The drive happened, so the attempt is owed whatever the caller's context is doing.
	// Note this row takes the ABANDON path (record sees the cancellation first), which
	// charges through the detached hand-back -- so the assertion covers the route a
	// shutdown actually takes, not a hypothetical one.
	if got := f.attempts[order]; got != 1 {
		t.Fatalf("attempts = %d after a driven failure whose caller context died, want 1: "+
			"the drive happened, so the attempt is owed; losing it lets the row exceed its "+
			"budget against work that may already have moved money", got)
	}
}

// cancelDuringDrive lets the drive happen and cancels before returning, so whatever
// follows sees a dead caller context.
type cancelDuringDrive struct {
	cancel context.CancelFunc
	inner  *fakeRefunder
}

func (c *cancelDuringDrive) Refund(ctx context.Context, in store.RefundRequest) (refunds.Result, error) {
	res, err := c.inner.Refund(ctx, in)
	c.cancel()
	return res, err
}

func (c *cancelDuringDrive) DriveReversal(ctx context.Context, r store.Refund) store.Refund {
	return c.inner.DriveReversal(ctx, r)
}

func (c *cancelDuringDrive) Void(ctx context.Context, in store.VoidRequest) (refunds.VoidResult, error) {
	return c.inner.Void(ctx, in)
}

// The charge write's two failure modes, which are a dilemma unless the write is
// idempotent. Both reach `chargeDriven` -- unlike the abandon-path test above, these
// drive a retryable failure to completion without cancelling, so `record` passes its
// cancellation check and takes the charge branch.
//
// Found by ai-review passes 1-3 and the decision audit: the audit established that the
// compare-and-set needs no migration, which is what lets both of these hold at once.
func TestTheChargeWriteIsIdempotentPerClaim(t *testing.T) {
	// A transient failure must not lose the attempt: nothing else bounds re-drives, so a
	// row whose charges keep failing would exceed maxAttempts drives of a failure whose
	// money may already have moved.
	//
	// Mutation that must make this RED: drop the retry loop in chargeDriven.
	t.Run("a failed charge write is retried and the attempt is not lost", func(t *testing.T) {
		order := uuid.New()
		f := newFakeStore()
		f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), refuse: refunds.ErrPaymentsUnresolved}
		f.work = append(f.work, work(order))
		f.chargeFailures = 1 // fails, no commit

		New(f, newFakeRefunder(f), time.Minute, 4, time.Minute).RunOnce(context.Background())

		if got := f.attempts[order]; got != 1 {
			t.Fatalf("attempts = %d after a driven failure whose first charge write failed, want 1: "+
				"the drive happened, so the attempt is owed, and nothing else bounds re-drives", got)
		}
	})

	// The other half: a write that COMMITTED and then reported an error must not be
	// counted twice. Double-charging parks a recoverable row early -- money possibly
	// gone, tickets still valid, nothing driving the reversal, and only a human clears it.
	//
	// Mutation that must make this RED: drop `AND attempts=$5` from
	// ChargeCancellationAttempt (and the mirroring guard in this fake).
	t.Run("a committed write whose acknowledgement was lost is not counted twice", func(t *testing.T) {
		order := uuid.New()
		f := newFakeStore()
		f.orders[order] = &fakeOrder{state: completedOrder(1, 1000), refuse: refunds.ErrPaymentsUnresolved}
		f.work = append(f.work, work(order))
		f.chargeFailures = 1
		f.chargeCommitsAnyway = true // the write lands, the ack does not

		New(f, newFakeRefunder(f), time.Minute, 4, time.Minute).RunOnce(context.Background())

		if got := f.attempts[order]; got != 1 {
			t.Fatalf("attempts = %d after one driven failure whose charge committed but was not "+
				"acknowledged, want 1: the retry must find the count already moved and change "+
				"nothing, or a recoverable row parks early", got)
		}
	})
}
