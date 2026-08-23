-- The index behind the voided-ticket feed (TKT-162, ADR-066).
--
-- Why an index is part of this ticket rather than a later optimisation. ADR-019
-- says a scoped read is only scoped if an index BACKS the filter: without one the
-- feed returns exactly the right rows, having read every organizer's tickets to
-- find them. The result is correct and the scan is not, which no assertion about
-- the returned rows can detect — hence the EXPLAIN proof in
-- voided_feed_smoke_test.go, and hence this migration shipping alongside the
-- query it serves.
--
-- Why (organizer_id, id) and not something over lifecycle_events. The feed reads
-- `refunded`/`exchanged` events for ONE organizer, and lifecycle_events carries
-- no organizer column — organizer lives on tickets — so the join is unavoidable
-- and the only useful access path starts from tickets. An index on
-- lifecycle_events (occurred_at DESC, id DESC) WHERE event_type IN (...) is the
-- tempting alternative and is wrong for this read: it optimises the GLOBAL voided
-- stream and would walk other organizers' voided rows before discarding them.
--
-- The trailing `id` is the join key, so the planner can walk one organizer's
-- ticket set and reach lifecycle_events through the existing
-- lifecycle_events_ticket_idx without returning to the heap for it.
--
-- Not partial. `tickets` has no column saying whether a ticket is voided — that
-- fact lives in lifecycle_events — so there is no predicate to be partial on. The
-- commerce precedent (0017_customer_order_index.sql) is partial only because its
-- table carries the status it filters.
--
-- Plain CREATE INDEX, and deliberately not the non-blocking variant ADR-020
-- still refuses: its preconditions are conjunctive and two of the three remain
-- false, so nothing has changed there. (The keyword itself is not spelled here
-- on purpose — the statement-order test greps this file's raw text for it.)

-- +goose Up
CREATE INDEX tickets_organizer_feed_idx ON tickets (organizer_id, id);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  -- Every migration in this service refuses to roll back, and this one is not an
  -- exception for a weaker reason than its neighbours. Dropping the index does
  -- not lose data, so the temptation to make it reversible is real — but the feed
  -- keeps returning correct results at whole-table cost, which is silent. A
  -- rollback here does not break the read; it makes it quietly unscoped, which is
  -- the exact failure ADR-019 exists to prevent and the one no test downstream of
  -- the rollback would notice.
  RAISE EXCEPTION 'cannot roll back the voided-feed index: the feed would still answer correctly while scanning every organizer''s tickets (ADR-019). Drop it deliberately if that is truly intended.';
END $$;
-- +goose StatementEnd
