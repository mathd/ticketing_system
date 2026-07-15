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
)

// Payments resolves what a payment operation actually did. This is the
// payments-operation LOOKUP, not real-PSP status: it reads an outcome payments already
// recorded. Resolving a genuinely unknown result needs the PSP port (TKT-56).
type Payments interface {
	// LookupOperation returns the recorded outcome. found=false means no operation
	// exists — evidence the charge was never submitted.
	LookupOperation(ctx context.Context, organizerID uuid.UUID, key string) (op Operation, found bool, err error)
}

type Operation struct {
	// Resolved is false when the operation was bound but carries no terminal result.
	// That is the payment_unknown case: it needs PSP status, so the runner leaves it.
	Resolved bool
	Status   string
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
	ClearRecoveryClaim(ctx context.Context, orderID, claimID uuid.UUID) error
	AbandonRecoveryClaim(ctx context.Context, orderID, claimID uuid.UUID) error
	MarkReleased(ctx context.Context, s store.StuckOrder) error
	ReleaseStuckOrder(ctx context.Context, orderID, claimID uuid.UUID, cause error) error
}

// DBStore binds Store to the commerce store package.
type DBStore struct {
	DB *sql.DB
}

func (d DBStore) ClaimStuckOrders(ctx context.Context, limit int, lease time.Duration) ([]store.StuckOrder, error) {
	return store.ClaimStuckOrders(ctx, d.DB, limit, lease)
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

// MaxCallsPerOrder is the longest external-call chain one order can drive: resolve the
// payment operation, act on the claim, then journal the fact.
const MaxCallsPerOrder = 3

// LeaseFor sizes the batch lease from the caller's own I/O budget.
//
// The lease has to outlast the work it covers. A batch is driven sequentially and each
// order can make MaxCallsPerOrder calls, each bounded only by the HTTP client timeout —
// so the pass's worst case is batch × calls × timeout. Sizing the lease from an
// unrelated per-order guess is how it silently ends up shorter than the batch it
// protects: the lease lapses mid-pass, a second runner claims rows the first is still
// driving, and the claim token only fences the final database write — not the inventory
// call or the journal submission already in flight.
func LeaseFor(batch int, callTimeout time.Duration) time.Duration {
	if batch <= 0 {
		batch = 1
	}
	if callTimeout <= 0 {
		callTimeout = 10 * time.Second
	}
	// Plus a margin for database work and scheduling between calls.
	return time.Duration(batch)*MaxCallsPerOrder*callTimeout + 60*time.Second
}

func New(st Store, payments Payments, inventory Inventory, journal Journal, completer Completer,
	interval time.Duration, batch int, callTimeout time.Duration, log *slog.Logger) *Runner {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if batch <= 0 {
		batch = 16
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{store: st, payments: payments, inventory: inventory, journal: journal,
		completer: completer, interval: interval, batch: batch,
		lease: LeaseFor(batch, callTimeout), log: log}
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

	case "created":
		// Ambiguous: payment never attempted, or attempted and its result lost. The row
		// cannot tell. Ask payments what actually happened rather than guess — and the
		// runner cannot re-drive the charge itself, because commerce never persists the
		// payment token and payments folds it into a one-way fingerprint.
		return r.resolveCreated(ctx, s)

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
		// Bound but no terminal result: this is payment_unknown. Resolving it needs
		// real-PSP status (TKT-56). Leaving it claimable would spin; park it so a human
		// sees a real order awaiting a capability that does not exist yet.
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"payment result unknown; needs PSP status (TKT-56)")
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
		// sell a claim that is gone; releasing would strand the buyer's money. The
		// honest resolution is a refund, which needs the PSP port (TKT-56) — so park it
		// visibly rather than silently pick one.
		r.log.ErrorContext(ctx, "captured order cannot be confirmed; parked for reconciliation",
			"order_id", s.OrderID, "amount", s.Amount, "currency", s.Currency)
		return r.store.ParkForReconciliation(ctx, s.OrderID, s.ClaimID,
			"captured payment whose claim is gone; needs void/refund (TKT-56)")
	}
	if err != nil {
		return fmt.Errorf("confirm claim: %w", err)
	}
	if err := r.completer.Complete(ctx, s); err != nil {
		return fmt.Errorf("complete order: %w", err)
	}
	return r.store.ClearRecoveryClaim(ctx, s.OrderID, s.ClaimID)
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
