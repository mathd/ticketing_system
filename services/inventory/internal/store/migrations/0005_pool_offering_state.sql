-- +goose Up
-- Pool offering state (TKT-75 / US-012): inventory mirrors the catalog slot's offer state so a
-- dead slot stops taking NEW holds. Two orthogonal axes, exactly the spike's (TKT-50 §Case 3)
-- shape: archival is terminal lifecycle, closure is a reversible attribute.
-- Existing pools were provisioned from performance.published, so published/open is factual at
-- migration time; pools that died before this deploy converge as the new multi-subject durable
-- replays the retained archive/closure events (deliver-all + consumed_events dedupe).
ALTER TABLE inventory_pools
    ADD COLUMN lifecycle_status text NOT NULL DEFAULT 'published'
        CHECK (lifecycle_status IN ('published', 'archived')),
    ADD COLUMN closure_status text NOT NULL DEFAULT 'open'
        CHECK (closure_status IN ('open', 'closed'));

-- Catalog's closure_version is monotonic PER PERFORMANCE, and grouped festival days share one
-- pool — so version ordering must be tracked per (pool, performance) and the pool's
-- closure_status derived (any closed member closes the pool). Comparing unrelated per-slot
-- counters at pool level discards a legitimate closure as stale (ai-review finding 1).
CREATE TABLE pool_slot_closures (
    pool_id uuid NOT NULL REFERENCES inventory_pools(slot_id),
    performance_id uuid NOT NULL,
    closure_status text NOT NULL CHECK (closure_status IN ('open', 'closed')),
    closure_version integer NOT NULL CHECK (closure_version >= 1),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pool_id, performance_id)
);

-- +goose Down
-- Refuse to silently forget that a pool stopped being offered.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM inventory_pools
             WHERE lifecycle_status <> 'published' OR closure_status <> 'open')
     OR EXISTS (SELECT 1 FROM pool_slot_closures) THEN
    RAISE EXCEPTION 'pools carry non-default offering state; resolve them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE pool_slot_closures;
ALTER TABLE inventory_pools
    DROP COLUMN lifecycle_status,
    DROP COLUMN closure_status;
