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
	// Release takes the obligations as OBSERVED AT CLAIM TIME and decides progress in SQL
	// against the row as it stands. It does not take a `progressed bool`: this runner does
	// not hold a monopoly on discharging a reversal, so a verdict computed from its own
	// before/after would be wrong exactly when a concurrent replay helped (ai-review F2).
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

// LeaseFor sizes the batch lease from the I/O budget of the client that will actually make
// the calls. A batch is driven sequentially and each refund can make MaxCallsPerRefund
// calls, each bounded only by that client's timeout, so the pass's worst case is
// batch × calls × timeout.
//
// **Pass the timeout of the transport DriveReversal really uses**, which is
// `obs.Client()`'s (`shared/go/obs`), not some other worker's constant. The first version of
// this call site borrowed `recoveryCallTimeout` (10s) while the refund service runs on
// obs.Client (30s), giving a 380s lease over work that can take 960s (ai-review F1). A lease
// shorter than the work it protects is worse than no lease: a second replica reclaims rows
// the first is still driving, and the claim token fences only the final database write —
// never the access or inventory call already in flight. `LeaseIsNotShorterThanItsBatch`
// pins the relationship so the two cannot drift apart again.
func LeaseFor(batch int, callTimeout time.Duration) time.Duration {
	if batch <= 0 {
		batch = 1
	}
	if callTimeout <= 0 {
		callTimeout = 30 * time.Second
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
// MaxBatchesPerPass bounds one drain (ai-review pass 3).
//
// A pass drains rather than doing one batch per tick, because after an outage the backlog is
// deepest exactly when waiting an interval per batch is least affordable. But an UNBOUNDED
// drain is two defects at once: the per-pass `driven` set grows for as long as the loop runs,
// and a workload arriving at or above processing rate means the loop never returns at all —
// so the set grows for the life of the process and the ticker never fires again.
//
// A bound makes both finite. What is left undrained is not lost: it is claimable, and the
// next tick is a minute away at most. The number is deliberately generous — 64 batches of 16
// is 1024 refunds per pass — because the common case is a backlog far smaller than one batch
// and the bound should only ever bite on a genuinely pathological queue.
const MaxBatchesPerPass = 64

// ONE DRIVE PER REFUND PER PASS, enforced here rather than left to the backoff (ai-review
// pass 2). A released row becomes due again after its floor or its backoff — both of which
// can be shorter than the time the rest of a slow batch takes — so a drain loop that only
// re-claimed would happily pick the same refund up again in a later batch of the same pass.
// At the extreme that lets one row spend its whole attempt budget and PARK inside a single
// RunOnce, which is the opposite of what a bounded budget spread over passes is for.
//
// `driven` is per-pass, bounded by MaxBatchesPerPass × batch, and discarded on return.
func (r *Runner) RunOnce(ctx context.Context) int {
	var resolved int
	driven := make(map[uuid.UUID]struct{})
	for pass := 0; pass < MaxBatchesPerPass; pass++ {
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
		var fresh int
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
			if _, seen := driven[c.Refund.ID]; seen {
				// Already driven this pass. Hand the claim straight back — undriven, so it
				// costs no attempt.
				//
				// It stays DUE, so the store can offer it again immediately: `Abandon`
				// clears the lease and the token and deliberately does not touch
				// next_attempt_at, since the row was never tried. That is why a duplicate
				// must not merely be skipped — a batch made entirely of duplicates would
				// otherwise spin. The `fresh == 0` exit below is what stops it, and the
				// bound above is what stops everything else.
				//
				// On the CALLER's context, not abandonUndriven's detached one: that exists
				// because a shutdown's context is already cancelled, and reusing it here
				// would both mislabel this as a shutdown and let a degraded database burn a
				// 5s timeout per duplicate that cancellation cannot interrupt.
				// The ctx.Err() check above is NOT atomic with this call: a shutdown landing
				// between them, or while the write is in flight, fails it on a cancelled
				// context (ai-review pass 4). Logging and moving on would leave the row
				// leased for the full lease — ~17 minutes at the defaults — with its
				// obligation outstanding the whole time. So a cancellation here falls back
				// to the detached path, which is exactly the case that path exists for.
				if err := r.store.Abandon(ctx, c.Refund.OrganizerID, c.Refund.ID, c.ClaimID); err != nil {
					if ctx.Err() != nil {
						r.abandonUndriven(claimed[i : i+1])
					} else {
						r.log.WarnContext(ctx, "hand back a reversal claim already driven this pass",
							"refund_id", c.Refund.ID, "err", err)
					}
				}
				continue
			}
			driven[c.Refund.ID] = struct{}{}
			fresh++
			if r.drive(ctx, c) {
				resolved++
			}
		}
		// A batch that was full but contained nothing new means the queue is now just this
		// pass's own releases coming back round; stop rather than spin.
		//
		// This can end a drain while genuinely new work sorts behind those duplicates —
		// accepted, and bounded in cost: the claim is ordered by next_attempt_at, a
		// duplicate's is in the past and a fresh row's is at most a minute out, so the next
		// tick reaches them. Trading a bounded delay for a loop that cannot spin is the
		// right side of that: the obligations are under-selling while they wait, never
		// over-selling.
		if len(claimed) < r.batch || fresh == 0 {
			return resolved
		}
	}
	r.log.InfoContext(ctx, "reversal drain hit its per-pass bound; the rest waits for the next tick",
		"batches", MaxBatchesPerPass, "driven", len(driven))
	return resolved
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
	// "no progress" would park a recovering refund (ai-review F2).
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
		if err := r.store.Abandon(ctx, c.Refund.OrganizerID, c.Refund.ID, c.ClaimID); err != nil {
			r.log.WarnContext(ctx, "abandon undriven reversal claim", "refund_id", c.Refund.ID, "err", err)
			continue
		}
		released++
	}
	r.log.InfoContext(ctx, "released undriven reversal claims on shutdown",
		"released", released, "of", len(claims))
}
