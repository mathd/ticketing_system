-- +goose Up
-- Seat adjacency projection for the orphan rule (TKT-181 / ADR-041).
--
-- Inventory decides seat claims but holds seat identities as opaque
-- "section/row/seat" strings: it has no idea which seats are neighbours. Catalog owns
-- that geometry and decides nothing. The rule needs both, plus the deciding
-- transaction, at the same moment.
--
-- The projection is safe because a PUBLISHED seat-map version is immutable (ADR-029)
-- and a seated pool binds to exactly one version. So the geometry a pool will ever
-- need is fixed the moment it is provisioned: fetched once, before the transaction,
-- and never revalidated. That is what separates this from a subscription-fed
-- projection, whose costs -- replay, reconciliation, staleness -- are all consequences
-- of data that can drift.
--
-- Scoped per POOL rather than per map. It duplicates immutable rows across seated
-- performances on purpose: provisioning completeness, version binding and claim-time
-- scoping all become local and atomic, and the claim path never joins across pools.

-- Which pools enforce the rule. Separate from the projection so a rule-off pool is
-- distinguishable from a pool whose projection failed to load -- the two must never
-- look alike, because one is fine and the other must fail closed.
ALTER TABLE inventory_pools
    ADD COLUMN orphan_prevention_enabled boolean NOT NULL DEFAULT false;

-- One row per seat, naming its immediate neighbours in its row's `position` order.
-- NULL means "no neighbour that side" -- a row end -- and is a real answer, not a gap:
-- an end seat has one neighbour and a one-seat row has none.
--
-- Neighbours are identities, not ids: the claim path holds identities and nothing else,
-- and a join through a surrogate key would need a lookup the lock is trying not to pay
-- for.
CREATE TABLE seat_claim_adjacency (
    pool_id       uuid NOT NULL REFERENCES inventory_pools (slot_id) ON DELETE CASCADE,
    seat_identity text NOT NULL CHECK (length(btrim(seat_identity)) > 0),
    left_identity  text CHECK (left_identity  IS NULL OR length(btrim(left_identity))  > 0),
    right_identity text CHECK (right_identity IS NULL OR length(btrim(right_identity)) > 0),
    PRIMARY KEY (pool_id, seat_identity)
);

-- The claim-path read is `WHERE pool_id = $1 AND seat_identity = ANY($2)`, which the
-- primary key serves directly (ADR-019: a scoped read is only scoped if an index backs
-- the filter). No second index: nothing queries adjacency any other way.

-- +goose Down
-- Refuse to discard a live projection: dropping it silently disables the rule on every
-- pool that has one, and the flag column would still claim it is on.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM inventory_pools WHERE orphan_prevention_enabled) THEN
        RAISE EXCEPTION 'rule-enabled seated pools exist; resolve them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE IF EXISTS seat_claim_adjacency;
ALTER TABLE inventory_pools DROP COLUMN IF EXISTS orphan_prevention_enabled;
