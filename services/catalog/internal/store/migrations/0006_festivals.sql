-- Festival capacity groups (US-011 / TKT-53). A festival owns one shared
-- inventory capacity; member festival-day performances reference that group.
-- +goose Up
CREATE TABLE festivals (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id       uuid NOT NULL REFERENCES organizers (id),
    name               jsonb NOT NULL,
    shared_capacity    integer NOT NULL CHECK (shared_capacity > 0),
    status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'archived')),
    event_emitted_at   timestamptz,
    archive_emitted_at timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT festivals_id_organizer_unique UNIQUE (id, organizer_id)
);

ALTER TABLE performances
    ADD CONSTRAINT performances_capacity_group_kind CHECK (
        capacity_group_id IS NULL OR kind = 'festival_day'
    ),
    ADD CONSTRAINT performances_capacity_group_fk
    FOREIGN KEY (capacity_group_id, organizer_id)
    REFERENCES festivals (id, organizer_id) ON DELETE RESTRICT;
CREATE INDEX performances_capacity_group_idx ON performances (capacity_group_id);

-- +goose Down
-- Fail closed rather than silently discard a festival aggregate or detach one
-- of its members. As with 0005, the guard runs before any destructive DDL.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM festivals)
       OR EXISTS (SELECT 1 FROM performances WHERE capacity_group_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0006: festival data exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX performances_capacity_group_idx;
ALTER TABLE performances DROP CONSTRAINT performances_capacity_group_fk;
ALTER TABLE performances DROP CONSTRAINT performances_capacity_group_kind;
ALTER TABLE festivals DROP CONSTRAINT festivals_id_organizer_unique;
DROP TABLE festivals;
