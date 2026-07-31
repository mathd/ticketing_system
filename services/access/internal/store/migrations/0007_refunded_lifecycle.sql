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
-- Lock before the guard: checking first leaves a window in which a refund voids
-- a ticket between the check and the DROP, and silently destroying the record of
-- a voided ticket is worse than refusing to roll back.
LOCK TABLE lifecycle_events IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM lifecycle_events WHERE event_type='refunded') THEN
    RAISE EXCEPTION 'cannot roll back 0007: tickets have been voided by refunds';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX lifecycle_events_singleton_type_uidx;
CREATE UNIQUE INDEX lifecycle_events_singleton_type_uidx
  ON lifecycle_events (ticket_id, event_type)
  WHERE event_type IN ('issued', 'delivered', 'redeemed');
ALTER TABLE lifecycle_events
  ADD CONSTRAINT lifecycle_events_event_type_admission_check
  CHECK (event_type IN ('issued','delivered','redeemed','entry','exit','duplicate_admit'));
ALTER TABLE lifecycle_events
  DROP CONSTRAINT lifecycle_events_event_type_refund_check;
