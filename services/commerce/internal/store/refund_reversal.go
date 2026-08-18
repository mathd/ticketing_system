package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Reversal reconciliation (TKT-163, ADR-062): the durable side of driving an outstanding
// refund obligation to completion with no human replaying the request.
//
// The lifecycle is `orders`' recovery lifecycle (recovery.go), copied rather than shared —
// same reasoning ADR-040 gave for not merging the cancellation runner into
// internal/recovery: different eligibility, different terminal states.

// MaxReversalAttempts bounds attempts before a refund's reversal is parked.
//
// A bound is required because some obligations can NEVER be discharged: inventory refuses
// a partial return of a seated claim (TKT-164), and commerce cannot predict that refusal —
// seatedness lives in inventory's `claim_seats` and "partial" depends on
// `claims.returned_quantity`, neither of which commerce can read. So the refusal is
// observed rather than predicted, and this is the budget that observation spends.
//
// The bound is safe only because attempts RESET on progress (see ClaimedReversal.Progressed
// and ReleaseReversalClaim): without that, a long access outage would retire a row that was
// about to recover, reintroducing the exact failure this ticket closes.
const MaxReversalAttempts = 10

// ClaimedReversal is one leased refund whose reversal is still owed. It carries the
// identifiers DriveReversal needs — including HoldID, which lives on the reservation and is
// the reason this cannot reuse LookupRefundByID (which documents that it does not populate
// it).
type ClaimedReversal struct {
	Refund   Refund
	ClaimID  uuid.UUID
	Attempts int
}

// Progressed reports whether driving moved either obligation forward, comparing the state
// after the drive against the state the row was claimed with.
//
// This is what makes a bounded attempt budget compatible with an outage of any length: a
// pass that discharges an obligation restores the budget, so only a row that is making NO
// progress at all spends it down to parking. Discharging one of two obligations counts —
// otherwise a refund whose voiding succeeds and whose capacity keeps failing would burn its
// budget on the half that already works.
func (c ClaimedReversal) Progressed(after Refund) bool {
	return (after.TicketsVoided && !c.Refund.TicketsVoided) ||
		(after.CapacityReturned && !c.Refund.CapacityReturned)
}

// Outstanding reports whether either obligation is still owed.
func (c ClaimedReversal) Outstanding() bool {
	return !c.Refund.TicketsVoided || !c.Refund.CapacityReturned
}

// ClaimOutstandingReversals leases refunds whose reversal is still owed, oldest due first.
//
// Eligibility, and why each predicate is load-bearing:
//
//   - `status='completed'` — the money is durable. A PENDING refund has not moved money, and
//     voiding its tickets would reverse a sale that has not happened.
//   - either obligation NULL — the outstanding set. A refund with both discharged is done and
//     must never be re-driven.
//   - `reversal_parked_at IS NULL` — a parked row has spent its budget and awaits a human.
//     Without this the runner re-claims permanently-refused rows forever, oldest-first, and
//     starves everything behind them.
//   - `reversal_next_attempt_at<=now()` — the backoff. Written on release; this is what
//     reads it.
//   - lease absent or expired — the fence against a concurrent runner or a second replica.
//
// The claimable set is chosen in a CTE under FOR UPDATE SKIP LOCKED, then joined to
// orders/reservations for the identifiers the drive needs — the row lock has to sit on the
// selection, not the join, or concurrent runners would contend on reservations too. Same
// shape as ClaimStuckOrders.
func ClaimOutstandingReversals(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]ClaimedReversal, error) {
	claim := uuid.New()
	rows, err := db.QueryContext(ctx, `
		WITH claimable AS (
			SELECT id FROM order_refunds
			WHERE status='completed'
			  AND (tickets_voided_at IS NULL OR capacity_returned_at IS NULL)
			  AND reversal_parked_at IS NULL
			  AND reversal_next_attempt_at<=now()
			  AND (reversal_lease_until IS NULL OR reversal_lease_until<=now())
			ORDER BY reversal_next_attempt_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		), claimed AS (
			UPDATE order_refunds r
			SET reversal_lease_until=now()+make_interval(secs => $2),
			    reversal_claim_id=$3,
			    reversal_attempts=r.reversal_attempts+1
			WHERE r.id IN (SELECT id FROM claimable)
			RETURNING r.id, r.order_id, r.organizer_id, r.quantity, r.status,
			          r.tickets_voided_at, r.capacity_returned_at,
			          r.reversal_claim_id, r.reversal_attempts
		)
		SELECT c.id, c.order_id, c.organizer_id, c.quantity, c.status,
		       c.tickets_voided_at, c.capacity_returned_at,
		       res.hold_id, c.reversal_claim_id, c.reversal_attempts
		FROM claimed c
		JOIN orders o ON o.id = c.order_id
		JOIN reservations res ON res.id = o.reservation_id`,
		limit, lease.Seconds(), claim)
	if err != nil {
		return nil, fmt.Errorf("claim outstanding reversals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ClaimedReversal
	for rows.Next() {
		var c ClaimedReversal
		var voidedAt, returnedAt sql.NullTime
		if err := rows.Scan(&c.Refund.ID, &c.Refund.OrderID, &c.Refund.OrganizerID,
			&c.Refund.Quantity, &c.Refund.Status, &voidedAt, &returnedAt,
			&c.Refund.HoldID, &c.ClaimID, &c.Attempts); err != nil {
			return nil, err
		}
		c.Refund.Completed = c.Refund.Status == "completed"
		c.Refund.TicketsVoided = voidedAt.Valid
		c.Refund.CapacityReturned = returnedAt.Valid
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReleaseReversalClaim returns a still-outstanding refund to the claimable set, backing off
// and parking it once attempts are exhausted. Conditional on the claim id, so a claimant
// whose lease lapsed mid-drive cannot disturb its successor.
//
// `progressed` resets the attempt budget. That is the whole reason a bound is safe here: an
// access outage of any length costs at most one attempt per pass while nothing moves, and
// the first discharged obligation restores the budget in full. Parking is reserved for a row
// that makes no progress at all — which is what a permanently refused obligation looks like
// from commerce, since it cannot see WHY inventory refused.
//
// The backoff expression is `orders`' (recovery.go), deliberately: two runners in one service
// with different backoff curves is an operational surprise for no benefit.
func ReleaseReversalClaim(ctx context.Context, db OutboxDB, refundID, claimID uuid.UUID, progressed bool, cause string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    reversal_last_error=$4,
		    reversal_attempts=CASE WHEN $3 THEN 0 ELSE reversal_attempts END,
		    reversal_next_attempt_at=now() + CASE WHEN $3 THEN interval '0'
		        ELSE least(make_interval(secs => power(2, least(reversal_attempts, 8))::double precision), interval '5 minutes') END,
		    reversal_parked_at=CASE WHEN NOT $3 AND reversal_attempts>=$5 THEN now() ELSE NULL END
		WHERE id=$1 AND reversal_claim_id=$2`,
		refundID, claimID, progressed, cause, MaxReversalAttempts)
	return err
}

// FinishReversalClaim releases the lease of a refund whose reversal is now COMPLETE. It
// clears the backoff and the error rather than leaving the last transient failure's text on
// a discharged row, where it would read as an unresolved problem.
//
// Conditional on the claim id for the same fencing reason as the release path.
func FinishReversalClaim(ctx context.Context, db OutboxDB, refundID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    reversal_attempts=0,
		    reversal_last_error=NULL,
		    reversal_next_attempt_at=now()
		WHERE id=$1 AND reversal_claim_id=$2`, refundID, claimID)
	return err
}

// AbandonReversalClaim hands back a claim the pass never drove — a shutdown mid-batch. It
// refunds the attempt charged at claim time, because a row must not arrive at its first real
// failure with budget already spent on work that never happened, and it does NOT touch
// next_attempt_at: the row was never tried, so there is nothing to back off from.
//
// Conditional on the claim id, so a lease that lapsed and was re-claimed by a successor
// mid-shutdown is left alone.
func AbandonReversalClaim(ctx context.Context, db OutboxDB, refundID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    reversal_attempts=greatest(reversal_attempts-1, 0)
		WHERE id=$1 AND reversal_claim_id=$2`, refundID, claimID)
	return err
}

// ReversalBacklog is what an operator (and the gauges) read to know whether the reconciler
// is keeping up, and whether anything has given up.
type ReversalBacklog struct {
	// Outstanding counts completed refunds still owing an obligation, parked or not.
	Outstanding int64
	// Parked counts those that spent their attempt budget and now await a human. This is
	// the number that makes parking honest: it converts "retries forever" into "stopped,
	// visibly" rather than "stopped, silently".
	Parked int64
	// OldestAgeSeconds is the age of the oldest outstanding obligation, parked included.
	// A backlog that is small but old is a different problem from one that is large and
	// fresh, and a count alone cannot tell them apart.
	OldestAgeSeconds int64
}

// ReadReversalBacklog reports the reconciler's queue depth for observability. It is not a
// gate: nothing here can make commerce unready.
func ReadReversalBacklog(ctx context.Context, db *sql.DB) (ReversalBacklog, error) {
	var b ReversalBacklog
	err := db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE reversal_parked_at IS NOT NULL),
		       coalesce(max(extract(epoch FROM now()-created_at))::bigint, 0)
		FROM order_refunds
		WHERE status='completed'
		  AND (tickets_voided_at IS NULL OR capacity_returned_at IS NULL)`).
		Scan(&b.Outstanding, &b.Parked, &b.OldestAgeSeconds)
	return b, err
}
