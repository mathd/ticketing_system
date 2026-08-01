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
	FixQuantity(ctx context.Context, w store.CancellationWork, quantity int32, priorRun bool) error
	ClearQuantity(ctx context.Context, w store.CancellationWork) error
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

// plan is what resolveQuantity decided for one order. Threaded through the call rather than
// stashed on the Runner: a per-order map on a process-lifetime struct grows without bound and
// keeps every order id the service has ever refunded.
type plan struct {
	quantity int32
	// priorRun records that the cancellation refund already existed before this run touched
	// the order — i.e. a PREVIOUS run refunded it. That, not the refund unit's replay flag,
	// is what `already_refunded` means.
	priorRun bool
	outcome  store.CancellationOutcome
	decided  bool
	// terminal marks a decided outcome that a retry cannot repair, so it is finalized
	// rather than left for another attempt. The distinction matters for an outstanding
	// reversal: one on THIS run's own refund is repaired by replaying it, while one on a
	// staff refund this run does not own is not — retrying that is five reads and a wait.
	terminal bool
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
// runListLimit bounds how many unfinished runs one pass enumerates. Deliberately NOT the
// order batch: the order batch is sized for how many refunds fit in one lease, and reusing
// it here means N concurrent cancellations starve the N+1th out of enumeration entirely —
// its book never materializes, so its rows are never even claimable.
const runListLimit = 64

// maxAttempts bounds retries of AMBIGUOUS failures only — a provider timeout, an
// unavailable journal, a completion that did not persist. Those must not be terminal on the
// first try: the money may already have moved, and a terminal verdict leaves it moved with
// the tickets still valid and nothing driving the reversal. They must not retry forever
// either, or the run never completes and its report is never readable. A DEFINITE refusal
// is terminal immediately and never consumes an attempt.
const maxAttempts = 5

func (r *Runner) RunOnce(ctx context.Context) int {
	runs, err := r.store.Runs(ctx, runListLimit)
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
	for ctx.Err() == nil {
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
	p := r.resolveQuantity(ctx, w, key)
	if p.decided {
		if p.terminal {
			return r.finalize(ctx, w, p.outcome)
		}
		return r.record(ctx, w, p.outcome)
	}

	result, err := r.refunder.Refund(ctx, store.RefundRequest{
		OrderID: w.OrderID, OrganizerID: w.OrganizerID, Quantity: p.quantity,
		IdempotencyKey: key,
		// FIXED attribution, not the operator's: order_refunds.request_fingerprint covers
		// actor and reason, so a second run carrying a different operator would conflict
		// with its own earlier attempt instead of replaying it (ADR-040 §3).
		Actor: store.CancellationRefundActor, Reason: store.CancellationRefundReason,
	})
	if err != nil {
		// The ceiling moved under us: a staff refund landed between reading the remainder
		// and binding, so the number this row fixed is now wrong and every retry of it
		// would fail the same way. Clear it so the next attempt recomputes, rather than
		// stranding a refundable order on a stale quantity.
		if errors.Is(err, store.ErrRefundExceedsOrder) {
			if clearErr := r.store.ClearQuantity(context.WithoutCancel(ctx), w); clearErr != nil {
				slog.Default().ErrorContext(ctx, "clear cancellation quantity", "order_id", w.OrderID, "err", clearErr)
			}
			// `ceiling_moved`, NOT `refund_refused`: clearing the quantity only helps if
			// something recomputes it, and a refusal is terminal. The first version of this
			// fix cleared the quantity and then finalized anyway, which stranded exactly the
			// refundable tickets it was meant to rescue.
			return r.record(ctx, w, failure("ceiling_moved", err.Error()))
		}
		return r.record(ctx, w, failure(refundFailureCode(err), err.Error()))
	}
	return r.record(ctx, w, r.classify(ctx, w, p, result))
}

// record commits a verdict — unless the verdict is one that must not stick.
//
// Two things are deliberately NOT terminal here. An interruption is not a business outcome:
// if the context ended, the row goes back to the queue whatever the in-flight verdict said,
// because a shutdown between a successful refund and its classification would otherwise
// commit a permanent `failed` for an order that is fully discharged. And an AMBIGUOUS
// failure — one where the money may already have moved — is retried within the run until
// maxAttempts, because a terminal verdict on it leaves money gone with the tickets still
// valid and nothing driving the reversal.
func (r *Runner) record(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome) bool {
	// An interruption releases the claim immediately: a successor should pick the row up
	// now, not after a lease it never used.
	if ctx.Err() != nil {
		r.abandon(w)
		return false
	}
	// A retryable failure deliberately does NOT release the claim. Leaving the lease in
	// place IS the backoff — releasing it would let the very next claim in the same pass
	// re-drive an unavailable downstream, burning the whole attempt budget in a tight loop
	// instead of spreading it over lease-length intervals.
	if retryable(out) && w.Attempts < maxAttempts {
		slog.Default().WarnContext(ctx, "cancellation order left for retry",
			"order_id", w.OrderID, "attempt", w.Attempts, "code", out.FailureCode)
		return false
	}
	return r.finalize(ctx, w, out)
}

// retryable reports whether a failure leaves the order worth attempting again. The test is
// "could this have moved money, or could it succeed later" — not "was it annoying".
func retryable(out store.CancellationOutcome) bool {
	if out.Outcome != "failed" {
		return false
	}
	switch out.FailureCode {
	case "unavailable", "internal", "reversal_outstanding", "ceiling_moved":
		return true
	default:
		// refund_refused, not_refundable, no_captured_money: definite answers. Retrying
		// them just burns the book.
		return false
	}
}

// resolveQuantity decides how much this row refunds, or decides the row outright when
// there is nothing to refund. The order of precedence is the resumability contract:
//
//  1. a quantity this row already fixed — the durable record of an earlier attempt, and
//     the reason a crash-resume re-derives the same request fingerprint;
//  2. the quantity an existing cancellation refund was bound with — same reason, for a
//     crash between the fix and the bind;
//  3. only then, the order's remaining quantity.
func (r *Runner) resolveQuantity(ctx context.Context, w store.CancellationWork, key string) plan {
	if w.RequestedQuantity.Valid {
		// A resumed attempt. The attribution comes from the row, not from re-deriving it:
		// a lookup here cannot tell "a previous run refunded this" from "this run did,
		// before it was interrupted".
		return plan{quantity: w.RequestedQuantity.Int32, priorRun: w.PriorRun}
	}
	existing, found, err := r.store.LookupRefund(ctx, w.OrganizerID, store.RefundID(w.OrganizerID, key))
	if err != nil {
		return decidedPlan(failure("internal", "read existing cancellation refund"))
	}
	// This run found a refund it did not create and has done no work of its own for this
	// order: a PREVIOUS run already refunded it. That, not the replay flag, is what
	// `already_refunded` means — the replay flag cannot tell a second run apart from this
	// run resuming after a crash, and would mis-attribute both.
	priorRun := found
	if found {
		// Persist it here too, not just on the branch that computes it. A later run finds
		// this refund and takes THIS branch, and a row that reaches a `refunded` outcome
		// with no requested_quantity is rejected by
		// `cancellation_refund_orders_refunded_has_refund` — which fails the finalize, not
		// the refund, so the row stays pending and its run never completes.
		if err := r.store.FixQuantity(ctx, w, existing.Quantity, priorRun); err != nil {
			return decidedPlan(failure("internal", "persist refund quantity"))
		}
		return plan{quantity: existing.Quantity, priorRun: priorRun}
	}

	state, err := r.store.OrderState(ctx, w.OrganizerID, w.OrderID)
	if err != nil {
		return decidedPlan(failure("internal", "read order state"))
	}
	if state.OrderStatus != "completed" {
		return decidedPlan(failure("not_refundable", "only a completed order can be refunded"))
	}
	remaining := state.SoldQuantity - state.RefundedQuantity
	if remaining <= 0 {
		// Already fully refunded by someone else. It is only DONE if that refund's
		// obligations were discharged too — money back with the tickets still valid is
		// not a success (ADR-039).
		if state.Outstanding() {
			out := failure("reversal_outstanding", "the order is fully refunded but a reversal obligation is outstanding")
			out.MoneyRefunded = true
			out.TicketsVoided = state.VoidingOutstanding == 0
			out.CapacityReturned = state.CapacityOutstanding == 0
			out.RefundedQuantity, out.RefundedAmount = state.RefundedQuantity, state.RefundedAmount
			// Terminal: the outstanding obligation belongs to a refund this run did not
			// create, and repairing it would need that refund's own idempotency key, which
			// the row does not carry. Retrying would re-read the same state five times and
			// then fail anyway.
			return terminalPlan(out)
		}
		return decidedPlan(store.CancellationOutcome{
			Outcome: "already_refunded", MoneyRefunded: true, TicketsVoided: true, CapacityReturned: true,
			RefundedQuantity: state.RefundedQuantity, RefundedAmount: state.RefundedAmount,
		})
	}
	if state.UnitAmount <= 0 {
		// A comped order has no money leg — and therefore gets no reversal at all, so its
		// tickets keep admitting. Recorded visibly rather than skipped; closing it is a
		// follow-up, not this ticket (ADR-040 §6).
		return decidedPlan(failure("no_captured_money", "order has no captured money to refund"))
	}
	// Fixed BEFORE the provider call: recomputing it afterwards reads a different
	// remainder, which would change the refund's request fingerprint and turn a resume
	// into a conflict with its own earlier attempt.
	if err := r.store.FixQuantity(ctx, w, remaining, false); err != nil {
		return decidedPlan(failure("internal", "persist refund quantity"))
	}
	return plan{quantity: remaining}
}

func decidedPlan(out store.CancellationOutcome) plan {
	return plan{outcome: out, decided: true}
}

func terminalPlan(out store.CancellationOutcome) plan {
	return plan{outcome: out, decided: true, terminal: true}
}

// classify reads the order back and decides the verdict from what is actually true, not
// from what the refund call returned. A success requires EVERY obligation discharged.
func (r *Runner) classify(ctx context.Context, w store.CancellationWork, p plan, result refunds.Result) store.CancellationOutcome {
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
	// Reported SEPARATELY. Voiding can succeed while the capacity return fails — the
	// ordinary case for a seated order — and collapsing them tells the operator to chase
	// work that is already done.
	out.TicketsVoided = state.VoidingOutstanding == 0
	out.CapacityReturned = state.CapacityOutstanding == 0
	discharged := !state.Outstanding()
	switch {
	case out.MoneyRefunded && discharged:
		// `already_refunded` means a PREVIOUS run had already refunded this order when
		// this one arrived — decided from that, not from the refund unit's replay flag.
		// Replay cannot tell a second run apart from this run resuming after a crash, so
		// it mis-attributes both directions.
		out.Outcome = "refunded"
		if p.priorRun {
			out.Outcome = "already_refunded"
		}
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
	// A verdict reached just as the context ended is still a verdict, and losing a SUCCESS
	// would re-drive an order whose money already moved — so the write is detached. But a
	// FAILURE decided in that same window may have been caused by the shutdown itself, and
	// committing it makes a permanent failure out of an interruption. Re-checked here
	// because the check in `record` and this write are not atomic.
	if ctx.Err() != nil && out.Outcome == "failed" {
		r.abandon(w)
		return false
	}
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
