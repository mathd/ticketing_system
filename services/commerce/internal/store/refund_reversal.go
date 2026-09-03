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

// Progress is deliberately NOT a method here. It was, comparing the claimant's before/after
// values, and that was wrong: nothing outside the runner respects this lease — the staff
// refund endpoint and the cancellation runner drive the same reversal whenever they like —
// so a concurrent replay can persist a discharge this claimant never sees, and an in-memory
// verdict would call that "no progress" and park a row that just advanced (ai-review F2).
// ReleaseReversalClaim decides it in SQL, against the row as it stands, using the
// claim-time observations passed to it.

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
//   - `source_organizer_id = organizer_id` — the source reservation belongs to this refund's
//     organizer. Checked here rather than only at the join below, because this CTE selects
//     and the next one leases before that join runs (TKT-267). It is a plain column
//     comparison on the queue row, and it is IN the partial index, so a malformed row is
//     absent from the index rather than read and filtered (TKT-268, ADR-070).
//
// The claimable set is chosen in a CTE under FOR UPDATE SKIP LOCKED, then joined to
// orders/reservations for the identifiers the drive needs — the row lock has to sit on the
// selection, not the join, or concurrent runners would contend on reservations too. Same
// shape as ClaimStuckOrders.
// claimOutstandingReversalsSQL is the shipped claim statement, extracted so the acceptance test can
// EXPLAIN THE EXACT SQL THIS FUNCTION RUNS rather than a copy of it. A copied query in a
// plan test drifts from production silently and keeps asserting about the copy
// (season_smoke_test.go says the same thing about catalog's scoped reads).
const claimOutstandingReversalsSQL = `
		WITH claimable AS (
			SELECT rf.organizer_id, rf.id FROM order_refunds AS rf
			WHERE rf.status='completed'
			  AND (rf.tickets_voided_at IS NULL OR rf.capacity_returned_at IS NULL)
			  AND rf.reversal_parked_at IS NULL
			  AND rf.reversal_next_attempt_at<=now()
			  AND (rf.reversal_lease_until IS NULL OR rf.reversal_lease_until<=now())
			  -- The source reservation is this organizer's, checked BEFORE the lease
			  -- (TKT-267). The sibling of the exchange claim's conjunct 6; both queries have
			  -- the same three-stage shape and had the same defect. This CTE selects and the
			  -- next one LEASES before the final join runs, so a malformed row — completed,
			  -- obligations outstanding, but whose source reservation belongs to another
			  -- organizer — used to take a lease and a claim slot and then vanish at the
			  -- join: never returned, so never released and never abandoned, so nothing
			  -- charged an attempt, nothing parked it and nothing cleared the lease.
			  --
			  -- Worse than one slot: a leased-then-dropped row makes the store return FEWER
			  -- rows than it leased, and RunOnce reads a short batch as a drained queue
			  -- (len(claimed) < r.batch) and ends the pass.
			  --
			  -- FOR UPDATE OF rf, not a bare FOR UPDATE: the lock target is explicit and a
			  -- mistyped alias is a parse error rather than a silently wider lock.
			  --
			  -- TKT-268 replaced TKT-267's correlated EXISTS over orders/reservations with
			  -- this column comparison. The EXISTS was correct and unindexable: PostgreSQL
			  -- evaluated it per candidate row, so the LIMIT could not stop early on the rows
			  -- it rejected and the work was linear in the REJECTED PREFIX rather than bounded
			  -- by the batch. Measured at a 50,000-row malformed prefix, batch 16: 263ms over
			  -- 350,585 buffers before, 0.039ms over 2 buffers after, with zero rows removed by
			  -- filter. source_organizer_id is derived from the source reservation BY TRIGGER
			  -- (migration 0026), never from this row's own organizer_id and never from a
			  -- caller-supplied value — a copy of organizer_id would make this predicate true by
			  -- construction and unable to refuse anything.
			  --
			  -- The predicate is also in order_refunds_reversal_queue_idx. Both halves matter:
			  -- in the query alone the index still holds the malformed rows and they are read
			  -- and discarded; in the index alone the planner cannot use it for this query.
			  --
			  -- ADR-021: honest-writer consistency and bounded claim work, not tamper-evidence.
			  -- No code path writes a mismatched pair; a writer with commerce database access
			  -- can, and can equally drop the trigger or the index. ADR-070 states the scope.
			  AND rf.source_organizer_id = rf.organizer_id
			ORDER BY rf.reversal_next_attempt_at
			FOR UPDATE OF rf SKIP LOCKED
			LIMIT $1
		), claimed AS (
			-- The claim takes the lease and does NOT charge an attempt (ai-review F4).
			-- Charging here means a crash, an OOM or a SIGKILL after claiming spends budget
			-- on rows that were never driven — and since parking only happens on release,
			-- repeated crash-after-claim cycles could push a row most of the way to parked
			-- without a single real failure, then park it on its first transient one. The
			-- attempt is charged by ReleaseReversalClaim, which only runs when a drive
			-- actually happened and failed to complete. An expired lease is therefore free:
			-- the row simply becomes claimable again with its budget intact.
			UPDATE order_refunds r
			SET reversal_lease_until=now()+make_interval(secs => $2),
			    reversal_claim_id=$3
			WHERE (r.organizer_id, r.id) IN (SELECT organizer_id, id FROM claimable)
			RETURNING r.id, r.order_id, r.organizer_id, r.quantity, r.status,
			          r.tickets_voided_at, r.capacity_returned_at,
			          r.reversal_claim_id, r.reversal_attempts
		)
		SELECT c.id, c.order_id, c.organizer_id, c.quantity, c.status,
		       c.tickets_voided_at, c.capacity_returned_at,
		       res.hold_id, c.reversal_claim_id, c.reversal_attempts
		FROM claimed c
		JOIN orders o ON o.id = c.order_id
		-- The reservation is joined on organizer too: it is where hold_id and the tenant
		-- identity live, and an unscoped join is how a row acquires another tenant's hold.
		JOIN reservations res ON res.id = o.reservation_id AND res.organizer_id = c.organizer_id`

func ClaimOutstandingReversals(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]ClaimedReversal, error) {
	claim := uuid.New()
	// EVERY statement in this lifecycle keys on the FULL composite (organizer_id, id).
	// `order_refunds`' primary key is (organizer_id, id) — `id` alone is not unique by
	// schema. The obvious version of this query copies `ClaimStuckOrders`, which matches on
	// `id` alone because ITS table (`orders`) has `id` as a sole primary key; that shape does
	// not transfer, and matching on `id` here would let one eligible row hand its claim token
	// to every same-id row in another tenant — including pending, parked, discharged and
	// live-leased ones that satisfied none of the predicates below (ai-review F3). A refund's
	// id is derived by SHA-1 over its organizer, so a collision is not reachable today; the
	// scoping is still correct rather than lucky, and costs nothing.
	rows, err := db.QueryContext(ctx, claimOutstandingReversalsSQL,
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
// and parking it once attempts are exhausted. Conditional on the claim id **and the full
// composite key**, so a claimant whose lease lapsed mid-drive cannot disturb its successor
// and cannot reach another organizer's row.
//
// PROGRESS IS DECIDED BY THE DATABASE, NOT BY THE CALLER (ai-review F2). The obvious version
// takes a `progressed bool` computed from the claimant's own before/after values — and that
// is wrong, because **nothing outside this runner respects the lease**: the staff refund
// endpoint and the cancellation runner call the same `DriveReversal` whenever they like. So a
// concurrent replay can persist `tickets_voided_at` while this claimant's own access call
// fails, and a caller-computed verdict would report "no progress" about a row that just
// advanced — then park it at the budget boundary, stranding the remaining obligation with
// nothing driving it. The comparison is therefore made here, against the row as it stands
// now, versus the timestamps the claimant observed when it claimed.
//
// Parking is additionally guarded on the row still being outstanding: a row that became
// fully discharged between the drive and this write must not be parked at all.
//
// Progress resets the attempt budget. That is the whole reason a bound is safe: an access
// outage of any length costs at most one attempt per pass while nothing moves, and the first
// discharged obligation restores the budget in full. Parking is reserved for a row that
// makes no progress at all — which is what a permanently refused obligation looks like from
// commerce, since it cannot see WHY inventory refused.
//
// A progressed row is NOT made instantly claimable (own finding S1). `RunOnce` drains in a
// loop, so `next_attempt_at = now()` would let the same row be re-claimed within the same
// pass, re-driving a downstream that just half-failed with no backoff at all and resetting
// its budget each time. It gets the floor instead: prompt on the next pass, not inside this
// one.
//
// The backoff expression is `orders`' (recovery.go), deliberately: two runners in one service
// with different backoff curves is an operational surprise for no benefit.
func ReleaseReversalClaim(ctx context.Context, db OutboxDB, org, refundID, claimID uuid.UUID,
	voidedAtClaim, capacityAtClaim bool, cause string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds SET
		    reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    -- A row that is no longer outstanding gets FINISH semantics, not release
		    -- semantics (ai-review pass 2). Someone else — a staff replay, a cancellation
		    -- run — can complete both obligations while this claimant is mid-flight, and
		    -- writing this claimant's failure onto a finished row leaves a permanent error
		    -- on work that succeeded: the claim query never selects it again, so nothing
		    -- ever clears the field, and 0021's rollback guard then reads it as failed
		    -- reconciliation and refuses a legitimate rollback.
		    reversal_last_error=CASE WHEN observed.still_outstanding THEN $4 ELSE NULL END,
		    -- The attempt is charged HERE, on a drive that actually ran and did not
		    -- complete — not at claim time, where a crash would spend it on work that
		    -- never happened (ai-review F4). A row that is already complete is charged
		    -- nothing: there is no failure to record.
		    reversal_attempts=CASE
		        WHEN observed.progressed OR NOT observed.still_outstanding THEN 0
		        ELSE order_refunds.reversal_attempts+1
		    END,
		    reversal_next_attempt_at=now() + CASE
		        WHEN observed.progressed THEN make_interval(secs => $6)
		        ELSE least(make_interval(secs => power(2, least(order_refunds.reversal_attempts+1, 8))::double precision), interval '5 minutes')
		    END,
		    reversal_parked_at=CASE
		        WHEN NOT observed.progressed AND observed.still_outstanding
		             AND order_refunds.reversal_attempts+1>=$5 THEN now()
		        ELSE NULL
		    END
		FROM (
		    SELECT
		        -- Progress measured against the DATABASE, not the caller's in-memory view.
		        -- Each half pairs a COLUMN with the claim-time observation OF THAT COLUMN;
		        -- crossing them ($7 with capacity, $8 with voiding) type-checks, runs, and
		        -- reports progress exactly backwards, so the pairing is spelled out rather
		        -- than left to positional luck.
		        ((tickets_voided_at    IS NOT NULL) AND NOT $7::boolean)  -- $7 = voidedAtClaim
		         OR ((capacity_returned_at IS NOT NULL) AND NOT $8::boolean)  -- $8 = capacityAtClaim
		         AS progressed,
		        (tickets_voided_at IS NULL OR capacity_returned_at IS NULL) AS still_outstanding
		    FROM order_refunds WHERE organizer_id=$1 AND id=$2
		) AS observed
		WHERE order_refunds.organizer_id=$1 AND order_refunds.id=$2
		  AND order_refunds.reversal_claim_id=$3`,
		/* $1 */ org,
		/* $2 */ refundID,
		/* $3 */ claimID,
		/* $4 */ cause,
		/* $5 */ MaxReversalAttempts,
		/* $6 */ progressedFloorSeconds,
		/* $7 */ voidedAtClaim,
		/* $8 */ capacityAtClaim)
	return err
}

// progressedFloorSeconds is how long a refund that DID advance waits before its next attempt.
//
// Not zero: `RunOnce` drains, so zero means the same row is re-claimable on the very next
// loop iteration, hammering a downstream that just partially failed and resetting its budget
// each time. Short, because the row is genuinely making progress and its remaining obligation
// should not wait out a whole interval.
const progressedFloorSeconds = 5.0

// FinishReversalClaim releases the lease of a refund whose reversal is now COMPLETE. It
// clears the backoff and the error rather than leaving the last transient failure's text on
// a discharged row, where it would read as an unresolved problem.
//
// Conditional on the claim id for the same fencing reason as the release path.
func FinishReversalClaim(ctx context.Context, db OutboxDB, org, refundID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    reversal_attempts=0,
		    reversal_last_error=NULL,
		    reversal_next_attempt_at=now()
		WHERE organizer_id=$1 AND id=$2 AND reversal_claim_id=$3`, org, refundID, claimID)
	return err
}

// AbandonReversalClaim hands back a claim the pass never drove — a shutdown mid-batch — so
// the next pass or the next boot picks it up immediately rather than waiting out the lease.
//
// It does not touch `reversal_attempts`: since ai-review F4 nothing is charged at claim
// time, so there is nothing to refund. It does not touch `next_attempt_at` either — the row
// was never tried, so there is nothing to back off from.
//
// This makes an ORDERLY shutdown and a CRASH differ only in latency, which is the property
// worth having: an abandoned claim is reclaimable now, a crashed one when its lease lapses,
// and neither costs the row any budget.
//
// Conditional on the full composite key and the claim id, so a lease that lapsed and was
// re-claimed by a successor mid-shutdown is left alone.
func AbandonReversalClaim(ctx context.Context, db OutboxDB, org, refundID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL
		WHERE organizer_id=$1 AND id=$2 AND reversal_claim_id=$3`, org, refundID, claimID)
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
