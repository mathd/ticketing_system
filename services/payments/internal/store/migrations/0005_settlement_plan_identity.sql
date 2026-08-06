-- TKT-219 / ADR-048 §3e: bind a settlement plan's identity to the operation.
--
-- The request fingerprint covers order, buyer, amount, currency and payment
-- token. It does NOT cover the settlement plan, and without this column the
-- operation remembered no plan at all -- so an unresolved operation whose lease
-- expired could be retried with a DIFFERENT valid plan, and an amount the
-- provider had already captured would be recorded under the new attribution.
--
-- Nullable on purpose. Rows bound before this migration carry no digest, and
-- there is nothing to backfill from: the plan was never persisted. Such a row
-- ADOPTS the first retry's digest rather than refusing it, which is exactly the
-- behaviour that existed before this column and no worse. The window closes as
-- those operations resolve.
-- +goose Up
ALTER TABLE payment_operations ADD COLUMN settlement_digest text;

-- +goose Down
ALTER TABLE payment_operations DROP COLUMN settlement_digest;
