-- +goose Up
-- TKT-114 (S2, ADR-032): durable provider evidence on payment_operations, and the
-- compensation table. Provider identifiers (pi_/ch_/re_) are mutable operational
-- evidence and live here, never in the journal payload. Money stays BIGINT minor
-- units + ISO currency text; floats are banned on money paths.
ALTER TABLE payment_operations
  ADD COLUMN order_id uuid,
  ADD COLUMN buyer_id uuid,
  ADD COLUMN request_amount bigint CHECK (request_amount >= 0),
  ADD COLUMN request_currency varchar(3) CHECK (request_currency ~ '^[A-Z]{3}$'),
  -- Opaque provider payment-method reference (Stripe pm_…), kept ONLY so Status can
  -- replay the original create under the same idempotency key after a crash. Sensitive
  -- operational data: never in journal facts, endpoint responses, or logs.
  ADD COLUMN payment_method_ref text,
  ADD COLUMN provider_payment_ref text,
  ADD COLUMN provider_charge_ref text,
  -- Normalized provider state ('authorized','captured','declined','timeout','voided'),
  -- the durable evidence compensation decisions read (void needs authorized-uncaptured,
  -- refund needs captured money).
  ADD COLUMN provider_state text,
  ADD COLUMN authorized_amount bigint CHECK (authorized_amount >= 0),
  ADD COLUMN captured_amount bigint CHECK (captured_amount >= 0),
  ADD COLUMN provider_state_at timestamptz;

-- One compensation row per (organizer, source operation, kind): the primary key is what
-- makes a duplicate/concurrent void or refund converge on ONE deterministic provider
-- idempotency key instead of issuing a second provider operation.
CREATE TABLE payment_compensations (
  organizer_id uuid NOT NULL,
  source_idempotency_key text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('void','refund')),
  provider_idempotency_key text NOT NULL,
  status text,
  provider_ref text,
  fact_id uuid,
  amount bigint CHECK (amount >= 0),
  currency varchar(3) CHECK (currency ~ '^[A-Z]{3}$'),
  bound_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  PRIMARY KEY (organizer_id, source_idempotency_key, kind)
);

-- +goose Down
DROP TABLE payment_compensations;
ALTER TABLE payment_operations
  DROP COLUMN provider_state_at,
  DROP COLUMN captured_amount,
  DROP COLUMN authorized_amount,
  DROP COLUMN provider_state,
  DROP COLUMN provider_charge_ref,
  DROP COLUMN provider_payment_ref,
  DROP COLUMN payment_method_ref,
  DROP COLUMN request_currency,
  DROP COLUMN request_amount,
  DROP COLUMN buyer_id,
  DROP COLUMN order_id;
