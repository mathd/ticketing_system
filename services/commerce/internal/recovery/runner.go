// Package recovery re-drives checkouts that died mid-protocol.
//
// Before this, commerce had no background work of any kind: an order that reached
// `created` or `confirmation_pending` and lost its request was advanced only by a
// byte-identical checkout replay, which nothing in the system generates. The buyer's
// claim stayed `finalizing` — counted against availability and exempt from expiry — so
// the seat leaked permanently and the order never resolved.
//
// The runner implements ADR-016 §Decision 3's decision table. Its governing rule is
// §Decision 2: decide from durable evidence, never from an inference about what a
// failed transport did.
package recovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
	"ticketing/services/commerce/internal/worklease"
)

// Payments resolves what a payment operation actually did — the recorded outcome via
// LookupOperation, and since TKT-115 the real-PSP surface behind /internal/psp/*:
// provider-neutral status, and the void/refund compensations whose amounts and journal
// facts payments owns (ADR-011 sole-writer; commerce only drives the calls).
type Payments interface {
	// LookupOperation returns the recorded outcome. found=false means no operation
	// exists — evidence the charge was never submitted.
	LookupOperation(ctx context.Context, organizerID uuid.UUID, key string) (op Operation, found bool, err error)
	// Status resolves provider state (GET /internal/psp/status). Sentinels:
	// ErrReplayWindowExpired (park), ErrProviderUnresolved (retry), ErrOperationNotFound.
	Status(ctx context.Context, organizerID uuid.UUID, key string) (PSPStatus, error)
	// Void cancels an authorized, uncaptured hold (POST /internal/psp/void).
	Void(ctx context.Context, organizerID uuid.UUID, key string) (CompensationResult, error)
	// Refund returns captured money (POST /internal/psp/refund). ErrWrongCompensation
	// means the stored evidence does not support a refund: re-derive, not failure.
	Refund(ctx context.Context, organizerID uuid.UUID, key string) (CompensationResult, error)
}

type Operation struct {
	// Resolved is false when the operation was bound but carries no terminal result.
	// That is the payment_unknown case, resolved against PSP status since TKT-115.
	Resolved bool
	Status   string
	// OccurredAt is the durable bind time — the basis of the status-replay deadline.
	OccurredAt time.Time
	// StatusReplayDeadlineAt bounds status resolution when set (unresolved, ref-less
	// operation under a provider that expires idempotency keys — Stripe ~24h). Past it,
	// the runner parks: a replay would mint a NEW PaymentIntent (ADR-032 amendment).
	StatusReplayDeadlineAt *time.Time
}

// PSPStatus is payments' provider-neutral status answer: the normalized outcome plus
// the amounts the durable evidence proves. Integer minor units; never provider refs.
type PSPStatus struct {
	Outcome              string `json:"outcome"`
	TerminalNoSideEffect bool   `json:"terminal_no_side_effect"`
	Captured             bool   `json:"captured"`
	Authorized           bool   `json:"authorized"`
	AuthorizedAmount     int64  `json:"authorized_amount"`
	CapturedAmount       int64  `json:"captured_amount"`
	Currency             string `json:"currency"`
}

// CompensationResult is payments' answer to a driven void/refund. Replay reports that
// the compensation had already completed — same terminal state, idempotent.
type CompensationResult struct {
	Status string `json:"status"`
	Replay bool   `json:"replay"`
}

// Inventory owns the claim. Both calls are idempotent for a repeated target, including
// the "committed but response lost" case that makes an ambiguous release ambiguous.
type Inventory interface {
	Confirm(ctx context.Context, organizerID, holdID uuid.UUID) error
	Release(ctx context.Context, organizerID, holdID uuid.UUID) error
	// ErrClaimGone reports a claim that can never be confirmed (released/expired).
	// Distinguishing it from a transport failure is what separates "retry later" from
	// "this money can never buy this seat".
}

// ErrClaimGone is returned by Inventory.Confirm when the claim is terminally
// unconfirmable. The order then holds captured money for a seat it cannot have.
var ErrClaimGone = errors.New("inventory claim is gone")

// Journal records order facts. Failures are retried; the fact write is idempotent.
type Journal interface {
	OrderFailed(ctx context.Context, s store.StuckOrder) error
}

// Completer finishes an order whose claim is confirmed, owing its completion event.
type Completer interface {
	Complete(ctx context.Context, s store.StuckOrder) error
}

// Store is the durable state the runner decides against and writes back to. It is a
// port rather than a *sql.DB so the decision table can be exercised against fakes: the
// question these tests must answer is "which transition did the runner choose for this
// evidence", and that is unreadable through SQL side effects alone. The transitions
// themselves are covered against real PostgreSQL by the store's smoke tests.
type Store interface {
	ClaimStuckOrders(ctx context.Context, limit int, lease time.Duration) ([]store.StuckOrder, error)
	RecordTerminalOutcome(ctx context.Context, orderID, claimID uuid.UUID, outcome string) error
	ParkForReconciliation(ctx context.Context, orderID, claimID uuid.UUID, reason string) error
	QueueForCompensation(ctx context.Context, orderID, claimID uuid.UUID, reason string) error
	MarkRefunded(ctx context.Context, s store.StuckOrder) error
	ClearRecoveryClaim(ctx context.Context, orderID, claimID uuid.UUID) error
	AbandonRecoveryClaim(ctx context.Context, orderID, claimID uuid.UUID) error
	MarkReleased(ctx context.Context, s store.StuckOrder) error
	ReleaseStuckOrder(ctx context.Context, orderID, claimID uuid.UUID, cause error) error
	// Backlog reports the parked population for observability only. It is never read
	// by a recovery decision — a runner that steered on its own queue depth would be
	// deciding from an aggregate instead of from durable per-order evidence, which is
	// exactly what ADR-016 §Decision 2 forbids.
	Backlog(ctx context.Context) (store.RecoveryBacklog, error)
}

// DBStore binds Store to the commerce store package.
type DBStore struct {
	DB *sql.DB
}

func (d DBStore) ClaimStuckOrders(ctx context.Context, limit int, lease time.Duration) ([]store.StuckOrder, error) {
	return store.ClaimStuckOrders(ctx, d.DB, limit, lease)
}

func (d DBStore) Backlog(ctx context.Context) (store.RecoveryBacklog, error) {
	return store.ReadRecoveryBacklog(ctx, d.DB)
}

func (d DBStore) RecordTerminalOutcome(ctx context.Context, orderID, claimID uuid.UUID, outcome string) error {
	return store.RecordTerminalOutcome(ctx, d.DB, orderID, claimID, outcome)
}

func (d DBStore) ParkForReconciliation(ctx context.Context, orderID, claimID uuid.UUID, reason string) error {
	return store.ParkForReconciliation(ctx, d.DB, orderID, claimID, reason)
}

func (d DBStore) ClearRecoveryClaim(ctx context.Context, orderID, claimID uuid.UUID) error {
	return store.ClearRecoveryClaim(ctx, d.DB, orderID, claimID)
}

func (d DBStore) AbandonRecoveryClaim(ctx context.Context, orderID, claimID uuid.UUID) error {
	return store.AbandonRecoveryClaim(ctx, d.DB, orderID, claimID)
}

func (d DBStore) QueueForCompensation(ctx context.Context, orderID, claimID uuid.UUID, reason string) error {
	return store.QueueForCompensation(ctx, d.DB, orderID, claimID, reason)
}

func (d DBStore) MarkRefunded(ctx context.Context, s store.StuckOrder) error {
	return store.MarkRefunded(ctx, d.DB, s)
}

func (d DBStore) MarkReleased(ctx context.Context, s store.StuckOrder) error {
	return store.MarkReleased(ctx, d.DB, s)
}

func (d DBStore) ReleaseStuckOrder(ctx context.Context, orderID, claimID uuid.UUID, cause error) error {
	return store.ReleaseStuckOrder(ctx, d.DB, orderID, claimID, cause)
}

type Runner struct {
	store     Store
	payments  Payments
	inventory Inventory
	journal   Journal
	completer Completer
	interval  time.Duration
	batch     int
	lease     time.Duration
	log       *slog.Logger
}

// MaxCallsPerOrder is the longest external-call chain one order can drive. Since
// TKT-115 that is the compensation re-derivation chain: operation lookup, PSP status,
// a compensation the evidence refuses (409), the status re-derivation, the correct
// compensation (or the inventory release), then the fact journal submission. LeaseFor
// derives from this constant, so lease and chain grow together (ADR-032 §Consequences);
// the enumerating test pins the chain itself.
const MaxCallsPerOrder = 6

// LeaseFor sizes a sequential batch from the recovery client's timeout.
func LeaseFor(batch int, callTimeout time.Duration) (time.Duration, error) {
	if batch <= 0 {
		batch = 1
	}
	if callTimeout <= 0 {
		callTimeout = 10 * time.Second
	}
	return worklease.ForBatch(batch, MaxCallsPerOrder, callTimeout, 60*time.Second)
}

func New(st Store, payments Payments, inventory Inventory, journal Journal, completer Completer,
	interval time.Duration, batch int, lease time.Duration, log *slog.Logger) (*Runner, error) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if batch <= 0 {
		batch = 16
	}
	if lease <= 0 {
		return nil, errors.New("lease must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{store: st, payments: payments, inventory: inventory, journal: journal,
		completer: completer, interval: interval, batch: batch,
		lease: lease, log: log}, nil
}

// Run drives until ctx is cancelled. It runs once immediately: on restart, orders
// stranded by the process that died are the whole point, and waiting an interval to
// notice them keeps seats leaked for no reason.
func (r *Runner) Run(ctx context.Context) {
	r.RunOnce(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce claims a batch and re-drives each order. Returns how many reached a terminal
// state, for tests and for callers draining to quiescence.
func (r *Runner) RunOnce(ctx context.Context) int {
	orders, err := r.store.ClaimStuckOrders(ctx, r.batch, r.lease)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.log.ErrorContext(ctx, "claim stuck orders", "err", err)
		}
		return 0
	}
	var resolved int
	for i, s := range orders {
		if ctx.Err() != nil {
			// Shutdown. The rest of the batch is claimed but undriven, and abandoning it
			// parks those orders behind the full lease — with a big batch that is minutes
			// of doing nothing, for orders whose seats are already leaking. The lease
			// exists to survive a crash, not to be the cost of an orderly restart.
			r.releaseUndriven(orders[i:])
			return resolved
		}
		if err := r.drive(ctx, s); err != nil {
			r.fail(ctx, s, err)
			continue
		}
		resolved++
	}
	return resolved
}

// releaseUndriven hands back claims the pass never got to, so the next runner (or the
// next boot) can pick them up immediately rather than waiting out the lease.
//
// It uses a fresh, bounded context: the caller's is already cancelled, so reusing it
// would fail every one of these writes and defeat the point.
func (r *Runner) releaseUndriven(orders []store.StuckOrder) {
	if len(orders) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released int
	for _, s := range orders {
		// Conditional on the claim token, so a lease that lapsed and was re-claimed by a
		// successor mid-shutdown is left alone.
		if err := r.store.AbandonRecoveryClaim(ctx, s.OrderID, s.ClaimID); err != nil {
			r.log.WarnContext(ctx, "release undriven recovery claim",
				"order_id", s.OrderID, "err", err)
			continue
		}
		released++
	}
	r.log.InfoContext(ctx, "released undriven recovery claims on shutdown",
		"released", released, "of", len(orders))
}

// drive re-drives one order per ADR-016 §Decision 3.
func (r *Runner) drive(ctx context.Context, s store.StuckOrder) error {
	switch s.Status {
	case "confirmation_pending":
		// Capture returned 200, so the money is KNOWN captured. No PSP lookup: the
		// evidence is already in hand. Retry the claim confirmation and complete.
		return r.confirmAndComplete(ctx, s)

	case "release_pending":
		// A terminal answer was already recorded before the release was attempted.
		// Retry the idempotent release and finish.
		if s.TerminalOutcome == "" {
			// Should not happen: the outcome is recorded before the state is set. If it
			// does, the row is not self-describing and a human must look.
			return fmt.Errorf("release_pending order has no recorded terminal outcome")
		}
		return r.releaseAndFail(ctx, s)

	case "created", "payment_unknown":
		// Ambiguous: payment never attempted, or attempted and its result lost. The row
		// cannot tell. Ask payments what actually happened rather than guess — and the
		// runner cannot re-drive the charge itself, because commerce never persists the
		// payment token and payments folds it into a one-way fingerprint.
		// payment_unknown shares the path: it differs from `created` only in that the
		// checkout learned the operation was bound; the lookup re-establishes that and
		// the PSP status resolves it (TKT-115).
		return r.resolveCreated(ctx, s)

	case "reconciliation_required":
		// A queued compensation (unparked rows only reach here — parked ones are never
		// claimed). Re-derive the compensation kind from current durable evidence.
		return r.resolveReconciliation(ctx, s)

	default:
		return fmt.Errorf("order status %q is not recoverable here", s.Status)
	}
}

func (r *Runner) resolveCreated(ctx context.Context, s store.StuckOrder) error {
	op, found, err := r.payments.LookupOperation(ctx, s.OrganizerID, s.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("lookup payment operation: %w", err)
	}
	if !found {
		// No operation exists for this key, so payments never bound a charge: the PSP
		// was never asked, and no side effect can exist. This is durable evidence of no
		// side effect, not an inference from a failed transport — the distinction
		// ADR-016 §Decision 2 turns on.
		//
		// Recorded as `not_attempted` rather than `timeout`: nothing timed out, and
		// overloading a PSP answer to mean "we never called it" would make the column
		// lie to whoever reads it next.
		if err := r.store.RecordTerminalOutcome(ctx, s.OrderID, s.ClaimID, "not_attempted"); err != nil {
			return fmt.Errorf("record terminal outcome: %w", err)
		}
		// Mirror the transition the store just committed: the row is release_pending now,
		// and MarkReleased's predicate is written against that.
		s.TerminalOutcome, s.Status = "not_attempted", "release_pending"
		return r.releaseAndFail(ctx, s)
	}
	if !op.Resolved {
		// Bound but no terminal result: this is payment_unknown, resolved against real
		// PSP status since TKT-115 — behind the deadline gate below.
		return r.resolveWithProviderStatus(ctx, s, op)
	}
	switch op.Status {
	case "captured":
		// Payments captured, commerce crashed before recording it. Money is known
		// captured — same position as confirmation_pending.
		return r.confirmAndComplete(ctx, s)
	case "declined", "timeout":
		// A terminal answer proving no side effect. Record it, then release.
		if err := r.store.RecordTerminalOutcome(ctx, s.OrderID, s.ClaimID, op.Status); err != nil {
			return fmt.Errorf("record terminal outcome: %w", err)
		}
		s.TerminalOutcome, s.Status = op.Status, "release_pending"
		return r.releaseAndFail(ctx, s)
	default:
		return fmt.Errorf("unrecognized payment status %q", op.Status)
	}
}

func (r *Runner) confirmAndComplete(ctx context.Context, s store.StuckOrder) error {
	err := r.inventory.Confirm(ctx, s.OrganizerID, s.HoldID)
	if errors.Is(err, ErrClaimGone) {
		// The money is captured and the seat can never be delivered. Completing would
		// sell a claim that is gone; releasing without compensation would strand the
		// buyer's money. The honest resolution is a refund (TKT-115): queue the order —
		// KEEPING the claim, staying unparked — and drive the compensation in this same
		// pass. A later failure backs off through the normal retry path.
		r.log.WarnContext(ctx, "captured order cannot be confirmed; queued for refund",
			"order_id", s.OrderID, "amount", s.Amount, "currency", s.Currency)
		if err := r.store.QueueForCompensation(ctx, s.OrderID, s.ClaimID,
			"captured payment whose claim is gone; refunding via PSP port"); err != nil {
			return fmt.Errorf("queue for compensation: %w", err)
		}
		s.Status = "reconciliation_required"
		return r.resolveReconciliation(ctx, s)
	}
	if err != nil {
		return fmt.Errorf("confirm claim: %w", err)
	}
	if err := r.completer.Complete(ctx, s); err != nil {
		return fmt.Errorf("complete order: %w", err)
	}
	return r.store.ClearRecoveryClaim(ctx, s.OrderID, s.ClaimID)
}

// resolveWithProviderStatus resolves a bound-but-unresolved operation against real PSP
// status. The status-replay deadline gates the call, not the resolution: past it, the
// same-key replay the status endpoint would perform can mint a NEW PaymentIntent — so
// the runner parks BEFORE asking (ADR-032 §Status/replay amendment), and a payments-side
// 409 (defense in depth: another caller may race the same window) parks identically.
func (r *Runner) resolveWithProviderStatus(ctx context.Context, s store.StuckOrder, op Operation) error {
	// The deadline was computed by payments' clock; this pre-check reads commerce's.
	// Skew is conservative in both directions: a fast commerce clock parks an order
	// payments would still resolve (a human un-parks), a slow one makes a status call
	// payments answers 409 (one wasted hop, same one-pass park below). Accepted bound.
	if d := op.StatusReplayDeadlineAt; d != nil && time.Now().After(*d) {
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"status replay window expired; manual reconciliation required")
	}
	st, err := r.payments.Status(ctx, s.OrganizerID, s.IdempotencyKey)
	switch {
	case errors.Is(err, ErrReplayWindowExpired):
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"status replay window expired; manual reconciliation required")
	case errors.Is(err, ErrOperationNotFound):
		// The lookup just proved the operation exists; status says it does not. The
		// durable state is inconsistent and no automated decision is safe.
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"operation vanished between lookup and status; manual reconciliation required")
	case err != nil:
		// Still ambiguous (502, transport): the operation stays exactly as recoverable
		// as before. Never terminal.
		return fmt.Errorf("psp status: %w", err)
	}
	return r.actOnProviderStatus(ctx, s, st)
}

// actOnProviderStatus is the ADR-032 decision table over the normalized outcome.
func (r *Runner) actOnProviderStatus(ctx context.Context, s store.StuckOrder, st PSPStatus) error {
	switch st.Outcome {
	case "captured":
		// Money is proven captured: same position as confirmation_pending. If the claim
		// turns out gone, confirmAndComplete queues the refund.
		return r.confirmAndComplete(ctx, s)

	case "authorized":
		// An authorization is NOT terminal-no-side-effect — funds are held. Void first
		// (payments validates the evidence and appends payment.voided), and only then
		// record the release-enabling outcome.
		if _, err := r.payments.Void(ctx, s.OrganizerID, s.IdempotencyKey); err != nil {
			// 409 means the evidence moved under us (a prior pass's void completed, a
			// capture landed): retry — the next pass re-derives from fresh status.
			return fmt.Errorf("void authorized hold: %w", err)
		}
		return r.recordAndRelease(ctx, s, "no_side_effect")

	case "declined", "timeout":
		// The provider's exact terminal answer records ITSELF — never blurred into
		// no_side_effect, or the audit column stops distinguishing a decline from a
		// timeout (ADR-032 amendment, TKT-115 G1).
		return r.recordAndRelease(ctx, s, st.Outcome)

	case "voided":
		// The hold is already released — by a prior pass's void, or externally (P2-3:
		// an externally-canceled authorization journals order.failed + no_side_effect;
		// commerce never fabricates a payment.voided it did not drive).
		return r.recordAndRelease(ctx, s, "no_side_effect")

	case "refunded":
		// The compensation already completed (evidence supersedes stale status). Money
		// moved and came back — NOT a no-side-effect release; the order is refunded.
		return r.finishRefunded(ctx, s)

	default:
		// unknown, or a future outcome: proves nothing. Retry later; never release,
		// never record, never park (ADR-016 §Decision 3).
		return fmt.Errorf("provider status %q proves nothing; retrying", st.Outcome)
	}
}

// resolveReconciliation drives a queued compensation: status first — the kind is derived
// from CURRENT durable evidence, never from the order's history (three populations wear
// reconciliation_required; only proven-captured money is refunded).
func (r *Runner) resolveReconciliation(ctx context.Context, s store.StuckOrder) error {
	st, err := r.payments.Status(ctx, s.OrganizerID, s.IdempotencyKey)
	switch {
	case errors.Is(err, ErrReplayWindowExpired):
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"status replay window expired; manual reconciliation required")
	case errors.Is(err, ErrOperationNotFound):
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"reconciliation_required order has no payment operation; manual reconciliation required")
	case err != nil:
		return fmt.Errorf("psp status: %w", err)
	}
	if st.Outcome == "captured" {
		if st.CapturedAmount <= 0 {
			// The operation predates the durable-evidence schema (payments migration
			// 0002): a refund would be refused forever — and rightly, its basis is 0.
			// Park in ONE pass instead of burning the retry budget (plan-final F1).
			return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
				"operation predates durable provider evidence; manual reconciliation required")
		}
		if _, err := r.payments.Refund(ctx, s.OrganizerID, s.IdempotencyKey); err != nil {
			// ErrWrongCompensation: evidence moved between status and refund — the next
			// pass re-derives. ErrProviderUnresolved: the compensation stays BOUND in
			// payments; retry later. Both are retry, neither is terminal.
			return fmt.Errorf("refund captured money: %w", err)
		}
		return r.finishRefunded(ctx, s)
	}
	// Anything else re-enters the shared decision table: authorized voids, voided and
	// terminal answers release, refunded finishes, unknown retries.
	return r.actOnProviderStatus(ctx, s, st)
}

// recordAndRelease persists the terminal outcome, then releases — ADR-016 ordering:
// the outcome must be durable before the release, or a crash between them leaves an
// unrestartable release.
func (r *Runner) recordAndRelease(ctx context.Context, s store.StuckOrder, outcome string) error {
	if err := r.store.RecordTerminalOutcome(ctx, s.OrderID, s.ClaimID, outcome); err != nil {
		return fmt.Errorf("record terminal outcome: %w", err)
	}
	s.TerminalOutcome, s.Status = outcome, "release_pending"
	return r.releaseAndFail(ctx, s)
}

// finishRefunded closes an order whose captured money has been returned: discharge any
// remaining inventory obligation, journal order.failed (commerce's own workflow fact —
// payments already appended payment.refunded), then the terminal transition. Ordering
// is load-bearing: MarkRefunded runs last, only after the refund evidence and the fact
// are durable, and never appears on a 409/502 path (plan-final F4).
func (r *Runner) finishRefunded(ctx context.Context, s store.StuckOrder) error {
	err := r.inventory.Release(ctx, s.OrganizerID, s.HoldID)
	if errors.Is(err, ErrClaimNotReleasable) {
		// The claim is CONFIRMED — inventory sold the seat — while the money came back.
		// A refunded order holding a sold seat is not resolvable by either transition;
		// a human must reconcile.
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"refunded money against a confirmed claim; manual reconciliation required")
	}
	if err != nil {
		return fmt.Errorf("release claim: %w", err)
	}
	if err := r.journal.OrderFailed(ctx, s); err != nil {
		return fmt.Errorf("journal order.failed: %w", err)
	}
	if err := r.store.MarkRefunded(ctx, s); err != nil {
		return fmt.Errorf("mark refunded: %w", err)
	}
	return nil
}

func (r *Runner) releaseAndFail(ctx context.Context, s store.StuckOrder) error {
	// Idempotent for a repeated target, including the case that made this ambiguous:
	// inventory committed the release and lost the response.
	err := r.inventory.Release(ctx, s.OrganizerID, s.HoldID)
	if errors.Is(err, ErrClaimNotReleasable) {
		// The claim is confirmed: inventory counts the seat as sold, while this order's
		// payment did not capture. Journalling order.failed here would leave a sold seat
		// attached to a failed order and silently remove it from availability forever.
		// Retrying cannot help — confirmed is terminal. A human must reconcile.
		r.log.ErrorContext(ctx, "failed order holds a confirmed claim; parked for reconciliation",
			"order_id", s.OrderID, "hold_id", s.HoldID, "outcome", s.TerminalOutcome)
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"claim is confirmed but payment did not capture; needs manual reconciliation")
	}
	if err != nil {
		return fmt.Errorf("release claim: %w", err)
	}
	if err := r.journal.OrderFailed(ctx, s); err != nil {
		return fmt.Errorf("journal order.failed: %w", err)
	}
	return r.store.MarkReleased(ctx, s)
}

func (r *Runner) fail(ctx context.Context, s store.StuckOrder, cause error) {
	// Shutdown can be what failed the drive: a cancelled context reaches here, and
	// reusing it would fail this write too — leaving the current order's claim leased
	// for the FULL lease (batch×calls×timeout, ~17 min at defaults) on every restart.
	// Same fresh-bounded-context rule as releaseUndriven (ai-review B5).
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := r.store.ReleaseStuckOrder(ctx, s.OrderID, s.ClaimID, cause); err != nil {
		r.log.ErrorContext(ctx, "release stuck order", "order_id", s.OrderID, "err", err)
	}
	if s.Attempts >= store.MaxRecoveryAttempts {
		// Parked: never claimed again, so this is the last notice anyone gets that a
		// real order is stuck.
		r.log.ErrorContext(ctx, "stuck order parked after exhausting recovery attempts",
			"order_id", s.OrderID, "status", s.Status, "attempts", s.Attempts, "err", cause)
		return
	}
	r.log.WarnContext(ctx, "re-drive stuck order",
		"order_id", s.OrderID, "status", s.Status, "attempts", s.Attempts, "err", cause)
}
