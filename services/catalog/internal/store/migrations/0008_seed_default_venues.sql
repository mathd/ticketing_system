-- Seed venues for the default organizer (US-018 / TKT-101). The back-office
-- venue list needs venues to render; v1 has one organizer (0002) and, until
-- now, no venues. Seed data is a migration like any other (ADR-008), run
-- out-of-band via the catalog `migrate` subcommand (ADR-022). Fixed UUIDs keep
-- local stacks and smoke fixtures predictable; ORDER BY name in the read makes
-- output deterministic. Plain INSERTs — no index (ADR-020 N/A).
-- +goose Up
INSERT INTO venues (id, organizer_id, name, ga_capacity)
VALUES
  ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-000000000001', 'La Grande Salle', 2500),
  ('00000000-0000-0000-0000-0000000000a2', '00000000-0000-0000-0000-000000000001', 'Le Petit Théâtre', 350),
  ('00000000-0000-0000-0000-0000000000a3', '00000000-0000-0000-0000-000000000001', 'Parc des Festivals', 12000);

-- +goose Down
DELETE FROM venues WHERE id IN (
  '00000000-0000-0000-0000-0000000000a1',
  '00000000-0000-0000-0000-0000000000a2',
  '00000000-0000-0000-0000-0000000000a3'
);
