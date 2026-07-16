-- +goose Up
-- Channel allocations (TKT-78 / ADR-024): a pool's sellable capacity can be split across
-- opaque sales channels with per-channel caps and an optional scheduled give-back.
-- Allocations are configuration rows; consumption is always derived from claims under the
-- pool lock — no counters, so nothing can drift. An allocation past release_at simply stops
-- matching the active predicate (release_at IS NULL OR release_at > now()), exactly like
-- hold expiry: lazy, DB-time, correct without a sweeper.
CREATE TABLE channel_allocations (
    pool_id uuid NOT NULL REFERENCES inventory_pools(slot_id),
    channel_code text NOT NULL CHECK (length(channel_code) BETWEEN 1 AND 100),
    cap integer NOT NULL CHECK (cap > 0),
    release_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pool_id, channel_code)
);

-- NULL = the default/public channel. No foreign key to channel_allocations: closing a
-- channel must not destroy historical claim attribution.
ALTER TABLE claims ADD COLUMN channel_code text
  CHECK (channel_code IS NULL OR length(channel_code) BETWEEN 1 AND 100);

-- Operational holds stay unchanneled: they consume pool capacity only.
ALTER TABLE claims DROP CONSTRAINT claims_kind_shape;
ALTER TABLE claims ADD CONSTRAINT claims_kind_shape CHECK (
  (claim_kind = 'buyer' AND expires_at IS NOT NULL AND operational_purpose IS NULL AND operational_label IS NULL)
  OR
  (claim_kind = 'operational' AND expires_at IS NULL
    AND operational_purpose IN ('house','artist','kill','other')
    AND operational_label IS NOT NULL AND length(trim(operational_label)) > 0
    AND channel_code IS NULL)
);

CREATE INDEX claims_pool_channel_status_expiry ON claims(pool_id, channel_code, status, expires_at);

-- +goose Down
-- Refuse to silently discard channel attribution or allocation configuration.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM claims WHERE channel_code IS NOT NULL)
     OR EXISTS (SELECT 1 FROM channel_allocations) THEN
    RAISE EXCEPTION 'channel allocations or channel-attributed claims exist; resolve them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX claims_pool_channel_status_expiry;
ALTER TABLE claims DROP CONSTRAINT claims_kind_shape;
ALTER TABLE claims ADD CONSTRAINT claims_kind_shape CHECK (
  (claim_kind = 'buyer' AND expires_at IS NOT NULL AND operational_purpose IS NULL AND operational_label IS NULL)
  OR
  (claim_kind = 'operational' AND expires_at IS NULL
    AND operational_purpose IN ('house','artist','kill','other')
    AND operational_label IS NOT NULL AND length(trim(operational_label)) > 0)
);
ALTER TABLE claims DROP COLUMN channel_code;
DROP TABLE channel_allocations;
