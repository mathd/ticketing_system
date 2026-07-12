-- v1 has a single organizer (US-002 AC5); the fixed UUID keeps local stacks
-- and smoke fixtures predictable. Seed data is a migration like any other
-- (ADR-008) — nothing in code depends on this specific id.
-- +goose Up
INSERT INTO organizers (id, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'Default Organizer');

-- +goose Down
DELETE FROM organizers WHERE id = '00000000-0000-0000-0000-000000000001';
