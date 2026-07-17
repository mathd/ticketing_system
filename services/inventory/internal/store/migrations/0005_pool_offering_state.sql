-- +goose Up
-- Pool offering state (TKT-75 / US-012): inventory mirrors the catalog slot's offer state so a
-- dead slot stops taking NEW holds. Two orthogonal axes, exactly the spike's (TKT-50 §Case 3)
-- shape: archival is terminal lifecycle, closure is a reversible attribute. closure_version is
-- the catalog's monotonic closure counter — a delayed closed(v1) must not override reopened(v2).
-- Existing pools were provisioned from performance.published, so published/open/0 is factual.
ALTER TABLE inventory_pools
    ADD COLUMN lifecycle_status text NOT NULL DEFAULT 'published'
        CHECK (lifecycle_status IN ('published', 'archived')),
    ADD COLUMN closure_status text NOT NULL DEFAULT 'open'
        CHECK (closure_status IN ('open', 'closed')),
    ADD COLUMN closure_version integer NOT NULL DEFAULT 0
        CHECK (closure_version >= 0);

-- +goose Down
-- Refuse to silently forget that a pool stopped being offered.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM inventory_pools
             WHERE lifecycle_status <> 'published' OR closure_status <> 'open' OR closure_version <> 0) THEN
    RAISE EXCEPTION 'pools carry non-default offering state; resolve them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE inventory_pools
    DROP COLUMN lifecycle_status,
    DROP COLUMN closure_status,
    DROP COLUMN closure_version;
