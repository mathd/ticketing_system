-- TKT-156 (ADR-037): durable identity for POST-PURCHASE partial refund legs.
--
-- payment_compensations is not widened, deliberately. Its primary key
-- (organizer, source operation, kind) encodes "the ONE whole compensation for a
-- failed checkout", and /internal/psp/status reads a completed refund
-- compensation as "this operation now holds NO money" (zeroed amounts). Under
-- partial refunds that answer would be false, and the recovery runner depends on
-- it. Two tables, two meanings.
--
-- Money is BIGINT minor units + ISO currency; floats are banned on money paths.
-- +goose Up
CREATE TABLE payment_refund_legs (
  organizer_id uuid NOT NULL,
  -- The CHARGE's idempotency key: the payment_operations row this leg refunds.
  source_idempotency_key text NOT NULL,
  -- The refund's own key, minted by the caller. Two legs against one charge differ
  -- only here, and that is what makes them two provider operations.
  refund_idempotency_key text NOT NULL,
  -- Deterministic from the three columns above (store.RefundLegKey). A crashed leg
  -- that re-binds re-derives the SAME key, so its retry lands on the provider's
  -- idempotency layer instead of issuing a second refund.
  provider_idempotency_key text NOT NULL,
  amount bigint NOT NULL CHECK (amount > 0),
  currency varchar(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  status text NOT NULL DEFAULT 'bound' CHECK (status IN ('bound','refunded')),
  provider_ref text,
  fact_id uuid UNIQUE,
  bound_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  PRIMARY KEY (organizer_id, source_idempotency_key, refund_idempotency_key),
  UNIQUE (organizer_id, provider_idempotency_key),
  -- A completed leg must carry its journalled fact. Without this, "completed" could
  -- mean money moved with nothing in the trail saying so.
  CHECK (
    (status = 'bound' AND completed_at IS NULL AND fact_id IS NULL)
    OR
    (status = 'refunded' AND completed_at IS NOT NULL AND fact_id IS NOT NULL)
  )
);

-- No second index: the primary-key prefix (organizer, source key) is exactly the
-- lookup the cumulative-ceiling sum performs.

-- +goose Down
-- Lock before the guard. Checking first leaves a window in which a leg binds
-- between the check and the DROP, and a "fail closed" guard that silently
-- destroys evidence of money that left the account is worse than none.
LOCK TABLE payment_refund_legs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM payment_refund_legs) THEN
    RAISE EXCEPTION 'cannot roll back 0003: partial refund legs exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE payment_refund_legs;
