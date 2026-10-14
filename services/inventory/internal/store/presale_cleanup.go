package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The cross-pool presale cleanup (TKT-543; ADR-064 §3).
//
// codeRedeemedQuantity counts an expired held claim until a terminal write commits
// (presaleUsageClaims). When that count refuses an admission, the admission rolls back and
// this file expires the code's due claims on the OTHER pools, each pool in its own
// transaction, so that an expired hold on a quiet pool does not lock its code out of every
// other pool.
//
// It is its own file for one reason: it commits with a plain tx.Commit() and invalidates
// no availability cache (D16), so it is exempt, as a whole file, from
// TestAvailabilityMutationsUseInvalidatingCommit. Keep any other kind of write out of it.

// expiredPresalePoolsSQL is the cleanup's DISCOVERY (D2, D3): the pools other than the
// admission's that hold an expired, still-held claim citing exactly this (organizer,
// channel, code), with the quantity on each that a terminal write would stop counting (D7).
//
// It decides which pools the cleanup locks. Which claims of such a pool are written is
// decided again by presaleCleanupExpireSQL, under the pool's lock. A pool this query
// selects wrongly therefore costs a lock wait and an empty commit, but no write. Each
// predicate is load-bearing all the same, and TestRedemptionCountIsIndexBacked has a row
// that only it excludes (all but `expires_at IS NOT NULL`, which keeps sweepExpired's own
// spelling: a NULL expiry already fails `<=`).
//
// The admission's own pool is excluded because the admission already swept it inside its
// own transaction before it counted: its expired claims were not part of the usage that
// refused it.
//
// now() belongs to this statement alone: discovery runs outside any transaction, and the
// cleanup rechecks status and expiry under the pool lock anyway. Backed by
// claims_presale_usage (organizer_id, channel_code, presale_code, status, expires_at), the
// same index as codeRedeemedQuantity (ADR-019).
const expiredPresalePoolsSQL = `SELECT pool_id, sum(quantity)::bigint FROM claims
	WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3
	  AND status='held' AND expires_at IS NOT NULL AND expires_at <= now()
	  AND pool_id <> $4
	GROUP BY pool_id ORDER BY pool_id`

// presaleCleanupPoolLock locks ONE foreign pool for the cleanup (D9).
//
// Scoped to the organizer so that a discovery mistake never makes this request wait for
// or lock another organizer's pool: such a pool finds no row and is skipped.
// presaleCleanupExpireSQL is scoped to the organizer as well, so the lock is not what
// stops the write.
//
// The marker exists for the tests. Transition locks a pool with the same statement minus
// the marker and the organizer, and a handshake must tell the cleanup's wait from the
// finalize's (plan amendment A1).
const presaleCleanupPoolLock = `SELECT 1 /* presale-cleanup pool lock */ FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`

// presaleCleanupLockTimeout bounds how long ONE cleanup transaction waits for a foreign
// pool (D8).
//
// The cleanup WAITS rather than skipping a locked pool: the pools that matter are held by
// a finalize or a draw-down, which never sweep, and SKIP LOCKED would give up on exactly
// those. The bound stops a refused request from queuing behind a hot pool for a whole
// on-sale. When it fires, that pool's cleanup ends and the next pool is tried (D14): a
// cleanup failure costs liveness, never correctness.
const presaleCleanupLockTimeout = 2 * time.Second

// presaleCleanupExpireSQL is the cleanup's terminal write for ONE pool (D13): it expires
// the due held claims of that pool that cite exactly this (organizer, channel, code), and
// records each expiry exactly as sweepExpired does. The history columns and values are
// sweepExpired's, and TestPresaleCleanupTouchesOnlyClaimsCitingTheRequestedCode compares
// the rows.
//
// NOT sweepExpired, which expires EVERY due claim of the pool. A GA replay answers 409 for
// a hold that is held and past its expiry, and 200 for one already expired. So if a
// request with a guessed code could expire an unrelated hold, the hold's owner could
// replay it and learn that the code is valid and exhausted (review finding 2), a
// distinction ADR-064 §4 forbids.
//
// What a caller can see of this write. It changes only claims that cite the code, it
// changes no availability answer (see cleanupExpiredPresalePool), and it invalidates no
// cache (D16). Through a status or a body, it is therefore visible only to a caller who
// holds a claim citing the code (by replaying that claim) or who presents the code. Its
// pool lock shows only as latency, which ADR-064 §4 does not claim to hide.
//
// Every predicate is load-bearing (TestPresaleCleanupTouchesOnlyClaimsCitingTheRequestedCode
// and, for pool_id, TestPresaleCleanupContinuesPastALockedPool), except organizer_id: the
// store writes into a pool only claims of the pool's organizer, so no test can build the
// row it excludes. It stays so the write never depends on that.
const presaleCleanupExpireSQL = `WITH swept AS (
		UPDATE claims SET status='expired', updated_at=now()
		WHERE pool_id=$1 AND organizer_id=$2 AND channel_code=$3 AND presale_code=$4
		  AND status='held' AND expires_at IS NOT NULL AND expires_at<=now()
		RETURNING id, organizer_id, quantity)
	INSERT INTO claim_history(id, organizer_id, claim_id, action, actor, reason, quantity, quantity_after, status_after)
	SELECT gen_random_uuid(), organizer_id, id, 'expire', 'system', 'ttl_elapsed', quantity, 0, 'expired' FROM swept`

// reclaimExpiredPresaleUsage is the cross-pool cleanup (D2, D3, D7, D8, D9, D13, D14, D16).
// It reports whether the caller should run its admission once more.
//
// WHY IT EXISTS. Terminal writes are pool-local and lazy (ADR-010: no sweeper is
// required), so an expired hold on a quiet pool would otherwise lock its code out of every
// other pool indefinitely. This is the "next mutation" that reclaims it.
//
// WHEN IT RUNS. Only after an admission was refused by the code count AND rolled back, so
// it starts holding no lock at all. When it waits for a foreign pool, its caller holds
// neither its own pool nor the code, so a mirror admission that holds that foreign pool
// and waits for the code is not waiting for anything of the caller's
// (TestPresaleMirrorAdmissionsReleaseLocksBeforeCleanup). The refusal comes after the
// window and seller checks, so a caller who may not sell never reaches it.
//
// WHAT IT COSTS. One discovery query, then one transaction per discovered pool, each
// committed before the next starts, in pool-id order. It returns false, and the caller
// refuses at once, when nothing it could reclaim would cover the shortfall (D7): if a code
// is genuinely exhausted, an on-sale of refused requests must not take the hot pool's lock
// twice each.
//
// ERRORS ARE NOT RETURNED (D8). A returned error would be a 500 that only a valid,
// exhausted code can produce, which is the oracle ADR-064 §4 forbids. The cost is that a
// persistent cleanup failure is silent.
//
// ONE FAILED POOL DOES NOT END THE CLEANUP (D14). A pool that cannot be locked in time, or
// fails otherwise, is left as it is and the next pool is tried: stopping would let a busy
// pool that sorts first keep a quiet pool's expired usage counted for as long as it stays
// busy, and would throw away a retry that the pools already cleaned make fit. The caller
// runs its admission again only if at least one cleanup transaction committed. With none,
// it does not retry: that is a chosen policy, to keep a refusal whose cleanup changed
// nothing at one admission. Something concurrent could still have made a retry succeed,
// such as another terminal write or an operator raising the cap (IssuePresaleCode); that
// request is refused, and the next one sees the change. A discovery error ends the
// cleanup with no retry.
func (p *Postgres) reclaimExpiredPresaleUsage(ctx context.Context, org, admissionPool uuid.UUID, channel, code string, qty int32, refused presaleCapExceeded) bool {
	pools, reclaimable, err := p.expiredPresalePools(ctx, org, channel, code, admissionPool)
	if err != nil || len(pools) == 0 {
		return false
	}
	if refused.redeemed-reclaimable+int64(qty) > refused.max {
		return false
	}
	committed := false
	for _, pool := range pools {
		if ok, err := p.cleanupExpiredPresalePool(ctx, org, pool, channel, code); err == nil && ok {
			committed = true
		}
	}
	return committed
}

// expiredPresalePools reads discovery completely and closes it before any cleanup
// transaction opens.
func (p *Postgres) expiredPresalePools(ctx context.Context, org uuid.UUID, channel, code string, exclude uuid.UUID) ([]uuid.UUID, int64, error) {
	rows, err := p.db.QueryContext(ctx, expiredPresalePoolsSQL, org, channel, code, exclude)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var pools []uuid.UUID
	var total int64
	for rows.Next() {
		var pool uuid.UUID
		var qty int64
		if err = rows.Scan(&pool, &qty); err != nil {
			return nil, 0, err
		}
		pools = append(pools, pool)
		total += qty
	}
	return pools, total, rows.Err()
}

// cleanupExpiredPresalePool writes the terminal state of the due claims citing this code
// in ONE foreign pool, in its own transaction, under that pool's lock only (D9, D13). It
// reports whether its transaction committed.
//
// The UPDATE in presaleCleanupExpireSQL rechecks status and expiry under the lock, in a
// statement that starts after the lock is granted. A claim that a queued finalize or
// draw-down settled first is no longer due, so the cleanup sees the committed state and
// leaves it.
//
// A PLAIN COMMIT, with no availability-cache invalidation (D16). An invalidation evicts
// every cached variant of the pool, and the public availability read shows the eviction (a
// fresh Age header and body): only a valid, exhausted code triggers this cleanup, so that
// would be the same code oracle D13 closed for the claim rows. No answer needs it. The
// write flips only held claims that are already due, and every availability answer counts
// held claims through liveClaims (directly, or through consumingClaims), which already
// leaves out a due hold: Availability, SeatOccupancy, StaffAvailability and its channel
// rows, reservedForChannelsSQL, the sold-out decision and effectiveCapacity. A fresh read
// gives the same answer before and after this write, and a cached one is no staler than
// without the cleanup.
//
// Two things sweepExpired does are left out on purpose (D13):
//   - reconcileCapacity. Settling a draining cut at cleanup time would be one more change
//     that a code request triggers in a pool it does not sell from. It changes no count
//     either: reconcileCapacity reads demand through liveClaims, which already leaves out
//     every due hold. The cut settles at the next write on the pool that runs
//     reconcileCapacity (a new hold, placement or capacity adjustment, which sweep first;
//     a release; a refund return; an expiry that Transition or a draw-down commits; not a
//     finalize or a confirm), exactly as it would have without this cleanup. Reads do not
//     wait for it: effectiveCapacity re-derives the clamp from liveClaims.
//   - releaseSeatsForTerminal. A presale code can be cited only by GA claims: no seated
//     claim path takes a code, so there are no seats to release.
//
// NO offering guard. An archived or closed pool refuses new holds, so no hold sweeps it:
// an archived pool never again (archival is terminal, and AdjustCapacity refuses it too), a
// closed one only after it reopens or staff adjust its capacity. Guarding here would pin
// the code that long. classifySeatClaimsInPool is the precedent for a sweep without the
// guard.
func (p *Postgres) cleanupExpiredPresalePool(ctx context.Context, org, pool uuid.UUID, channel, code string) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	timeout := presaleCleanupLockTimeout
	if p.cleanupLockTimeout > 0 {
		timeout = p.cleanupLockTimeout
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, timeout.Milliseconds())); err != nil {
		return false, err
	}
	var locked int
	err = tx.QueryRowContext(ctx, presaleCleanupPoolLock, pool, org).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, presaleCleanupExpireSQL, pool, org, channel, code); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
