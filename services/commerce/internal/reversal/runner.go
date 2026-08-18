// Package reversal drives outstanding refund reversal obligations to completion (TKT-163,
// ADR-062).
//
// ADR-038 §7 shipped the reversal as "visible and retryable" with nothing retrying it: a
// refund whose money moved but whose tickets were not voided — access down, ACCESS_URL
// unset, or issuance not caught up (503) — stayed outstanding until a human replayed the
// idempotency key, and the caller got a 200 telling it nothing was owed. §7 recorded a
// leased runner as designed and rejected, on the grounds that nobody had stated the
// requirement, and noted that "adding one later is additive". This is that later.
//
// It copies `internal/recovery`'s lifecycle — claim under a lease, work outside the
// transaction, release what it did not finish — rather than merging into it or into
// `internal/bulkrefund`: those have different eligibility and different terminal states,
// and bulkrefund only ever sees orders enumerated into a cancellation book, so an ordinary
// staff refund with a stuck obligation is invisible to it (ADR-040 also makes obligations
// on refunds a run does not own terminal rather than repaired).
//
// It composes NO reversal of its own. Every obligation is discharged through the same
// refunds.Service.DriveReversal the staff endpoint and the bulk runner already use, which
// is idempotent, never errors, and enforces the void-before-capacity ordering that is a
// safety property rather than a preference (ADR-038 §1). One reversal path, three callers.
package reversal

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
)

// Store is the durable state the runner decides against. A port rather than a *sql.DB so
// the decision table can be exercised against fakes; the SQL predicates it stands for —
// eligibility, the lease, the claim fence, the backoff, parking — are covered against real
// PostgreSQL by the store's smoke tests, because that is the tier those mechanisms live at.
type Store interface {
	Claim(ctx context.Context, limit int, lease time.Duration) ([]store.ClaimedReversal, error)
	Release(ctx context.Context, refundID, claimID uuid.UUID, progressed bool, cause string) error
	Finish(ctx context.Context, refundID, claimID uuid.UUID) error
	Abandon(ctx context.Context, refundID, claimID uuid.UUID) error
	Backlog(ctx context.Context) (store.ReversalBacklog, error)
}

// Reverser is the shared reversal unit (internal/refunds). Only the one method: this
// runner must never be able to move money, and a port that cannot express a refund is a
// stronger guarantee of that than a comment saying it does not.
type Reverser interface {
	DriveReversal(ctx context.Context, refund store.Refund) store.Refund
}

// MaxCallsPerRefund is the longest external-call chain one refund can drive: the access
// void, then the inventory capacity return. LeaseFor derives from it, so the lease and the
// chain grow together rather than drifting apart.
const MaxCallsPerRefund = 2

// LeaseFor sizes the batch lease from the caller's own I/O budget. A batch is driven
// sequentially and each refund can make MaxCallsPerRefund calls, each bounded only by the
// HTTP client timeout, so the pass's worst case is batch × calls × timeout. Sizing the
// lease from an unrelated per-row guess is how it silently ends up shorter than the batch
// it protects: the lease lapses mid-pass, a successor claims rows the first runner is still
// driving, and the claim token only fences the final database write — not the HTTP call
// already in flight.
func LeaseFor(batch int, callTimeout time.Duration) time.Duration {
	if batch <= 0 {
		batch = 1
	}
	if callTimeout <= 0 {
		callTimeout = 10 * time.Second
	}
	// Plus a margin for database work and scheduling between calls.
	return time.Duration(batch)*MaxCallsPerRefund*callTimeout + 60*time.Second
}

// Runner reconciles outstanding refund reversals.
type Runner struct {
	store    Store
	reverser Reverser
	interval time.Duration
	batch    int
	lease    time.Duration
	log      *slog.Logger
}

func New(st Store, rev Reverser, interval time.Duration, batch int, lease time.Duration, log *slog.Logger) *Runner {
	if interval <= 0 {
		interval = time.Minute
	}
	if batch <= 0 {
		batch = 16
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{store: st, reverser: rev, interval: interval, batch: batch, lease: lease, log: log}
}

// Run drives until ctx is cancelled, starting with one pass immediately: on restart, the
// obligations stranded by the process that died are the whole point, and waiting an
// interval to notice them leaves refunded tickets admitting for no reason.
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

// RunOnce drains the claimable backlog in bounded batches. Returns how many reversals it
// drove to completion, for tests and for callers draining to quiescence.
//
// It drains rather than doing one batch per tick: a backlog deeper than one batch would
// otherwise wait a whole interval per batch, which after a long outage is exactly when the
// backlog is deepest and the wait is least affordable.
func (r *Runner) RunOnce(ctx context.Context) int {
	var resolved int
	for {
		claimed, err := r.store.Claim(ctx, r.batch, r.lease)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				r.log.ErrorContext(ctx, "claim outstanding reversals", "err", err)
			}
			return resolved
		}
		if len(claimed) == 0 {
			return resolved
		}
		for i, c := range claimed {
			// Checked per ROW, not per batch: an interrupted pass must leave the rest of
			// its claim immediately reclaimable rather than parking it behind the full
			// lease — with a big batch that is minutes of nothing happening, for
			// obligations that are already overdue. The lease exists to survive a crash,
			// not to be the cost of an orderly restart.
			if ctx.Err() != nil {
				r.abandonUndriven(claimed[i:])
				return resolved
			}
			if r.drive(ctx, c) {
				resolved++
			}
		}
		if len(claimed) < r.batch {
			return resolved
		}
	}
}

// drive discharges what it can of one refund's reversal and records the outcome. It
// reports whether the reversal is now COMPLETE.
func (r *Runner) drive(ctx context.Context, c store.ClaimedReversal) bool {
	after := r.reverser.DriveReversal(ctx, c.Refund)

	if after.TicketsVoided && after.CapacityReturned {
		if err := r.store.Finish(ctx, after.ID, c.ClaimID); err != nil {
			r.log.ErrorContext(ctx, "finish reversal claim", "refund_id", after.ID, "err", err)
		}
		return true
	}

	// Still outstanding. Whether this pass made PROGRESS is what decides between backing
	// off with the budget reset and spending it down toward parking. Commerce cannot see
	// WHY a downstream refused — inventory's partial-seated refusal (TKT-164) is decided
	// from `claim_seats` and `claims.returned_quantity` in ITS database — so a permanently
	// undischargeable obligation is recognised by making no progress, never by predicting
	// the refusal from state commerce does not have.
	progressed := c.Progressed(after)
	cause := "ticket voiding outstanding"
	if after.TicketsVoided {
		cause = "capacity return outstanding"
	}
	if err := r.store.Release(ctx, after.ID, c.ClaimID, progressed, cause); err != nil {
		r.log.ErrorContext(ctx, "release reversal claim", "refund_id", after.ID, "err", err)
	}
	return false
}

// abandonUndriven hands back claims the pass never got to, so the next pass (or the next
// boot) picks them up immediately rather than waiting out the lease, and refunds the
// attempt charged at claim time.
//
// It uses a fresh, bounded context: the caller's is already cancelled, so reusing it would
// fail every one of these writes and defeat the point.
func (r *Runner) abandonUndriven(claims []store.ClaimedReversal) {
	if len(claims) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released int
	for _, c := range claims {
		// Conditional on the claim token in SQL, so a lease that lapsed and was re-claimed
		// by a successor mid-shutdown is left alone.
		if err := r.store.Abandon(ctx, c.Refund.ID, c.ClaimID); err != nil {
			r.log.WarnContext(ctx, "abandon undriven reversal claim", "refund_id", c.Refund.ID, "err", err)
			continue
		}
		released++
	}
	r.log.InfoContext(ctx, "released undriven reversal claims on shutdown",
		"released", released, "of", len(claims))
}
