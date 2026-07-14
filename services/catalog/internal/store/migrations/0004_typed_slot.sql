-- Generalize the dated slot (US-009 / TKT-51; ADR-005 amendment + spike
-- docs/spikes/TKT-50-dated-slot-pressure-test.md). A performance becomes one
-- kind of dated slot; festival days and park operating days share the shape.
-- Existing performances migrate to kind 'performance' with no behavioural
-- change. Capacity authority stays in Inventory (ADR-010): no count here.
-- +goose Up
ALTER TABLE performances
    ADD COLUMN kind text NOT NULL DEFAULT 'performance'
        CHECK (kind IN ('performance', 'festival_day', 'operating_day')),
    -- Operating window: a local operating date + open/close time-of-day
    -- (spike §Case 4). Local-date semantics, not a UTC instant, so DST and
    -- midnight-spanning days (closes_at < opens_at) are representable. Null
    -- for kind 'performance', which keeps its starts_at instant.
    ADD COLUMN operating_date date,
    ADD COLUMN opens_at  text CHECK (opens_at  ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    ADD COLUMN closes_at text CHECK (closes_at ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    -- Re-entry policy (spike §Case 1): an Access-layer attribute described here.
    ADD COLUMN re_entry_mode text NOT NULL DEFAULT 'single'
        CHECK (re_entry_mode IN ('single', 'multi', 'count_limited')),
    ADD COLUMN max_entries integer CHECK (max_entries IS NULL OR max_entries > 0),
    ADD COLUMN requires_exit boolean NOT NULL DEFAULT false,
    -- Closure (spike §Case 3): orthogonal to draft|published|archived. A closed
    -- day is still published. closure_version is a monotonic counter; the
    -- closed/reopened domain event id is derived from it, so re-emitting one
    -- transition de-duplicates while a new toggle is a distinct event.
    -- closure_emitted_version is the poor-man's-outbox marker for that counter.
    ADD COLUMN closure_status text NOT NULL DEFAULT 'open'
        CHECK (closure_status IN ('open', 'closed')),
    ADD COLUMN closed_at timestamptz,
    ADD COLUMN closure_reason text,
    ADD COLUMN closure_version integer NOT NULL DEFAULT 0,
    ADD COLUMN closure_emitted_version integer NOT NULL DEFAULT 0,
    -- The instant of the latest closure transition, persisted so the
    -- closed/reopened event's occurred_at is stable across emission retries
    -- (deterministic id ⇒ byte-stable payload). Set on every toggle.
    ADD COLUMN closure_changed_at timestamptz,
    -- Forward-compat seam for shared festival capacity (TKT-14/US-011); the
    -- claim mechanics stay out of scope, this must only not dead-end them.
    ADD COLUMN capacity_group_id uuid;

-- Only kind 'performance' carries an instant; day kinds carry the operating
-- window. Enforced so a slot can never be temporally ambiguous.
ALTER TABLE performances ALTER COLUMN starts_at DROP NOT NULL;
-- Exclusive by kind: a performance carries ONLY an instant; a day kind carries
-- ONLY the operating window. Forbidding the wrong-kind fields keeps the public
-- read's COALESCE unambiguous (a day row can never also carry a starts_at that
-- would silently win over its operating window).
ALTER TABLE performances ADD CONSTRAINT performances_kind_temporal CHECK (
    (kind = 'performance'
        AND starts_at IS NOT NULL
        AND operating_date IS NULL AND opens_at IS NULL AND closes_at IS NULL)
    OR (kind IN ('festival_day', 'operating_day')
        AND starts_at IS NULL
        AND operating_date IS NOT NULL AND opens_at IS NOT NULL AND closes_at IS NOT NULL)
);
-- count_limited needs its ceiling; the other modes must not carry one.
ALTER TABLE performances ADD CONSTRAINT performances_count_limited_max CHECK (
    (re_entry_mode = 'count_limited' AND max_entries IS NOT NULL)
    OR (re_entry_mode <> 'count_limited' AND max_entries IS NULL)
);
-- closure attributes are only meaningful once closed at least once.
ALTER TABLE performances ADD CONSTRAINT performances_closure_consistent CHECK (
    (closure_status = 'closed' AND closed_at IS NOT NULL)
    OR (closure_status = 'open' AND closed_at IS NULL)
);

-- Existing rows already default to kind 'performance' and keep starts_at;
-- the explicit backfill states the intent and is idempotent.
UPDATE performances SET kind = 'performance' WHERE kind IS NULL;

-- +goose Down
-- Fail closed: refuse rollback if ANY row carries TKT-51 state that migration
-- 0003's schema cannot represent — not just non-performance kinds, but also a
-- non-default re-entry policy, any closure (current or historical), or a
-- capacity-group link. Dropping the columns would silently lose all of it, so
-- the down is all-or-nothing and only pristine baseline performance rows roll
-- back. Mirrors 0003's guard.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM performances WHERE NOT (
        kind = 'performance'
        AND starts_at IS NOT NULL
        AND operating_date IS NULL AND opens_at IS NULL AND closes_at IS NULL
        AND re_entry_mode = 'single' AND max_entries IS NULL AND requires_exit = false
        AND closure_status = 'open' AND closed_at IS NULL AND closure_reason IS NULL
        AND closure_version = 0 AND closure_emitted_version = 0 AND closure_changed_at IS NULL
        AND capacity_group_id IS NULL
    )) THEN
        RAISE EXCEPTION 'cannot roll back 0004: rows carry typed-slot state (kind, re-entry, closure or capacity group) not representable in 0003';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE performances DROP CONSTRAINT performances_closure_consistent;
ALTER TABLE performances DROP CONSTRAINT performances_count_limited_max;
ALTER TABLE performances DROP CONSTRAINT performances_kind_temporal;
ALTER TABLE performances ALTER COLUMN starts_at SET NOT NULL;
ALTER TABLE performances
    DROP COLUMN capacity_group_id,
    DROP COLUMN closure_changed_at,
    DROP COLUMN closure_emitted_version,
    DROP COLUMN closure_version,
    DROP COLUMN closure_reason,
    DROP COLUMN closed_at,
    DROP COLUMN closure_status,
    DROP COLUMN requires_exit,
    DROP COLUMN max_entries,
    DROP COLUMN re_entry_mode,
    DROP COLUMN closes_at,
    DROP COLUMN opens_at,
    DROP COLUMN operating_date,
    DROP COLUMN kind;
