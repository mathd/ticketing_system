-- +goose Up
CREATE TABLE consumed_events (event_id uuid PRIMARY KEY, consumed_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE tickets (
 id uuid PRIMARY KEY, order_id uuid NOT NULL, guest_order_ref uuid NOT NULL, organizer_id uuid NOT NULL,
 buyer_id uuid NOT NULL, slot_id uuid NOT NULL, ticket_type_id uuid NOT NULL, qr_payload text NOT NULL,
 issued_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX tickets_guest_reference_idx ON tickets(guest_order_ref,id);
CREATE TABLE lifecycle_events (
 id uuid PRIMARY KEY, ticket_id uuid NOT NULL REFERENCES tickets(id), event_type text NOT NULL CHECK(event_type IN ('issued','delivered')),
 occurred_at timestamptz NOT NULL DEFAULT now(), UNIQUE(ticket_id,event_type)
);
CREATE TABLE delivery_attempts (ticket_id uuid PRIMARY KEY REFERENCES tickets(id), message_id uuid NOT NULL UNIQUE, accepted_at timestamptz);
CREATE FUNCTION lifecycle_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'lifecycle events are immutable'; END; $$;
CREATE TRIGGER lifecycle_events_no_update BEFORE UPDATE OR DELETE ON lifecycle_events FOR EACH ROW EXECUTE FUNCTION lifecycle_events_immutable();
CREATE TRIGGER lifecycle_events_no_truncate BEFORE TRUNCATE ON lifecycle_events FOR EACH STATEMENT EXECUTE FUNCTION lifecycle_events_immutable();

-- +goose Down
DROP TRIGGER lifecycle_events_no_truncate ON lifecycle_events; DROP TRIGGER lifecycle_events_no_update ON lifecycle_events; DROP FUNCTION lifecycle_events_immutable;
DROP TABLE delivery_attempts; DROP TABLE lifecycle_events; DROP TABLE tickets; DROP TABLE consumed_events;
