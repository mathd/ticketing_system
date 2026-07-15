package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// These tests assert ADR-016 §Decision 3's decision table at the runner level: given
// the evidence, which transition does the runner choose? The store's smoke tests cover
// what each transition writes; what they cannot show is the choice itself.
//
// The rule under test throughout is §Decision 2: decide from durable evidence, never
// from an inference about what a failed transport did.

// fakeStore records the transitions the runner chose. It is not a database simulator:
// every method exists to answer "was this transition taken, with what arguments".
type fakeStore struct {
	tr    *trace
	claim []store.StuckOrder

	outcomes  []string           // RecordTerminalOutcome
	parked    []string           // ParkForReconciliation reasons
	cleared   int                // ClearRecoveryClaim
	released  []store.StuckOrder // MarkReleased
	failed    []error            // ReleaseStuckOrder causes
	abandoned []uuid.UUID        // AbandonRecoveryClaim (shutdown hand-back)
}

func (f *fakeStore) ClaimStuckOrders(context.Context, int, time.Duration) ([]store.StuckOrder, error) {
	out := f.claim
	f.claim = nil // one pass only: the runner leases rows, it does not re-read them
	return out, nil
}

func (f *fakeStore) RecordTerminalOutcome(_ context.Context, _, _ uuid.UUID, outcome string) error {
	f.tr.add("store.RecordTerminalOutcome")
	f.outcomes = append(f.outcomes, outcome)
	return nil
}

func (f *fakeStore) ParkForReconciliation(_ context.Context, _, _ uuid.UUID, reason string) error {
	f.tr.add("store.ParkForReconciliation")
	f.parked = append(f.parked, reason)
	return nil
}

func (f *fakeStore) ClearRecoveryClaim(context.Context, uuid.UUID, uuid.UUID) error {
	f.tr.add("store.ClearRecoveryClaim")
	f.cleared++
	return nil
}

func (f *fakeStore) AbandonRecoveryClaim(_ context.Context, orderID, _ uuid.UUID) error {
	f.tr.add("store.AbandonRecoveryClaim")
	f.abandoned = append(f.abandoned, orderID)
	return nil
}

func (f *fakeStore) MarkReleased(_ context.Context, s store.StuckOrder) error {
	f.tr.add("store.MarkReleased")
	f.released = append(f.released, s)
	return nil
}

func (f *fakeStore) ReleaseStuckOrder(_ context.Context, _, _ uuid.UUID, cause error) error {
	f.tr.add("store.ReleaseStuckOrder")
	f.failed = append(f.failed, cause)
	return nil
}

type fakePayments struct {
	tr    *trace
	op    Operation
	found bool
	err   error
	calls int
}

func (f *fakePayments) LookupOperation(context.Context, uuid.UUID, string) (Operation, bool, error) {
	f.tr.add("payments.LookupOperation")
	f.calls++
	return f.op, f.found, f.err
}

type fakeInventory struct {
	tr         *trace
	confirmErr error
	releaseErr error
	confirmed  int
	releases   int
}

func (f *fakeInventory) Confirm(context.Context, uuid.UUID, uuid.UUID) error {
	f.tr.add("inventory.Confirm")
	f.confirmed++
	return f.confirmErr
}

func (f *fakeInventory) Release(context.Context, uuid.UUID, uuid.UUID) error {
	f.tr.add("inventory.Release")
	f.releases++
	return f.releaseErr
}

type fakeJournal struct {
	tr    *trace
	facts []store.StuckOrder
	err   error
}

func (f *fakeJournal) OrderFailed(_ context.Context, s store.StuckOrder) error {
	f.tr.add("journal.OrderFailed")
	if f.err != nil {
		return f.err
	}
	f.facts = append(f.facts, s)
	return nil
}

type fakeCompleter struct {
	tr         *trace
	completed  []store.StuckOrder
	err        error
	onComplete func() // fires after the first completion; used to cancel mid-batch
}

func (f *fakeCompleter) Complete(_ context.Context, s store.StuckOrder) error {
	f.tr.add("completer.Complete")
	if f.onComplete != nil {
		f.onComplete()
	}
	if f.err != nil {
		return f.err
	}
	f.completed = append(f.completed, s)
	return nil
}

type ports struct {
	store     *fakeStore
	payments  *fakePayments
	inventory *fakeInventory
	journal   *fakeJournal
	completer *fakeCompleter
	trace     *trace
}

// trace is one ordered log across every port. Per-port counters cannot express
// "recorded the outcome BEFORE releasing", and that ordering is the whole reason the
// release is restartable — so it needs an assertion that can actually see order.
type trace struct {
	steps []string
}

func (t *trace) add(step string) { t.steps = append(t.steps, step) }

func (t *trace) indexOf(step string) int {
	for i, s := range t.steps {
		if s == step {
			return i
		}
	}
	return -1
}

// mustPrecede fails unless both steps happened, in this order.
func (t *trace) mustPrecede(tb testing.TB, first, second string) {
	tb.Helper()
	i, j := t.indexOf(first), t.indexOf(second)
	if i < 0 {
		tb.Fatalf("%q never happened; trace=%v", first, t.steps)
	}
	if j < 0 {
		tb.Fatalf("%q never happened; trace=%v", second, t.steps)
	}
	if i > j {
		tb.Errorf("%q must precede %q; trace=%v", first, second, t.steps)
	}
}

func stuck(status string) store.StuckOrder {
	return store.StuckOrder{
		OrderID: uuid.New(), ReservationID: uuid.New(), OrganizerID: uuid.New(),
		HoldID: uuid.New(), BuyerID: uuid.New(), SlotID: uuid.New(), TicketTypeID: uuid.New(),
		Quantity: 2, Amount: 5000, Currency: "CAD", Status: status,
		IdempotencyKey: "key-" + uuid.NewString(), ClaimID: uuid.New(),
	}
}

// run drives exactly one pass over the given orders and returns the ports for assertion.
func run(t *testing.T, orders []store.StuckOrder, tune func(*ports)) (*ports, int) {
	t.Helper()
	tr := &trace{}
	p := &ports{
		store:     &fakeStore{tr: tr, claim: orders},
		payments:  &fakePayments{tr: tr},
		inventory: &fakeInventory{tr: tr},
		journal:   &fakeJournal{tr: tr},
		completer: &fakeCompleter{tr: tr},
		trace:     tr,
	}
	if tune != nil {
		tune(p)
	}
	r := New(p.store, p.payments, p.inventory, p.journal, p.completer,
		time.Minute, 8, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return p, r.RunOnce(context.Background())
}

// The lease must outlast the pass it protects. If it lapses mid-batch a second runner
// claims rows the first is still driving, and the claim token fences only the final
// database write — not the inventory call or journal submission already in flight.
func TestLeaseOutlastsTheBatchsWorstCaseIO(t *testing.T) {
	for _, tc := range []struct {
		batch   int
		timeout time.Duration
	}{{1, 10 * time.Second}, {16, 10 * time.Second}, {64, 30 * time.Second}} {
		worst := time.Duration(tc.batch) * MaxCallsPerOrder * tc.timeout
		got := LeaseFor(tc.batch, tc.timeout)
		if got <= worst {
			t.Errorf("LeaseFor(%d, %s) = %s, which does not outlast the worst-case pass of %s",
				tc.batch, tc.timeout, got, worst)
		}
	}
}

// Zero/negative inputs must not collapse the lease to something shorter than a single
// call — New falls back to a default batch, and the lease has to match that reality.
func TestLeaseForRejectsDegenerateInputs(t *testing.T) {
	if got := LeaseFor(0, 0); got < time.Minute {
		t.Errorf("LeaseFor(0,0) = %s, want at least the 60s margin", got)
	}
	if got := LeaseFor(-5, -1); got < time.Minute {
		t.Errorf("LeaseFor(-5,-1) = %s, want at least the 60s margin", got)
	}
}

// Shutdown mid-batch must hand back the claims it never drove. The whole batch is
// leased up front, so walking away parks every undriven order behind the full lease —
// with the default batch that is nine minutes of nothing happening to orders whose seats
// are already leaking. The lease is there to survive a crash, not to be the price of an
// orderly restart.
func TestShutdownHandsBackUndrivenClaims(t *testing.T) {
	orders := []store.StuckOrder{
		stuck("confirmation_pending"), stuck("confirmation_pending"), stuck("confirmation_pending"),
	}
	tr := &trace{}
	p := &ports{
		store: &fakeStore{tr: tr, claim: orders}, payments: &fakePayments{tr: tr},
		inventory: &fakeInventory{tr: tr}, journal: &fakeJournal{tr: tr},
		completer: &fakeCompleter{tr: tr}, trace: tr,
	}

	// Cancel as soon as the first order is driven: the remaining two are claimed but
	// undriven, which is exactly the shutdown case.
	ctx, cancel := context.WithCancel(context.Background())
	p.completer.onComplete = cancel

	r := New(p.store, p.payments, p.inventory, p.journal, p.completer,
		time.Minute, 8, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	resolved := r.RunOnce(ctx)

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (cancelled after the first order)", resolved)
	}
	if len(p.store.abandoned) != 2 {
		t.Fatalf("handed back %d undriven claims, want 2; abandoned=%v", len(p.store.abandoned), p.store.abandoned)
	}
	// The exact orders that were never driven, not just any two.
	for i, want := range []store.StuckOrder{orders[1], orders[2]} {
		if p.store.abandoned[i] != want.OrderID {
			t.Errorf("abandoned[%d] = %s, want %s", i, p.store.abandoned[i], want.OrderID)
		}
	}
	// The driven order completed: its claim is cleared, not abandoned.
	for _, id := range p.store.abandoned {
		if id == orders[0].OrderID {
			t.Error("handed back the claim of an order that was fully driven")
		}
	}
}

// Row: confirmation_pending — capture returned 200, money is KNOWN captured.
func TestConfirmationPendingConfirmsAndCompletes(t *testing.T) {
	order := stuck("confirmation_pending")
	p, resolved := run(t, []store.StuckOrder{order}, nil)

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	// The evidence is already in hand. Asking payments would be a pointless round trip
	// against a question already answered — and the ADR calls that lookup out as "No".
	if p.payments.calls != 0 {
		t.Errorf("payments lookup called %d times; known-captured needs no lookup", p.payments.calls)
	}
	if p.inventory.confirmed != 1 {
		t.Errorf("confirmed = %d, want 1", p.inventory.confirmed)
	}
	if len(p.completer.completed) != 1 {
		t.Fatalf("completed %d orders, want 1", len(p.completer.completed))
	}
	if p.completer.completed[0].OrderID != order.OrderID {
		t.Errorf("completed the wrong order")
	}
	if p.store.cleared != 1 {
		t.Errorf("recovery claim cleared %d times, want 1", p.store.cleared)
	}
	if len(p.store.released) != 0 || len(p.store.parked) != 0 {
		t.Errorf("captured order must not be released or parked: released=%v parked=%v",
			p.store.released, p.store.parked)
	}
	// The seat must be secured before the order is completed, and the claim released
	// only once the completion is durable: clearing first would drop the lease on an
	// order that still owes its completion.
	p.trace.mustPrecede(t, "inventory.Confirm", "completer.Complete")
	p.trace.mustPrecede(t, "completer.Complete", "store.ClearRecoveryClaim")
}

// Row: confirmation_pending + confirm terminally impossible — captured money, no seat.
func TestCapturedOrderWithGoneClaimIsParkedNotResolved(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("confirmation_pending")}, func(p *ports) {
		p.inventory.confirmErr = ErrClaimGone
	})

	// Parking IS the resolution here: the runner did its job by refusing to guess.
	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (parking is a terminal decision)", resolved)
	}
	if len(p.store.parked) != 1 {
		t.Fatalf("parked %d orders, want 1", len(p.store.parked))
	}
	// Completing would sell a claim that is gone; releasing would strand the buyer's
	// money. Both are worse than a human looking at it.
	if len(p.completer.completed) != 0 {
		t.Error("must not complete an order whose claim is gone")
	}
	if p.inventory.releases != 0 {
		t.Error("must not release the claim of a captured order — that strands the money")
	}
	if len(p.store.released) != 0 {
		t.Error("must not mark a captured order released")
	}
}

// Row: created + no operation — payments never bound a charge, so no side effect exists.
func TestCreatedWithNoOperationIsNotAttemptedThenReleased(t *testing.T) {
	order := stuck("created")
	p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) {
		p.payments.found = false
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if p.payments.calls != 1 {
		t.Fatalf("payments lookup called %d times, want 1: `created` is ambiguous and must be resolved", p.payments.calls)
	}
	// `not_attempted`, never `timeout`: nothing timed out. Overloading a PSP answer to
	// mean "we never called it" makes the audit column lie to whoever reads it next.
	if len(p.store.outcomes) != 1 || p.store.outcomes[0] != "not_attempted" {
		t.Fatalf("outcomes = %v, want exactly [not_attempted]", p.store.outcomes)
	}
	if p.inventory.releases != 1 {
		t.Errorf("releases = %d, want 1", p.inventory.releases)
	}
	if len(p.journal.facts) != 1 {
		t.Errorf("journalled %d order.failed facts, want 1", len(p.journal.facts))
	}
	if len(p.store.released) != 1 {
		t.Fatalf("marked released %d times, want 1", len(p.store.released))
	}
	// The outcome must reach MarkReleased: it is what makes the release restartable,
	// and it is what the terminal status is derived from.
	if p.store.released[0].TerminalOutcome != "not_attempted" {
		t.Errorf("MarkReleased got TerminalOutcome %q, want not_attempted",
			p.store.released[0].TerminalOutcome)
	}
	if len(p.completer.completed) != 0 {
		t.Error("must not complete an order that never charged")
	}
	// Evidence, then answer, then act: the lookup must precede the recorded outcome, and
	// the outcome must be durable before the seat is released.
	p.trace.mustPrecede(t, "payments.LookupOperation", "store.RecordTerminalOutcome")
	p.trace.mustPrecede(t, "store.RecordTerminalOutcome", "inventory.Release")
	p.trace.mustPrecede(t, "journal.OrderFailed", "store.MarkReleased")
}

// Row: created + payments resolved `captured` — crashed before persisting the state.
func TestCreatedResolvedCapturedConfirmsAndCompletes(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("created")}, func(p *ports) {
		p.payments.found = true
		p.payments.op = Operation{Resolved: true, Status: "captured"}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if p.inventory.confirmed != 1 {
		t.Errorf("confirmed = %d, want 1", p.inventory.confirmed)
	}
	if len(p.completer.completed) != 1 {
		t.Errorf("completed %d orders, want 1: captured money must buy the seat", len(p.completer.completed))
	}
	// The money is captured, so no terminal-failure outcome may be recorded and the
	// claim must not be released.
	if len(p.store.outcomes) != 0 {
		t.Errorf("recorded outcomes %v for a captured order", p.store.outcomes)
	}
	if p.inventory.releases != 0 {
		t.Error("must not release the claim of a captured order")
	}
}

// Row: created + payments resolved `declined`/`timeout` — a durable answer proving no
// side effect.
func TestCreatedResolvedTerminalFailureRecordsOutcomeThenReleases(t *testing.T) {
	for _, status := range []string{"declined", "timeout"} {
		t.Run(status, func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{stuck("created")}, func(p *ports) {
				p.payments.found = true
				p.payments.op = Operation{Resolved: true, Status: status}
			})

			if resolved != 1 {
				t.Fatalf("resolved = %d, want 1", resolved)
			}
			// The recorded outcome is the PSP's own answer, not a substitute for it.
			if len(p.store.outcomes) != 1 || p.store.outcomes[0] != status {
				t.Fatalf("outcomes = %v, want exactly [%s]", p.store.outcomes, status)
			}
			if p.inventory.releases != 1 {
				t.Errorf("releases = %d, want 1", p.inventory.releases)
			}
			if len(p.store.released) != 1 || p.store.released[0].TerminalOutcome != status {
				t.Errorf("MarkReleased outcome = %v, want %s", p.store.released, status)
			}
			if len(p.completer.completed) != 0 {
				t.Error("must not complete a declined/timed-out order")
			}
			// The ordering IS the invariant: the outcome must be durable before the
			// release is attempted, or a crash mid-release leaves no evidence the answer
			// was ever known and the release is not restartable. Per-port counters cannot
			// see this — they pass just as well with the order reversed.
			p.trace.mustPrecede(t, "store.RecordTerminalOutcome", "inventory.Release")
			p.trace.mustPrecede(t, "inventory.Release", "journal.OrderFailed")
			p.trace.mustPrecede(t, "journal.OrderFailed", "store.MarkReleased")
		})
	}
}

// Row: created + operation bound but unresolved — this is payment_unknown, and
// resolving it needs real-PSP status (TKT-56).
func TestCreatedWithUnresolvedOperationIsParkedForPSPStatus(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("created")}, func(p *ports) {
		p.payments.found = true
		p.payments.op = Operation{Resolved: false}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (parking is a terminal decision)", resolved)
	}
	if len(p.store.parked) != 1 {
		t.Fatalf("parked %d orders, want 1", len(p.store.parked))
	}
	// The whole point of the design: with the side effect genuinely unknown, the runner
	// must take NO action that assumes an answer. Not a release, not a completion, and
	// above all not a recorded outcome — that column may only hold proven results.
	if len(p.store.outcomes) != 0 {
		t.Errorf("recorded outcome %v for an unknown payment result — that column may only hold proven outcomes", p.store.outcomes)
	}
	if len(p.completer.completed) != 0 {
		t.Error("must not complete an order whose payment result is unknown")
	}
	if p.inventory.releases != 0 {
		t.Error("must not release a claim whose payment might have captured")
	}
	if len(p.store.released) != 0 {
		t.Error("must not mark released an order whose payment result is unknown")
	}
}

// release_pending is the restart case: the outcome was recorded before the release was
// attempted, so the re-drive must reuse it rather than resolve anything again.
func TestReleasePendingReusesTheRecordedOutcome(t *testing.T) {
	order := stuck("release_pending")
	order.TerminalOutcome = "declined"
	p, resolved := run(t, []store.StuckOrder{order}, nil)

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if p.payments.calls != 0 {
		t.Errorf("payments lookup called %d times; the answer is already recorded", p.payments.calls)
	}
	if len(p.store.outcomes) != 0 {
		t.Errorf("re-recorded outcome %v; it was already durable", p.store.outcomes)
	}
	if p.inventory.releases != 1 {
		t.Errorf("releases = %d, want 1", p.inventory.releases)
	}
	if len(p.store.released) != 1 {
		t.Errorf("marked released %d times, want 1", len(p.store.released))
	}
}

// A release_pending row with no outcome is not self-describing. It must not be resolved
// by guessing an outcome — it goes back for a human.
func TestReleasePendingWithoutOutcomeFailsRatherThanGuess(t *testing.T) {
	order := stuck("release_pending")
	order.TerminalOutcome = ""
	p, resolved := run(t, []store.StuckOrder{order}, nil)

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if len(p.store.failed) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(p.store.failed))
	}
	if p.inventory.releases != 0 || len(p.store.released) != 0 {
		t.Error("must not release on an order whose recorded outcome is missing")
	}
}

// An order the runner cannot re-drive must be handed back, not silently dropped. This
// is the guard that keeps payment_unknown — excluded from the claim query on purpose —
// from being resolved here if it ever reaches the runner.
func TestUnrecoverableStatusIsHandedBack(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("payment_unknown")}, nil)

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if len(p.store.failed) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(p.store.failed))
	}
	if len(p.completer.completed) != 0 || len(p.store.released) != 0 || len(p.store.parked) != 0 {
		t.Error("an unrecoverable status must produce no transition at all")
	}
}

// A port failure must leave the order re-claimable rather than resolved: the next pass
// retries it. This is what makes every re-drive restartable.
func TestPortFailureLeavesTheOrderForTheNextPass(t *testing.T) {
	boom := errors.New("inventory unreachable")
	p, resolved := run(t, []store.StuckOrder{stuck("confirmation_pending")}, func(p *ports) {
		p.inventory.confirmErr = boom
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if len(p.store.failed) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(p.store.failed))
	}
	if !errors.Is(p.store.failed[0], boom) {
		t.Errorf("failure cause = %v, want it to wrap %v", p.store.failed[0], boom)
	}
	if len(p.completer.completed) != 0 {
		t.Error("must not complete when the claim confirmation failed")
	}
}

// A transport failure to the lookup is NOT evidence that no charge exists. Treating it
// as `not_attempted` would release a seat whose money may have been captured — exactly
// the inference §Decision 2 forbids.
func TestLookupFailureIsNotTreatedAsNoOperation(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("created")}, func(p *ports) {
		p.payments.err = errors.New("payments unreachable")
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if len(p.store.outcomes) != 0 {
		t.Errorf("recorded outcome %v from a failed lookup", p.store.outcomes)
	}
	if p.inventory.releases != 0 || len(p.store.released) != 0 {
		t.Error("must not release a claim on the strength of a failed lookup")
	}
	if len(p.store.failed) != 1 {
		t.Errorf("recorded %d failures, want 1", len(p.store.failed))
	}
}

// Inventory answers 200 for an already-released claim, so a 409 on release is NOT
// "already gone" — it means the claim is confirmed and the seat is sold, while this
// order's payment did not capture. Journalling order.failed against a confirmed claim
// would remove the seat from availability forever, attached to a failed order.
func TestFailedOrderWithConfirmedClaimIsParkedNotJournalled(t *testing.T) {
	order := stuck("release_pending")
	order.TerminalOutcome = "declined"
	p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) {
		p.inventory.releaseErr = ErrClaimNotReleasable
	})

	// Parking is the resolution: `confirmed` is terminal, so retrying cannot help.
	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (parking is a terminal decision)", resolved)
	}
	if len(p.store.parked) != 1 {
		t.Fatalf("parked %d orders, want 1", len(p.store.parked))
	}
	if len(p.journal.facts) != 0 {
		t.Error("must not journal order.failed against a confirmed claim")
	}
	if len(p.store.released) != 0 {
		t.Error("must not mark released an order whose claim is confirmed")
	}
}

// A journal failure must not let the order reach a terminal state: MarkReleased is what
// makes the release final, and an order.failed fact that was never recorded would leave
// the buyer's failed checkout absent from the journal for good. The next pass retries
// both — the release is idempotent and the fact write collapses on its deterministic id.
func TestJournalFailureLeavesTheOrderUnreleased(t *testing.T) {
	order := stuck("release_pending")
	order.TerminalOutcome = "declined"
	p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) {
		p.journal.err = errors.New("journal unavailable")
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if len(p.store.released) != 0 {
		t.Error("must not mark released when the order.failed fact was not journalled")
	}
	if len(p.store.failed) != 1 {
		t.Errorf("recorded %d failures, want 1 (the order must stay re-claimable)", len(p.store.failed))
	}
}

// A completion failure must not clear the recovery claim: the order still owes its
// completion, and clearing the claim would stop anything from ever re-driving it.
func TestCompletionFailureLeavesTheClaimForTheNextPass(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("confirmation_pending")}, func(p *ports) {
		p.completer.err = errors.New("completion conflict")
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if p.store.cleared != 0 {
		t.Error("must not clear the recovery claim of an order that never completed")
	}
	if len(p.store.failed) != 1 {
		t.Errorf("recorded %d failures, want 1", len(p.store.failed))
	}
}

// Every order in a batch is driven, and one order's failure does not abort the pass.
func TestBatchContinuesPastAFailedOrder(t *testing.T) {
	orders := []store.StuckOrder{stuck("payment_unknown"), stuck("confirmation_pending")}
	p, resolved := run(t, orders, nil)

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (the second order must still be driven)", resolved)
	}
	if len(p.completer.completed) != 1 {
		t.Errorf("completed %d orders, want 1", len(p.completer.completed))
	}
	if len(p.store.failed) != 1 {
		t.Errorf("recorded %d failures, want 1", len(p.store.failed))
	}
}
