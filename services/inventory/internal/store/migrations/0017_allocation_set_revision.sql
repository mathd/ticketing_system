-- +goose Up
-- The allocation set carries a REVISION (TKT-250, amending ADR-024).
--
-- ADR-024 accepted, in as many words: "Full-set PUT has no stale-write protection
-- (If-Match); acceptable while allocation editing is single-operator." TKT-244 shipped
-- the first UI for that endpoint, so allocation editing stopped being a deliberate
-- one-person curl and became a screen any admin can open. Two admins on one slot is now
-- ordinary, and the premise the ADR rested on is false.
--
-- WHAT GOES WRONG WITHOUT IT. The write is a full-set atomic replace: it DELETEs every
-- allocation row for the pool and re-INSERTs what was submitted. The pool row lock
-- serializes those transactions perfectly, and that is exactly the point -- a lock
-- cannot detect that the FORM was populated before another writer committed. So a save
-- built on a stale read silently overwrites the caps another operator just set, and a
-- row created since the read is deleted with no error on either side.
--
-- WHY A COUNTER ON THE POOL, and not the three alternatives:
--
--   * NOT updated_at on inventory_pools. That column moves on every claim confirmation
--     (store.go), every refund (refund_returns.go), every capacity adjustment and every
--     offering-state change. A revision riding on it would be invalidated by ordinary
--     ticket sales, and an editor that refuses every save during an on-sale is an editor
--     nobody uses.
--   * NOT a hash of the set. It needs a canonical encoding of opaque channel strings,
--     nullable timestamps, ordering and every field added later -- and ADR-024 forbids
--     normalizing channel codes, so the encoding is load-bearing and fragile. It also
--     cannot distinguish A -> B -> A from no change at all.
--   * NOT a column on channel_allocations. Those rows do not survive a replace: they are
--     deleted and re-inserted, so any per-row version resets to its default on every
--     save. The pool row is the only thing that persists across the write, and it is
--     already the thing being locked.
--
-- A monotonic counter detects A -> B -> A, costs one integer, and is read by the same
-- locked SELECT that already reads capacity -- no extra round trip on the write path.
--
-- BUMPED BY THE REPLACE ONLY. ReplaceChannelAllocations is the sole production writer of
-- channel_allocations (nothing else INSERTs, UPDATEs or DELETEs that table), so a
-- revision moved only there cannot miss a change to the set. Capacity adjustments,
-- claims, refunds and offering-state changes deliberately do NOT move it: they do not
-- change the allocation set, and invalidating an operator's open form because a ticket
-- sold would be a false conflict.
--
-- ADVERSARY (ADR-021): this is honest-writer lost-update protection, NOT tamper-evidence
-- and NOT authorization. State inside the database cannot constrain an adversary who
-- writes to the database: anyone with inventory DB access, or reaching /internal/
-- directly with the shared token, can present or set any revision they like. It
-- constrains two honest operators racing through the back office. Nothing more.
ALTER TABLE inventory_pools
    ADD COLUMN allocation_revision bigint NOT NULL DEFAULT 0
        CHECK (allocation_revision >= 0);

-- No index: the column is only ever read by the pool's primary-key lookup, which is
-- already the locked SELECT in ReplaceChannelAllocations. ADR-020 stands -- plain
-- CREATE INDEX where one is warranted, and here none is.

-- +goose Down
-- Keyed on the presence of REAL STATE, exactly as 0014 and 0015 are: every pool
-- predating this migration has allocation_revision 0, so a bare row-count guard on
-- inventory_pools would refuse every rollback including the safe ones. A revision above
-- zero means some editor is relying on this protection right now, and dropping the
-- column would silently return those saves to last-write-wins.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM inventory_pools WHERE allocation_revision > 0) THEN
    RAISE EXCEPTION 'allocation set revisions are in use; reset them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE inventory_pools DROP COLUMN allocation_revision;
