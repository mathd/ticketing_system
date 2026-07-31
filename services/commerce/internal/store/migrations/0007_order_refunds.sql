-- Post-purchase refunds (TKT-156, ADR-037).
--
-- `orders.status` is NOT extended, deliberately. It already contains `refunded`,
-- written by the RECOVERY runner for a compensated FAILED checkout (migration
-- 0005: "captured money whose claim could not be delivered ... Terminal — never
-- claimed again"), and `classifyRecovered` maps it to HTTP 402. A refunded
-- COMPLETED purchase is a different fact: the checkout succeeded. Reusing the
-- token would make a successful, refunded order answer "payment declined" on a
-- checkout replay. So refund state lives on its own dimension and
-- `orders.status` keeps meaning "how the checkout ended".
--
-- Money is BIGINT minor units + ISO currency; floats are banned on money paths.
-- +goose Up
ALTER TABLE orders
  ADD COLUMN refund_status text NOT NULL DEFAULT 'none',
  ADD COLUMN refunded_quantity integer NOT NULL DEFAULT 0,
  ADD COLUMN refunded_amount bigint NOT NULL DEFAULT 0,
  ADD CONSTRAINT orders_refund_status_check CHECK (refund_status IN ('none','partial','full')),
  ADD CONSTRAINT orders_refund_totals_check CHECK (
    refunded_quantity >= 0
    AND refunded_amount >= 0
    AND (
      (refund_status = 'none' AND refunded_quantity = 0 AND refunded_amount = 0)
      OR
      (refund_status IN ('partial','full') AND refunded_quantity > 0)
    )
  );

-- One row per refund attempt. `amount = quantity * unit_amount` is a CHECK rather
-- than a comment: the derivation is the AC, and an implementation that ever
-- divides the order total instead is rejected by the database.
CREATE TABLE order_refunds (
  organizer_id uuid NOT NULL,
  id uuid NOT NULL,
  order_id uuid NOT NULL REFERENCES orders(id),
  idempotency_key text NOT NULL,
  request_fingerprint text NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  unit_amount bigint NOT NULL CHECK (unit_amount > 0),
  amount bigint NOT NULL CHECK (amount > 0),
  currency varchar(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  -- Staff attribution, mirroring the operational-hold conversion path: an operation
  -- that moves money cannot be less attributable than one that moves a seat.
  actor text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
  reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','completed')),
  payment_fact_id uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  PRIMARY KEY (organizer_id, id),
  UNIQUE (organizer_id, idempotency_key),
  CHECK (amount = quantity::bigint * unit_amount),
  CHECK (
    (status = 'pending' AND completed_at IS NULL AND payment_fact_id IS NULL)
    OR
    (status = 'completed' AND completed_at IS NOT NULL AND payment_fact_id IS NOT NULL)
  )
);

-- The cumulative-ceiling sum and the per-order projection both read by order.
CREATE INDEX order_refunds_order_idx ON order_refunds (order_id);

-- +goose Down
-- Lock before the guard: checking first leaves a window in which a refund commits
-- between the check and the DROP, and a "fail closed" guard that silently destroys
-- the record of money returned to a buyer is worse than none.
LOCK TABLE order_refunds, orders IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM order_refunds)
     OR EXISTS (SELECT 1 FROM orders WHERE refund_status <> 'none') THEN
    RAISE EXCEPTION 'cannot roll back 0007: post-purchase refunds exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE order_refunds;
ALTER TABLE orders
  DROP CONSTRAINT orders_refund_totals_check,
  DROP CONSTRAINT orders_refund_status_check,
  DROP COLUMN refunded_amount,
  DROP COLUMN refunded_quantity,
  DROP COLUMN refund_status;
