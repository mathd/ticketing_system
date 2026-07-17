-- +goose Up
-- Capacity adjustment with the clamp floor (TKT-76 / ADR-026, per the ADR-005 amendment):
-- capacity stays the applied ceiling and never falls below live demand; target_capacity is
-- non-null only while a forward-only cut is draining toward a lower target. Blocking new
-- claims is derived (demand vs COALESCE(target_capacity, capacity)) — no flag column.
ALTER TABLE inventory_pools
    ADD COLUMN target_capacity integer
        CHECK (target_capacity IS NULL OR (target_capacity > 0 AND target_capacity < capacity));

-- Pool-level capacity records join the claim_history audit/idempotency registry: exactly
-- one of (claim-shaped, pool-capacity-shaped) per row. target_capacity records the
-- requested target when a cut clamped; NULL when the adjustment applied fully.
ALTER TABLE claim_history
    ALTER COLUMN claim_id DROP NOT NULL,
    ADD COLUMN pool_id uuid REFERENCES inventory_pools(slot_id),
    ADD COLUMN target_capacity integer,
    DROP CONSTRAINT claim_history_action_check,
    ADD CONSTRAINT claim_history_action_check
        CHECK (action IN ('create','place','release','convert','finalize','confirm','expire','adjust_capacity')),
    ADD CONSTRAINT claim_history_shape CHECK (
        (claim_id IS NOT NULL AND pool_id IS NULL AND target_capacity IS NULL)
        OR (claim_id IS NULL AND pool_id IS NOT NULL AND action = 'adjust_capacity')
    );
CREATE INDEX claim_history_pool ON claim_history(pool_id, occurred_at) WHERE pool_id IS NOT NULL;

-- +goose Down
-- Refuse to silently forget a draining cut or its audit trail.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM inventory_pools WHERE target_capacity IS NOT NULL)
     OR EXISTS (SELECT 1 FROM claim_history WHERE pool_id IS NOT NULL) THEN
    RAISE EXCEPTION 'pools carry capacity-adjustment state; resolve it before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX claim_history_pool;
ALTER TABLE claim_history
    DROP CONSTRAINT claim_history_shape,
    DROP CONSTRAINT claim_history_action_check,
    ADD CONSTRAINT claim_history_action_check
        CHECK (action IN ('create','place','release','convert','finalize','confirm','expire')),
    DROP COLUMN target_capacity,
    DROP COLUMN pool_id,
    ALTER COLUMN claim_id SET NOT NULL;
ALTER TABLE inventory_pools DROP COLUMN target_capacity;
