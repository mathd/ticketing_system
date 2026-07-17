-- +goose Up
-- Repeatable admission event types (ADR-025 §D1/§D7). Statement order is
-- BINDING: the widened CHECK (distinct name — the old one still exists) and the
-- partial unique index are both in place before the table-wide UNIQUE drops, so
-- no window exists in which a second singleton row could land. Three full-table
-- scans (CHECK validation + two index builds); the complete migration is
-- measured against ADR-008's 30-second bound — see
-- docs/learnings/TKT-84-lifecycle-migration-measurement.md. Plain index builds
-- only; ADR-020 still rejects the concurrent build variant.
ALTER TABLE lifecycle_events
  ADD CONSTRAINT lifecycle_events_event_type_admission_check
  CHECK (event_type IN ('issued', 'delivered', 'redeemed', 'entry', 'exit', 'duplicate_admit'));

-- Singleton uniqueness survives only for the types ADR-025 §D1 keeps singleton
-- (ADR-012's unique 'delivered' preserved). Repeatable types are protected from
-- retry duplication by the primary key: the occurrence id (ADR-025 §D3).
CREATE UNIQUE INDEX lifecycle_events_singleton_type_uidx
  ON lifecycle_events (ticket_id, event_type)
  WHERE event_type IN ('issued', 'delivered', 'redeemed');

-- The table-wide UNIQUE below carried the only index serving plain ticket_id
-- lookups (History, per-scan chain verification). The partial index cannot
-- serve rows outside its predicate, so repeatable rows need this replacement or
-- every gate scan seq-scans the table (ADR-019's lesson, generalized).
CREATE INDEX lifecycle_events_ticket_idx ON lifecycle_events (ticket_id);

ALTER TABLE lifecycle_events
  DROP CONSTRAINT lifecycle_events_ticket_id_event_type_key;
ALTER TABLE lifecycle_events
  DROP CONSTRAINT lifecycle_events_event_type_check;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  RAISE EXCEPTION 'cannot roll back repeatable admission lifecycle events without destroying immutable ticket history';
END $$;
-- +goose StatementEnd
