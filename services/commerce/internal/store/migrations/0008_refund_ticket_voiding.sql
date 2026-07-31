-- The reversal half of a refund (TKT-157, ADR-038).
--
-- TKT-156 returned the money and deliberately left the seat sold and the ticket
-- valid. This records the ticket-voiding half.
--
-- A nullable timestamp, not a status vocabulary. TKT-161 adds
-- `capacity_returned_at` beside it and the reversal is complete when both are
-- set. A `reversal_status` CHECK would have to be MIGRATED by TKT-161 to admit
-- its own terminal value; a second nullable column is purely additive. It is
-- also honest about the shape of the thing: two independent obligations, each
-- either discharged or not, in no required order.
--
-- Deliberately NOT backfilled. Refunds written before this migration returned
-- money with their tickets still valid; stamping them would assert a voiding
-- that never happened. They read as outstanding, which is exactly what they are.
-- +goose Up
ALTER TABLE order_refunds
    ADD COLUMN tickets_voided_at timestamptz;

-- No index. The only read is by refund identity, which the primary key already
-- serves; a partial index over outstanding reversals would be write-path tax on
-- the refund path for a query nothing performs yet (ADR-019's own caveat,
-- applied against adding one).

-- +goose Down
LOCK TABLE order_refunds IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM order_refunds WHERE tickets_voided_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0008: tickets have been voided by refunds';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE order_refunds
    DROP COLUMN tickets_voided_at;
