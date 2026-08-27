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
	OrderState(ctx context.Context, org, order, ownRefund uuid.UUID) (store.OrderCancellationState, error)
	LookupRefund(ctx context.Context, org, refundID uuid.UUID) (store.Refund, bool, error)
	FixQuantity(ctx context.Context, w store.CancellationWork, quantity int32, priorRun bool) error
	ClearQuantity(ctx context.Context, w store.CancellationWork) error
	Finalize(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome) error
	Abandon(ctx context.Context, w store.CancellationWork, refundAttempt bool) error
	CompleteRuns(ctx context.Context) (int, error)
}

// Refunder is the shared single-order refund unit (internal/refunds).
type Refunder interface {
	Refund(ctx context.Context, in store.RefundRequest) (refunds.Result, error)
	DriveReversal(ctx context.Context, refund store.Refund) store.Refund
	// Void reverses a COMPED order — tickets and capacity, never money (TKT-171).
	// On the interface beside Refund rather than behind a separate seam because
	// the runner's choice between them is one branch on one fact (does the order
	// have a money leg), and splitting the seam would let a caller hold one
	// without the other.
	Void(ctx context.Context, in store.VoidRequest) (refunds.VoidResult, error)
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
	// void routes this order to the comped reversal instead of the refund (TKT-171).
	// A separate flag rather than `quantity == 0`: the void's quantity comes from the
	// reservation under the order lock, so the runner never carries one, and reusing
	// a zero quantity as the signal would make an unrelated zero mean "void".
	void bool
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
				// Never driven, so the attempt charge comes back.
				r.abandon(w, true)
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
func (r *Runner) abandon(w store.CancellationWork, refundAttempt bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := r.store.Abandon(ctx, w, refundAttempt); err != nil {
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
		return r.record(ctx, w, p.outcome, false)
	}

	if p.void {
		return r.processVoid(ctx, w)
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
			return r.record(ctx, w, failure("ceiling_moved", err.Error()), true)
		}
		return r.record(ctx, w, failure(refundFailureCode(err), err.Error()), true)
	}
	out, terminal := r.classify(ctx, w, p, result)
	if terminal {
		return r.finalize(ctx, w, out)
	}
	return r.record(ctx, w, out, true)
}

// processVoid reverses a comped order (TKT-171).
//
// Deliberately NOT folded into process's refund path with conditionals. The two
// share only the reversal, and the refund path's machinery — the quantity
// ceiling, ErrRefundExceedsOrder recovery, the stale-quantity clear — is entirely
// about a money amount that a void does not have. Threading `if void` through it
// would put a money path's error handling in front of an operation with no money.
//
// The outcome carries MoneyRefunded:false with the other two flags true. That is
// the whole reason store.CancellationOutcome kept three independent flags rather
// than one status: `voided` is a real, complete reversal, and reporting it as
// `refunded` would tell an operator money went back to a buyer when none did.
func (r *Runner) processVoid(ctx context.Context, w store.CancellationWork) bool {
	result, err := r.refunder.Void(ctx, store.VoidRequest{
		OrderID: w.OrderID, OrganizerID: w.OrganizerID,
		// The same fixed attribution the refund path uses, and for the same reason
		// (ADR-040 §3): actor and reason are in the void's request fingerprint, so a
		// second run carrying a different operator would conflict with its own
		// earlier attempt instead of replaying it.
		IdempotencyKey: store.CancellationRefundKey(w.SlotID, w.OrderID),
		Actor:          store.CancellationRefundActor,
		Reason:         store.CancellationRefundReason,
	})
	if err != nil {
		return r.record(ctx, w, failure(voidFailureCode(err), err.Error()), true)
	}
	out := store.CancellationOutcome{
		Outcome:       "voided",
		MoneyRefunded: false,
		// Reported from the void's OWN progress, never assumed from the absence of an
		// error: a downstream outage leaves an obligation outstanding and the call
		// still returns, which is exactly the state an operator needs to see.
		TicketsVoided:    result.Void.TicketsVoided,
		CapacityReturned: result.Void.CapacityReturned,
	}
	// An outstanding obligation is not terminal — a replay of the same void id is
	// how it gets retried, the same way an outstanding refund reversal is.
	if !out.TicketsVoided || !out.CapacityReturned {
		return r.record(ctx, w, out, true)
	}
	return r.finalize(ctx, w, out)
}

// voidFailureCode maps a void refusal onto the run's outcome vocabulary.
func voidFailureCode(err error) string {
	switch {
	case errors.Is(err, store.ErrVoidHasMoney):
		// Reachable only if the unit amount changed between resolveQuantity reading
		// it and the bind re-reading it under the order lock. The lock's answer wins.
		return "refund_refused"
	case errors.Is(err, store.ErrOrderNotVoidable):
		return "refund_refused"
	default:
		return "internal"
	}
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
func (r *Runner) record(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome, drove bool) bool {
	// An interruption releases the claim immediately: a successor should pick the row up
	// now, not after a lease it never used. The attempt charge comes back only if the
	// refund unit was never called — a claim released after real money-path work keeps it,
	// or a cancellation window recurring at exactly that point would hold the row below the
	// cap forever and its run would never complete.
	if ctx.Err() != nil {
		r.abandon(w, !drove)
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
	refundID := store.RefundID(w.OrganizerID, key)
	if w.RequestedQuantity.Valid {
		// A resumed attempt. The attribution comes from the row, not from re-deriving it:
		// a lookup here cannot tell "a previous run refunded this" from "this run did,
		// before it was interrupted".
		p := plan{quantity: w.RequestedQuantity.Int32, priorRun: w.PriorRun}
		if _, bound, err := r.store.LookupRefund(ctx, w.OrganizerID, refundID); err == nil && bound {
			// A bound refund vouches for the number: it is already in that refund's
			// request fingerprint, so it must not be second-guessed.
			return p
		}
		// Nothing is bound under it, so the number is only a note this run left itself —
		// and it may be stale, because the ceiling can move between fixing it and binding.
		// Re-validate rather than resubmitting it forever: a clear that failed transiently
		// would otherwise make every successor repeat the same oversized request until the
		// budget ran out, permanently under-refunding the order.
		state, err := r.store.OrderState(ctx, w.OrganizerID, w.OrderID, refundID)
		if err != nil {
			return decidedPlan(failure("internal", "read order state"))
		}
		if remaining := state.SoldQuantity - state.RefundedQuantity; remaining < p.quantity {
			if err := r.store.ClearQuantity(ctx, w); err != nil {
				return decidedPlan(failure("internal", "clear stale refund quantity"))
			}
			w.RequestedQuantity.Valid = false
			return r.resolveQuantity(ctx, w, key)
		}
		return p
	}
	existing, found, err := r.store.LookupRefund(ctx, w.OrganizerID, refundID)
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

	state, err := r.store.OrderState(ctx, w.OrganizerID, w.OrderID, refundID)
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
	if state.UnitAmount == 0 && state.TotalAmount == 0 {
		// A comped order. It has no money leg, so it cannot be refunded — but it
		// still admits and still holds a seat, so it must still be REVERSED
		// (TKT-171). Route it to the void, which discharges the same two downstream
		// obligations in the same order and moves no money.
		//
		// Strictly `== 0`, and the negative case falls through to the failure below
		// on purpose: a negative unit amount is not a comped order, it is corrupt
		// data, and voiding it would hide the corruption behind a successful-looking
		// reversal.
		return plan{void: true}
	}
	if state.UnitAmount <= 0 {
		// Two shapes reach here, and neither is voidable.
		//
		// A zero FACE with a captured TOTAL is a comped ticket carrying a passed-on
		// fee — real money the buyer paid. The void refuses it (it is not a
		// no-money reversal) and so does the refund (its unit is 0), so it reports
		// here and stays visible. That is the owner's decision of 2026-08-27:
		// refuse rather than reverse it partially, because a void that returned
		// fees would be a money path, which is the thing a void exists to avoid.
		// What such an order's cancellation SHOULD do is a separate decision.
		//
		// A NEGATIVE unit amount is corrupt data — unreachable through any
		// supported write, since reservations.unit_amount is CHECK(unit_amount >= 0)
		// — and is reported rather than assumed away.
		return decidedPlan(failure("no_captured_money", "order has no refundable unit amount"))
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
// classify decides the verdict from what is actually true afterwards, not from what the
// refund call returned. The second result reports that the verdict cannot be improved by
// retrying, so it is finalized rather than left for another attempt.
func (r *Runner) classify(ctx context.Context, w store.CancellationWork, p plan, result refunds.Result) (store.CancellationOutcome, bool) {
	state, err := r.store.OrderState(ctx, w.OrganizerID, w.OrderID, result.Refund.ID)
	if err != nil {
		return failure("internal", "read order state after refund"), false
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
		return out, false
	case out.MoneyRefunded:
		out.Outcome = "failed"
		out.FailureCode, out.FailureReason = "reversal_outstanding", "the money was returned but a reversal obligation is outstanding"
		if state.OwnOutstanding == 0 {
			// Only a FOREIGN obligation is left — this run's own refund is fully
			// discharged. Replaying it would re-drive nothing, so five retries would be
			// five reads and a wait before failing anyway.
			out.FailureReason = "the money was returned but another refund's reversal obligation is outstanding"
			return out, true
		}
		return out, false
	default:
		out.Outcome = "failed"
		out.FailureCode, out.FailureReason = "refund_refused", "the refund did not return the whole order"
		return out, false
	}
}

func (r *Runner) finalize(ctx context.Context, w store.CancellationWork, out store.CancellationOutcome) bool {
	// Detached: a verdict reached just as the context ended is still a verdict, and losing
	// it would make the next pass re-drive an order whose money already moved.
	// A verdict reached just as the context ended is still a verdict, and losing a SUCCESS
	// would re-drive an order whose money already moved — so the write is detached. A FAILURE
	// decided in that window may instead have been caused by the shutdown, so it is re-checked
	// here, `record`'s check and this write not being atomic.
	//
	// This NARROWS the window; it does not close it, and no check-then-write can. What makes
	// that acceptable is which failures can slip through: one caused by the shutdown surfaces
	// as a context error and is handled in `process` before ever reaching here, so a failure
	// that survives both checks is a genuine one, and committing it is correct.
	if ctx.Err() != nil && out.Outcome == "failed" {
		r.abandon(w, false)
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
