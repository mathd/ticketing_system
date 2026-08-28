-- Staff-triggered redelivery of a completed order's tickets (TKT-203, ADR-068).
--
-- `redelivered` joins the lifecycle vocabulary and, UNLIKE issued/delivered/
-- redeemed/refunded/exchanged, is REPEATABLE: a resend can genuinely happen more
-- than once per ticket, and recording only the first would make the trail claim
-- the ticket was delivered once when it was delivered four times (ADR-021: the
-- trail's claim must match what happened). So it stays OUT of the singleton
-- partial index, alongside the admission types, and its retry idempotency comes
-- from the event id being derived per (request, ticket) — the same mechanism
-- 0007 uses for `refunded` (ADR-025 §D3: repeatable types are protected from
-- retry duplication by the event id, not by an index).
--
-- The canonical form is NOT changed, for the same reason 0007 and 0008 did not
-- change it: `event_type` is already a canonical field, so adding a VALUE to it
-- changes no signed bytes and needs no canonical-version migration (ADR-021).
-- Adding a canonical FIELD would. The golden literals in
-- internal/lifecycle/canonical_test.go are untouched by this file, and that is
-- the assertion rather than an oversight.
--
-- `delivered` is deliberately left alone. It keeps its singleton index entry and
-- keeps meaning "the automatic delivery on issuance happened once". A resend is
-- a DIFFERENT act by a DIFFERENT actor, and collapsing the two would destroy the
-- only record of which is which.
-- +goose Up
-- Widen the guard BEFORE anything can write the new value, as 0007 and 0008 did:
-- there is never an instant where a `redelivered` row could be written against a
-- constraint that forbids it.
ALTER TABLE lifecycle_events
  ADD CONSTRAINT lifecycle_events_event_type_redelivery_check
  CHECK (event_type IN ('issued','delivered','redeemed','entry','exit','duplicate_admit','refunded','exchanged','redelivered'));
ALTER TABLE lifecycle_events
  DROP CONSTRAINT lifecycle_events_event_type_exchange_check;

-- The singleton index is NOT touched. `redelivered` is repeatable, so it must
-- not join that predicate — and rebuilding the index to the same definition
-- would be a full-table scan for no change. Stated here because the neighbouring
-- migrations all DROP and CREATE it, and its ABSENCE from this file is the
-- decision, not a forgotten step.

-- One idempotency key, one order.
--
-- The lifecycle event id records WHICH tickets a redelivery request resent — it
-- is derived from (request, ticket), so an event under that id can only have
-- been written by that request. What it cannot express is the binding the other
-- way: nothing stops the same key being presented against a DIFFERENT order,
-- whose tickets produce different event ids, so the replay check would find
-- nothing of its own and send a fresh batch. This table is that one fact, and
-- the same reasoning 0007 records for ticket_refund_batches.
--
-- `requested_at` is what the per-order rolling-window bound counts (ADR-068). It
-- is a real column rather than a derivation from the trail because the bound
-- must refuse a request that creates NO events at all — counting the trail would
-- make a refused request invisible to the next one and the bound unenforceable.
--
-- Deliberately does NOT store the ticket ids or the recipient. The ticket ids
-- are in the trail, and copying them here would be a second, divergeable record
-- of the same fact; the recipient is PII and ADR-003 §D3 keeps it out of
-- anything the trail-adjacent surface holds.
CREATE TABLE redelivery_requests (
  organizer_id uuid NOT NULL,
  idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  order_id uuid NOT NULL,
  ticket_count integer NOT NULL CHECK (ticket_count BETWEEN 1 AND 50),
  requested_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organizer_id, idempotency_key)
);

-- The bound's read is "how many distinct requests for THIS order since T", so
-- the index leads on the order and carries the timestamp. Without it the count
-- returns the right number having read every organizer's requests to find them —
-- correct result, wrong scan, which no assertion about the returned value can
-- detect (ADR-019). Plain CREATE INDEX: ADR-020 still rejects CONCURRENTLY here.
CREATE INDEX redelivery_requests_order_time_idx
  ON redelivery_requests (order_id, requested_at DESC);

-- One row per (request, ticket): the delivery attempt a resend makes.
--
-- A SEPARATE table from delivery_attempts rather than a relaxation of it.
-- delivery_attempts.ticket_id is a PRIMARY KEY and its message_id is derived
-- deterministically as ticket+":delivery" with ON CONFLICT DO NOTHING
-- (postgres.go DeliveryID) — that table records "the automatic delivery on
-- issuance", at most once per ticket for all time, and several paths depend on
-- exactly that. Turning its primary key into a non-unique foreign key would
-- change the meaning of every existing row to serve a feature that can be
-- represented beside it.
--
-- message_id is UNIQUE across resends AND distinct from the original attempt's,
-- because a transport that deduplicates on message id would otherwise silently
-- drop the resend as a replay of the first send — which is the precise failure
-- this ticket exists to fix.
CREATE TABLE redelivery_attempts (
  organizer_id uuid NOT NULL,
  idempotency_key text NOT NULL,
  ticket_id uuid NOT NULL REFERENCES tickets(id),
  message_id uuid NOT NULL UNIQUE,
  accepted_at timestamptz,
  PRIMARY KEY (organizer_id, idempotency_key, ticket_id),
  FOREIGN KEY (organizer_id, idempotency_key) REFERENCES redelivery_requests(organizer_id, idempotency_key)
);

-- +goose Down
-- Unconditionally irreversible, like 0004 through 0008 before it. Access
-- migrations over the lifecycle trail do not roll back: the trail is signed and
-- immutable, and this service's migration tests assert that the head migration
-- refuses. A conditional guard ("only if no redelivered rows exist") would be
-- weaker than the convention and would quietly make the head reversible on any
-- database that simply had not resent anything yet.
-- +goose StatementBegin
DO $$
BEGIN
  RAISE EXCEPTION 'migration 0011 is irreversible: ticket lifecycle history is immutable';
END $$;
-- +goose StatementEnd
