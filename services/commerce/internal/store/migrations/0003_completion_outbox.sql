-- +goose Up
-- Transactional outbox for the order-completion event (ADR-016 §Decision 6).
--
-- The row is inserted inside CompleteOrder's transaction, so an order can never be
-- `completed` without an owed event: the commit that completes the order is the same
-- commit that owes the publication. A crash between commit and publish leaves a
-- claimable row instead of a paid order with no ticket.
--
-- `envelope` is the FROZEN canonical ADR-009 envelope, serialized once at completion
-- time. The publisher must transmit these bytes verbatim rather than rebuild them:
-- rebuilding per attempt makes the payload a function of retry timing while the
-- deterministic message id stays fixed, so two attempts of "the same" event differ.
CREATE TABLE completion_outbox (
  -- The deterministic ADR-009 event id (events.EventID). PK, so the owed event is
  -- unique per order by construction and a replayed completion cannot double-insert.
  event_id uuid PRIMARY KEY,
  order_id uuid NOT NULL REFERENCES orders(id),
  subject text NOT NULL,
  envelope jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- NULL until the broker has acked. Set only after a successful publish, so the
  -- unpublished set is exactly `published_at IS NULL` (ADR-016: ack before marking).
  published_at timestamptz,
  -- Lease held by a drainer while it attempts publication. Past-due or NULL means
  -- claimable; FOR UPDATE SKIP LOCKED + this column is what makes concurrent
  -- drainers and multi-replica ownership safe.
  lease_until timestamptz,
  attempts integer NOT NULL DEFAULT 0,
  last_error text
);

-- The drainer's only query path: unpublished rows whose lease has lapsed, oldest first.
-- Partial index — published rows are the overwhelming majority over time and are never
-- scanned again, so they do not belong in the index.
CREATE INDEX completion_outbox_claimable_idx
  ON completion_outbox (created_at)
  WHERE published_at IS NULL;

-- +goose Down
DROP TABLE completion_outbox;
