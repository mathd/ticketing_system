-- +goose Up
-- Per-occurrence quarantine records (ADR-025 §D2/§D3). The quarantine side of
-- the admission union must record EVERY occurrence Access learns about, not one
-- per ticket: two distinct offline admissions reconciled while a chain is
-- invalid would otherwise leave the second recorded nowhere. Rows split into
-- two shapes, distinguished by admitted_at:
--   live degraded admission  — admitted_at NOT NULL (decision time), occurred_at NULL;
--     still exactly one per ticket (§D6 admit-once), now via the partial unique
--     index below instead of the old ticket_id primary key.
--   reconciliation-learned   — admitted_at NULL, occurred_at NOT NULL (the
--     device-claimed time; recording, not deciding), repeatable per ticket,
--     idempotent by occurrence id.
-- occurrence_id is NULL only on grandfathered pre-protocol rows — no id ever
-- existed to claim, so they are replay-safe by absence.
--
-- Statement order is binding: the one-admission partial unique index exists
-- BEFORE the old ticket_id primary key drops, so no window admits a second
-- live row. Plain index builds only (ADR-020); out-of-band migrate job under
-- the 30-second bound (ADR-022). The measured run is recorded in
-- docs/learnings/TKT-85-quarantine-migration-measurement.md.
ALTER TABLE lifecycle_integrity_quarantine ADD COLUMN occurrence_id uuid;
ALTER TABLE lifecycle_integrity_quarantine ADD COLUMN occurred_at timestamptz;
ALTER TABLE lifecycle_integrity_quarantine ALTER COLUMN admitted_at DROP NOT NULL;
-- The old default (now()) is dropped with the NOT NULL: a reconciliation-learned
-- row must not silently claim a live admission time by omission.
ALTER TABLE lifecycle_integrity_quarantine ALTER COLUMN admitted_at DROP DEFAULT;
-- Every row carries a time — its own full-table validation scan, counted in the
-- measured bound.
ALTER TABLE lifecycle_integrity_quarantine
  ADD CONSTRAINT lifecycle_integrity_quarantine_time_check
  CHECK (admitted_at IS NOT NULL OR occurred_at IS NOT NULL);
CREATE UNIQUE INDEX lifecycle_integrity_quarantine_one_admission_uidx
  ON lifecycle_integrity_quarantine (ticket_id) WHERE admitted_at IS NOT NULL;
-- The §D3 identity check must be indexed or every degraded scan and reconcile
-- seq-scans the table (ADR-019's lesson, generalized).
CREATE UNIQUE INDEX lifecycle_integrity_quarantine_occurrence_uidx
  ON lifecycle_integrity_quarantine (occurrence_id) WHERE occurrence_id IS NOT NULL;
ALTER TABLE lifecycle_integrity_quarantine ADD COLUMN quarantine_id uuid NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE lifecycle_integrity_quarantine DROP CONSTRAINT lifecycle_integrity_quarantine_pkey;
ALTER TABLE lifecycle_integrity_quarantine ADD PRIMARY KEY (quarantine_id);
-- The append-only trigger (lifecycle_quarantine_no_change, 0003) is untouched
-- and still fires: rows of either shape are immutable once written.

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  RAISE EXCEPTION 'cannot roll back per-occurrence quarantine records without destroying degraded-admission history';
END $$;
-- +goose StatementEnd
