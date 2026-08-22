package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Exchange reversal reconciliation (TKT-259, ADR-063): the durable side of driving an
// outstanding exchange obligation to completion with no human replaying anything.
//
// The lifecycle is refund_reversal.go's, copied rather than shared. Same reasoning ADR-062
// §1 gave for making that a third runner instead of extending `internal/recovery` or
// `internal/bulkrefund`: different eligibility, different terminal states, and one state
// machine serving several lifecycles would be readable by nobody.
//
// WHAT THIS SWEEP DOES NOT DO. It never writes `tickets_exchanged_at`. Only access can
// establish that the old tickets stopped admitting, and migration 0011's CHECK gates the
// capacity return on that marker. A sweep that set the marker itself would assert a fact
// about another service's state in order to unlock the one write that can OVERSELL
// (ADR-038 §1). So it drives the CAPACITY half of a switched exchange; an exchange still
// awaiting its switch is claimed, observed, counted and released — visible, never completed.

// MaxExchangeReversalAttempts bounds attempts before an exchange obligation is parked.
//
// The bound exists for ADR-062 §2's GENERAL reason — a permanently refused shape nobody has
// enumerated is recognised by making no progress, never by a predicate that would drift from
// inventory's rule — and deliberately NOT for its specific one. ADR-062's parking rationale
// is inventory's refusal of a PARTIAL return of a SEATED claim; an exchange is whole-line
// only and TKT-158 refuses a seated source outright, so that case is essentially unreachable
// here (ADR-063 says which of ADR-062's reasoning transfers).
//
// The bound is safe only because attempts RESET on progress, exactly as on the refund side:
// without that, a long inventory outage would retire a row that was about to recover.
const MaxExchangeReversalAttempts = 10

// ClaimedExchangeReversal is one leased exchange whose obligation is still owed. It carries
// what the discharge unit needs: the source hold that gives capacity back, and how much.
type ClaimedExchangeReversal struct {
	Exchange ExchangeSwitch
	ClaimID  uuid.UUID
	Attempts int
}

// Outstanding reports whether either obligation was still owed when the row was claimed.
func (c ClaimedExchangeReversal) Outstanding() bool {
	return !c.Exchange.TicketsExchanged || !c.Exchange.CapacityReturned
}

// ClaimOutstandingExchangeReversals leases settled exchanges still owing an obligation,
// oldest due first.
//
// SIX independent conjuncts guard this claim, and an earlier refusal short-circuits the
// rest, so each needs its own test with the earlier ones satisfied (AGENTS.md; the store
// smoke file has one case per conjunct).
func ClaimOutstandingExchangeReversals(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]ClaimedExchangeReversal, error) {
	claim := uuid.New()
	// EVERY statement in this lifecycle keys on the FULL composite (organizer_id, id).
	// `order_exchanges`' primary key is (organizer_id, id) — `id` alone is not unique by
	// schema, and matching on it would let one eligible row hand its claim token to every
	// same-id row in another tenant, including settled, parked and live-leased ones that
	// satisfied none of the predicates below. This is ADR-062's ai-review F3, applied here
	// at write time rather than re-learned.
	rows, err := db.QueryContext(ctx, `
		WITH claimable AS (
			SELECT oe.organizer_id, oe.id FROM order_exchanges AS oe
			WHERE oe.settled_at IS NOT NULL
			  -- ACTIONABLE only: switched, capacity outstanding. A row awaiting its switch
			  -- is deliberately NOT claimed (ai-review pass 2, F4).
			  --
			  -- The first fix made such rows harmless to claim — no attempt charged, no
			  -- parking. It did not make them free: the claim is ORDER BY next_attempt_at
			  -- with a LIMIT, and a pass is bounded at MaxBatchesPerPass batches, so a large
			  -- awaiting-switch backlog after an access outage fills every batch with rows
			  -- on which commerce performs no work and pushes genuinely actionable capacity
			  -- returns past the pass bound — head-of-line blocking that delays exactly what
			  -- the sweep exists to do, while the capacity under-sells.
			  --
			  -- Excluding them costs nothing, because there was never anything to do: the
			  -- switch is access's fact and DriveExchange refuses an unswitched row anyway.
			  -- They stay visible through ReadExchangeReversalBacklog's awaiting_switch
			  -- count, which is what "monitored rather than driven" means — the sweep is not
			  -- how they are observed, the gauge is. When access confirms the switch the row
			  -- becomes claimable immediately, with its next-attempt time still at its
			  -- default of the row's creation time and its budget untouched.
			  AND oe.tickets_exchanged_at IS NOT NULL
			  AND oe.capacity_returned_at IS NULL
			  AND oe.reversal_parked_at IS NULL
			  AND oe.reversal_next_attempt_at<=now()
			  AND (oe.reversal_lease_until IS NULL OR oe.reversal_lease_until<=now())
			  -- CONJUNCT 6 (TKT-267): the source reservation is this organizer's. Scoped
			  -- HERE and not only at the final join, because this CTE selects and the next
			  -- one LEASES before that join runs. A malformed row — settled, switched,
			  -- capacity outstanding, but whose source reservation belongs to another
			  -- organizer — used to take a lease and a claim slot and then vanish at the
			  -- join: never returned, so never released and never abandoned, so nothing
			  -- charged an attempt, nothing parked it and nothing cleared the lease. It
			  -- re-took a slot on every lease expiry while the function reported no error.
			  --
			  -- The cost was worse than one slot. A leased-then-dropped row makes the
			  -- store return FEWER rows than it leased, and RunOnce reads a short batch as
			  -- a drained queue (len(claimed) < r.batch) and ends the pass — so a single
			  -- such row cut a drain from its per-pass bound to one batch per tick, and a
			  -- batch of only such rows ended the pass having driven nothing. Same
			  -- head-of-line blocking the awaiting-switch conjunct above exists to prevent,
			  -- reached by a different route.
			  --
			  -- FOR UPDATE OF oe, not a bare FOR UPDATE: the lock target is then explicit
			  -- in the SQL, and a mistyped alias is a parse error rather than a silently
			  -- wider lock. Measured, not assumed — this EXISTS leaves orders and
			  -- reservations at AccessShareLock, so sweep replicas do not contend on them.
			  --
			  -- ADR-021, name the adversary: honest-writer consistency, not
			  -- tamper-evidence. No code path writes a mismatched pair; a writer with
			  -- commerce database access still can, and this constrains that writer not at
			  -- all. What it removes is the sweep's inability to progress once one exists.
			  AND EXISTS (
			      SELECT 1 FROM orders o
			      JOIN reservations r ON r.id = o.reservation_id
			                         AND r.organizer_id = oe.organizer_id
			      WHERE o.id = oe.source_order_id)
			ORDER BY oe.reversal_next_attempt_at
			FOR UPDATE OF oe SKIP LOCKED
			LIMIT $1
		), claimed AS (
			-- The claim takes the lease and does NOT charge an attempt. Charging here means
			-- a crash or a SIGKILL after claiming spends budget on rows that were never
			-- driven, and since parking only happens on release, repeated crash-after-claim
			-- cycles could push a row most of the way to parked without a single real
			-- failure. The attempt is charged by ReleaseExchangeReversalClaim, which runs
			-- only when a drive actually happened and did not complete. An expired lease is
			-- therefore free (ADR-062 ai-review F4).
			UPDATE order_exchanges x
			SET reversal_lease_until=now()+make_interval(secs => $2),
			    reversal_claim_id=$3
			WHERE (x.organizer_id, x.id) IN (SELECT organizer_id, id FROM claimable)
			RETURNING x.organizer_id, x.id, x.source_order_id, x.quantity,
			          x.tickets_exchanged_at, x.capacity_returned_at,
			          x.reversal_claim_id, x.reversal_attempts
		)
		SELECT c.organizer_id, c.id, c.quantity,
		       c.tickets_exchanged_at, c.capacity_returned_at,
		       res.hold_id, c.reversal_claim_id, c.reversal_attempts
		FROM claimed c
		JOIN orders o ON o.id = c.source_order_id
		-- The reservation is joined on ORGANIZER too: it is where hold_id and the tenant
		-- identity live, and an unscoped join is how a row acquires another tenant's hold.
		-- LoadExchangeSwitch carries the same predicate since TKT-260 — the read path this
		-- comment used to record as an outstanding gap.
		--
		-- This join is now defence in depth rather than the only scoping: TKT-267 moved the
		-- check into claimable, so a malformed row is filtered BEFORE the limit and the
		-- lease and can no longer reach this join at all. It is kept because a claim that
		-- silently returned another tenant's hold would be the worse failure, and the
		-- predicate costs nothing.
		JOIN reservations res ON res.id = o.reservation_id AND res.organizer_id = c.organizer_id`,
		limit, lease.Seconds(), claim)
	if err != nil {
		return nil, fmt.Errorf("claim outstanding exchange reversals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ClaimedExchangeReversal
	for rows.Next() {
		var c ClaimedExchangeReversal
		var switchedAt, returnedAt sql.NullTime
		if err := rows.Scan(&c.Exchange.OrganizerID, &c.Exchange.ID, &c.Exchange.Quantity,
			&switchedAt, &returnedAt, &c.Exchange.SourceHoldID, &c.ClaimID, &c.Attempts); err != nil {
			return nil, err
		}
		c.Exchange.TicketsExchanged = switchedAt.Valid
		c.Exchange.CapacityReturned = returnedAt.Valid
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReleaseExchangeReversalClaim hands back a claim that was driven and did not complete,
// charging the attempt, backing off, and parking at the budget.
//
// It takes the obligations as OBSERVED AT CLAIM TIME and decides progress in SQL against the
// row as it stands. It does not take a `progressed bool`: this runner holds no monopoly on
// discharging an exchange — the tickets-switched callback drives the same obligation
// whenever access redelivers — so a verdict computed from the claimant's own before/after
// would be wrong exactly when a concurrent callback helped (ADR-062 ai-review F2).
//
// AN EXCHANGE AWAITING ITS SWITCH IS NOT A FAILED ATTEMPT, and the awaiting_switch arms
// below encode that (ai-review F1, then F3/F4).
//
// The claim query no longer offers such a row, and these arms are therefore NOT reachable
// through any application path — stated plainly, because the previous version of this
// comment justified them with a concurrent writer clearing the marker, and that writer does
// not exist: MarkExchangeTicketsSwitched is the only writer of tickets_exchanged_at and it
// only ever goes NULL -> now() (ai-review pass 3, F7). Claiming a race that cannot happen is
// how a false invariant gets written down and then believed.
//
// They are kept as ADMIN-WRITE / CORRUPTION handling, which is a real if narrow class: a
// human or a repair script clearing the marker on a claimed row, or a restore that rolls one
// back. ADR-021's rule applies — this is honest-writer consistency, and a writer with
// database access is outside what any of this constrains. The arms make the failure mode
// benign rather than silently destructive: without them such a row would be charged, have an
// error written, and eventually PARK, and since the claim excludes parked rows a later switch
// confirmation whose capacity return failed could never be swept. The recorded error would
// also block 0022's rollback for work never attempted.
//
// The runner's TestAnExchangeAwaitingItsSwitchIsNeverCompleted covers the same state at the
// decision tier.
//
// The rule in one sentence: a budget and an error describe what this row ASKED a downstream
// and how that went. A row that asked nobody has neither.
func ReleaseExchangeReversalClaim(ctx context.Context, db OutboxDB, org, exchangeID, claimID uuid.UUID,
	switchedAtClaim, capacityAtClaim bool, cause string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET
		    reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    -- A row no longer outstanding gets FINISH semantics, not release semantics:
		    -- the callback can complete both obligations while this claimant is mid-flight,
		    -- and writing this claimant's failure onto a finished row leaves a permanent
		    -- error on work that succeeded — the claim query never selects it again, so
		    -- nothing ever clears the field, and 0022's rollback guard then reads it as
		    -- failed reconciliation and refuses a legitimate rollback.
		    -- An awaiting-switch row records NO error, and CLEARS one it recorded before
		    -- (ai-review pass 2, F3). The first fix stopped charging such a row an attempt
		    -- but still wrote a cause into the error column — and 0022's rollback guard
		    -- refuses on any non-NULL error, so the routine, transient state of waiting for
		    -- access would have made the migration unrollbackable, for work where nothing
		    -- failed and nothing was even attempted. The column means "the last thing this
		    -- row tried, and how it failed"; a row that tried nothing has no answer to it.
		    reversal_last_error=CASE
		        WHEN observed.still_outstanding AND NOT observed.awaiting_switch THEN $4
		        ELSE NULL
		    END,
		    -- An awaiting_switch row never charges: see the note above. The budget exists to
		    -- bound retries against a downstream this service is ASKING, and it asks none here.
		    reversal_attempts=CASE
		        WHEN observed.progressed OR NOT observed.still_outstanding THEN 0
		        WHEN observed.awaiting_switch THEN order_exchanges.reversal_attempts
		        ELSE order_exchanges.reversal_attempts+1
		    END,
		    reversal_next_attempt_at=now() + CASE
		        WHEN observed.progressed THEN make_interval(secs => $6)
		        -- A fixed interval rather than a backoff: there is no failing downstream to
		        -- spare, and the row must be picked up promptly once access confirms.
		        WHEN observed.awaiting_switch THEN make_interval(secs => $9)
		        ELSE least(make_interval(secs => power(2, least(order_exchanges.reversal_attempts+1, 8))::double precision), interval '5 minutes')
		    END,
		    reversal_parked_at=CASE
		        WHEN NOT observed.progressed AND observed.still_outstanding
		             AND NOT observed.awaiting_switch
		             AND order_exchanges.reversal_attempts+1>=$5 THEN now()
		        ELSE NULL
		    END
		FROM (
		    SELECT
		        -- Progress measured against the DATABASE, not the caller's in-memory view.
		        -- Each half pairs a COLUMN with the claim-time observation OF THAT COLUMN;
		        -- crossing them ($7 with capacity, $8 with the switch) type-checks, runs, and
		        -- reports progress exactly BACKWARDS, so the pairing is spelled out rather
		        -- than left to positional luck. A fixture that moves both obligations
		        -- together cannot tell the two apart, which is why the smoke tests move
		        -- exactly one at a time, in each direction.
		        ((tickets_exchanged_at IS NOT NULL) AND NOT $7::boolean)  -- $7 = switchedAtClaim
		         OR ((capacity_returned_at IS NOT NULL) AND NOT $8::boolean)  -- $8 = capacityAtClaim
		         AS progressed,
		        (tickets_exchanged_at IS NULL OR capacity_returned_at IS NULL) AS still_outstanding,
		        -- Read from the ROW, not from the claim-time observation: access may have
		        -- confirmed the switch mid-flight, and this claimant's failed capacity call
		        -- IS then a real attempt against inventory.
		        (tickets_exchanged_at IS NULL) AS awaiting_switch
		    FROM order_exchanges WHERE organizer_id=$1 AND id=$2
		) AS observed
		WHERE order_exchanges.organizer_id=$1 AND order_exchanges.id=$2
		  AND order_exchanges.reversal_claim_id=$3`,
		/* $1 */ org,
		/* $2 */ exchangeID,
		/* $3 */ claimID,
		/* $4 */ cause,
		/* $5 */ MaxExchangeReversalAttempts,
		/* $6 */ progressedFloorSeconds,
		/* $7 */ switchedAtClaim,
		/* $8 */ capacityAtClaim,
		/* $9 */ awaitingSwitchFloorSeconds)
	return err
}

// awaitingSwitchFloorSeconds is how long a settled exchange whose switch access has not
// confirmed waits before the sweep looks at it again.
//
// It is a flat interval, not a backoff, and it charges nothing. Such a row is not failing —
// it is waiting on another service's event, and the sweep re-reads it only to keep the
// `awaiting_switch` gauge honest and to pick the capacity work up promptly once the marker
// lands. Long enough not to spin on a stuck consumer; short enough that a recovered exchange
// is discharged in about a minute rather than after a backoff that grew while nothing was
// wrong.
const awaitingSwitchFloorSeconds = 60.0

// FinishExchangeReversalClaim closes a fully discharged exchange: lease and token released,
// budget restored, error cleared.
func FinishExchangeReversalClaim(ctx context.Context, db OutboxDB, org, exchangeID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL,
		    reversal_attempts=0,
		    reversal_last_error=NULL,
		    reversal_next_attempt_at=now()
		WHERE organizer_id=$1 AND id=$2 AND reversal_claim_id=$3`, org, exchangeID, claimID)
	return err
}

// AbandonExchangeReversalClaim hands back a claim the pass never drove — a shutdown
// mid-batch — so the next pass or the next boot picks it up immediately rather than waiting
// out the lease.
//
// It touches neither `reversal_attempts` (nothing is charged at claim time, so there is
// nothing to refund) nor `reversal_next_attempt_at` (the row was never tried, so there is
// nothing to back off from). That makes an orderly shutdown and a crash differ only in
// latency, and leaves the row DUE — which is what lets the runner hand back a duplicate
// within a pass and still have it offered again on the next one.
//
// Conditional on the full composite key and the claim id, so a lease that lapsed and was
// re-claimed by a successor mid-shutdown is left alone.
func AbandonExchangeReversalClaim(ctx context.Context, db OutboxDB, org, exchangeID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges
		SET reversal_lease_until=NULL,
		    reversal_claim_id=NULL
		WHERE organizer_id=$1 AND id=$2 AND reversal_claim_id=$3`, org, exchangeID, claimID)
	return err
}

// ExchangeReversalBacklog is what an operator (and the gauges) read to know whether the
// sweep is keeping up, and whether anything has given up.
type ExchangeReversalBacklog struct {
	// Outstanding counts settled exchanges still owing an obligation, parked or not.
	Outstanding int64
	// Parked counts those that spent their attempt budget and now await a human.
	Parked int64
	// AwaitingSwitch counts the subset the sweep can never complete on its own: settled,
	// but access has not confirmed the switch. Separated from Outstanding because it is a
	// different incident with a different owner — those rows are COS 1's second branch
	// (explicitly monitored), not its first (driven to completion), and an operator seeing
	// a stuck backlog needs to know which kind it is.
	AwaitingSwitch int64
	// OldestAgeSeconds is the age of the oldest outstanding obligation, parked included.
	OldestAgeSeconds int64
}

// ReadExchangeReversalBacklog reports the sweep's queue depth for observability. It is not a
// gate: nothing here can make commerce unready.
//
// The age is measured from `settled_at`, NOT `created_at`. An exchange row is created at
// bind time, before settlement, so a long-lived unsettled bind would inflate the gauge while
// owing nothing — and the gauge's whole justification (ADR-062 §4: a small old backlog and a
// large fresh one are different incidents) only holds if the age means what the operator
// thinks it means. `settled_at` is the earliest moment an obligation can exist.
func ReadExchangeReversalBacklog(ctx context.Context, db *sql.DB) (ExchangeReversalBacklog, error) {
	var b ExchangeReversalBacklog
	err := db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE reversal_parked_at IS NOT NULL),
		       count(*) FILTER (WHERE tickets_exchanged_at IS NULL),
		       coalesce(max(extract(epoch FROM now()-settled_at))::bigint, 0)
		FROM order_exchanges
		WHERE settled_at IS NOT NULL
		  AND (tickets_exchanged_at IS NULL OR capacity_returned_at IS NULL)`).
		Scan(&b.Outstanding, &b.Parked, &b.AwaitingSwitch, &b.OldestAgeSeconds)
	return b, err
}
