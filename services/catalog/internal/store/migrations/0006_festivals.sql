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
    created_at         timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE performances
    ADD CONSTRAINT performances_capacity_group_fk
    FOREIGN KEY (capacity_group_id) REFERENCES festivals (id) ON DELETE RESTRICT;
CREATE INDEX performances_capacity_group_idx ON performances (capacity_group_id);

-- +goose Down
DROP INDEX performances_capacity_group_idx;
ALTER TABLE performances DROP CONSTRAINT performances_capacity_group_fk;
DROP TABLE festivals;
