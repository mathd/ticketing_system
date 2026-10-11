-- +goose Up
-- The channel carries a REVISION (TKT-325, ADR-080).
--
-- WHAT GOES WRONG WITHOUT IT. PUT /channels/{id} is a full replacement of the
-- mutable fields, and the back office builds it from hidden inputs taken from
-- the page it rendered. Operator A opens the page, operator B disables the
-- channel, A renames it: A's stale `enabled=true` re-enables the channel, and
-- both operators see success. The toggle form has the same shape, so a toggle
-- reverts a concurrent rename. This is the lost update TKT-250 closed for
-- allocations with allocation_revision (inventory 0017).
--
-- A MONOTONIC COUNTER, not updated_at and not a hash. A counter detects
-- A -> B -> A, costs one integer, and is read by the locked SELECT that
-- UpdateChannel already needs. updated_at has a resolution and a clock; a hash
-- needs a canonical encoding of every field added later.
--
-- BUMPED BY UpdateChannel ONLY, once per successful update, including one whose
-- values are unchanged. It is the sole production UPDATE of this table, and
-- CreateChannel inserts at the default 0. A refused update writes nothing, so
-- the revision does not move.
--
-- ADVERSARY (ADR-021): this is lost-update protection between HONEST writers.
-- It is NOT authorization and NOT tamper-evidence. A caller can send any
-- revision it likes; ownership is decided by the organizer predicate, never by
-- this number. Anyone with catalog DB access can set any value here. It stops
-- two operators who race through the back office from silently overwriting
-- each other. Nothing more.
--
-- COST. The constant default is stored as metadata, so there is no table
-- rewrite and no backfill. The CHECK is validated by one scan of the table
-- under the ALTER's lock. The channel registry is small (operator
-- configuration), so that scan is bounded. No index: the column is only read
-- through the primary-key lookup, and ADR-020 stands.
ALTER TABLE channels
    ADD COLUMN revision bigint NOT NULL DEFAULT 0
        CHECK (revision >= 0);

-- +goose Down
-- Keyed on REAL STATE, like inventory 0017: every channel that predates this
-- migration is at revision 0, so a row-count guard would refuse every rollback,
-- including the safe ones. A revision above 0 means an editor relies on this
-- protection now, and dropping the column would silently return its saves to
-- last-write-wins. The lock and the check are one atomic decision, as in 0018:
-- without the lock, an update could bump a revision between them.
LOCK TABLE channels IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM channels WHERE revision > 0) THEN
        RAISE EXCEPTION 'cannot roll back 0025: channel revisions are in use';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE channels DROP COLUMN revision;
