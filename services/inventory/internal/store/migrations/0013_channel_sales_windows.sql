-- +goose Up
-- Per-channel sales windows (TKT-238 / ADR-054): WHEN a channel may sell, alongside
-- ADR-024's HOW MUCH. A presale that opens Tuesday and a public on-sale that opens Friday
-- were previously inexpressible — the only windows in the system were rule-level ones on
-- price/fee/split rules, and the nearest on-sale concept was the pool's binary closure.
--
-- Two nullable columns on the existing allocation row rather than a sibling table. The
-- identity is already exactly (pool_id, channel_code); PUT replacement, the pool lock,
-- derived consumption and cache invalidation all operate on this row. A sibling table
-- would add a join and a SECOND SOURCE OF TRUTH to a decision taken under the pool lock,
-- which is the one place in this schema that cannot afford one.
--
-- Half-open [opens_at, closes_at): a channel that opens at T is sellable AT T, and one
-- that closes at T is not. Same convention as the price/fee/split rule windows, and the
-- reason is the same — two windows that abut must not both admit the instant between them.
--
-- Enforced by a predicate on clock_timestamp(), never now(): now() freezes at transaction
-- start, so a hold queued on the pool lock across the cutoff would decide with stale time
-- and sell a channel that had closed. ADR-024 wrote that reasoning down for release_at and
-- TestReleaseCutoffHoldsUnderPoolLockContention pins it; a window is the same shape.
-- Lazy, DB-time, correct without a sweeper.
ALTER TABLE channel_allocations
    ADD COLUMN opens_at  timestamptz,
    ADD COLUMN closes_at timestamptz;

-- A reversed window is unrepresentable. Without this an instant could be simultaneously
-- before the open and after the close, so the predicate's two halves would stop being
-- jointly exhaustive and "closed" would have two incompatible reasons. NULL on either side
-- is unbounded there: no open = always open, no close = never closes.
ALTER TABLE channel_allocations
    ADD CONSTRAINT channel_allocations_window_order
    CHECK (opens_at IS NULL OR closes_at IS NULL OR opens_at < closes_at);

-- No index. Deliberate, and the same call 0016_fee_rules.sql made in catalog: the window is
-- evaluated on rows already located by the (pool_id, channel_code) primary key inside a
-- transaction that holds the pool row — there is no scan for an index to improve, and an
-- index on a column only ever read under a lock costs writes for nothing.

-- +goose Down
-- Keyed on NON-NULL window values, NOT on a row count.
--
-- Every allocation that existed before this migration has NULL in both columns, so a
-- row-count guard would refuse EVERY rollback including the safe ones -- vacuous in
-- reverse, which is the failure shape a nullable column added to a populated table invites.
-- Catalog's 0019 learned this; 0013 inherits the lesson rather than the bug.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM channel_allocations
             WHERE opens_at IS NOT NULL OR closes_at IS NOT NULL) THEN
    RAISE EXCEPTION 'per-channel sales windows exist; clear them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE channel_allocations
    DROP CONSTRAINT channel_allocations_window_order,
    DROP COLUMN closes_at,
    DROP COLUMN opens_at;
