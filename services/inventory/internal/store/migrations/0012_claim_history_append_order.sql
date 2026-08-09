-- +goose Up
-- A meaningful total order for claim_history (TKT-230 / ADR-003).
--
-- The trail was read with `ORDER BY occurred_at, id`. That is total but not MEANINGFUL:
-- `id` is uuid.New() -- UUIDv4, random, no time component -- so rows tying on occurred_at
-- were ordered by a coin flip. And ties are not exotic: occurred_at defaults to now(),
-- which is TRANSACTION-START time, and separate concurrent transactions can be issued the
-- same value. Measured before this change: 8 concurrent writers x 150 inserts produced
-- 1199 distinct timestamps over 1200 rows. Serial writers never collided.
--
-- ADR-003 makes the trail's order a RECONSTRUCTION guarantee ("any ticket's authoritative
-- history is reconstructible from its trace"), so an ambiguous order is a defect against
-- that ADR, not merely a flaky test.

-- Why occurred_at stays PRIMARY and append_order is only the tie-break.
--
-- A sequence value is allocated at INSERT, not at commit, so in the general case a lower
-- append_order can commit after a higher one and the pair would be ordered by allocation
-- rather than by what actually happened. That would matter if two transactions could
-- interleave their history writes for the same claim -- and they cannot: EVERY path that
-- appends to claim_history holds `SELECT ... FROM inventory_pools ... FOR UPDATE` on the
-- pool row for the duration (ADR-010's pool-then-claim lock order; operational.go:121,217,
-- 283, store.go:162,201,313, capacity.go:40, refund_returns.go:74, reservations.go:47,161).
-- A claim belongs to exactly one pool, so the rows any one History() call returns were
-- written under one lock, strictly serialized: allocation order IS commit order for them.
--
-- So append_order is not load-bearing for cross-transaction chronology, and making it
-- primary would be WRONG -- it would reorder histories whose timestamps differ, which is
-- almost all of them. occurred_at remains the chronology; append_order only decides the
-- coin flips. Both directions are pinned by tests (TKT-230).
CREATE SEQUENCE claim_history_append_order_seq AS bigint;

-- NULLABLE on purpose, and this is the crux of the design.
--
-- claim_history is append-only and carries a BEFORE UPDATE OR DELETE trigger (0003). A
-- NOT NULL column would require backfilling existing rows, and that backfill is an UPDATE
-- the trigger refuses BY DESIGN. Nor should it be worked around: the true append order of
-- pre-existing rows is not recoverable -- the database holds only occurred_at and a random
-- uuid -- so inventing one would canonize the coin flip as if it were history.
--
-- Pre-existing rows therefore keep append_order IS NULL and fall back to the old uuid
-- order among themselves. That order is a STABLE LEGACY TIE-BREAK, not reconstructed
-- chronology, and must not be described as one.
-- No CHECK constraint and no unique index on this column, deliberately (ai-review
-- finding 2). Both would scan the whole table, and goose runs the migration in one
-- transaction, so the ACCESS EXCLUSIVE lock ALTER TABLE takes is held for the duration of
-- every scan that follows. On a realistically populated audit table that can exceed the
-- 30s migration bound (ADR-008, kept by ADR-022) -- and a migration job that times out
-- stops the service from starting at all (ADR-022 gates the service on it).
--
-- Neither would buy anything a scan-free rule does not already give:
--   * positivity -- `nextval` on a sequence starting at 1 and never reset cannot emit
--     zero or a negative, and the trigger below is the only writer;
--   * uniqueness -- `nextval` does not repeat, and no writer supplies the value.
-- A constraint that restates what the only writer already guarantees is not defence in
-- depth here; it is a full-table scan under an exclusive lock in exchange for nothing.
-- ADD COLUMN with no default and no constraint is metadata-only in PostgreSQL 11+.
ALTER TABLE claim_history ADD COLUMN append_order bigint;

ALTER SEQUENCE claim_history_append_order_seq OWNED BY claim_history.append_order;

ALTER TABLE claim_history
  ALTER COLUMN append_order SET DEFAULT nextval('claim_history_append_order_seq');

-- The DEFAULT alone is not enough. A DEFAULT does not apply when a writer NAMES the column
-- and passes NULL explicitly -- verified against PostgreSQL:
--     INSERT INTO t(id)          -> DEFAULT applies
--     INSERT INTO t(id, ord) ... NULL -> stored as NULL
-- Every writer today omits the column, so the DEFAULT covers them; this trigger is what
-- keeps that true for a writer that does not yet exist. Without it the ordering guarantee
-- has a silent hole.
-- +goose StatementBegin
CREATE FUNCTION claim_history_assign_append_order() RETURNS trigger AS $$
BEGIN
  IF NEW.append_order IS NULL THEN
    NEW.append_order := nextval('claim_history_append_order_seq');
  END IF;
  RETURN NEW;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER claim_history_set_append_order
  BEFORE INSERT ON claim_history
  FOR EACH ROW EXECUTE FUNCTION claim_history_assign_append_order();

-- The two lookup indexes (claim_history_claim, claim_history_pool) are deliberately NOT
-- rebuilt to include append_order. Both queries filter to a single claim_id / pool_id, so
-- the rows per key are few and sorting them is free; rebuilding two indexes inside the 30s
-- migration bound (ADR-008, kept by ADR-022) would be a real cost against no measured
-- benefit. Adding an index later if a plan regression appears is the cheap direction.

-- +goose Down
DROP TRIGGER claim_history_set_append_order ON claim_history;
DROP FUNCTION claim_history_assign_append_order();
ALTER TABLE claim_history DROP COLUMN append_order;
DROP SEQUENCE IF EXISTS claim_history_append_order_seq;
