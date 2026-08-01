// Package bulkrefund drives event-cancellation refund runs (TKT-159, ADR-040).
//
// It copies `internal/recovery`'s proven lifecycle — claim under a lease, work outside the
// transaction, release what it did not finish — rather than merging into it: checkout
// recovery and cancellation reporting have different eligibility, terminal states and
// retention semantics, and one state machine serving both would be readable by nobody.
//
// It composes NO money path of its own. Every refund goes through the same
// internal/refunds unit the staff endpoint uses, under an idempotency key derived from
// (slot, order) so that any run over the slot converges on ONE refund per order.
package bulkrefund

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/store"
)

// Store is the durable state the runner decides against and writes back to. A port rather
// than a *sql.DB so the decision table can be exercised against fakes: the question the
// unit tests must answer is "which outcome did the runner choose for this evidence", and
// that is unreadable through SQL side effects alone. The transitions themselves are covered
// against real PostgreSQL by the store's smoke tests.
type Store interface {
	Runs(ctx context.Context, limit int) ([]store.CancellationRun, error)
	Enumerate(ctx context.Context, org, runID uuid.UUID, batch int) (bool, error)
	Claim(ctx context.Context, limit int, lease time.Duration) ([]store.CancellationWork, error)
	OrderState(ctx context.Context, org, order uuid.UUID) (store.OrderCancellationState, error)
	LookupRefund(ctx context.Context, org, refundID uuid.UUID) (store.Refund, bool, error)
	FixQuantity(ctx context.Context, w store.CancellationWork, quantity int32) error
	Finalize(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome) error
	Abandon(ctx context.Context, w store.CancellationWork) error
	CompleteRuns(ctx context.Context) (int, error)
}

// Refunder is the shared single-order refund unit (internal/refunds).
type Refunder interface {
	Refund(ctx context.Context, in store.RefundRequest) (refunds.Result, error)
	DriveReversal(ctx context.Context, refund store.Refund) store.Refund
}

// Runner enumerates and refunds cancellation books.
type Runner struct {
	store    Store
	refunder Refunder
	interval time.Duration
	batch    int
	lease    time.Duration
}

func New(s Store, r Refunder, interval time.Duration, batch int, lease time.Duration) *Runner {
	return &Runner{store: s, refunder: r, interval: interval, batch: batch, lease: lease}
}

// Run drives passes until the context ends, starting with one immediately: a run created
// seconds ago should not wait a whole interval for its first batch.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce enumerates every unfinished run's next pages, then drains the claimable work in
// bounded batches, then completes whatever finished. Returns the number of orders resolved.
func (r *Runner) RunOnce(ctx context.Context) int {
	runs, err := r.store.Runs(ctx, r.batch)
	if err != nil {
		slog.Default().ErrorContext(ctx, "list cancellation runs", "err", err)
		return 0
	}
	for _, run := range runs {
		for {
			if ctx.Err() != nil {
				return 0
			}
			done, err := r.store.Enumerate(ctx, run.OrganizerID, run.ID, r.batch)
			if err != nil {
				slog.Default().ErrorContext(ctx, "enumerate cancellation book", "run_id", run.ID, "err", err)
				break
			}
			if done {
				break
			}
		}
	}

	resolved := 0
	for {
		if ctx.Err() != nil {
			break
		}
		claimed, err := r.store.Claim(ctx, r.batch, r.lease)
		if err != nil {
			slog.Default().ErrorContext(ctx, "claim cancellation orders", "err", err)
			break
		}
		if len(claimed) == 0 {
			break
		}
		for _, w := range claimed {
			// The context is checked per ORDER, not per batch: an interrupted runner must
			// leave the rest of its claim reclaimable rather than half-driving it.
			if ctx.Err() != nil {
				r.abandon(w)
				continue
			}
			if r.process(ctx, w) {
				resolved++
			}
		}
	}

	if _, err := r.store.CompleteRuns(ctx); err != nil {
		slog.Default().ErrorContext(ctx, "complete cancellation runs", "err", err)
	}
	return resolved
}

// abandon releases a claim without a verdict, on a context detached from the cancelled one
// — the whole point is to record the release, and a cancelled context cannot.
func (r *Runner) abandon(w store.CancellationWork) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := r.store.Abandon(ctx, w); err != nil {
		slog.Default().ErrorContext(ctx, "abandon cancellation claim", "order_id", w.OrderID, "err", err)
	}
}

// process resolves one order, and never returns an error: a failure is an OUTCOME of this
// row, not of the run (AC 3). It reports whether the row reached a terminal verdict.
func (r *Runner) process(ctx context.Context, w store.CancellationWork) bool {
	key := store.CancellationRefundKey(w.SlotID, w.OrderID)
	quantity, outcome, decided := r.resolveQuantity(ctx, w, key)
	if decided {
		return r.finalize(ctx, w, outcome)
	}

	result, err := r.refunder.Refund(ctx, store.RefundRequest{
		OrderID: w.OrderID, OrganizerID: w.OrganizerID, Quantity: quantity,
		IdempotencyKey: key,
		// FIXED attribution, not the operator's: order_refunds.request_fingerprint covers
		// actor and reason, so a second run carrying a different operator would conflict
		// with its own earlier attempt instead of replaying it (ADR-040 §3).
		Actor: store.CancellationRefundActor, Reason: store.CancellationRefundReason,
	})
	if err != nil {
		// An interruption is not a verdict. Leave the row reclaimable — a successor
		// re-derives the same key and converges on the same refund.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			r.abandon(w)
			return false
		}
		return r.finalize(ctx, w, failure(refundFailureCode(err), err.Error()))
	}
	return r.finalize(ctx, w, r.classify(ctx, w, result))
}

// resolveQuantity decides how much this row refunds, or decides the row outright when
// there is nothing to refund. The order of precedence is the resumability contract:
//
//  1. a quantity this row already fixed — the durable record of an earlier attempt, and
//     the reason a crash-resume re-derives the same request fingerprint;
//  2. the quantity an existing cancellation refund was bound with — same reason, for a
//     crash between the fix and the bind;
//  3. only then, the order's remaining quantity.
func (r *Runner) resolveQuantity(ctx context.Context, w store.CancellationWork, key string) (int32, store.CancellationOutcome, bool) {
	if w.RequestedQuantity.Valid {
		return w.RequestedQuantity.Int32, store.CancellationOutcome{}, false
	}
	existing, found, err := r.store.LookupRefund(ctx, w.OrganizerID, store.RefundID(w.OrganizerID, key))
	if err != nil {
		return 0, failure("internal", "read existing cancellation refund"), true
	}
	if found {
		return existing.Quantity, store.CancellationOutcome{}, false
	}

	state, err := r.store.OrderState(ctx, w.OrganizerID, w.OrderID)
	if err != nil {
		return 0, failure("internal", "read order state"), true
	}
	if state.OrderStatus != "completed" {
		return 0, failure("not_refundable", "only a completed order can be refunded"), true
	}
	remaining := state.SoldQuantity - state.RefundedQuantity
	if remaining <= 0 {
		// Already fully refunded by someone else. It is only DONE if that refund's
		// obligations were discharged too — money back with the tickets still valid is
		// not a success (ADR-039).
		if len(state.OutstandingRefunds) > 0 {
			out := failure("reversal_outstanding", "the order is fully refunded but a reversal obligation is outstanding")
			out.MoneyRefunded = true
			out.RefundedQuantity, out.RefundedAmount = state.RefundedQuantity, state.RefundedAmount
			return 0, out, true
		}
		return 0, store.CancellationOutcome{
			Outcome: "already_refunded", MoneyRefunded: true, TicketsVoided: true, CapacityReturned: true,
			RefundedQuantity: state.RefundedQuantity, RefundedAmount: state.RefundedAmount,
		}, true
	}
	if state.UnitAmount <= 0 {
		// A comped order has no money leg — and therefore gets no reversal at all, so its
		// tickets keep admitting. Recorded visibly rather than skipped; closing it is a
		// follow-up, not this ticket (ADR-040 §6).
		return 0, failure("no_captured_money", "order has no captured money to refund"), true
	}
	// Fixed BEFORE the provider call: recomputing it afterwards reads a different
	// remainder, which would change the refund's request fingerprint and turn a resume
	// into a conflict with its own earlier attempt.
	if err := r.store.FixQuantity(ctx, w, remaining); err != nil {
		return 0, failure("internal", "persist refund quantity"), true
	}
	return remaining, store.CancellationOutcome{}, false
}

// classify reads the order back and decides the verdict from what is actually true, not
// from what the refund call returned. A success requires EVERY obligation discharged.
func (r *Runner) classify(ctx context.Context, w store.CancellationWork, result refunds.Result) store.CancellationOutcome {
	state, err := r.store.OrderState(ctx, w.OrganizerID, w.OrderID)
	if err != nil {
		return failure("internal", "read order state after refund")
	}
	out := store.CancellationOutcome{
		RefundID:         result.Refund.ID,
		MoneyRefunded:    state.RefundStatus == "full",
		RefundedQuantity: state.RefundedQuantity,
		RefundedAmount:   state.RefundedAmount,
	}
	discharged := len(state.OutstandingRefunds) == 0
	out.TicketsVoided, out.CapacityReturned = discharged, discharged
	switch {
	case out.MoneyRefunded && discharged:
		out.Outcome = "refunded"
	case out.MoneyRefunded:
		out.Outcome = "failed"
		out.FailureCode, out.FailureReason = "reversal_outstanding", "the money was returned but a reversal obligation is outstanding"
	default:
		out.Outcome = "failed"
		out.FailureCode, out.FailureReason = "refund_refused", "the refund did not return the whole order"
	}
	return out
}

func (r *Runner) finalize(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome) bool {
	// Detached: a verdict reached just as the context ended is still a verdict, and losing
	// it would make the next pass re-drive an order whose money already moved.
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.store.Finalize(write, w, out); err != nil {
		if !errors.Is(err, store.ErrCancellationClaimLost) {
			slog.Default().ErrorContext(write, "finalize cancellation order", "order_id", w.OrderID, "err", err)
		}
		return false
	}
	return true
}

func failure(code, reason string) store.CancellationOutcome {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return store.CancellationOutcome{Outcome: "failed", FailureCode: code, FailureReason: reason}
}

// refundFailureCode maps the refund unit's failure modes onto the report's bounded
// vocabulary. Never the raw downstream body: a report is read by an operator, and a
// provider payload in it is both unreadable and a disclosure risk.
func refundFailureCode(err error) string {
	switch {
	case errors.Is(err, refunds.ErrPaymentsRefused),
		errors.Is(err, store.ErrRefundExceedsOrder),
		errors.Is(err, store.ErrRefundConflict):
		return "refund_refused"
	case errors.Is(err, store.ErrOrderNotRefundable):
		return "not_refundable"
	case errors.Is(err, store.ErrRefundNoMoney):
		return "no_captured_money"
	case errors.Is(err, refunds.ErrPaymentsUnresolved), errors.Is(err, refunds.ErrJournalUnavailable):
		return "unavailable"
	default:
		return "internal"
	}
}
