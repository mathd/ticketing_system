-- Safe edit of a published seat map (US-021 / TKT-104). An edit produces a NEW
-- published version in the same map FAMILY; the previous version stays immutable
-- (ADR-018: the version decision is state-deriving — it depends on which seats
-- are currently pinned — so EditSeatMap decides under a `SELECT … FOR UPDATE`
-- lock on the family's current published row and emits after commit). This
-- migration adds only what that transition and its pinning contract need:
--
--   * seat_maps.map_family_id — all versions of one edited map share this UUID.
--     A pin is version-INDEPENDENT within a family (it survives version bumps),
--     so pins key on the family, never on a specific seat_maps row. Existing
--     maps are each a family of one: backfilled to their own id. NOT NULL after
--     backfill; DEFAULT gen_random_uuid() so a freshly CREATEd v1 map gets its
--     own family id, and EditSeatMap copies the predecessor's family into the
--     new version explicitly.
--
--   * seat_map_pins — the fact-source a sale/hold writes into to say "this seat
--     identity is referenced; do not orphan it". `pinned_by` is free-form text
--     (e.g. "sale:<order_id>", "hold:<hold_id>") so TKT-80 plugs in real holds
--     with NO catalog change (COS-5). The contract is honest-writer CONSISTENCY,
--     not tamper-evidence (ADR-021: an adversary with catalog write access can
--     insert/delete pins at will — this guards our own bugs and races, see
--     ADR-029). Idempotency is a UNIQUE on (map_family_id, seat_identity,
--     pinned_by) so PinSeat can ON CONFLICT DO NOTHING. Organizer-scoped
--     (ADR-002).
--
-- Plain CREATE INDEX (ADR-020 — every table is small/empty, trivially within the
-- 30s budget; CONCURRENTLY still not adopted). Runs out-of-band via the catalog
-- `migrate` subcommand (ADR-022). No event/schema change: the new version emits
-- the existing seat_map.published (schema 1) — it IS a published map.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE seat_maps ADD COLUMN map_family_id uuid;
-- +goose StatementEnd
-- Backfill: every pre-existing map is its own family root (no edits exist yet).
UPDATE seat_maps SET map_family_id = id WHERE map_family_id IS NULL;
ALTER TABLE seat_maps ALTER COLUMN map_family_id SET DEFAULT gen_random_uuid();
ALTER TABLE seat_maps ALTER COLUMN map_family_id SET NOT NULL;
-- The family read (current published version of a family) scopes by
-- (map_family_id, status); ADR-019 — an index backs the filter.
CREATE INDEX seat_maps_by_family ON seat_maps (map_family_id, status);

CREATE TABLE seat_map_pins (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id  uuid NOT NULL REFERENCES organizers (id),
    -- Version-independent: a pin applies to the whole family lineage, so it
    -- references the family UUID (a plain column on seat_maps, not unique — every
    -- version shares it), not a specific seat_maps row. No hard FK for that
    -- reason; EditSeatMap/PinSeat enforce the relationship under the row lock.
    map_family_id uuid NOT NULL,
    seat_identity text NOT NULL,
    -- Free-form reference to the sale/hold that pins this seat; TKT-80 fills it
    -- with a real reference. Part of the idempotency key so distinct references
    -- to the same seat are distinct pins.
    pinned_by     text NOT NULL,
    pinned_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (map_family_id, seat_identity, pinned_by)
);
-- The orphan check reads pins by family (ADR-019).
CREATE INDEX seat_map_pins_by_family ON seat_map_pins (map_family_id);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM seat_map_pins) THEN
        RAISE EXCEPTION 'cannot roll back 0011: seat-map pins exist';
    END IF;
    IF EXISTS (SELECT 1 FROM seat_maps GROUP BY map_family_id HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'cannot roll back 0011: edited seat-map versions exist';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE seat_map_pins;
DROP INDEX seat_maps_by_family;
ALTER TABLE seat_maps DROP COLUMN map_family_id;
