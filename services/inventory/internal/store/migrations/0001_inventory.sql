-- +goose Up
CREATE TABLE inventory_pools (
    slot_id uuid PRIMARY KEY,
    organizer_id uuid NOT NULL,
    capacity integer NOT NULL CHECK (capacity > 0),
    confirmed_quantity integer NOT NULL DEFAULT 0 CHECK (confirmed_quantity >= 0),
    source_event_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE claims (
    id uuid PRIMARY KEY,
    organizer_id uuid NOT NULL,
    pool_id uuid NOT NULL REFERENCES inventory_pools(slot_id),
    quantity integer NOT NULL CHECK (quantity > 0),
    status text NOT NULL CHECK (status IN ('held','confirmed','released','expired')),
    expires_at timestamptz NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organizer_id, idempotency_key)
);
CREATE INDEX claims_pool_status_expiry ON claims(pool_id, status, expires_at);

CREATE TABLE consumed_events (
    event_id uuid PRIMARY KEY,
    consumed_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE consumed_events;
DROP TABLE claims;
DROP TABLE inventory_pools;
