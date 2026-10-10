-- +goose Up
-- Durable outcomes for catalog offering events that cannot be applied as they arrive (TKT-317,
-- ADR-077).
--
-- moot_slots: catalog did not publish this slot when one of its events was handled, so the event
-- was acked as moot. source names the kind of event that was moot. Only a 'publication' record
-- lets a later archive or closure be consumed without a pool. A 'closure' record is kept, and it
-- authorises nothing (ADR-077, D7). The key includes source, so a closure record can neither shadow
-- nor overwrite a publication record for the same slot. There is deliberately no foreign key to
-- inventory_pools: a missing pool is the condition this table answers for. Rows are never deleted.
CREATE TABLE moot_slots (
    organizer_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    source text NOT NULL CHECK (source IN ('publication', 'closure')),
    source_event_id uuid NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organizer_id, slot_id, source)
);

-- catalog_event_parked: the exact envelope of a known catalog event whose catalog answer stayed
-- unusable through every delivery bound. The broker message is terminated, not consumed, so the
-- event has no consumed_events row and a re-driven copy with the same id still applies. This is
-- operational storage. It does not latch readiness: it is not version skew (ADR-017).
CREATE TABLE catalog_event_parked (
    subject text NOT NULL,
    event_id uuid NOT NULL,
    schema bigint NOT NULL CHECK (schema > 0),
    organizer_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    envelope bytea NOT NULL,
    delivery_count bigint NOT NULL CHECK (delivery_count > 0),
    reason text NOT NULL,
    parked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (subject, event_id)
);

-- +goose Down
-- Lock both tables before the emptiness check (R2, TKT-317). A writer's uncommitted row is not
-- visible to a plain read, so without the lock the DROP below would discard that row when the
-- writer commits. SHARE ROW EXCLUSIVE conflicts with the ROW EXCLUSIVE lock that every insert takes:
-- a writer in flight is waited for, and a writer that starts later waits until this step ends.
LOCK TABLE moot_slots, catalog_event_parked IN SHARE ROW EXCLUSIVE MODE;
-- Refuse to discard either table while it holds rows. A publication record decides what a later
-- archive does, and a parked row is the only durable copy of its event's bytes.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM moot_slots) OR EXISTS (SELECT 1 FROM catalog_event_parked) THEN
    RAISE EXCEPTION 'moot slot records or parked catalog events exist; resolve them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE catalog_event_parked;
DROP TABLE moot_slots;
