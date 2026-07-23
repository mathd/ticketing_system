package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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
	queued    []string           // QueueForCompensation reasons
	refunded  []store.StuckOrder // MarkRefunded
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

func (f *fakeStore) QueueForCompensation(_ context.Context, _, _ uuid.UUID, reason string) error {
	f.tr.add("store.QueueForCompensation")
	f.queued = append(f.queued, reason)
	return nil
}

func (f *fakeStore) MarkRefunded(_ context.Context, s store.StuckOrder) error {
	f.tr.add("store.MarkRefunded")
	f.refunded = append(f.refunded, s)
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

	status       PSPStatus
	statusErr    error
	statusCalls  int
	voidResult   CompensationResult
	voidErr      error
	voidCalls    int
	refundResult CompensationResult
	refundErr    error
	refundCalls  int
}

func (f *fakePayments) LookupOperation(context.Context, uuid.UUID, string) (Operation, bool, error) {
	f.tr.add("payments.LookupOperation")
	f.calls++
	return f.op, f.found, f.err
}

func (f *fakePayments) Status(context.Context, uuid.UUID, string) (PSPStatus, error) {
	f.tr.add("payments.Status")
	f.statusCalls++
	return f.status, f.statusErr
}

func (f *fakePayments) Void(context.Context, uuid.UUID, string) (CompensationResult, error) {
	f.tr.add("payments.Void")
	f.voidCalls++
	return f.voidResult, f.voidErr
}

func (f *fakePayments) Refund(context.Context, uuid.UUID, string) (CompensationResult, error) {
	f.tr.add("payments.Refund")
	f.refundCalls++
	return f.refundResult, f.refundErr
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

// externalCalls counts the trace steps that leave the process — the calls
// MaxCallsPerOrder budgets and LeaseFor must outlast. Store writes are local.
func (t *trace) externalCalls() int {
	var n int
	for _, s := range t.steps {
		if strings.HasPrefix(s, "payments.") || strings.HasPrefix(s, "inventory.") || strings.HasPrefix(s, "journal.") {
			n++
		}
	}
	return n
}

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

// COS6: MaxCallsPerOrder is pinned by ENUMERATING the longest external-call chain, not
// by referencing the constant back at itself. The TKT-115 worst case is the compensation
// re-derivation chain: operation lookup → PSP status → first compensation refused with
// 409 → status re-derivation → correct compensation (or release) → order.failed journal
// submission = 6 calls. LeaseFor derives from the same constant, so the two grow
// together by construction — this test exists so a future shortening of the constant
// fails loudly against the enumerated chain (ADR-032 §Consequences).
func TestMaxCallsPerOrderCoversTheLongestChain(t *testing.T) {
	longestChain := []string{
		"payments.LookupOperation",
		"payments.Status",
		"payments.Refund (409 wrong compensation)",
		"payments.Status (re-derivation)",
		"payments.Void or inventory.Release",
		"journal.OrderFailed",
	}
	if MaxCallsPerOrder < len(longestChain) {
		t.Fatalf("MaxCallsPerOrder = %d cannot cover the %d-call chain %v",
			MaxCallsPerOrder, len(longestChain), longestChain)
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
// Since TKT-115 the runner refunds in the SAME pass: queue for compensation (keeping
// the claim), re-derive from PSP status, refund, and only after payments' 200 mark the
// order refunded. Payments appends payment.refunded; commerce never touches the journal
// beyond its own order.failed.
func TestCapturedOrderWithGoneClaimIsRefundedSamePass(t *testing.T) {
	order := stuck("confirmation_pending")
	p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) {
		p.inventory.confirmErr = ErrClaimGone
		p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
			AuthorizedAmount: 5000, CapturedAmount: 5000, Currency: "CAD"}
		p.payments.refundResult = CompensationResult{Status: "refunded"}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if len(p.store.queued) != 1 {
		t.Fatalf("queued %d orders for compensation, want 1", len(p.store.queued))
	}
	if p.payments.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1: the compensation kind is re-derived from evidence", p.payments.statusCalls)
	}
	if p.payments.refundCalls != 1 {
		t.Fatalf("refund calls = %d, want 1", p.payments.refundCalls)
	}
	if len(p.store.refunded) != 1 || p.store.refunded[0].OrderID != order.OrderID {
		t.Fatalf("MarkRefunded = %v, want exactly this order", p.store.refunded)
	}
	if len(p.journal.facts) != 1 {
		t.Errorf("journalled %d order.failed facts, want 1", len(p.journal.facts))
	}
	if len(p.completer.completed) != 0 {
		t.Error("must not complete an order whose claim is gone")
	}
	if len(p.store.parked) != 0 {
		t.Error("a refundable order must be refunded, not parked")
	}
	if len(p.store.outcomes) != 0 {
		t.Errorf("recorded outcomes %v; refunded money is NOT a no-side-effect outcome (ADR-032)", p.store.outcomes)
	}
	// F4 (plan-final): only a payments refund success may precede MarkRefunded, and the
	// order.failed fact must be durable before the terminal transition.
	p.trace.mustPrecede(t, "payments.Status", "payments.Refund")
	p.trace.mustPrecede(t, "payments.Refund", "store.MarkRefunded")
	p.trace.mustPrecede(t, "journal.OrderFailed", "store.MarkRefunded")
	// The EXECUTED chain must fit the budget the lease is derived from (ai-review B6:
	// the enumerating test alone is a hand-maintained list; this counts the real trace).
	if calls := p.trace.externalCalls(); calls > MaxCallsPerOrder {
		t.Fatalf("refund chain made %d external calls, exceeding MaxCallsPerOrder=%d — the lease no longer covers its own pass", calls, MaxCallsPerOrder)
	}
}

// Refund 502: the compensation stays bound in payments and the order stays claimable —
// never terminal, never parked, and above all never marked refunded (COS2).
func TestRefundProviderUnresolvedRetriesLater(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("reconciliation_required")}, func(p *ports) {
		p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
			AuthorizedAmount: 5000, CapturedAmount: 5000, Currency: "CAD"}
		p.payments.refundErr = ErrProviderUnresolved
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0 (retry later)", resolved)
	}
	if len(p.store.failed) != 1 {
		t.Fatalf("ReleaseStuckOrder calls = %d, want 1 (backoff)", len(p.store.failed))
	}
	if len(p.store.refunded) != 0 {
		t.Fatal("a 502 must never mark the order refunded")
	}
	if len(p.store.parked) != 0 {
		t.Fatal("a recoverable 502 must not park")
	}
	if len(p.store.outcomes) != 0 || p.inventory.releases != 0 {
		t.Fatal("an unresolved compensation proves nothing; no outcome, no release")
	}
}

// Refund 409 = wrong compensation for the current evidence: re-derive next pass (the
// evidence moved between status and refund), never success, never a terminal park.
func TestRefundWrongCompensationReDerivesNextPass(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("reconciliation_required")}, func(p *ports) {
		p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
			AuthorizedAmount: 5000, CapturedAmount: 5000, Currency: "CAD"}
		p.payments.refundErr = ErrWrongCompensation
	})

	if resolved != 0 {
		t.Fatalf("resolved = %d, want 0", resolved)
	}
	if len(p.store.failed) != 1 {
		t.Fatalf("ReleaseStuckOrder calls = %d, want 1", len(p.store.failed))
	}
	if len(p.store.refunded) != 0 || len(p.store.parked) != 0 {
		t.Fatal("a 409 is neither success nor a terminal park — the next pass re-derives")
	}
}

// F1 (plan-final): an operation that predates durable provider evidence reports
// captured with a ZERO captured amount — a refund against it 409s forever on a zero
// basis. Recognize the shape and park in ONE pass, without burning the retry budget.
func TestPreDurableEvidenceOperationParksInOnePass(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("reconciliation_required")}, func(p *ports) {
		p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
			AuthorizedAmount: 0, CapturedAmount: 0, Currency: "EUR"}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (parking is the decision)", resolved)
	}
	if p.payments.refundCalls != 0 {
		t.Fatal("a zero-evidence operation must not be refunded — the basis would be 0")
	}
	if len(p.store.parked) != 1 || !strings.Contains(p.store.parked[0], "predates durable provider evidence") {
		t.Fatalf("parked = %v, want the self-describing pre-0002 reason", p.store.parked)
	}
}

// A reconciliation_required order whose evidence is captured money is refunded (COS2).
func TestReconciliationRequiredRefundsCapturedMoney(t *testing.T) {
	order := stuck("reconciliation_required")
	p, resolved := run(t, []store.StuckOrder{order}, func(p *ports) {
		p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
			AuthorizedAmount: 5000, CapturedAmount: 5000, Currency: "CAD"}
		p.payments.refundResult = CompensationResult{Status: "refunded"}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if p.payments.refundCalls != 1 || len(p.store.refunded) != 1 {
		t.Fatalf("refund calls = %d, refunded = %v; want the refund driven to MarkRefunded",
			p.payments.refundCalls, p.store.refunded)
	}
	p.trace.mustPrecede(t, "payments.Refund", "store.MarkRefunded")
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

// Rows: created/payment_unknown + operation bound but unresolved — resolved against
// real PSP status since TKT-115. The decision table below is ADR-032's: captured buys
// the seat; an exact provider decline/timeout records ITSELF (never blurred into
// no_side_effect — the audit column keeps decline and timeout distinguishable, G1);
// authorized voids FIRST (an authorization is not terminal-no-side-effect); an already-
// voided hold records no_side_effect (P2-3: no synthetic payment.voided — commerce never
// initiated a compensation); refunded finishes as a refunded order; unknown retries.
func TestUnresolvedOperationStatusDecisionTable(t *testing.T) {
	for _, status := range []string{"created", "payment_unknown"} {
		t.Run(status+"/captured confirms and completes", func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{stuck(status)}, func(p *ports) {
				p.payments.found = true
				p.payments.op = Operation{Resolved: false}
				p.payments.status = PSPStatus{Outcome: "captured", Captured: true, Authorized: true,
					AuthorizedAmount: 5000, CapturedAmount: 5000, Currency: "CAD"}
			})
			if resolved != 1 {
				t.Fatalf("resolved = %d, want 1", resolved)
			}
			if p.inventory.confirmed != 1 || len(p.completer.completed) != 1 {
				t.Fatal("captured money must buy the seat: confirm + complete")
			}
			if len(p.store.outcomes) != 0 || p.inventory.releases != 0 {
				t.Fatal("captured is not a terminal-failure outcome")
			}
			p.trace.mustPrecede(t, "payments.Status", "inventory.Confirm")
		})

		t.Run(status+"/authorized voids before releasing", func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{stuck(status)}, func(p *ports) {
				p.payments.found = true
				p.payments.op = Operation{Resolved: false}
				p.payments.status = PSPStatus{Outcome: "authorized", Authorized: true,
					AuthorizedAmount: 5000, Currency: "CAD"}
				p.payments.voidResult = CompensationResult{Status: "voided"}
			})
			if resolved != 1 {
				t.Fatalf("resolved = %d, want 1", resolved)
			}
			if p.payments.voidCalls != 1 {
				t.Fatal("an authorized hold is NOT terminal-no-side-effect: it must be voided first")
			}
			if len(p.store.outcomes) != 1 || p.store.outcomes[0] != "no_side_effect" {
				t.Fatalf("outcomes = %v, want [no_side_effect]", p.store.outcomes)
			}
			// The void must be durable at the provider before the outcome is recorded,
			// and the outcome durable before the seat is released (ADR-016 ordering).
			p.trace.mustPrecede(t, "payments.Void", "store.RecordTerminalOutcome")
			p.trace.mustPrecede(t, "store.RecordTerminalOutcome", "inventory.Release")
			if calls := p.trace.externalCalls(); calls > MaxCallsPerOrder {
				t.Fatalf("void chain made %d external calls, exceeding MaxCallsPerOrder=%d", calls, MaxCallsPerOrder)
			}
		})

		for _, terminal := range []string{"declined", "timeout"} {
			t.Run(status+"/"+terminal+" records the exact outcome", func(t *testing.T) {
				p, resolved := run(t, []store.StuckOrder{stuck(status)}, func(p *ports) {
					p.payments.found = true
					p.payments.op = Operation{Resolved: false}
					p.payments.status = PSPStatus{Outcome: terminal, TerminalNoSideEffect: true}
				})
				if resolved != 1 {
					t.Fatalf("resolved = %d, want 1", resolved)
				}
				if len(p.store.outcomes) != 1 || p.store.outcomes[0] != terminal {
					t.Fatalf("outcomes = %v, want [%s]: the provider's exact answer, never blurred", p.store.outcomes, terminal)
				}
				if p.payments.voidCalls != 0 || p.payments.refundCalls != 0 {
					t.Fatal("a terminal provider decision needs no compensation")
				}
				p.trace.mustPrecede(t, "store.RecordTerminalOutcome", "inventory.Release")
			})
		}

		t.Run(status+"/voided records no_side_effect without compensating", func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{stuck(status)}, func(p *ports) {
				p.payments.found = true
				p.payments.op = Operation{Resolved: false}
				p.payments.status = PSPStatus{Outcome: "voided", TerminalNoSideEffect: true}
			})
			if resolved != 1 {
				t.Fatalf("resolved = %d, want 1", resolved)
			}
			if len(p.store.outcomes) != 1 || p.store.outcomes[0] != "no_side_effect" {
				t.Fatalf("outcomes = %v, want [no_side_effect]", p.store.outcomes)
			}
			// P2-3: the hold was released without commerce's involvement. No journal fact
			// beyond order.failed is fabricated, and no compensation is driven.
			if p.payments.voidCalls != 0 || p.payments.refundCalls != 0 {
				t.Fatal("an externally-released hold needs no compensation call")
			}
			if len(p.journal.facts) != 1 {
				t.Errorf("journalled %d order.failed facts, want 1", len(p.journal.facts))
			}
		})

		t.Run(status+"/refunded finishes as a refunded order", func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{stuck(status)}, func(p *ports) {
				p.payments.found = true
				p.payments.op = Operation{Resolved: false}
				p.payments.status = PSPStatus{Outcome: "refunded"}
			})
			if resolved != 1 {
				t.Fatalf("resolved = %d, want 1", resolved)
			}
			if len(p.store.refunded) != 1 {
				t.Fatalf("MarkRefunded calls = %d, want 1", len(p.store.refunded))
			}
			if len(p.store.outcomes) != 0 {
				t.Fatalf("outcomes = %v; refunded money moved and came back — never a no-side-effect outcome", p.store.outcomes)
			}
			if p.payments.refundCalls != 0 {
				t.Fatal("the refund already completed; driving another is a duplicate compensation")
			}
		})

		t.Run(status+"/unknown retries without acting", func(t *testing.T) {
			p, resolved := run(t, []store.StuckOrder{stuck(status)}, func(p *ports) {
				p.payments.found = true
				p.payments.op = Operation{Resolved: false}
				p.payments.status = PSPStatus{Outcome: "unknown"}
			})
			if resolved != 0 {
				t.Fatalf("resolved = %d, want 0 (retry later)", resolved)
			}
			if len(p.store.failed) != 1 {
				t.Fatalf("ReleaseStuckOrder calls = %d, want 1", len(p.store.failed))
			}
			if len(p.store.outcomes) != 0 || p.inventory.releases != 0 || len(p.store.parked) != 0 {
				t.Fatal("unknown proves nothing: no outcome, no release, no park")
			}
		})
	}
}

// COS3: the status-replay deadline. Past it, the runner parks BEFORE calling status —
// a replay would mint a second PaymentIntent — and a payments-side 409 (defense in
// depth) parks in one pass too, never burning the retry budget.
func TestExpiredReplayDeadlineParksWithoutCallingStatus(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	p, resolved := run(t, []store.StuckOrder{stuck("payment_unknown")}, func(p *ports) {
		p.payments.found = true
		p.payments.op = Operation{Resolved: false, OccurredAt: expired.Add(-24 * time.Hour), StatusReplayDeadlineAt: &expired}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1 (parking is the decision)", resolved)
	}
	if p.payments.statusCalls != 0 {
		t.Fatal("an expired replay window must park BEFORE the status call — the call is the hazard")
	}
	if len(p.store.parked) != 1 || !strings.Contains(p.store.parked[0], "replay window expired") {
		t.Fatalf("parked = %v, want the self-describing expired-window reason", p.store.parked)
	}
	if len(p.store.outcomes) != 0 || p.inventory.releases != 0 {
		t.Fatal("expiry proves nothing about the money: no outcome, no release")
	}
}

func TestStatusReplayExpired409ParksInOnePass(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("payment_unknown")}, func(p *ports) {
		p.payments.found = true
		p.payments.op = Operation{Resolved: false}
		p.payments.statusErr = ErrReplayWindowExpired
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if len(p.store.parked) != 1 || !strings.Contains(p.store.parked[0], "replay window expired") {
		t.Fatalf("parked = %v, want the expired-window park", p.store.parked)
	}
	if len(p.store.failed) != 0 {
		t.Fatal("expiry is not retryable — retrying cannot un-expire the idempotency key")
	}
}

// A within-window unresolved operation IS resolved via status (the deadline gates the
// hazard, not the resolution).
func TestWithinWindowUnresolvedOperationCallsStatus(t *testing.T) {
	future := time.Now().Add(23 * time.Hour)
	p, resolved := run(t, []store.StuckOrder{stuck("payment_unknown")}, func(p *ports) {
		p.payments.found = true
		p.payments.op = Operation{Resolved: false, OccurredAt: time.Now().Add(-time.Hour), StatusReplayDeadlineAt: &future}
		p.payments.status = PSPStatus{Outcome: "declined", TerminalNoSideEffect: true}
	})

	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if p.payments.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1", p.payments.statusCalls)
	}
	if len(p.store.parked) != 0 {
		t.Fatalf("parked = %v; a within-window operation is resolvable", p.store.parked)
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
// is the guard that keeps a status outside the decision table (a terminal row that
// somehow reached the claim set) from being resolved by guesswork.
func TestUnrecoverableStatusIsHandedBack(t *testing.T) {
	p, resolved := run(t, []store.StuckOrder{stuck("completed")}, nil)

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
	orders := []store.StuckOrder{stuck("completed"), stuck("confirmation_pending")}
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
