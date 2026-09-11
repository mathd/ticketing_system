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
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/store"
	"ticketing/services/commerce/internal/worklease"
)

// Store is the durable state the runner decides against. A port rather than a *sql.DB so
// the decision table can be exercised against fakes; the SQL predicates it stands for —
// eligibility, the lease, the claim fence, the backoff, parking — are covered against real
// PostgreSQL by the store's smoke tests, because that is the tier those mechanisms live at.
type Store interface {
	Claim(ctx context.Context, limit int, lease time.Duration) ([]store.ClaimedReversal, error)
	// Release takes the obligations as OBSERVED AT CLAIM TIME and decides progress in SQL
	// against the row as it stands. It does not take a `progressed bool`: this runner does
	// not hold a monopoly on discharging a reversal, so a verdict computed from its own
	// before/after would be wrong exactly when a concurrent replay helped.
	Release(ctx context.Context, org, refundID, claimID uuid.UUID, voidedAtClaim, capacityAtClaim bool, cause string) error
	Finish(ctx context.Context, org, refundID, claimID uuid.UUID) error
	Abandon(ctx context.Context, org, refundID, claimID uuid.UUID) error
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

// LeaseFor sizes a sequential batch from the refund service client's timeout.
func LeaseFor(batch int, callTimeout time.Duration) (time.Duration, error) {
	if batch <= 0 {
		batch = 1
	}
	if callTimeout <= 0 {
		callTimeout = 30 * time.Second
	}
	return worklease.ForBatch(batch, MaxCallsPerRefund, callTimeout, 60*time.Second)
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

// MaxBatchesPerPass bounds one drain. It keeps the loop's per-pass `driven` map finite and
// guarantees that a continuously replenished queue cannot monopolize the runner.
//
// What is left undrained is not lost: it is claimable, and the next tick is a minute away
// at most. The number is deliberately generous — 64 batches of 16 is 1024 refunds per pass
// — because the common case is a backlog far smaller than one batch and the bound should
// only ever bite on a genuinely pathological queue.
const MaxBatchesPerPass = 64

// RunOnce drains the claimable backlog in bounded batches. Returns how many reversals it
// drove to completion, for tests and for callers draining to quiescence. Draining avoids
// waiting a full interval between batches after an outage.
//
// The lifecycle — bounded batching, one drive per refund per pass, per-row cancellation,
// and the detached hand-back of an undriven suffix — lives in `worklease.Drain`, which
// carries the reasoning for each of those. What stays here is the part that is reversal's:
// which refund a claim is, and what `drive` does with it.
func (r *Runner) RunOnce(ctx context.Context) int {
	return worklease.Drain(ctx, worklease.Adapter[store.ClaimedReversal, uuid.UUID]{
		Claim: r.store.Claim,
		Key: func(c store.ClaimedReversal) uuid.UUID {
			return c.Refund.ID
		},
		Process: r.drive,
		Abandon: func(ctx context.Context, c store.ClaimedReversal) error {
			return r.store.Abandon(ctx, c.Refund.OrganizerID, c.Refund.ID, c.ClaimID)
		},
		Batch:             r.batch,
		Lease:             r.lease,
		MaxBatchesPerPass: MaxBatchesPerPass,
		Log:               r.log,
		Name:              "reversal",
	})
}

// drive discharges what it can of one refund's reversal and records the outcome. It
// reports whether the reversal is now COMPLETE.
func (r *Runner) drive(ctx context.Context, c store.ClaimedReversal) bool {
	after := r.reverser.DriveReversal(ctx, c.Refund)

	if after.TicketsVoided && after.CapacityReturned {
		if err := r.store.Finish(ctx, after.OrganizerID, after.ID, c.ClaimID); err != nil {
			r.log.ErrorContext(ctx, "finish reversal claim", "refund_id", after.ID, "err", err)
		}
		return true
	}

	// Still outstanding as far as THIS claimant can see. Whether the row actually made
	// progress is decided by the store, against the row as it stands, from the obligations
	// observed at claim time — because a concurrent staff replay or cancellation run can
	// discharge one of these obligations without this runner's knowledge, and calling that
	// "no progress" would park a recovering refund.
	//
	// Progress is what decides between backing off with the budget reset and spending it
	// down toward parking. Commerce cannot see WHY a downstream refused — inventory's
	// partial-seated refusal (TKT-164) is decided from `claim_seats` and
	// `claims.returned_quantity` in ITS database — so a permanently undischargeable
	// obligation is recognised by making no progress, never by predicting the refusal.
	cause := "ticket voiding outstanding"
	if after.TicketsVoided {
		cause = "capacity return outstanding"
	}
	if err := r.store.Release(ctx, c.Refund.OrganizerID, c.Refund.ID, c.ClaimID,
		c.Refund.TicketsVoided, c.Refund.CapacityReturned, cause); err != nil {
		r.log.ErrorContext(ctx, "release reversal claim", "refund_id", c.Refund.ID, "err", err)
	}
	return false
}
