-- +goose Up
-- Seat-level claims (US-017 / TKT-80): a seated slot is claimed seat-by-seat,
-- reusing the ADR-010 claim lifecycle and pool lock order while adding a per-seat
-- resource constraint. The GA quantity path is untouched (AC2): existing pools
-- backfill as inventory_kind='ga' and quantity claims keep working on them.

-- Pool kind. A seated pool carries the catalog seat-map id (for pinning) and
-- rejects quantity claims; a GA pool has no seat map. The shape CHECK makes an
-- inconsistent pool (seated without a map, or ga with one) unrepresentable.
ALTER TABLE inventory_pools ADD COLUMN inventory_kind text NOT NULL DEFAULT 'ga'
    CHECK (inventory_kind IN ('ga', 'seated'));
ALTER TABLE inventory_pools ADD COLUMN seat_map_id uuid;
ALTER TABLE inventory_pools ADD CONSTRAINT inventory_pools_kind_shape CHECK (
    (inventory_kind = 'ga'     AND seat_map_id IS NULL)
    OR
    (inventory_kind = 'seated' AND seat_map_id IS NOT NULL)
);

-- Composite-FK target: a claim_seats row must belong to its claim's pool. claims.id
-- is already PK; the extra UNIQUE(id, pool_id) is what a composite FK can reference.
ALTER TABLE claims ADD CONSTRAINT claims_id_pool_unique UNIQUE (id, pool_id);

-- One row per held seat: a seated claim is one buyer claims row with quantity=N and
-- N claim_seats rows. released_at IS NULL means the seat is still consumed by a
-- held/finalizing/confirmed claim; it is set in the SAME transaction as the claim's
-- terminal flip (an already-expired claim is never revisited, so a decoupled update
-- would block the seat forever — ADR-031).
CREATE TABLE claim_seats (
    claim_id      uuid        NOT NULL,
    pool_id       uuid        NOT NULL,
    seat_identity text        NOT NULL CHECK (length(btrim(seat_identity)) > 0),
    claimed_at    timestamptz NOT NULL DEFAULT now(),
    released_at   timestamptz CHECK (released_at IS NULL OR released_at >= claimed_at),
    PRIMARY KEY (claim_id, seat_identity),
    FOREIGN KEY (claim_id, pool_id) REFERENCES claims (id, pool_id) ON DELETE RESTRICT
);

-- AC1 (DB-enforced no-oversell per seat): at most one LIVE claim per (pool, seat).
-- Covers held, finalizing AND confirmed because they all keep released_at NULL —
-- the finalizing window is exactly where a status-based predicate would leak.
CREATE UNIQUE INDEX claim_seats_one_live_per_seat
    ON claim_seats (pool_id, seat_identity)
    WHERE released_at IS NULL;
CREATE INDEX claim_seats_by_claim ON claim_seats (claim_id);

-- +goose Down
-- Refuse to silently discard live seat claims or seated pools.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM claim_seats)
       OR EXISTS (SELECT 1 FROM inventory_pools WHERE inventory_kind = 'seated') THEN
        RAISE EXCEPTION 'seat claims or seated pools exist; resolve them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE IF EXISTS claim_seats;
ALTER TABLE claims DROP CONSTRAINT IF EXISTS claims_id_pool_unique;
ALTER TABLE inventory_pools DROP CONSTRAINT IF EXISTS inventory_pools_kind_shape;
ALTER TABLE inventory_pools DROP COLUMN IF EXISTS seat_map_id;
ALTER TABLE inventory_pools DROP COLUMN IF EXISTS inventory_kind;
