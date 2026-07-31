-- The second obligation of a refund's reversal (TKT-161, ADR-038 §6).
--
-- A second nullable timestamp beside tickets_voided_at, not a status vocabulary: two
-- independent obligations, each either discharged or not. The reversal is complete when
-- both are set.
--
-- Independent STORAGE, not independent EXECUTION. ADR-038 §6 originally said "in no
-- required order", which was wrong and is corrected by this ticket: §1 makes
-- voiding-before-capacity a safety property, because freeing the seat while the ticket
-- still admits is the one ordering that can OVERSELL. Commerce enforces that ordering —
-- it will not attempt the return until tickets_voided_at is set.
--
-- Not backfilled. Refunds written before this migration returned money without freeing
-- capacity; stamping them would assert a return that never happened.
-- +goose Up
ALTER TABLE order_refunds
    ADD COLUMN capacity_returned_at timestamptz;

-- +goose Down
LOCK TABLE order_refunds IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM order_refunds WHERE capacity_returned_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0009: refunds have returned confirmed capacity';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE order_refunds
    DROP COLUMN capacity_returned_at;
