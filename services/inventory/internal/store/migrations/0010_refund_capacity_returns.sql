-- Refund capacity return (TKT-161, ADR-038).
--
-- Confirming a claim adds its WHOLE quantity to inventory_pools.confirmed_quantity in
-- one step, and there was no vocabulary for giving part of it back. This is that
-- vocabulary.
--
-- `returned_quantity` is orthogonal to `status`, deliberately. A returned claim is still
-- a CONFIRMED claim — the sale happened — and a new status would have to express
-- "partially refunded, twice, by two different refunds", which a lifecycle state cannot.
-- Mutating `quantity` was the other option and it destroys the original sale quantity,
-- which is the thing an audit asks about.
-- +goose Up
ALTER TABLE claims
  ADD COLUMN returned_quantity integer NOT NULL DEFAULT 0,
  ADD CONSTRAINT claims_returned_quantity_check
    CHECK (returned_quantity >= 0 AND returned_quantity <= quantity);

-- `refund_return` joins the history vocabulary. claim_history is already the
-- organizer-scoped idempotency registry (its UNIQUE (organizer_id, idempotency_key)) and
-- is append-only by trigger, so it is the receipt AND the replay check in one — no
-- separate returns table, which would have duplicated a registry that already exists.
ALTER TABLE claim_history
  DROP CONSTRAINT claim_history_action_check,
  ADD CONSTRAINT claim_history_action_check CHECK (
    action IN ('create','place','release','convert','finalize','confirm','expire',
               'adjust_capacity','reserve','draw_down','refund_return')
  );

-- +goose Down
-- Lock before the guard: checking first leaves a window in which a return commits
-- between the check and the drop, and silently discarding the record of capacity that
-- went back on sale is worse than refusing to roll back.
LOCK TABLE claims, claim_history IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM claims WHERE returned_quantity <> 0)
     OR EXISTS (SELECT 1 FROM claim_history WHERE action='refund_return') THEN
    RAISE EXCEPTION 'cannot roll back 0010: refunds have returned confirmed capacity';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE claim_history
  DROP CONSTRAINT claim_history_action_check,
  ADD CONSTRAINT claim_history_action_check CHECK (
    action IN ('create','place','release','convert','finalize','confirm','expire',
               'adjust_capacity','reserve','draw_down')
  );
ALTER TABLE claims
  DROP CONSTRAINT claims_returned_quantity_check,
  DROP COLUMN returned_quantity;
