-- Exchanged tickets (TKT-166, ADR-039). An exchange voids ALL of an order's tickets and
-- issues the replacement line in their place; `exchanged` joins the lifecycle vocabulary
-- and, like issued/delivered/redeemed/refunded, can happen at most once per ticket.
--
-- The canonical form is NOT changed, for the same reason 0007 did not change it:
-- `event_type` is already a canonical field, so adding a VALUE to it changes no signed
-- bytes and needs no canonical-version migration (ADR-021). Adding a canonical FIELD
-- would. The golden literals pinning the canonical forms are untouched by this file, and
-- that is the assertion — not an oversight.
--
-- There is deliberately NO `ticket_exchange_batches` analogue of 0007's refund binding.
-- That table exists because a refund's identity and its order arrive as two independent
-- caller-supplied fields, so nothing but a stored binding stops the same refund id being
-- presented against a second order. An exchange arrives as ONE domain event whose id
-- commerce derives from the exchange, and `consumed_events` already refuses it twice —
-- the source order is inside the event, not beside it. A binding table here would record
-- a fact the dedupe already holds.
-- +goose Up
-- Widen the guard BEFORE anything can write the new value, as 0007 did: there is never an
-- instant where an `exchanged` row could be written against a constraint forbidding it.
ALTER TABLE lifecycle_events
  ADD CONSTRAINT lifecycle_events_event_type_exchange_check
  CHECK (event_type IN ('issued','delivered','redeemed','entry','exit','duplicate_admit','refunded','exchanged'));
ALTER TABLE lifecycle_events
  DROP CONSTRAINT lifecycle_events_event_type_refund_check;

-- `exchanged` joins the once-per-ticket set. The admission types (entry/exit/
-- duplicate_admit) stay repeatable.
DROP INDEX lifecycle_events_singleton_type_uidx;
CREATE UNIQUE INDEX lifecycle_events_singleton_type_uidx
  ON lifecycle_events (ticket_id, event_type)
  WHERE event_type IN ('issued', 'delivered', 'redeemed', 'refunded', 'exchanged');

-- +goose Down
-- Unconditionally irreversible, like 0004 through 0007 before it. Access migrations over
-- the lifecycle trail do not roll back: the trail is signed and immutable, and three tests
-- in this service assert that the head migration refuses. A conditional guard ("only if no
-- exchanged rows exist") would be weaker than the convention and would quietly make the
-- head reversible on any database that simply had not exchanged anything yet.
-- +goose StatementBegin
DO $$
BEGIN
  RAISE EXCEPTION 'migration 0008 is irreversible: ticket lifecycle history is immutable';
END $$;
-- +goose StatementEnd
