-- +goose Up
-- Ordering metadata for best-available selection (TKT-81 / ADR-061).
--
-- ADR-041 projected adjacency as a LINKED LIST: each seat names its left and right
-- neighbour, which is exactly what arbitration needs (given these seats, would anything
-- be stranded?) and is useless for selection (find me four free seats together). A
-- linked list has no head you can index to, no order you can sort by, and no way to
-- enumerate runs without chasing pointers -- and 0011 deliberately added no second
-- index, because nothing queried adjacency any other way.
--
-- Selection needs the other access pattern: walk a row in seat order and group free
-- seats into runs. Catalog already has that data and inventory already reads it --
-- `SeatMapAdjacency` sorts each row by `position` and then discards both the row it was
-- in and the position it held, keeping only the neighbour edges. These two columns keep
-- what was being thrown away.
--
-- NULLABLE ON PURPOSE, and the NULL is a real answer rather than a gap. A pool
-- provisioned before this migration has adjacency rows with no ordering metadata, and
-- that state must be DISTINGUISHABLE from "this pool has ordering metadata" -- the same
-- discipline 0011 applied when it kept `orphan_prevention_enabled` separate from the
-- projection, so that a rule-off pool and a pool whose projection failed to load could
-- never look alike. Best-available fails closed on NULL with its own refusal code
-- (`best_available_unsupported`), which is a different thing from "this slot cannot
-- seat your party right now".
--
-- There is deliberately NO BACKFILL, and it is not an omission. `position` could be
-- recovered by walking each list from its `left_identity IS NULL` head, but `row_key`
-- could not: the row's identity was never projected, and any value synthesised here
-- (the head's identity, a hash, an ordinal) would fail to match what the consumer
-- derives on the next re-provision -- leaving two incompatible row namings in one
-- projection with nothing to report it. Repair is a re-provision, which carries the
-- real geometry, through the correction-wave machinery ADR-041 already built for
-- exactly this shape of gap.
ALTER TABLE seat_claim_adjacency ADD COLUMN row_key  text;
ALTER TABLE seat_claim_adjacency ADD COLUMN position integer;

-- Both or neither. A row carrying one half of the ordering pair is meaningless: a
-- position with no row does not say what it is a position IN, and a row with no
-- position does not order anything. Making the half-populated state unrepresentable is
-- cheaper than teaching every reader to distrust it.
ALTER TABLE seat_claim_adjacency ADD CONSTRAINT seat_claim_adjacency_order_shape CHECK (
    (row_key IS NULL     AND position IS NULL)
    OR
    (row_key IS NOT NULL AND position IS NOT NULL AND length(btrim(row_key)) > 0 AND position > 0)
);

-- No two seats share a position within a row. The consumer already refuses a duplicate
-- position when deriving from catalog geometry (`ErrGeometryInvalid`), so this cannot be
-- reached through the provisioning path -- it is here because the ordering is only
-- deterministic if it is total, and a selection query that silently returns an arbitrary
-- order among ties is the kind of defect that passes every test on a fixture with no
-- ties. Partial so pre-metadata rows (all NULL) do not collide with each other.
CREATE UNIQUE INDEX seat_claim_adjacency_row_position
    ON seat_claim_adjacency (pool_id, row_key, position)
    WHERE row_key IS NOT NULL;

-- +goose Down
-- Dropping the metadata silently turns every best-available-capable pool into an
-- unsupported one, with no signal beyond requests starting to refuse. Same stance 0011
-- takes on the projection it added.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM seat_claim_adjacency WHERE row_key IS NOT NULL) THEN
        RAISE EXCEPTION 'pools with best-available ordering metadata exist; resolve them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX IF EXISTS seat_claim_adjacency_row_position;
ALTER TABLE seat_claim_adjacency DROP CONSTRAINT IF EXISTS seat_claim_adjacency_order_shape;
ALTER TABLE seat_claim_adjacency DROP COLUMN IF EXISTS position;
ALTER TABLE seat_claim_adjacency DROP COLUMN IF EXISTS row_key;
