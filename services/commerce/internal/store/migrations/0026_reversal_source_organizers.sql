-- The reversal claims' organizer check becomes INDEXABLE (TKT-268, ADR-070).
--
-- TKT-267 put a correlated EXISTS over orders/reservations inside the `claimable` CTE of
-- both reversal claim queries, so a row whose source reservation belongs to another
-- organizer is filtered BEFORE it can be leased. That closed a real liveness defect and it
-- is not in question here. What it cost is the subject: an EXISTS cannot be part of the
-- partial queue index, so PostgreSQL evaluates it per candidate row and the LIMIT cannot
-- stop early on the rows it rejects. The work is linear in the REJECTED PREFIX rather than
-- bounded by the batch. Measured during TKT-267's review on PostgreSQL 18.4, batch 16:
-- 50,000 malformed rows ahead of the queue cost 263ms over 350,585 buffers, against ~0.34ms
-- for the unfiltered query.
--
-- This migration puts the relationship the claim needs ON the queue row, so the partial
-- index can carry it and malformed rows never enter the index at all. Re-measured with this
-- shape at the same 50,000-row prefix: 2 buffers, 0.039ms, zero rows removed by filter.
--
-- WHY NOT A CHECK CONSTRAINT, which is the first instinct. A
-- `CHECK (source_organizer_id = organizer_id)` would make the malformed state
-- UNREPRESENTABLE, which sounds strictly stronger and would destroy the only tests that
-- prove any of this works: the acceptance fixture seeds 50,000 malformed rows, and both
-- re-pointed TKT-267 tests construct a mismatched pair directly. A guard whose test cannot
-- reach the failing state is the defect class this repository has been bitten by most
-- (docs/learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md). The
-- malformed row must stay writable so the queue can be shown to exclude it.
--
-- WHERE THE VALUE COMES FROM, which is the whole correctness argument. It is derived by
-- trigger from the SOURCE RESERVATION, reached through the queue row's order:
--
--     queue row's order id -> orders.reservation_id -> reservations.organizer_id
--
-- NOT from the queue row's own organizer_id, and NOT from a caller-supplied value. Copying
-- the queue row's organizer would make `source_organizer_id = organizer_id` true by
-- construction: a predicate that can never refuse, guarding nothing, with a green test
-- beside it (docs/learnings/2026-08-15-a-precondition-that-cannot-fail.md). The trigger
-- OVERWRITES whatever a writer submits, so the column is authoritative for every writer that
-- goes through it rather than only for well-behaved ones.
--
-- The parent side is maintained too. Changing orders.reservation_id or
-- reservations.organizer_id moves the derived value on every affected queue row. Production
-- does not rewrite those identity links today, but without this half the guarantee would
-- rest on an unenforced convention and the mismatch would be reachable from the parent side.
--
-- ADR-021, name the adversary: honest-writer consistency and bounded claim work, NOT
-- tamper-evidence. A writer with commerce database access can disable or replace the
-- triggers, forge both organizer columns, drop the index, or edit rows afterwards, and this
-- constrains that writer not at all. What it removes is the sweep's unbounded scan and the
-- head-of-line blocking a malformed backlog causes. Same scope as the ADR-021 paragraphs in
-- ADR-062 and ADR-063.
--
-- Plain CREATE INDEX: ADR-020 still rejects CONCURRENTLY, its preconditions are conjunctive
-- and (2) and (3) remain false.

-- +goose Up
ALTER TABLE order_refunds ADD COLUMN source_organizer_id uuid;
ALTER TABLE order_exchanges ADD COLUMN source_organizer_id uuid;

-- Backfill from the authoritative source, never from the queue row's own organizer.
UPDATE order_refunds rf
SET source_organizer_id = r.organizer_id
FROM orders o JOIN reservations r ON r.id = o.reservation_id
WHERE o.id = rf.order_id;

UPDATE order_exchanges oe
SET source_organizer_id = r.organizer_id
FROM orders o JOIN reservations r ON r.id = o.reservation_id
WHERE o.id = oe.source_order_id;

-- A surviving NULL means a queue row whose order or reservation is missing. Both tables
-- carry an FK to orders and orders carries one to reservations, so this is unreachable
-- through the schema; if it happens the data is corrupt and a silent NOT NULL failure two
-- statements later would be a worse way to learn it.
-- +goose StatementBegin
DO $$
DECLARE orphans bigint;
BEGIN
    SELECT (SELECT count(*) FROM order_refunds WHERE source_organizer_id IS NULL)
         + (SELECT count(*) FROM order_exchanges WHERE source_organizer_id IS NULL)
      INTO orphans;
    IF orphans > 0 THEN
        RAISE EXCEPTION 'cannot apply 0026: % reversal queue rows have no reachable source reservation, so their source organizer cannot be derived', orphans;
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE order_refunds ALTER COLUMN source_organizer_id SET NOT NULL;
ALTER TABLE order_exchanges ALTER COLUMN source_organizer_id SET NOT NULL;

-- The derivation, owned by the database rather than by each caller. BEFORE INSERT OR UPDATE
-- so a submitted value is overwritten rather than trusted.
--
-- One function per table, not one shared function branching on TG_TABLE_NAME. plpgsql
-- resolves NEW's fields when the statement is COMPILED, not when the branch is taken, so a
-- single body naming both `NEW.order_id` and `NEW.source_order_id` fails at runtime on
-- whichever table lacks the other's column: `record "new" has no field "source_order_id"`.
-- The duplication is two lines and the alternative does not work.
-- +goose StatementBegin
CREATE FUNCTION order_refunds_source_organizer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE src uuid;
BEGIN
    SELECT r.organizer_id INTO src
    FROM orders o JOIN reservations r ON r.id = o.reservation_id
    WHERE o.id = NEW.order_id;
    IF src IS NULL THEN
        RAISE EXCEPTION 'order_refunds row for order % has no reachable source reservation', NEW.order_id;
    END IF;
    NEW.source_organizer_id := src;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION order_exchanges_source_organizer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE src uuid;
BEGIN
    SELECT r.organizer_id INTO src
    FROM orders o JOIN reservations r ON r.id = o.reservation_id
    WHERE o.id = NEW.source_order_id;
    IF src IS NULL THEN
        RAISE EXCEPTION 'order_exchanges row for source order % has no reachable source reservation', NEW.source_order_id;
    END IF;
    NEW.source_organizer_id := src;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER order_refunds_source_organizer
    BEFORE INSERT OR UPDATE OF order_id, source_organizer_id ON order_refunds
    FOR EACH ROW EXECUTE FUNCTION order_refunds_source_organizer();

CREATE TRIGGER order_exchanges_source_organizer
    BEFORE INSERT OR UPDATE OF source_order_id, source_organizer_id ON order_exchanges
    FOR EACH ROW EXECUTE FUNCTION order_exchanges_source_organizer();

-- The parent half: an identity link that moves must move the derived value with it.
-- +goose StatementBegin
CREATE FUNCTION reversal_source_organizer_reseat() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE order_refunds rf
    SET source_organizer_id = r.organizer_id
    FROM orders o JOIN reservations r ON r.id = o.reservation_id
    WHERE o.id = rf.order_id
      AND rf.source_organizer_id IS DISTINCT FROM r.organizer_id;

    UPDATE order_exchanges oe
    SET source_organizer_id = r.organizer_id
    FROM orders o JOIN reservations r ON r.id = o.reservation_id
    WHERE o.id = oe.source_order_id
      AND oe.source_organizer_id IS DISTINCT FROM r.organizer_id;

    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER orders_reversal_source_organizer_reseat
    AFTER UPDATE OF reservation_id ON orders
    FOR EACH STATEMENT EXECUTE FUNCTION reversal_source_organizer_reseat();

CREATE TRIGGER reservations_reversal_source_organizer_reseat
    AFTER UPDATE OF organizer_id ON reservations
    FOR EACH STATEMENT EXECUTE FUNCTION reversal_source_organizer_reseat();

-- The queue indexes, rebuilt under the same names with the equality in the predicate. Every
-- other predicate and the key are 0021's and 0022's, unchanged: a malformed row is now
-- absent from the index rather than filtered out of it after being read.
DROP INDEX order_refunds_reversal_queue_idx;
CREATE INDEX order_refunds_reversal_queue_idx
    ON order_refunds (reversal_next_attempt_at, id)
    WHERE status = 'completed'
      AND reversal_parked_at IS NULL
      AND (tickets_voided_at IS NULL OR capacity_returned_at IS NULL)
      AND source_organizer_id = organizer_id;

DROP INDEX order_exchanges_reversal_queue_idx;
CREATE INDEX order_exchanges_reversal_queue_idx
    ON order_exchanges (reversal_next_attempt_at, organizer_id, id)
    WHERE settled_at IS NOT NULL
      AND tickets_exchanged_at IS NOT NULL
      AND capacity_returned_at IS NULL
      AND reversal_parked_at IS NULL
      AND source_organizer_id = organizer_id;

-- +goose Down
-- Fails closed while a malformed row exists, for a reason specific to this migration rather
-- than copied from 0021/0022. Rolling back restores the correlated EXISTS, whose cost is
-- linear in the rejected prefix. Doing that WHILE a rejected prefix exists reinstates the
-- exact unbounded scan this migration removed, on a queue that is already carrying the
-- population that triggers it. A clean queue rolls back freely, which keeps the escape hatch
-- open for a deploy undone before any mismatch appears.
LOCK TABLE order_refunds, order_exchanges, orders, reservations IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
DECLARE bad bigint;
BEGIN
    SELECT (SELECT count(*) FROM order_refunds WHERE source_organizer_id IS DISTINCT FROM organizer_id)
         + (SELECT count(*) FROM order_exchanges WHERE source_organizer_id IS DISTINCT FROM organizer_id)
      INTO bad;
    IF bad > 0 THEN
        RAISE EXCEPTION 'cannot roll back 0026: % reversal queue rows have a source organizer differing from their own, and rolling back restores a claim scan whose cost is linear in exactly those rows — repair them or roll forward', bad;
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER reservations_reversal_source_organizer_reseat ON reservations;
DROP TRIGGER orders_reversal_source_organizer_reseat ON orders;
DROP FUNCTION reversal_source_organizer_reseat;
DROP TRIGGER order_exchanges_source_organizer ON order_exchanges;
DROP TRIGGER order_refunds_source_organizer ON order_refunds;
DROP FUNCTION order_exchanges_source_organizer;
DROP FUNCTION order_refunds_source_organizer;

DROP INDEX order_exchanges_reversal_queue_idx;
CREATE INDEX order_exchanges_reversal_queue_idx
    ON order_exchanges (reversal_next_attempt_at, organizer_id, id)
    WHERE settled_at IS NOT NULL
      AND tickets_exchanged_at IS NOT NULL
      AND capacity_returned_at IS NULL
      AND reversal_parked_at IS NULL;

DROP INDEX order_refunds_reversal_queue_idx;
CREATE INDEX order_refunds_reversal_queue_idx
    ON order_refunds (reversal_next_attempt_at, id)
    WHERE status = 'completed'
      AND reversal_parked_at IS NULL
      AND (tickets_voided_at IS NULL OR capacity_returned_at IS NULL);

ALTER TABLE order_exchanges DROP COLUMN source_organizer_id;
ALTER TABLE order_refunds DROP COLUMN source_organizer_id;
