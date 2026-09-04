package exchangesweep

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
	"ticketing/shared/obs"
)

// The runner's DECISIONS live here, against fake ports: which rows get finished, which get
// released and with what cause, which are driven at all, and what a shutdown does. Every SQL
// predicate — the five claim conjuncts, the lease, the claim fence, the backoff, parking —
// lives in the store's PostgreSQL smoke tests instead, because a fake that enforces in Go
// what the shipped SQL must enforce proves only that the fake and the runner agree.

type call struct {
	org, id    uuid.UUID
	progressed bool
	cause      string
}

type fakeStore struct {
	// rows is the DURABLE state, as the database would hold it. Release consults it rather
	// than trusting the claimant — the point of ADR-062's ai-review F2, inherited here.
	rows      map[uuid.UUID]store.ExchangeSwitch
	batches   [][]store.ClaimedExchangeReversal
	claims    int
	released  []call
	finished  []uuid.UUID
	abandoned []uuid.UUID
	// abandonedLive records, per abandon, whether the context carried a deadline. The
	// shutdown path detaches with a 5s timeout; the already-driven path must use the
	// caller's. A test that only counts abandons cannot tell them apart.
	abandonedLive []bool
	claimErr      error
}

func (f *fakeStore) Claim(_ context.Context, _ int, _ time.Duration) ([]store.ClaimedExchangeReversal, error) {
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
// and decides progress itself, against the row as it stands now.
func (f *fakeStore) Release(_ context.Context, org, id, _ uuid.UUID, switchedAtClaim, capacityAtClaim bool, cause string) error {
	row := f.rows[id]
	progressed := (row.TicketsExchanged && !switchedAtClaim) || (row.CapacityReturned && !capacityAtClaim)
	f.released = append(f.released, call{org: org, id: id, progressed: progressed, cause: cause})
	return nil
}

func (f *fakeStore) Finish(_ context.Context, _, id, _ uuid.UUID) error {
	f.finished = append(f.finished, id)
	return nil
}

// Abandon honours the context it is given. Deliberate and load-bearing: the real store write
// fails on a cancelled context, so a fake that ignored ctx could not tell a shutdown release
// that lands from one that silently does not.
func (f *fakeStore) Abandon(ctx context.Context, _, id, _ uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.abandoned = append(f.abandoned, id)
	_, hasDeadline := ctx.Deadline()
	f.abandonedLive = append(f.abandonedLive, !hasDeadline)
	return nil
}

func (f *fakeStore) Backlog(context.Context) (store.ExchangeReversalBacklog, error) {
	return store.ExchangeReversalBacklog{}, nil
}

// fakeDischarger answers with whatever state the drive is said to have reached. It never
// errors, exactly like the real DriveExchange.
type fakeDischarger struct {
	after map[uuid.UUID]store.ExchangeSwitch
	seen  []store.ExchangeSwitch
}

func (f *fakeDischarger) DriveExchange(_ context.Context, in store.ExchangeSwitch) store.ExchangeSwitch {
	f.seen = append(f.seen, in)
	if out, ok := f.after[in.ID]; ok {
		return out
	}
	return in
}

func outstanding(id uuid.UUID, switched, returned bool, attempts int) store.ClaimedExchangeReversal {
	return store.ClaimedExchangeReversal{
		Exchange: store.ExchangeSwitch{
			ID: id, OrganizerID: uuid.New(), SourceHoldID: uuid.New(), Quantity: 2,
			TicketsExchanged: switched, CapacityReturned: returned,
		},
		ClaimID: uuid.New(), Attempts: attempts,
	}
}

func runner(st Store, d Discharger) *Runner {
	return New(st, d, time.Minute, 8, time.Minute, nil)
}

func storeWith(batch []store.ClaimedExchangeReversal, rows map[uuid.UUID]store.ExchangeSwitch) *fakeStore {
	if rows == nil {
		rows = map[uuid.UUID]store.ExchangeSwitch{}
	}
	return &fakeStore{rows: rows, batches: [][]store.ClaimedExchangeReversal{batch}}
}

// An exchange that completes is finished, not released: nothing should schedule another
// attempt at a discharged obligation.
func TestACompletedExchangeIsFinished(t *testing.T) {
	id := uuid.New()
	c := outstanding(id, true, false, 0)
	done := c.Exchange
	done.CapacityReturned = true
	st := storeWith([]store.ClaimedExchangeReversal{c}, map[uuid.UUID]store.ExchangeSwitch{id: done})

	got := runner(st, &fakeDischarger{after: map[uuid.UUID]store.ExchangeSwitch{id: done}}).RunOnce(context.Background())

	if got != 1 {
		t.Fatalf("resolved = %d, want 1", got)
	}
	if len(st.finished) != 1 || st.finished[0] != id {
		t.Fatalf("finished = %v, want [%v]", st.finished, id)
	}
	if len(st.released) != 0 {
		t.Fatalf("released = %v, want none: a discharged obligation must not be scheduled again", st.released)
	}
}

// THE SAFETY TEST (ADR-063). An exchange whose switch access has not confirmed is released,
// never completed and never driven to a capacity return.
//
// Since ai-review F4 the claim query no longer OFFERS such a row, so this state should not
// reach the runner in production. The test is kept, and matters more rather than less: the
// marker is read from the row, so a concurrent writer can clear it between claim and
// release, and this is the behaviour that must hold when it does. Defence in depth is only
// depth if something proves the inner layer still works.
//
// The mechanism is structural: the Discharger port has no method that could set the marker.
// If a future edit gives the runner one, this test is what goes red.
func TestAnExchangeAwaitingItsSwitchIsNeverCompleted(t *testing.T) {
	id := uuid.New()
	c := outstanding(id, false, false, 0)
	st := storeWith([]store.ClaimedExchangeReversal{c}, map[uuid.UUID]store.ExchangeSwitch{id: c.Exchange})

	// The discharge unit is a no-op for an unswitched exchange, exactly as the real one is.
	// The discharger is given an answer in which capacity DID come back on an unswitched
	// row — a state the real unit refuses to produce. That is deliberate: with a no-op
	// discharger this test stays green even when the runner's completion check drops the
	// TicketsExchanged conjunct, because nothing would ever report capacity returned. The
	// fixture has to be able to REACH the failing state or it proves something else.
	bad := c.Exchange
	bad.CapacityReturned = true
	got := runner(st, &fakeDischarger{after: map[uuid.UUID]store.ExchangeSwitch{id: bad}}).RunOnce(context.Background())

	if got != 0 {
		t.Fatalf("resolved = %d, want 0: commerce cannot complete an exchange access has not "+
			"confirmed switched — only access can establish that the old tickets stopped admitting, "+
			"and freeing capacity behind a marker commerce invented is the one ordering that oversells "+
			"(ADR-038 §1)", got)
	}
	if len(st.finished) != 0 {
		t.Fatalf("finished = %v, want none", st.finished)
	}
	if len(st.released) != 1 {
		t.Fatalf("released = %v, want exactly one", st.released)
	}
	if st.released[0].cause != "awaiting access switch confirmation" {
		t.Fatalf("cause = %q, want %q: the two outstanding states have different owners and "+
			"different incidents, and the recorded cause is how an operator tells them apart",
			st.released[0].cause, "awaiting access switch confirmation")
	}
}

// The other cause, so the test above is about the CAUSE rather than about releasing at all:
// a switched exchange whose capacity return failed records the inventory-side cause.
func TestAFailedCapacityReturnRecordsTheCapacityCause(t *testing.T) {
	id := uuid.New()
	c := outstanding(id, true, false, 0)
	st := storeWith([]store.ClaimedExchangeReversal{c}, map[uuid.UUID]store.ExchangeSwitch{id: c.Exchange})

	runner(st, &fakeDischarger{}).RunOnce(context.Background())

	if len(st.released) != 1 || st.released[0].cause != "capacity return outstanding" {
		t.Fatalf("released = %v, want one with cause %q", st.released, "capacity return outstanding")
	}
}

// Progress is read from the DURABLE ROW, not from this claimant's own result. The
// tickets-switched callback drives the same obligation whenever access redelivers, so a
// concurrent discharge this claimant never saw must count as progress — calling it "no
// progress" would spend budget on, and eventually park, an exchange that just advanced.
func TestProgressIsReadFromTheRowNotFromThisClaimantsResult(t *testing.T) {
	id := uuid.New()
	c := outstanding(id, false, false, 0)
	// The callback landed while this claimant was in flight: the row is switched now.
	row := c.Exchange
	row.TicketsExchanged = true
	st := storeWith([]store.ClaimedExchangeReversal{c}, map[uuid.UUID]store.ExchangeSwitch{id: row})

	// This claimant's own drive saw nothing change.
	runner(st, &fakeDischarger{}).RunOnce(context.Background())

	if len(st.released) != 1 {
		t.Fatalf("released = %v, want one", st.released)
	}
	if !st.released[0].progressed {
		t.Fatal("progressed = false, want true: another caller discharged an obligation while " +
			"this claimant's drive failed, and a verdict computed from the claimant's own " +
			"before/after would park a recovering exchange")
	}
}

// The mirror, and what makes the test above prove something: when the row genuinely did not
// move, that is not progress and the budget is spent.
func TestAPassThatDischargesNothingIsNotProgress(t *testing.T) {
	id := uuid.New()
	c := outstanding(id, true, false, 0)
	st := storeWith([]store.ClaimedExchangeReversal{c}, map[uuid.UUID]store.ExchangeSwitch{id: c.Exchange})

	runner(st, &fakeDischarger{}).RunOnce(context.Background())

	if len(st.released) != 1 || st.released[0].progressed {
		t.Fatalf("released = %v, want one with progressed=false", st.released)
	}
}

// A shutdown mid-batch hands back every claim the pass never drove, on a DETACHED context —
// the caller's is already cancelled, so reusing it would fail every one of these writes and
// leave the rows leased for the full lease with their obligations outstanding.
func TestShutdownAbandonsUndrivenClaims(t *testing.T) {
	a, b, c := outstanding(uuid.New(), true, false, 0), outstanding(uuid.New(), true, false, 0), outstanding(uuid.New(), true, false, 0)
	st := storeWith([]store.ClaimedExchangeReversal{a, b, c}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner(st, &fakeDischarger{}).RunOnce(ctx)

	if len(st.abandoned) != 3 {
		t.Fatalf("abandoned = %d, want 3: every undriven claim must be handed back immediately "+
			"rather than waiting out the lease", len(st.abandoned))
	}
	for i, live := range st.abandonedLive {
		if live {
			t.Fatalf("abandon %d ran on a context with no deadline; the shutdown path must "+
				"detach with its own bounded context or the write fails on the cancelled one", i)
		}
	}
}

// One drive per exchange per pass. A released row becomes due again after its progress floor
// or backoff — both shorter than a slow batch — so a drain that only re-claimed would pick
// the same row up again in a later batch of the same pass, letting one row spend its whole
// budget and PARK inside a single RunOnce.
func TestAnExchangeIsDrivenAtMostOncePerPass(t *testing.T) {
	id := uuid.New()
	c := outstanding(id, true, false, 0)
	st := &fakeStore{
		rows: map[uuid.UUID]store.ExchangeSwitch{id: c.Exchange},
		// Two full batches, the second one the same row coming back round.
		batches: [][]store.ClaimedExchangeReversal{
			{c, outstanding(uuid.New(), true, false, 0), outstanding(uuid.New(), true, false, 0),
				outstanding(uuid.New(), true, false, 0), outstanding(uuid.New(), true, false, 0),
				outstanding(uuid.New(), true, false, 0), outstanding(uuid.New(), true, false, 0),
				outstanding(uuid.New(), true, false, 0)},
			{c},
		},
	}
	d := &fakeDischarger{}

	runner(st, d).RunOnce(context.Background())

	var drives int
	for _, seen := range d.seen {
		if seen.ID == id {
			drives++
		}
	}
	if drives != 1 {
		t.Fatalf("drove the same exchange %d times in one pass, want 1: a duplicate must be "+
			"handed back UNDRIVEN so it costs no attempt", drives)
	}
	if len(st.abandoned) != 1 || st.abandoned[0] != id {
		t.Fatalf("abandoned = %v, want [%v]: the duplicate is handed back, not merely skipped — "+
			"a batch made entirely of duplicates would otherwise spin", st.abandoned, id)
	}
}

// The duplicate check keys on the FULL COMPOSITE. `id` is not unique by schema —
// order_exchanges' primary key is (organizer_id, id) — so keying on it alone would let one
// tenant's row suppress another tenant's genuinely distinct work for the rest of the pass.
//
// Delete the `org` field from `key` and this is what goes red; nothing else in the package
// can see it.
func TestTwoTenantsSharingAnExchangeIDAreBothDriven(t *testing.T) {
	id := uuid.New()
	one := outstanding(id, true, false, 0)
	two := outstanding(id, true, false, 0) // same id, different organizer
	if one.Exchange.OrganizerID == two.Exchange.OrganizerID {
		t.Fatal("fixture is broken: the two rows must belong to different tenants")
	}
	st := storeWith([]store.ClaimedExchangeReversal{one, two}, nil)
	d := &fakeDischarger{}

	runner(st, d).RunOnce(context.Background())

	if len(d.seen) != 2 {
		t.Fatalf("drove %d rows, want 2: two tenants can hold the same exchange id, and "+
			"suppressing the second as a duplicate leaves a real obligation undriven for the "+
			"rest of the pass", len(d.seen))
	}
	if len(st.abandoned) != 0 {
		t.Fatalf("abandoned = %v, want none: neither row is a duplicate of the other", st.abandoned)
	}
}

// A lease must cover every sequential call in its batch. The claim token fences the
// database write, but it cannot stop a second claimant from repeating an inventory call.
func TestTheLeaseOutlastsTheBatchItProtects(t *testing.T) {
	const batch = 16
	lease, err := LeaseFor(batch, obs.ClientTimeout)
	if err != nil {
		t.Fatal(err)
	}
	worst := time.Duration(batch) * MaxCallsPerExchange * obs.ClientTimeout
	if lease <= worst {
		t.Fatalf("lease %s <= worst-case batch %s: a second replica would reclaim rows the "+
			"first is still driving", lease, worst)
	}
}

type endlessStore struct{ fakeStore }

func (e *endlessStore) Claim(_ context.Context, limit int, _ time.Duration) ([]store.ClaimedExchangeReversal, error) {
	e.claims++
	out := make([]store.ClaimedExchangeReversal, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, outstanding(uuid.New(), true, false, 0))
	}
	return out, nil
}

// A queue that never empties must not spin forever: the per-pass `driven` set would grow for
// the life of the process and the ticker would never fire again.
func TestAPassIsBoundedAgainstAQueueThatNeverEmpties(t *testing.T) {
	st := &endlessStore{fakeStore{rows: map[uuid.UUID]store.ExchangeSwitch{}}}
	runner(st, &fakeDischarger{}).RunOnce(context.Background())

	// An ABSOLUTE ceiling, not `== MaxBatchesPerPass`. Restating the constant makes the
	// assertion move with the mutation: raising the constant to a million would keep such a
	// test green while the drain became effectively unbounded, which is the defect. The
	// requirement is "a pass terminates in a bounded number of claims", so the number here
	// is derived from that requirement and deliberately not from the implementation.
	const ceiling = 1000
	if st.claims > ceiling {
		t.Fatalf("claims = %d, want <= %d: an unbounded drain never returns against a queue "+
			"arriving at or above processing rate — the per-pass `driven` set would grow for "+
			"the life of the process and the ticker would never fire again", st.claims, ceiling)
	}
	if st.claims != MaxBatchesPerPass {
		t.Fatalf("claims = %d, want exactly MaxBatchesPerPass (%d): the pass must stop AT its "+
			"bound, not before it — stopping early would mean a full batch was being read as "+
			"a drained queue", st.claims, MaxBatchesPerPass)
	}
}

// A claim failure ends the pass rather than spinning on a database that is refusing.
func TestAClaimFailureEndsThePass(t *testing.T) {
	st := &fakeStore{claimErr: errors.New("database down"), rows: map[uuid.UUID]store.ExchangeSwitch{}}
	if got := runner(st, &fakeDischarger{}).RunOnce(context.Background()); got != 0 {
		t.Fatalf("resolved = %d, want 0", got)
	}
}

// A short batch means the queue is drained; do not claim again.
func TestAShortBatchEndsThePass(t *testing.T) {
	st := storeWith([]store.ClaimedExchangeReversal{outstanding(uuid.New(), true, false, 0)}, nil)
	runner(st, &fakeDischarger{}).RunOnce(context.Background())
	if st.claims != 1 {
		t.Fatalf("claims = %d, want 1: a batch shorter than the limit is the end of the queue", st.claims)
	}
}
