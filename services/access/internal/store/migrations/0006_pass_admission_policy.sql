-- +goose Up
-- TKT-87: pass admission policy (ADR-005) over the repeatable event types and
-- occurrence protocol (ADR-025). Three objects, ordered so the hot quarantine
-- table is touched only after the new standalone tables cannot fail:
--   1. slot_re_entry_policies — access's projection of catalog's re_entry
--      policy, fed by the performance.published consumer. No row means
--      "single" (fail to today's semantics, never fail-open).
--   2. lifecycle_integrity_quarantine.event_type — quarantine-side records
--      gain their factual admission type. Existing rows are single-entry
--      degraded/reconciliation records, so the backfill types them 'redeemed'
--      (ADR-025 §D1 gives single tickets no entry/exit vocabulary); the
--      DEFAULT also keeps every existing single-path INSERT correct without
--      naming the column. PG adds constant defaults without a rewrite; the
--      CHECK still validates existing rows with its own scan — counted in the
--      measured bound.
--   3. pass_policy_conflicts — the derived, revisable conflict projection's
--      state (ADR-025 §D2). Deliberately mutable and rebuildable from
--      trace ∪ quarantine + policy; it exists only so raise/withdraw alarm
--      diffs survive the outbox drain, and it is never consulted by an
--      admission decision (ADR-003 §D2).
-- Plain index builds only (ADR-020); out-of-band migrate job under the
-- 30-second bound (ADR-022/ADR-008). Measured run:
-- docs/learnings/TKT-87-policy-migration-measurement.md.
CREATE TABLE slot_re_entry_policies (
  slot_id       uuid PRIMARY KEY,
  organizer_id  uuid NOT NULL,
  mode          text NOT NULL CHECK (mode IN ('single','multi','count_limited')),
  max_entries   integer CHECK (max_entries IS NULL OR max_entries > 0),
  requires_exit boolean NOT NULL DEFAULT false,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((mode = 'count_limited' AND max_entries IS NOT NULL)
      OR (mode <> 'count_limited' AND max_entries IS NULL))
);

ALTER TABLE lifecycle_integrity_quarantine
  ADD COLUMN event_type text NOT NULL DEFAULT 'redeemed'
  CHECK (event_type IN ('redeemed','entry','exit'));

-- Admission-union reads (derive pass state, conflict projection) are by
-- ticket; without this every pass scan seq-scans quarantine (ADR-019's
-- lesson, generalized by 0004).
CREATE INDEX lifecycle_integrity_quarantine_ticket_idx
  ON lifecycle_integrity_quarantine (ticket_id);

CREATE TABLE pass_policy_conflicts (
  ticket_id     uuid NOT NULL,
  organizer_id  uuid NOT NULL,
  slot_id       uuid NOT NULL,
  rule          text NOT NULL CHECK (rule IN ('entry_limit_reached','exit_required')),
  occurrence_id uuid NOT NULL,
  status        text NOT NULL CHECK (status IN ('raised','withdrawn')),
  version       integer NOT NULL CHECK (version > 0),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (ticket_id, rule, occurrence_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  RAISE EXCEPTION 'cannot roll back typed quarantine facts without destroying pass admission history';
END $$;
-- +goose StatementEnd
