-- +goose Up
-- The index the wallet read scans (TKT-222 / US-A3, ADR-019).
--
-- Migration 0016 deliberately shipped `orders.customer_id` WITHOUT an index and
-- said why: ADR-019's rule is that an index is justified by the scan it removes,
-- and until this ticket nothing scanned by customer. This is that ticket, so the
-- index arrives with its query.
--
-- Shape follows the query exactly. `customer_id` equality first, then the keyset
-- sort key in the order the read pages by, so one index serves both the filter and
-- the ordering and Postgres never sorts.
--
-- PARTIAL on completed: a wallet lists purchases, and the failed, declined and
-- in-flight rows for the same customer are noise the index should not carry. It
-- also keeps the index small on the hot table.
--
-- The sort key is `created_at`, NOT `updated_at` (plan-review F1). `updated_at` is
-- rewritten by at least six production paths — every checkout retry, the
-- payment_unknown and confirmation_pending transitions, recovery, and the
-- cancellation-refund runner — and a keyset cursor on a mutable key makes rows
-- jump pages: returned twice, or skipped, when a refund months later touches the
-- row. `created_at` is set by the INSERT default and nothing updates it, and it
-- means the thing a buyer expects a purchase list to be sorted by.
--
-- Plain CREATE INDEX. ADR-020 records that `CONCURRENTLY` is still NOT adopted
-- here: ADR-022 satisfied its first precondition, but they are conjunctive and the
-- other two remain false. Do not "improve" this line.
CREATE INDEX orders_customer_completed_idx
    ON orders (customer_id, created_at DESC, id DESC)
    WHERE status = 'completed';

-- +goose Down
DROP INDEX IF EXISTS orders_customer_completed_idx;
