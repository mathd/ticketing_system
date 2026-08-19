-- Outstanding exchange obligations get driven automatically (TKT-259, ADR-063).
--
-- ADR-062 shipped this mechanism for REFUNDS and named exchanges as its deliberate gap:
-- `order_exchanges` carries two obligations of identical shape (tickets_exchanged_at,
-- capacity_returned_at) and nothing in commerce ever swept them. Their only retry was
-- access's JetStream redelivery, driven by commerce answering 502 from the tickets-switched
-- callback. That works while the consumer is healthy; if it dead-letters, nothing revisits
-- the row. These columns are what lets commerce revisit it.
--
-- The lease lifecycle is 0021's, deliberately copied rather than shared — same reasoning
-- 0021 gave for copying `orders`' recovery lifecycle instead of merging into it: different
-- eligibility, different terminal states, and one state machine serving both would be
-- readable by nobody.
--
-- WHAT DOES NOT TRANSFER FROM 0021. That migration's parking rationale is inventory's
-- refusal of a PARTIAL return of a SEATED claim. For exchanges that case is essentially
-- unreachable: an exchange is whole-line only and TKT-158 refuses a seated source outright,
-- so the source claim is GA by construction and the return is FULL. Parking is still copied,
-- because ADR-062 §2's GENERAL argument stands on its own — a permanently refused shape
-- nobody has enumerated is recognised by making no progress, never by a predicate that
-- would drift from inventory's rule with every change to it. The seated rationale is
-- deliberately NOT restated here; ADR-063 says which of ADR-062's reasoning transfers.
--
-- NO SECOND ORDERING CONSTRAINT. 0011 already carries
-- `capacity_returned_at IS NULL OR tickets_exchanged_at IS NOT NULL`, which is the
-- capacity-after-switch guard 0021 had to ADD for refunds. The exchange side has had it
-- since the column existed; this migration must not duplicate it.
--
-- THE SWEEP NEVER WRITES `tickets_exchanged_at`. Only access can establish that the old
-- tickets stopped admitting, and the CHECK above gates the capacity return on that marker.
-- A sweep that set the marker itself would be asserting a fact about another service's
-- state in order to unlock the one write that can OVERSELL (ADR-038 §1). So these columns
-- drive the CAPACITY half; an exchange still awaiting its switch is visible and counted,
-- and is never completed here. That is a schema-level statement of scope, enforced in the
-- runner and pinned by a test.
--
-- Scope of the guarantees: honest-writer concurrency and crash/retry. These rows are NOT
-- tamper-evident — anyone with commerce database write access can clear a lease, unpark a
-- row or forge a discharge timestamp (ADR-021: name the adversary).
-- +goose Up
ALTER TABLE order_exchanges
    ADD COLUMN reversal_claim_id uuid,
    ADD COLUMN reversal_lease_until timestamptz,
    ADD COLUMN reversal_attempts integer NOT NULL DEFAULT 0 CHECK (reversal_attempts >= 0),
    ADD COLUMN reversal_next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN reversal_parked_at timestamptz,
    ADD COLUMN reversal_last_error text;

-- A lease and its token are one fact: a claim id with no expiry is a row leased forever,
-- and an expiry with no token is a lease no successor can fence against.
ALTER TABLE order_exchanges
    ADD CONSTRAINT order_exchanges_reversal_claim_shape CHECK (
        (reversal_claim_id IS NULL) = (reversal_lease_until IS NULL)
    );

-- The claim scan: SETTLED exchanges still owing an obligation, unparked, due, oldest first.
-- Partial on the outstanding predicate so the index holds only rows the runner can act on —
-- a discharged exchange is the overwhelming majority and never needs scanning again. A
-- scoped read is only scoped if an index backs the filter (ADR-019's rule, applied outside
-- catalog).
--
-- `settled_at IS NOT NULL` is in the predicate rather than the key: an unsettled exchange
-- owes nothing yet, and including it would make the index carry every in-flight bind.
-- The predicate matches the claim's ACTIONABLE set exactly: settled, switched, capacity
-- outstanding, unparked. A settled exchange still awaiting its switch is deliberately absent
-- from both — commerce can do nothing with it, and letting a large awaiting-switch backlog
-- into a LIMIT-ed, next-attempt-ordered queue would push genuinely actionable capacity
-- returns past the runner's per-pass bound. Those rows are monitored by the awaiting_switch
-- gauge instead, which reads them directly.
CREATE INDEX order_exchanges_reversal_queue_idx
    ON order_exchanges (reversal_next_attempt_at, organizer_id, id)
    WHERE settled_at IS NOT NULL
      AND tickets_exchanged_at IS NOT NULL
      AND capacity_returned_at IS NULL
      AND reversal_parked_at IS NULL;

-- +goose Down
-- Fails closed once the reconciler has recorded anything, exactly as 0021 does. A silent
-- rollback would drop parking decisions, attempt counts and the last recorded error — so
-- re-applying 0022 would unpark every permanently refused obligation with a fresh budget,
-- hand them back to the runner to hammer through another full budget, and erase the
-- diagnostics an operator needs to tell why they parked. Dropping a lease is harmless;
-- dropping the memory of a decision is not.
--
-- A row only ever claimed and released cleanly carries no state worth keeping: attempts back
-- at 0, never parked, no error. Those roll back freely, which keeps the escape hatch open
-- for a deploy undone before any real reconciliation happened.
LOCK TABLE order_exchanges IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM order_exchanges
        WHERE reversal_parked_at IS NOT NULL
           OR reversal_attempts > 0
           OR reversal_last_error IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'cannot roll back 0022: exchange reversals have parked, failed or been retried — rolling back would unpark them with a fresh retry budget and erase why they parked';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX order_exchanges_reversal_queue_idx;
ALTER TABLE order_exchanges DROP CONSTRAINT order_exchanges_reversal_claim_shape;
ALTER TABLE order_exchanges
    DROP COLUMN reversal_claim_id,
    DROP COLUMN reversal_lease_until,
    DROP COLUMN reversal_attempts,
    DROP COLUMN reversal_next_attempt_at,
    DROP COLUMN reversal_parked_at,
    DROP COLUMN reversal_last_error;
