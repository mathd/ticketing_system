-- +goose Up
-- Orphan-seat prevention, per seat-map VERSION (TKT-179 / ADR-041).
--
-- An orphan is a free seat left with no free neighbour in its row after a selection
-- is taken. Preventing buyers from creating them is worth real revenue, and it is
-- configurable because the rule is wrong for some venues -- boxes, accessible pairs,
-- rows sold like general admission.
--
-- Per VERSION, not per family: a published version is immutable and an edit mints a
-- new one (ADR-029), and a seated pool is bound to one specific version. That binding
-- is what lets inventory project the version's geometry once and never revalidate it,
-- so the rule a live pool enforces cannot be changed by republishing the map.
--
-- Default false so every existing map, and every existing test, behaves exactly as
-- before. Nothing reads this column yet: TKT-181 puts it on the wire and TKT-182
-- enforces it.
ALTER TABLE seat_maps
    ADD COLUMN orphan_prevention_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
-- Refuse to silently discard the setting: dropping it turns every rule-enabled map
-- back into an ordinary one, and nothing else records that the organizer asked for it.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM seat_maps WHERE orphan_prevention_enabled) THEN
        RAISE EXCEPTION 'seat maps with orphan prevention enabled exist; resolve them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE seat_maps DROP COLUMN IF EXISTS orphan_prevention_enabled;
