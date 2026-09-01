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

-- Two separate defects, two separate fixes. Read both before changing this.
--
-- (1) occurred_at was stamped from now(), which is TRANSACTION time -- fixed at the
--     transaction's first statement, NOT at commit and NOT when the pool lock is taken.
--     Every writer here does BeginTx and only then blocks on
--     `inventory_pools ... FOR UPDATE` (ADR-010's pool-then-claim order), so a transaction
--     that waits on the lock keeps a timestamp from BEFORE it waited. A later-starting
--     transaction can therefore acquire the lock, write and commit FIRST while carrying the
--     LATER timestamp -- and History() would return the two rows in the wrong causal order.
--     Reproduced against a real PostgreSQL: the transaction that committed first stamped
--     05:40:12.96, the one that committed second stamped 05:40:11.90.
--
--     Fixed by defaulting to clock_timestamp(), which is evaluated per STATEMENT. The
--     INSERT happens after the lock is held, so the stamp now reflects the serialized
--     order rather than when the transaction happened to begin. Rows written before this
--     migration keep whatever now() gave them; nothing rewrites them.
--
-- (2) Rows tying on occurred_at were ordered by `id` -- uuid.New(), UUIDv4, random. A
--     coin flip. clock_timestamp() makes ties rarer but cannot abolish them (two
--     statements can still land in the same microsecond), so the tie-break must be
--     meaningful: append_order, below.
--
-- occurred_at stays the PRIMARY sort key and append_order only breaks its ties. Making
-- append_order primary would be wrong in the other direction -- it would reorder every
-- history whose timestamps differ, which is nearly all of them. A sequence value is
-- allocated at INSERT, and while allocation order equals commit order for a single claim
-- (all its writers serialize on one pool row), it is not a commit counter in general.
-- Both directions are pinned by tests (TKT-230).
ALTER TABLE claim_history ALTER COLUMN occurred_at SET DEFAULT clock_timestamp();

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
-- ADD COLUMN with no default and no constraint is metadata-only in PostgreSQL 11+.
ALTER TABLE claim_history ADD COLUMN append_order bigint;

-- NOT VALID: enforced for every new row, but skips the full-table validation scan.
--
-- A plain (validated) CHECK would scan every existing row, and goose runs the migration
-- inside one transaction, so the ACCESS EXCLUSIVE lock ALTER TABLE takes is held across
-- the scan. On a realistically populated audit table that can exceed the 30s bound
-- (ADR-008, kept by ADR-022) -- and a migration job that times out leaves the service
-- unable to start, because ADR-022 gates startup on it. Legacy rows are all NULL and the
-- predicate admits NULL, so there is nothing for a validation pass to discover; the scan
-- would cost the bound and buy nothing. It can be VALIDATEd later, out of band, if ever
-- wanted.
--
-- This is NOT redundant with the trigger below. The trigger assigns a value only when one
-- was not supplied, so any INSERT, COPY or restore that supplies a non-NULL value writes it
-- through unchecked. A negative value would sort ahead of every legitimate row (ai-review
-- finding 3). This CHECK is also the ONLY thing standing between the column and a
-- replication apply, which skips the trigger entirely -- see the note on the trigger below.

ALTER TABLE claim_history
  ADD CONSTRAINT claim_history_append_order_positive
  CHECK (append_order IS NULL OR append_order > 0) NOT VALID;

ALTER SEQUENCE claim_history_append_order_seq OWNED BY claim_history.append_order;

ALTER TABLE claim_history
  ALTER COLUMN append_order SET DEFAULT nextval('claim_history_append_order_seq');

-- The trigger OWNS the value: it overwrites whatever the writer supplied, always.
--
-- Two reasons it cannot merely fill in NULLs.
--
-- First, the DEFAULT does not cover an explicit NULL -- verified against PostgreSQL:
--     INSERT INTO t(id)               -> DEFAULT applies
--     INSERT INTO t(id, ord) ... NULL -> stored as NULL
-- Every writer today omits the column, so the DEFAULT covers them; this keeps it true for
-- a writer that does not yet exist.
--
-- Second, and the reason for the unconditional assignment: a fill-in-NULLs trigger lets
-- any INSERT, COPY or restore supply its OWN value, which then rides through unchecked
-- (ai-review finding 3). A duplicate would collapse two rows back onto the random-uuid
-- tie-break -- reintroducing exactly the defect this migration closes -- and the NOT VALID
-- CHECK above only rejects non-positive values, not repeats.
--
-- CORRECTION (TKT-234, measured): this paragraph originally listed "logical-replication
-- apply" alongside INSERT, COPY and restore, as though the trigger governed it too. It does
-- not. This is an ordinary CREATE TRIGGER with no ENABLE REPLICA / ENABLE ALWAYS, and
-- PostgreSQL's apply worker runs with session_replication_role = replica, which skips
-- ordinary triggers -- verified: an INSERT supplying 999 was renumbered to 1, and the same
-- INSERT under `SET session_replication_role = replica` kept 999. COPY, checked the same
-- way, DOES fire. A subscription's initial sync uses COPY and would therefore renumber the
-- snapshot, after which streaming apply would preserve publisher values: two numbering
-- schemes in one column. Nothing here configures replication today, and while append_order
-- is only a tie-break the discontinuity is tolerable -- TKT-295, which would make it the
-- primary sort key, must decide deliberately. ADR-021 §Amendment (TKT-234) records this.
-- Overwriting is strictly stronger than a unique index here AND costs no scan: the
-- sequence is then the sole source of the value, by construction, so uniqueness is not a
-- constraint to enforce but a property of who assigns it.
--
-- The cost is that a restore cannot preserve append_order through a plain COPY. That is
-- acceptable and deliberate: this column orders rows within one claim's history, it is not
-- an external identifier, and a restore that renumbers it preserves relative order as long
-- as rows are inserted in their original order. A restore path that needs the original
-- values must disable this trigger explicitly and resynchronize the sequence -- which is
-- what `ALTER TABLE ... DISABLE TRIGGER` is for, and what the legacy-row test does.
-- +goose StatementBegin
CREATE FUNCTION claim_history_assign_append_order() RETURNS trigger AS $$
BEGIN
  NEW.append_order := nextval('claim_history_append_order_seq');
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
-- Restore the transaction-time default too. Up changes it; a Down that only removed the
-- append_order objects would leave a hybrid schema carrying this migration's timestamp
-- semantics under the previous version's code (ai-review pass 3, finding 1).
ALTER TABLE claim_history ALTER COLUMN occurred_at SET DEFAULT now();
DROP TRIGGER claim_history_set_append_order ON claim_history;
DROP FUNCTION claim_history_assign_append_order();
ALTER TABLE claim_history DROP COLUMN append_order;
DROP SEQUENCE IF EXISTS claim_history_append_order_seq;
