package reversal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
	"ticketing/shared/obs"
)

// The runner's DECISIONS live here, against fake ports: which rows get released with
// progress, which get abandoned, which are driven at all, and what a shutdown does. Every
// SQL predicate — eligibility, the lease, the claim fence, the backoff, parking — lives in
// the store's PostgreSQL smoke tests instead, because a fake that enforces in Go what the
// shipped SQL must enforce proves only that the fake and the runner agree.

type call struct {
	refund     store.Refund
	progressed bool
	cause      string
}

type fakeStore struct {
	// rows is the DURABLE state, as the database would hold it. Release consults it rather
	// than trusting the claimant, which is the whole point of ai-review F2.
	rows      map[uuid.UUID]store.Refund
	batches   [][]store.ClaimedReversal
	claims    int
	released  []call
	finished  []uuid.UUID
	abandoned []uuid.UUID
	claimErr  error
}

func (f *fakeStore) Claim(_ context.Context, _ int, _ time.Duration) ([]store.ClaimedReversal, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	f.claims++
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

// Release mirrors the real signature: it is told what the claimant OBSERVED at claim time
// and decides progress itself. The fake reproduces the store's rule — progress is measured
// against the row as it stands now, not against the claimant's after-value — so a test can
// exercise the case that motivated it: someone else discharged an obligation while this
// claimant's own call failed.
func (f *fakeStore) Release(_ context.Context, _, refundID, _ uuid.UUID, voidedAtClaim, capacityAtClaim bool, cause string) error {
	row := f.rows[refundID]
	progressed := (row.TicketsVoided && !voidedAtClaim) || (row.CapacityReturned && !capacityAtClaim)
	f.released = append(f.released, call{refund: store.Refund{ID: refundID}, progressed: progressed, cause: cause})
	return nil
}

func (f *fakeStore) Finish(_ context.Context, _, refundID, _ uuid.UUID) error {
	f.finished = append(f.finished, refundID)
	return nil
}

// Abandon honours the context it is given. That is deliberate and load-bearing: the real
// store write fails on a cancelled context, so a fake that ignores ctx cannot tell a
// shutdown release that lands from one that silently does not — and the whole point of the
// abandon path is that it runs on a context detached from the cancelled one.
func (f *fakeStore) Abandon(ctx context.Context, _, refundID, _ uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.abandoned = append(f.abandoned, refundID)
	return nil
}

func (f *fakeStore) Backlog(context.Context) (store.ReversalBacklog, error) {
	return store.ReversalBacklog{}, nil
}

// fakeReverser answers with whatever state the drive is said to have reached. It never
// errors, exactly like the real DriveReversal.
type fakeReverser struct {
	after map[uuid.UUID]store.Refund
	seen  []store.Refund
}

func (f *fakeReverser) DriveReversal(_ context.Context, in store.Refund) store.Refund {
	f.seen = append(f.seen, in)
	if out, ok := f.after[in.ID]; ok {
		return out
	}
	return in
}

func outstanding(id uuid.UUID, voided, returned bool, attempts int) store.ClaimedReversal {
	return store.ClaimedReversal{
		Refund: store.Refund{
			ID: id, OrderID: uuid.New(), OrganizerID: uuid.New(), Quantity: 2,
			Status: "completed", Completed: true, HoldID: uuid.New(),
			TicketsVoided: voided, CapacityReturned: returned,
		},
		ClaimID: uuid.New(), Attempts: attempts,
	}
}

func runner(st Store, rev Reverser) *Runner {
	return New(st, rev, time.Minute, 8, time.Minute, nil)
}

// storeWith builds a fake whose DURABLE rows already hold `rows` — the state the database
// would be in when Release consults it. Separating this from the reverser's answer is what
// lets a test say "someone else discharged this while my own call failed".
func storeWith(batch []store.ClaimedReversal, rows map[uuid.UUID]store.Refund) *fakeStore {
	if rows == nil {
		rows = map[uuid.UUID]store.Refund{}
	}
	return &fakeStore{rows: rows, batches: [][]store.ClaimedReversal{batch}}
}

// A reversal that completes is finished, not released: nothing should schedule another
// attempt at an obligation that is discharged.
func TestACompletedReversalIsFinished(t *testing.T) {
	id := uuid.New()
	claimed := outstanding(id, false, false, 1)
	done := claimed.Refund
	done.TicketsVoided, done.CapacityReturned = true, true

	st := storeWith([]store.ClaimedReversal{claimed}, nil)
	rev := &fakeReverser{after: map[uuid.UUID]store.Refund{id: done}}

	if got := runner(st, rev).RunOnce(context.Background()); got != 1 {
		t.Fatalf("resolved = %d, want 1", got)
	}
	if len(st.finished) != 1 || st.finished[0] != id {
		t.Fatalf("finished = %v, want [%v]", st.finished, id)
	}
	if len(st.released) != 0 {
		t.Fatalf("a discharged reversal was released for another attempt: %v", st.released)
	}
}

// Discharging ONE of two obligations is progress. Without this a refund whose voiding
// succeeds and whose capacity return keeps failing spends its whole budget on the half
// that already works, and parks a row that is recovering.
func TestDischargingOneObligationCountsAsProgress(t *testing.T) {
	id := uuid.New()
	claimed := outstanding(id, false, false, 3)
	half := claimed.Refund
	half.TicketsVoided = true // capacity still outstanding

	st := storeWith([]store.ClaimedReversal{claimed}, map[uuid.UUID]store.Refund{id: half})
	rev := &fakeReverser{after: map[uuid.UUID]store.Refund{id: half}}

	runner(st, rev).RunOnce(context.Background())

	if len(st.finished) != 0 {
		t.Fatalf("a half-discharged reversal was finished: %v", st.finished)
	}
	if len(st.released) != 1 {
		t.Fatalf("released = %d calls, want 1", len(st.released))
	}
	if !st.released[0].progressed {
		t.Fatal("discharging the voiding half was not reported as progress, so the attempt " +
			"budget will not reset and a recovering refund can park")
	}
}

// The other half of progress, and the one a test written only around the ticket's headline
// case (access down) misses: voiding was already discharged by an earlier pass, and THIS
// pass discharged the capacity return. A `Progressed` that only looks at voiding reports no
// progress here and spends budget on a refund that is actively recovering.
func TestDischargingTheCapacityHalfAloneCountsAsProgress(t *testing.T) {
	id := uuid.New()
	claimed := outstanding(id, true, false, 3) // voided earlier, capacity still owed
	// The capacity return lands, but the row is NOT complete afterwards — voiding was
	// recorded by an earlier pass and the drive answers with only what it did. This is the
	// arrangement that reaches Release rather than Finish, so the capacity half of the
	// progress rule is actually exercised.
	durable := claimed.Refund
	durable.CapacityReturned = true
	drove := claimed.Refund
	drove.TicketsVoided = false // this claimant could not confirm voiding this pass
	drove.CapacityReturned = true

	st := storeWith([]store.ClaimedReversal{claimed}, map[uuid.UUID]store.Refund{id: durable})
	rev := &fakeReverser{after: map[uuid.UUID]store.Refund{id: drove}}

	runner(st, rev).RunOnce(context.Background())

	if len(st.released) != 1 {
		t.Fatalf("released = %d calls, want 1", len(st.released))
	}
	if !st.released[0].progressed {
		t.Fatal("discharging the capacity half was not reported as progress: a refund whose " +
			"voiding landed earlier and whose capacity lands now would spend budget while recovering")
	}
}

// ai-review F2: progress is whatever the DATABASE shows, not what this claimant managed.
// The staff refund endpoint and the cancellation runner drive the same reversal without
// taking this lease, so a replay can persist a discharge while this claimant's own call
// fails. Calling that "no progress" parks a row that just advanced — and at the budget
// boundary it parks it permanently, with the remaining obligation owed and nothing driving.
func TestProgressIsReadFromTheRowNotFromThisClaimantsResult(t *testing.T) {
	id := uuid.New()
	claimed := outstanding(id, false, false, store.MaxReversalAttempts-1)
	// Someone else voided the tickets while this claimant was mid-flight.
	durable := claimed.Refund
	durable.TicketsVoided = true

	st := storeWith([]store.ClaimedReversal{claimed}, map[uuid.UUID]store.Refund{id: durable})
	// This claimant's own drive achieved nothing: its access call failed.
	rev := &fakeReverser{}

	runner(st, rev).RunOnce(context.Background())

	if len(st.released) != 1 {
		t.Fatalf("released = %d calls, want 1", len(st.released))
	}
	if !st.released[0].progressed {
		t.Fatal("a concurrent replay discharged an obligation and this pass reported no " +
			"progress: at the budget boundary that parks a refund that is recovering, and " +
			"its remaining obligation is then owed with nothing driving it")
	}
}

// A pass that discharges nothing is not progress, and must say so — that is what spends
// the budget and eventually parks a permanently refused obligation.
func TestAPassThatDischargesNothingIsNotProgress(t *testing.T) {
	id := uuid.New()
	claimed := outstanding(id, true, false, 4) // voided already; capacity refused forever

	st := storeWith([]store.ClaimedReversal{claimed}, nil)
	rev := &fakeReverser{} // returns its input unchanged: nothing moved

	runner(st, rev).RunOnce(context.Background())

	if len(st.released) != 1 {
		t.Fatalf("released = %d calls, want 1", len(st.released))
	}
	if st.released[0].progressed {
		t.Fatal("a pass that discharged nothing reported progress, which resets the budget " +
			"and makes a permanently refused obligation retry forever")
	}
	if st.released[0].cause == "" {
		t.Fatal("a row released without progress recorded no reason, so an operator reading " +
			"the parked row cannot tell what kept failing")
	}
}

// Shutdown mid-batch: the rest of the claim is handed back undriven, and the attempt
// charged at claim time comes back with it. A row must not reach its first real failure
// with budget already spent on work that never happened.
func TestShutdownAbandonsUndrivenClaims(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	st := &fakeStore{batches: [][]store.ClaimedReversal{{
		outstanding(first, false, false, 0),
		outstanding(second, false, false, 0),
	}}}
	done := store.Refund{ID: first, TicketsVoided: true, CapacityReturned: true}
	rev := &fakeReverser{after: map[uuid.UUID]store.Refund{first: done}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel as soon as the first row has been driven, so the second is claimed-but-undriven.
	r := New(st, reverserFunc(func(c context.Context, in store.Refund) store.Refund {
		out := rev.DriveReversal(c, in)
		cancel()
		return out
	}), time.Minute, 8, time.Minute, nil)

	r.RunOnce(ctx)

	if len(st.abandoned) != 1 || st.abandoned[0] != second {
		t.Fatalf("abandoned = %v, want [%v]: an undriven claim must be handed back, not left "+
			"leased for the whole lease while its obligation stays outstanding", st.abandoned, second)
	}
	if len(rev.seen) != 1 {
		t.Fatalf("drove %d refunds after cancellation, want 1", len(rev.seen))
	}
}

// The abandon path runs on a context detached from the cancelled one — reusing the
// caller's would fail every one of these writes and defeat the point.
func TestAbandonSurvivesACancelledContext(t *testing.T) {
	id := uuid.New()
	st := &fakeStore{batches: [][]store.ClaimedReversal{{outstanding(id, false, false, 0)}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	New(st, &fakeReverser{}, time.Minute, 8, time.Minute, nil).RunOnce(ctx)

	if len(st.abandoned) != 1 {
		t.Fatalf("abandoned = %v, want one row: a shutdown release that rides the cancelled "+
			"context never lands", st.abandoned)
	}
}

// ai-review F1: the lease must outlast the work it protects, and the number it is derived
// from must be the timeout of the client that actually makes the calls.
//
// The first version borrowed `recoveryCallTimeout` (10s) while the refund service drives its
// calls through obs.Client (30s), giving a 380s lease over work that can take 960s. A lease
// shorter than its own batch is worse than none: a second replica reclaims rows the first is
// still driving, and the claim token fences only the final database write — never the access
// or inventory call already in flight.
//
// Asserted as a RELATIONSHIP rather than against the literal 1020s, so the test survives a
// change to the batch, the call count or obs.ClientTimeout and only fails if the lease stops
// covering the work.
func TestTheLeaseOutlastsTheBatchItProtects(t *testing.T) {
	for _, batch := range []int{1, 8, 16, 64} {
		worstCase := time.Duration(batch) * MaxCallsPerRefund * obs.ClientTimeout
		got := LeaseFor(batch, obs.ClientTimeout)
		if got <= worstCase {
			t.Fatalf("batch %d: lease %s does not outlast its own worst case %s — a second "+
				"replica can reclaim rows this pass is still driving", batch, got, worstCase)
		}
	}
}

// And the failure it is meant to catch: sizing from a timeout SMALLER than the client's
// really uses produces a lease that does not cover the work. This is the mutation, written
// as a test, because the defect was not in LeaseFor — it was in what the caller passed.
func TestSizingTheLeaseFromTheWrongTimeoutUnderCoversTheBatch(t *testing.T) {
	const wrong = 10 * time.Second // recoveryCallTimeout, the value that shipped in the first draft
	if wrong >= obs.ClientTimeout {
		t.Skip("obs.ClientTimeout is no longer larger than the recovery constant; this test's premise is gone")
	}
	batch := 16
	worstCase := time.Duration(batch) * MaxCallsPerRefund * obs.ClientTimeout
	if LeaseFor(batch, wrong) > worstCase {
		t.Fatal("sizing from the wrong timeout accidentally still covers the batch, so this " +
			"test cannot detect the defect it names")
	}
}

// A claim error ends the pass rather than spinning.
func TestAClaimFailureEndsThePass(t *testing.T) {
	st := &fakeStore{claimErr: errors.New("database is down")}
	if got := runner(st, &fakeReverser{}).RunOnce(context.Background()); got != 0 {
		t.Fatalf("resolved = %d, want 0", got)
	}
}

// The runner drains: a FULL batch is followed by another claim, so a backlog deeper than
// one batch is not left waiting a whole interval per batch — which after a long outage is
// exactly when the backlog is deepest.
//
// The batch here is size 1 so that a one-row claim IS a full batch. With the runner's
// default batch of 8 a single row is a short batch, which correctly ends the drain, and the
// test would be asserting the opposite of what it names.
func TestAFullBatchIsFollowedByAnotherClaim(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	st := &fakeStore{batches: [][]store.ClaimedReversal{
		{outstanding(a, false, false, 0)},
		{outstanding(b, false, false, 0)},
	}}
	New(st, &fakeReverser{}, time.Minute, 1, time.Minute, nil).RunOnce(context.Background())
	if st.claims < 3 {
		t.Fatalf("claimed %d times, want at least 3 (two full batches plus the empty one that "+
			"ends the drain): a backlog deeper than one batch waits a whole interval", st.claims)
	}
}

// The mirror of the test above, and the reason the drain terminates: a SHORT batch means
// the queue is drained, so the pass stops rather than issuing a claim that can only come
// back empty. Without this the runner does one wasted round-trip per pass, forever.
func TestAShortBatchEndsThePass(t *testing.T) {
	st := &fakeStore{batches: [][]store.ClaimedReversal{
		{outstanding(uuid.New(), false, false, 0)},
	}}
	New(st, &fakeReverser{}, time.Minute, 8, time.Minute, nil).RunOnce(context.Background())
	if st.claims != 1 {
		t.Fatalf("claimed %d times, want exactly 1: a batch shorter than the limit already "+
			"proves the queue is empty", st.claims)
	}
}

type reverserFunc func(context.Context, store.Refund) store.Refund

func (f reverserFunc) DriveReversal(ctx context.Context, in store.Refund) store.Refund {
	return f(ctx, in)
}
