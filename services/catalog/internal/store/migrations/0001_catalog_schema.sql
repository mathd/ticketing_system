-- Catalog schema (US-002 / TKT-26). Every owned entity carries organizer_id
-- (ADR-002 tenancy invariant); organizers is the tenant root. Money is
-- integer minor units + ISO-4217 code (ADR-001). A performance is the first
-- dated slot (ADR-005): no concert-specific fields.
-- +goose Up
CREATE TABLE organizers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE venues (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    name         text NOT NULL,
    ga_capacity  integer NOT NULL CHECK (ga_capacity > 0),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    -- locale-keyed text (TKT-36): new locales are data, not DDL
    name         jsonb NOT NULL,
    description  jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE performances (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id     uuid NOT NULL REFERENCES organizers (id),
    event_id         uuid NOT NULL REFERENCES events (id),
    venue_id         uuid NOT NULL REFERENCES venues (id),
    starts_at        timestamptz NOT NULL,
    timezone         text NOT NULL,
    status           text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
    published_at     timestamptz,
    -- poor man's outbox marker: null while the publication's domain event
    -- has not been ack'd by JetStream (ADR-009; full outbox owed to US-004)
    event_emitted_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX performances_public_read ON performances (status, starts_at);

CREATE TABLE ticket_types (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id   uuid NOT NULL REFERENCES organizers (id),
    performance_id uuid NOT NULL REFERENCES performances (id),
    name           jsonb NOT NULL,
    price_amount   bigint NOT NULL CHECK (price_amount >= 0),
    currency       char(3) NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ticket_types_by_performance ON ticket_types (performance_id);

-- +goose Down
DROP TABLE ticket_types;
DROP TABLE performances;
DROP TABLE events;
DROP TABLE venues;
DROP TABLE organizers;
