-- Abandoning a settled exchange after its source was admitted (TKT-498, ADR-039 §2).
--
-- Access refuses the ticket switch of an exchange whose source tickets were already admitted.
-- The exchange stays settled and unswitched for good: the delta moved, the replacement order
-- exists, and the target capacity stays claimed. The owner decision of 2026-10-12 abandons it:
-- refund an upgrade delta, write off a downgrade delta, return the target capacity, and never
-- issue the replacement. This migration adds the durable record of that operation to the
-- exchange row. The exchange, both orders and the original gross facts stay as history.
--
-- PROGRESS IS NULLABLE COLUMNS, the shape 0010 and 0011 use for the settlement, the switch and
-- the capacity return:
--
--   abandonment_started_at, _operator_name, _session_user, _reason
--       The reservation. The command writes all four in one statement on a settled, unswitched
--       row. The operator name and the reason are the typed, trimmed attestation; the session
--       user is PostgreSQL `session_user` on the writing connection, never an argument.
--   abandonment_refund_fact_id     Upgrade: the payments fact of the delta refund.
--   abandonment_refund_failed_ref  Upgrade: the provider's `re_` reference of a terminal refund
--                                  failure. The operation stops there (TKT-588 owns the rest).
--   abandonment_target_returned_at The target hold's capacity came back in inventory.
--   abandonment_written_off_amount Downgrade: the delta the platform writes off, -delta_amount.
--   abandoned_at                   Terminal. Set with the write-off and the compensating
--                                  `order.exchange.abandoned` fact in one transaction.
--
-- A started row is a tombstone for the replacement only. It stays in the one-exchange-per-source
-- unique index and in BindOrderRefund's exchange count on purpose: the source was admitted and
-- must not be refunded or exchanged again. Only the reversal backlog read adds
-- `abandoned_at IS NULL`.
--
-- WRITE-ONCE. The command fills an empty field or reads back the identical value. The trigger
-- below refuses any change or clearing of a populated field. Like 0027's triggers it protects
-- against application defects only (ADR-021): a writer with Commerce database access can disable
-- it, and a coordinated restore can roll the rows back.
--
-- No CHECK reads `capacity_returned_at`. 0011's order_exchanges_capacity_after_switch makes a
-- returned row a switched one, so `tickets_exchanged_at IS NULL` already excludes it.
--
-- Money is bigint minor units; floats are banned.

-- +goose Up
-- One table, locked first. The abandonment reservation locks the source order, the replacement
-- order and then the exchange; this migration holds no order lock, so it cannot form a cycle
-- with it.
LOCK TABLE order_exchanges IN ACCESS EXCLUSIVE MODE;

ALTER TABLE order_exchanges
  ADD COLUMN abandonment_started_at timestamptz,
  ADD COLUMN abandonment_operator_name text,
  ADD COLUMN abandonment_session_user text,
  ADD COLUMN abandonment_reason text,
  ADD COLUMN abandonment_refund_fact_id uuid,
  ADD COLUMN abandonment_refund_failed_ref text,
  ADD COLUMN abandonment_target_returned_at timestamptz,
  ADD COLUMN abandonment_written_off_amount bigint,
  ADD COLUMN abandoned_at timestamptz,
  -- Every CHECK is written with explicit NULL tests. A CHECK accepts a NULL result, so a
  -- comparison against a NULL column would otherwise pass the row it exists to refuse.
  --
  -- The reservation's four fields travel together.
  ADD CONSTRAINT order_exchanges_abandonment_start_together CHECK (
    (abandonment_started_at IS NULL) = (abandonment_operator_name IS NULL)
    AND (abandonment_started_at IS NULL) = (abandonment_session_user IS NULL)
    AND (abandonment_started_at IS NULL) = (abandonment_reason IS NULL)
  ),
  -- Only a settled, unswitched exchange can be abandoned, and a started one cannot be switched
  -- afterwards: the tickets-switched callback would otherwise drive a source capacity return for
  -- an exchange whose target capacity is coming back too.
  ADD CONSTRAINT order_exchanges_abandonment_start_on_unswitched_settlement CHECK (
    abandonment_started_at IS NULL
    OR (settled_at IS NOT NULL AND tickets_exchanged_at IS NULL)
  ),
  ADD CONSTRAINT order_exchanges_abandonment_operator_name_check CHECK (
    abandonment_operator_name IS NULL
    OR (length(abandonment_operator_name) BETWEEN 1 AND 200
        AND abandonment_operator_name !~ '^[[:space:]]' AND abandonment_operator_name !~ '[[:space:]]$')
  ),
  ADD CONSTRAINT order_exchanges_abandonment_reason_check CHECK (
    abandonment_reason IS NULL
    OR (length(abandonment_reason) BETWEEN 1 AND 500
        AND abandonment_reason !~ '^[[:space:]]' AND abandonment_reason !~ '[[:space:]]$')
  ),
  ADD CONSTRAINT order_exchanges_abandonment_session_user_check CHECK (
    abandonment_session_user IS NULL OR length(abandonment_session_user) BETWEEN 1 AND 200
  ),
  -- Refund evidence exists only on a started upgrade, and it is either the payments fact of the
  -- refund or the provider's failure reference, never both.
  ADD CONSTRAINT order_exchanges_abandonment_refund_evidence CHECK (
    (abandonment_refund_fact_id IS NULL AND abandonment_refund_failed_ref IS NULL)
    OR (abandonment_started_at IS NOT NULL AND delta_amount IS NOT NULL AND delta_amount > 0
        AND (abandonment_refund_fact_id IS NULL) <> (abandonment_refund_failed_ref IS NULL))
  ),
  -- The reference payments returns for a failed provider refund: `re_…`, at most 255 bytes, the
  -- limit payments accepts (0027 uses the same bound).
  ADD CONSTRAINT order_exchanges_abandonment_refund_failed_ref_check CHECK (
    abandonment_refund_failed_ref IS NULL
    OR (left(abandonment_refund_failed_ref, 3) = 're_'
        AND length(abandonment_refund_failed_ref) BETWEEN 4 AND 255)
  ),
  -- The target capacity comes back only after a start, and on an upgrade only after the refund
  -- completed. That also refuses it after a provider failure: the refund evidence CHECK above
  -- allows a failure only on an upgrade and never beside a refund fact, so a separate
  -- failure predicate here could not change the result.
  ADD CONSTRAINT order_exchanges_abandonment_target_return_shape CHECK (
    abandonment_target_returned_at IS NULL
    OR (abandonment_started_at IS NOT NULL AND delta_amount IS NOT NULL
        AND (delta_amount <= 0 OR abandonment_refund_fact_id IS NOT NULL))
  ),
  ADD CONSTRAINT order_exchanges_abandonment_terminal_shape CHECK (
    abandoned_at IS NULL OR abandonment_target_returned_at IS NOT NULL
  ),
  -- The write-off exists exactly on a terminal downgrade, and it is the whole delta.
  ADD CONSTRAINT order_exchanges_abandonment_write_off_shape CHECK (
    (abandonment_written_off_amount IS NULL
      AND (abandoned_at IS NULL OR (delta_amount IS NOT NULL AND delta_amount >= 0)))
    OR
    (abandonment_written_off_amount IS NOT NULL AND abandoned_at IS NOT NULL
      AND delta_amount IS NOT NULL AND delta_amount < 0
      AND abandonment_written_off_amount = -delta_amount)
  );

-- +goose StatementBegin
CREATE FUNCTION order_exchanges_abandonment_write_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.abandonment_started_at IS NOT NULL AND NEW.abandonment_started_at IS DISTINCT FROM OLD.abandonment_started_at)
     OR (OLD.abandonment_operator_name IS NOT NULL AND NEW.abandonment_operator_name IS DISTINCT FROM OLD.abandonment_operator_name)
     OR (OLD.abandonment_session_user IS NOT NULL AND NEW.abandonment_session_user IS DISTINCT FROM OLD.abandonment_session_user)
     OR (OLD.abandonment_reason IS NOT NULL AND NEW.abandonment_reason IS DISTINCT FROM OLD.abandonment_reason)
     OR (OLD.abandonment_refund_fact_id IS NOT NULL AND NEW.abandonment_refund_fact_id IS DISTINCT FROM OLD.abandonment_refund_fact_id)
     OR (OLD.abandonment_refund_failed_ref IS NOT NULL AND NEW.abandonment_refund_failed_ref IS DISTINCT FROM OLD.abandonment_refund_failed_ref)
     OR (OLD.abandonment_target_returned_at IS NOT NULL AND NEW.abandonment_target_returned_at IS DISTINCT FROM OLD.abandonment_target_returned_at)
     OR (OLD.abandonment_written_off_amount IS NOT NULL AND NEW.abandonment_written_off_amount IS DISTINCT FROM OLD.abandonment_written_off_amount)
     OR (OLD.abandoned_at IS NOT NULL AND NEW.abandoned_at IS DISTINCT FROM OLD.abandoned_at) THEN
    RAISE EXCEPTION 'exchange abandonment fields are write-once';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- UPDATE OF the nine columns: a change or a clearing names its column in SET, and the reversal
-- sweep's frequent lease writes on other columns do not run the function.
CREATE TRIGGER order_exchanges_abandonment_write_once
BEFORE UPDATE OF abandonment_started_at, abandonment_operator_name, abandonment_session_user,
                 abandonment_reason, abandonment_refund_fact_id, abandonment_refund_failed_ref,
                 abandonment_target_returned_at, abandonment_written_off_amount, abandoned_at
ON order_exchanges FOR EACH ROW EXECUTE FUNCTION order_exchanges_abandonment_write_once();

-- The refund binder finds the exchange of a replacement order by its reservation, which is
-- persisted with the basis, before settlement publishes replacement_order_id. Plain, not
-- unique: the basis rules already make it one per exchange.
CREATE INDEX order_exchanges_replacement_reservation_idx
  ON order_exchanges (organizer_id, replacement_reservation_id)
  WHERE replacement_reservation_id IS NOT NULL;

-- +goose Down
-- Lock BEFORE the history check. Checking first leaves a window in which a first reservation
-- commits between the check and the DROP, and the DROP would then erase the only local record
-- that the target capacity or the delta refund may already be in motion.
LOCK TABLE order_exchanges IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM order_exchanges WHERE abandonment_started_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back 0028: exchange abandonment history exists (pending, failed or terminal); roll forward instead';
  END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER order_exchanges_abandonment_write_once ON order_exchanges;
DROP FUNCTION order_exchanges_abandonment_write_once();
DROP INDEX order_exchanges_replacement_reservation_idx;
ALTER TABLE order_exchanges
  DROP CONSTRAINT order_exchanges_abandonment_write_off_shape,
  DROP CONSTRAINT order_exchanges_abandonment_terminal_shape,
  DROP CONSTRAINT order_exchanges_abandonment_target_return_shape,
  DROP CONSTRAINT order_exchanges_abandonment_refund_failed_ref_check,
  DROP CONSTRAINT order_exchanges_abandonment_refund_evidence,
  DROP CONSTRAINT order_exchanges_abandonment_session_user_check,
  DROP CONSTRAINT order_exchanges_abandonment_reason_check,
  DROP CONSTRAINT order_exchanges_abandonment_operator_name_check,
  DROP CONSTRAINT order_exchanges_abandonment_start_on_unswitched_settlement,
  DROP CONSTRAINT order_exchanges_abandonment_start_together,
  DROP COLUMN abandoned_at,
  DROP COLUMN abandonment_written_off_amount,
  DROP COLUMN abandonment_target_returned_at,
  DROP COLUMN abandonment_refund_failed_ref,
  DROP COLUMN abandonment_refund_fact_id,
  DROP COLUMN abandonment_reason,
  DROP COLUMN abandonment_session_user,
  DROP COLUMN abandonment_operator_name,
  DROP COLUMN abandonment_started_at;
