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
-- Lock the PARENTS as well as the queue tables, for the whole migration (ai-review F2). The
-- ADD COLUMNs below take ACCESS EXCLUSIVE on order_refunds and order_exchanges by themselves,
-- but nothing here would otherwise hold orders or reservations still: a concurrent identity
-- change can commit between the backfill reading the relationship and this migration
-- committing, and it cannot fire the maintenance triggers because they are created later in
-- this same uncommitted transaction. The result is a row that is stale the moment the migration
-- lands, sitting in the new partial index.
--
-- The lock order is queue tables then parents, matching the Down and matching the application:
-- the queue-row triggers touch their own table first and reach the reservation second. Taking
-- them in one statement makes the order explicit rather than incidental.
--
-- Migrations run out-of-band as a one-shot job the services wait on (ADR-022), so this blocks a
-- deploy rather than live traffic.
LOCK TABLE order_refunds, order_exchanges, orders, reservations IN ACCESS EXCLUSIVE MODE;

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
-- FOR SHARE OF r is what makes the derivation correct under concurrency, and it is not
-- decoration (ai-review F1). Without it the lookup is an unlocked read: transaction A can update
-- a reservation's organizer and run its own maintenance trigger while still uncommitted, then
-- transaction B inserts a queue row for that order, reads the OLD committed organizer, and
-- commits. A's maintenance has already run and cannot revisit B's row, so the row lands with
-- source_organizer_id equal to its own organizer while the source reservation belongs elsewhere.
-- The claim then admits it and the final join drops it, which is exactly the recurring-lease and
-- head-of-line failure this migration exists to remove. Reproduced with two concurrent sessions
-- before the lock was added: passes_claim_predicate true, actually_malformed true.
--
-- FOR SHARE rather than FOR UPDATE: this reads an identity it must not see change, it never
-- writes the reservation, and a share lock lets concurrent inserts against the same reservation
-- proceed together while still conflicting with the FOR UPDATE that an identity change takes.
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
    WHERE o.id = NEW.order_id
    FOR SHARE OF r;
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
    WHERE o.id = NEW.source_order_id
    FOR SHARE OF r;
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
--
-- FOR EACH ROW, scoped to the row that changed — NOT a statement-level trigger rescanning both
-- queue tables. The statement-level version is the obvious shape and it is a performance defect:
-- it hash-joins the whole of order_refunds and order_exchanges against orders and reservations on
-- every parent identity update, whatever the update touched. Measured on a 200,000-row queue,
-- one reservation changing hands: 198ms, 4,812 buffers and 4,364 temp blocks spilled to disk, to
-- update ZERO rows. The row-level form below touches only the queue rows reachable from the row
-- that actually changed, through their existing indexes: 28 buffers and two index scans at the
-- same scale. Every index needed is already there and this migration adds none — orders'
-- reservation_id is UNIQUE (0001), order_refunds_order_idx is 0007's, and
-- order_exchanges_one_per_source is 0010's.
--
-- WHEN clauses so an UPDATE that rewrites other columns costs nothing at all. `IS DISTINCT FROM`
-- rather than `<>`: organizer_id and reservation_id are NOT NULL today, but a `<>` here would be
-- NULL for a null on either side and the trigger would silently not fire — a guard that fails
-- open is a suggestion.
-- +goose StatementBegin
CREATE FUNCTION reservations_reversal_source_organizer_reseat() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE order_refunds rf
    SET source_organizer_id = NEW.organizer_id
    FROM orders o
    WHERE o.id = rf.order_id
      AND o.reservation_id = NEW.id
      AND rf.source_organizer_id IS DISTINCT FROM NEW.organizer_id;

    UPDATE order_exchanges oe
    SET source_organizer_id = NEW.organizer_id
    FROM orders o
    WHERE o.id = oe.source_order_id
      AND o.reservation_id = NEW.id
      AND oe.source_organizer_id IS DISTINCT FROM NEW.organizer_id;

    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION orders_reversal_source_organizer_reseat() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE src uuid;
BEGIN
    SELECT r.organizer_id INTO src FROM reservations r WHERE r.id = NEW.reservation_id FOR SHARE;
    IF src IS NULL THEN
        RAISE EXCEPTION 'order % now points at reservation %, which does not exist', NEW.id, NEW.reservation_id;
    END IF;

    UPDATE order_refunds SET source_organizer_id = src
    WHERE order_id = NEW.id AND source_organizer_id IS DISTINCT FROM src;

    UPDATE order_exchanges SET source_organizer_id = src
    WHERE source_order_id = NEW.id AND source_organizer_id IS DISTINCT FROM src;

    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER orders_reversal_source_organizer_reseat
    AFTER UPDATE OF reservation_id ON orders
    FOR EACH ROW
    WHEN (OLD.reservation_id IS DISTINCT FROM NEW.reservation_id)
    EXECUTE FUNCTION orders_reversal_source_organizer_reseat();

CREATE TRIGGER reservations_reversal_source_organizer_reseat
    AFTER UPDATE OF organizer_id ON reservations
    FOR EACH ROW
    WHEN (OLD.organizer_id IS DISTINCT FROM NEW.organizer_id)
    EXECUTE FUNCTION reservations_reversal_source_organizer_reseat();

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
    -- Compute the mismatch from the AUTHORITATIVE relationship, joining each queue row through
    -- its order to its reservation, rather than reading source_organizer_id (ai-review F5).
    -- Trusting that column here would be circular: it is the thing this migration maintains and
    -- the thing the Down is about to drop, so a rollback prompted by a suspected trigger fault is
    -- exactly the moment its value cannot be taken as evidence. A stale column equal to the
    -- queue's own organizer would count zero mismatches and wave the rollback through, restoring
    -- the correlated scan over a rejected population that really does exist.
    SELECT (SELECT count(*) FROM order_refunds rf
              JOIN orders o ON o.id = rf.order_id
              JOIN reservations r ON r.id = o.reservation_id
             WHERE r.organizer_id IS DISTINCT FROM rf.organizer_id)
         + (SELECT count(*) FROM order_exchanges oe
              JOIN orders o ON o.id = oe.source_order_id
              JOIN reservations r ON r.id = o.reservation_id
             WHERE r.organizer_id IS DISTINCT FROM oe.organizer_id)
      INTO bad;
    IF bad > 0 THEN
        RAISE EXCEPTION 'cannot roll back 0026: % reversal queue rows have a source reservation belonging to another organizer, and rolling back restores a claim scan whose cost is linear in exactly those rows — repair them or roll forward', bad;
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER reservations_reversal_source_organizer_reseat ON reservations;
DROP TRIGGER orders_reversal_source_organizer_reseat ON orders;
DROP FUNCTION reservations_reversal_source_organizer_reseat;
DROP FUNCTION orders_reversal_source_organizer_reseat;
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
