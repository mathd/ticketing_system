-- Publish a seat map + seated slots (US-020 / TKT-103). Extends the stable
-- TKT-102 shape rather than reshaping it: seat_maps already carries version +
-- status ('draft'|'published'|'archived'), so publishing is a monotonic
-- draft->published flip (ADR-018: a monotonic one-way transition stays correct
-- under a plain conditional UPDATE and is lock-free — mirror PublishPerformance,
-- not ArchivePerformance). This migration adds only the columns that transition
-- needs plus the seated performance reference:
--
--   * seat_maps.published_at        — publication instant (mirrors performances.published_at).
--   * seat_maps.event_emitted_at    — poor-man's outbox marker for the seat_map.published
--                                     domain event (null while the event is still owed),
--                                     mirroring performances.event_emitted_at (0001).
--   * performances.seat_map_id       — nullable reference to the exact published seat-map version
--                                     (a version is a seat_maps row, TKT-102). NULL = GA slot;
--                                     non-null = seated. Tenancy is enforced at the DATABASE by a
--                                     COMPOSITE foreign key on (seat_map_id, organizer_id, venue_id)
--                                     — the same physical-boundary pattern festivals use for
--                                     capacity groups (0006: performances_capacity_group_fk ->
--                                     festivals(id, organizer_id)). A plain FK on seat_map_id alone
--                                     would only prove the map exists; the composite FK makes a
--                                     cross-organizer or cross-venue reference UNREPRESENTABLE even
--                                     for a direct SQL / repair path, not merely rejected in the
--                                     store query (ADR-002, ai-review L1). The published-status
--                                     precondition stays in the store query (a CHECK/FK cannot
--                                     express "referenced row's status = 'published'").
--
-- No index is added beyond what the composite FK needs: no read in this ticket
-- filters by seat_map_id (ADR-019 — a scoped read is only scoped if an index
-- backs the filter). Plain DDL, no CREATE INDEX (ADR-020). Runs out-of-band via
-- the catalog `migrate` subcommand (ADR-022); every table is small so the ALTERs
-- are trivially within the 30s budget. No inventory migration: the seated event
-- (schema 4) is acknowledged without provisioning a pool (seat-level claim is
-- TKT-80), so there is no new inventory state to store.
-- +goose Up
ALTER TABLE seat_maps
    ADD COLUMN published_at     timestamptz,
    ADD COLUMN event_emitted_at timestamptz;

-- The composite unique the tenancy FK targets (mirrors festivals_id_organizer_unique).
ALTER TABLE seat_maps
    ADD CONSTRAINT seat_maps_id_organizer_venue_unique UNIQUE (id, organizer_id, venue_id);

ALTER TABLE performances
    ADD COLUMN seat_map_id uuid;

ALTER TABLE performances
    ADD CONSTRAINT performances_seat_map_fk
    FOREIGN KEY (seat_map_id, organizer_id, venue_id)
    REFERENCES seat_maps (id, organizer_id, venue_id) ON DELETE RESTRICT;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM performances WHERE seat_map_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0010: seated performances exist';
    END IF;
    IF EXISTS (SELECT 1 FROM seat_maps WHERE status <> 'draft') THEN
        RAISE EXCEPTION 'cannot roll back 0010: published or archived seat maps exist';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE performances DROP CONSTRAINT performances_seat_map_fk;
ALTER TABLE performances DROP COLUMN seat_map_id;
ALTER TABLE seat_maps DROP CONSTRAINT seat_maps_id_organizer_venue_unique;
ALTER TABLE seat_maps DROP COLUMN published_at, DROP COLUMN event_emitted_at;
