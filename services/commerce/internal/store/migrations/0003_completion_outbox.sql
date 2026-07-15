-- +goose Up
-- Transactional outbox for the order-completion event (ADR-016 §Decision 6).
--
-- The row is inserted inside CompleteOrder's transaction, so an order can never be
-- `completed` without an owed event: the commit that completes the order is the same
-- commit that owes the publication. A crash between commit and publish leaves a
-- claimable row instead of a paid order with no ticket.
--
-- `envelope` is the FROZEN canonical ADR-009 envelope, serialized once at completion
-- time. The publisher transmits it rather than rebuilding it: rebuilding per attempt
-- makes the payload a function of retry timing while the deterministic message id
-- stays fixed, so two attempts of "the same" event would differ.
--
-- Stored as `json`, not `jsonb`: jsonb normalizes key order and whitespace, so it
-- preserves the logical document but not the bytes committed at completion. Byte
-- stability across attempts is the point of freezing.
--
-- Backfilling orders completed before this table existed is NOT done here: the event
-- id is uuid_v5(NameSpaceOID, subject||":"||order_id), and PostgreSQL has no built-in
-- UUIDv5 (uuid-ossp/pgcrypto are not installed, and no other migration needs them).
-- Reimplementing the derivation in SQL would fork the definition of an event id.
-- BackfillCompletionOutbox in Go owns it, reusing events.EventID.
CREATE TABLE completion_outbox (
  -- The deterministic ADR-009 event id (events.EventID). PK, so the owed event is
  -- unique per order by construction and a replayed completion cannot double-insert.
  event_id uuid PRIMARY KEY,
  -- One owed completion per order, enforced rather than assumed: event_id is derived
  -- from order_id, so a second row for the same order would mean the derivation broke.
  order_id uuid NOT NULL UNIQUE REFERENCES orders(id),
  subject text NOT NULL,
  envelope json NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- NULL until the broker acked. Set only after a successful publish, so the
  -- unpublished set is exactly `published_at IS NULL` (ADR-016: ack before marking).
  published_at timestamptz,
  -- Identifies the current claimant. Release and retirement are conditional on it, so
  -- a claimant whose lease expired mid-publish cannot clear or retire the lease of the
  -- drainer that superseded it.
  claim_id uuid,
  lease_until timestamptz,
  attempts integer NOT NULL DEFAULT 0,
  -- Backoff gate. A failing row must not be re-claimed immediately: claiming is
  -- oldest-first, so without this a few permanently-failing rows at the head starve
  -- every newer order forever.
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_error text,
  -- Terminal quarantine. A row that has exhausted its attempts stops being claimed at
  -- all, so one poison event cannot block the queue. It stays visible for an operator.
  dead_lettered_at timestamptz
);

-- The drainer's only query path: claimable rows, oldest first. Partial index —
-- published and dead-lettered rows are never scanned again and would only bloat it.
CREATE INDEX completion_outbox_claimable_idx
  ON completion_outbox (next_attempt_at)
  WHERE published_at IS NULL AND dead_lettered_at IS NULL;

-- +goose Down
DROP TABLE completion_outbox;
