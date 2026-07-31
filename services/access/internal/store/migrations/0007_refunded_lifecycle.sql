-- Refunded tickets (TKT-157, ADR-038). A refund voids q of an order's tickets;
-- `refunded` joins the lifecycle vocabulary and, like issued/delivered/redeemed,
-- can happen at most once per ticket.
--
-- The canonical form is NOT changed: `event_type` is already a canonical field,
-- so adding a value to it changes no signed bytes and needs no
-- canonical-version migration (ADR-021). Adding a canonical FIELD would.
-- +goose Up
-- Widen the guard BEFORE anything can write the new value: installing the
-- permissive CHECK first means there is never an instant where a `refunded` row
-- could be written against a constraint that forbids it.
ALTER TABLE lifecycle_events
  ADD CONSTRAINT lifecycle_events_event_type_refund_check
  CHECK (event_type IN ('issued','delivered','redeemed','entry','exit','duplicate_admit','refunded'));
ALTER TABLE lifecycle_events
  DROP CONSTRAINT lifecycle_events_event_type_admission_check;

-- `refunded` joins the once-per-ticket set. The admission types (entry/exit/
-- duplicate_admit) stay repeatable.
DROP INDEX lifecycle_events_singleton_type_uidx;
CREATE UNIQUE INDEX lifecycle_events_singleton_type_uidx
  ON lifecycle_events (ticket_id, event_type)
  WHERE event_type IN ('issued', 'delivered', 'redeemed', 'refunded');

-- One refund id, one order, one quantity.
--
-- The lifecycle event id already records WHICH tickets a refund voided — it is
-- derived from (refund_id, ticket_id), so an event under that id can only have
-- been written by that refund. What it cannot express is the binding the other
-- way: nothing stops the same refund id being presented against a DIFFERENT
-- order, whose tickets produce different event ids, so the replay check finds
-- nothing of its own and voids a fresh batch (ai-review F2).
--
-- Deliberately does NOT store the ticket ids. Those are in the trail, and
-- copying them here would be a second, divergeable record of the same fact.
CREATE TABLE ticket_refund_batches (
  organizer_id uuid NOT NULL,
  refund_id uuid NOT NULL,
  order_id uuid NOT NULL,
  quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 50),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organizer_id, refund_id)
);

-- +goose Down
-- Unconditionally irreversible, like 0004, 0005 and 0006 before it. Access
-- migrations over the lifecycle trail do not roll back: the trail is signed and
-- immutable, and three tests in this service assert that the head migration
-- refuses. A conditional guard ("only if no refunded rows exist") would be
-- weaker than the convention and would quietly make the head reversible on any
-- database that simply had not refunded anything yet.
-- +goose StatementBegin
DO $$
BEGIN
  RAISE EXCEPTION 'migration 0007 is irreversible: ticket lifecycle history is immutable';
END $$;
-- +goose StatementEnd
