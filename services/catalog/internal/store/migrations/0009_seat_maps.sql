-- Draft seat-map authoring (US-019 / TKT-102). Introduces the seat-map domain
-- model (seat_map -> section -> row -> seat) and its authoring path in the
-- DRAFT state only; publishing and the seated-slot/inventory contract are
-- TKT-103. Seating is modelled as venue/map attributes (ADR-005: the claim
-- primitive does not fork; ADR-014: typed columns, not new slot tables or
-- jsonb, so invalid geometry is unrepresentable). Every table is
-- organizer_id-scoped (ADR-002). Versioning is designed in from birth: a map
-- carries a version + a status starting 'draft', so TKT-103/104 extend a
-- stable shape rather than reshape it. Run out-of-band via the catalog
-- `migrate` subcommand (ADR-022); plain CREATE INDEX (ADR-020) — every table
-- is empty at migration time, so the builds are trivially within the 30s
-- budget. A venue keeps its GA capacity (venues.ga_capacity untouched) and may
-- carry seat maps simultaneously.
-- +goose Up
CREATE TABLE seat_maps (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    venue_id     uuid NOT NULL REFERENCES venues (id),
    name         text NOT NULL,
    version      integer NOT NULL DEFAULT 1 CHECK (version > 0),
    status       text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'published', 'archived')),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX seat_maps_by_venue ON seat_maps (venue_id);

CREATE TABLE seat_map_sections (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    seat_map_id  uuid NOT NULL REFERENCES seat_maps (id),
    -- '/' is the seat-identity delimiter (see seat_map_seats.seat_identity);
    -- forbidding it in the components keeps the composed identity unambiguous,
    -- so distinct seats can never collide into one identity. Enforced at the API
    -- too (openapi pattern) — this is the direct-SQL backstop.
    name         text NOT NULL CHECK (name NOT LIKE '%/%'),
    position     integer NOT NULL CHECK (position > 0),
    UNIQUE (seat_map_id, name),
    UNIQUE (seat_map_id, position)
);
-- The geometry read scopes every child level by seat_map_id (ADR-019: a scoped
-- read is only scoped if an index backs the filter). Explicit single-column
-- _by_map indexes give those reads a clean, deterministic access path — the
-- planner picks the narrow index over the same-prefix UNIQUE constraints, so
-- the ADR-019 scan-scope proof asserts a stable index name.
CREATE INDEX seat_map_sections_by_map ON seat_map_sections (seat_map_id);

CREATE TABLE seat_map_rows (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    -- seat_map_id denormalized so seat-identity uniqueness is a plain
    -- two-column UNIQUE on seats (Postgres cannot express a join-backed
    -- uniqueness scope); also gives the geometry read an index to scope by.
    seat_map_id  uuid NOT NULL REFERENCES seat_maps (id),
    section_id   uuid NOT NULL REFERENCES seat_map_sections (id),
    -- 'A', 'AA', '12' — venues don't number uniformly; no '/' (identity delimiter).
    label        text NOT NULL CHECK (label NOT LIKE '%/%'),
    position     integer NOT NULL CHECK (position > 0),
    UNIQUE (section_id, label),
    UNIQUE (section_id, position)
);
CREATE INDEX seat_map_rows_by_map ON seat_map_rows (seat_map_id);

CREATE TABLE seat_map_seats (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id  uuid NOT NULL REFERENCES organizers (id),
    seat_map_id   uuid NOT NULL REFERENCES seat_maps (id),
    row_id        uuid NOT NULL REFERENCES seat_map_rows (id),
    -- Stable seat identity: the contract TKT-104 pins against. Composed once,
    -- server-side, from the parent section/row/seat labels and never mutated.
    seat_identity text NOT NULL,
    -- display label; may differ from the identity; no '/' (identity delimiter).
    label         text NOT NULL CHECK (label NOT LIKE '%/%'),
    position      integer NOT NULL CHECK (position > 0),
    UNIQUE (seat_map_id, seat_identity),   -- duplicate seat within a map version -> 409
    UNIQUE (row_id, position)
);
CREATE INDEX seat_map_seats_by_map ON seat_map_seats (seat_map_id);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM seat_maps) THEN
        RAISE EXCEPTION 'cannot roll back seat-map migration: seat maps exist';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE seat_map_seats;
DROP TABLE seat_map_rows;
DROP TABLE seat_map_sections;
DROP TABLE seat_maps;
