-- The third exchange timestamp (TKT-166, ADR-039 §6 as amended).
--
-- 0010 shipped two: `settled_at` and `tickets_exchanged_at`. That is one short. The
-- reversal has THREE facts, not two — the money settled, the entitlement switched, and
-- the old capacity came back — and the third is separated from the second by a network
-- call to another service, so a crash between them is not a hypothetical.
--
-- Without this column that gap is invisible: `tickets_exchanged_at` is set BEFORE the
-- inventory call (deliberately, so the ADR-038 §1 ordering is checkable at all), which
-- means a settled, switched exchange with capacity still outstanding is indistinguishable
-- from a complete one. The TKT-166 plan draft proposed leaving it that way. Refused:
-- `order_refunds` already carries `capacity_returned_at` for exactly this reason
-- (ADR-038 §6), and a state the projection cannot express is strictly worse than a
-- nullable column.
--
-- The CHECK records the safety ordering rather than trusting the caller to honour it:
-- capacity cannot come back before the tickets stopped admitting. That is the one
-- ordering that can oversell, and it is now the database's rule.
-- +goose Up
ALTER TABLE order_exchanges ADD COLUMN capacity_returned_at timestamptz;
ALTER TABLE order_exchanges
    ADD CONSTRAINT order_exchanges_capacity_after_switch CHECK (
        capacity_returned_at IS NULL OR tickets_exchanged_at IS NOT NULL
    );

-- +goose Down
ALTER TABLE order_exchanges DROP CONSTRAINT order_exchanges_capacity_after_switch;
ALTER TABLE order_exchanges DROP COLUMN capacity_returned_at;
