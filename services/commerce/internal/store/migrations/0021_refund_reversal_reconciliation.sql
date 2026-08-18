-- Outstanding refund reversals get driven automatically (TKT-163, ADR-062).
--
-- ADR-038 §7 shipped the reversal as "visible and retryable" with nothing retrying it: a
-- refund whose money moved but whose tickets were not voided — access down, ACCESS_URL
-- unset, or issuance not caught up (503) — stayed outstanding until a human replayed the
-- idempotency key. §7 recorded a leased runner as designed and REJECTED, on the grounds
-- that nobody had stated the requirement and "adding one later is additive". This is that
-- later, and these columns are the additive part.
--
-- The lease lifecycle is `orders`' recovery lifecycle (0004/0005), deliberately copied
-- rather than shared: checkout recovery and reversal reconciliation have different
-- eligibility and different terminal states, and one state machine serving both would be
-- readable by nobody. Same reasoning ADR-040 gave for not merging the cancellation runner
-- into `internal/recovery`.
--
-- WHY PARKING AND NOT A PREDICATE. Some obligations can never be discharged: inventory
-- refuses a PARTIAL return of a SEATED claim (ErrPartialSeatedReturn, 409) because nothing
-- associates an issued ticket with a seat identity (TKT-164). Commerce cannot predict that
-- refusal — seatedness is `claim_seats` rows in INVENTORY's database, and "partial" depends
-- on `claims.returned_quantity`, which moves when any other refund of the same order
-- returns capacity. A predicate written here would be wrong in both directions and would
-- drift from inventory's rule. So the refusal is OBSERVED, not predicted: attempts are
-- charged, backoff grows, and the row parks itself. That also parks permanently-refused
-- shapes nobody has thought of, which a hand-written predicate cannot.
--
-- `reversal_attempts` RESETS on progress (the runner, not the schema, does this): a pass
-- that discharges an obligation it had not discharged before restores the full budget, so a
-- long outage followed by recovery does not arrive with the budget already spent. Without
-- that, bounding attempts would reintroduce the very failure this ticket exists to close.
--
-- The CHECK closes the refund side's asymmetry with `order_exchanges`, which has carried
-- this guard in both its WHERE clause and a constraint since 0011. 0009 deliberately left
-- it to application code, and said so: "Commerce enforces that ordering — it will not
-- attempt the return until tickets_voided_at is set." That was sufficient while there was
-- ONE caller. This migration adds a second (the runner), and one-caller-enforces-it stops
-- being a guarantee once callers multiply. Voiding-before-capacity is the one ordering that
-- can OVERSELL (ADR-038 §1), so it becomes the database's rule here too.
--
-- Safe to add without validation risk: `capacity_returned_at` has exactly one writer in
-- commerce (MarkRefundCapacityReturned), and it fires only downstream of DriveReversal's
-- ordering guard, so no existing row can violate the constraint.
--
-- Scope of the guarantees: honest-writer concurrency and crash/retry. These rows are NOT
-- tamper-evident — anyone with commerce database write access can clear a lease, unpark a
-- row or forge a discharge timestamp. The signed, append-only payments journal remains the
-- evidence that money moved; ADR-021's limits on it are unchanged (ADR-021: name the
-- adversary before writing "tamper-evident").
-- +goose Up
ALTER TABLE order_refunds
    ADD COLUMN reversal_claim_id uuid,
    ADD COLUMN reversal_lease_until timestamptz,
    ADD COLUMN reversal_attempts integer NOT NULL DEFAULT 0 CHECK (reversal_attempts >= 0),
    ADD COLUMN reversal_next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN reversal_parked_at timestamptz,
    ADD COLUMN reversal_last_error text;

-- A lease and its token are one fact: a claim id with no expiry is a row leased forever,
-- and an expiry with no token is a lease no successor can fence against.
ALTER TABLE order_refunds
    ADD CONSTRAINT order_refunds_reversal_claim_shape CHECK (
        (reversal_claim_id IS NULL) = (reversal_lease_until IS NULL)
    );

ALTER TABLE order_refunds
    ADD CONSTRAINT order_refunds_capacity_after_void CHECK (
        capacity_returned_at IS NULL OR tickets_voided_at IS NOT NULL
    );

-- The claim scan: completed refunds still owing an obligation, unparked, due, oldest
-- first. Partial on the outstanding predicate so the index holds only the rows the runner
-- can act on — a refund whose reversal is complete is the overwhelming majority and never
-- needs to be scanned again. A scoped read is only scoped if an index backs the filter
-- (ADR-019's rule, applied outside catalog).
CREATE INDEX order_refunds_reversal_queue_idx
    ON order_refunds (reversal_next_attempt_at, id)
    WHERE status = 'completed'
      AND reversal_parked_at IS NULL
      AND (tickets_voided_at IS NULL OR capacity_returned_at IS NULL);

-- +goose Down
DROP INDEX order_refunds_reversal_queue_idx;
ALTER TABLE order_refunds DROP CONSTRAINT order_refunds_capacity_after_void;
ALTER TABLE order_refunds DROP CONSTRAINT order_refunds_reversal_claim_shape;
ALTER TABLE order_refunds
    DROP COLUMN reversal_claim_id,
    DROP COLUMN reversal_lease_until,
    DROP COLUMN reversal_attempts,
    DROP COLUMN reversal_next_attempt_at,
    DROP COLUMN reversal_parked_at,
    DROP COLUMN reversal_last_error;
