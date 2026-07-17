-- +goose Up
-- Bounded quarantine for future-schema catalog events (TKT-68, ADR-017 §5b). A parked
-- (NakWithDelay) event occupies the durable's ack window, and ~1000 of them stall the whole
-- consumer; instead the raw envelope is persisted here and the original acked. `envelope` is the
-- exact received bytes: `inventory reprocess-quarantine` republishes them verbatim, without
-- decoding or normalization. Operational recovery storage only — no audit/integrity claim
-- (ADR-021 draws that line).
CREATE TABLE catalog_event_quarantine (
    subject text NOT NULL,
    event_id uuid NOT NULL,
    -- bigint, not integer: the consumer forwards any envelope schema above its known max here,
    -- and an int32-overflowing value must quarantine like any other future variant — an INSERT
    -- range error would send one malformed event into a permanent NAK loop (ai-review finding 1).
    schema bigint NOT NULL CHECK (schema > 0),
    envelope bytea NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    reinjected_at timestamptz,
    PRIMARY KEY (subject, event_id)
);

-- Keyset scans over unresolved rows, oldest first (the reprocessor's read path).
CREATE INDEX catalog_event_quarantine_pending
    ON catalog_event_quarantine (first_seen_at, subject, event_id)
    WHERE reinjected_at IS NULL;

-- +goose Down
-- Refuse to silently drop events still waiting for a binary that understands them —
-- losing them here is exactly the silent drop TKT-61 removed.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM catalog_event_quarantine WHERE reinjected_at IS NULL) THEN
    RAISE EXCEPTION 'unresolved quarantined events exist; reprocess them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE catalog_event_quarantine;
