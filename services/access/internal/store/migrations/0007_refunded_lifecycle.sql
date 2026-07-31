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
