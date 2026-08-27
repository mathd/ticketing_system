-- Comped-order voids (TKT-171, ADR-040 § TKT-171).
--
-- A comped (zero-price) order had no way to be reversed at all. The whole
-- reversal — ticket voiding and capacity return — hung off a refund, and
-- `BindOrderRefund` refuses `unit <= 0` because no provider issues a zero-amount
-- refund and pretending one happened would fabricate a money fact (ADR-003). So a
-- cancelled event's comped orders kept admitting and kept their seats.
--
-- WHY THIS IS A SEPARATE TABLE AND NOT A ZERO-AMOUNT `order_refunds` ROW.
-- Not merely a shape preference: `order_refunds` enforces `unit_amount > 0` and
-- `amount > 0` as CHECK constraints (migration 0007). A void cannot be recorded
-- there without relaxing a money constraint, which is exactly the shortcut the
-- owner rejected when deciding this ticket. The database already says a refund is
-- a money fact; this table says a void is not one.
--
-- So there is deliberately NO unit_amount, NO amount, NO currency, and NO
-- payment_fact_id here. A void moves tickets and capacity and never money — that
-- invariant is enforced by the absence of the columns that could record one,
-- rather than by a convention someone has to remember.
--
-- The two progress markers mirror `order_refunds`' reversal columns (ADR-062):
-- each obligation is discharged and recorded independently, so a downstream
-- failure leaves a visible outstanding obligation that a replay resumes rather
-- than a half-reversal nobody can find.

-- +goose Up
CREATE TABLE order_voids (
  organizer_id uuid NOT NULL,
  id uuid NOT NULL,
  order_id uuid NOT NULL REFERENCES orders(id),
  -- Derived from (organizer, order), not from the request key — see store.VoidID.
  -- A staff retry, a cancellation-run retry and a process restart must converge on
  -- ONE downstream operation, and the request key is different in each of those.
  request_fingerprint text NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  -- Staff attribution, as on order_refunds: an operation that stops a ticket
  -- admitting cannot be less attributable than one that moves money.
  actor text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
  reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
  tickets_voided_at timestamptz,
  capacity_returned_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organizer_id, id),
  -- One void per order. A comped reversal is whole-order by construction (quantity
  -- comes from the reservation, never from the client), so a second row for the
  -- same order could only ever be a double reversal.
  UNIQUE (organizer_id, order_id),
  -- ADR-038 §1 IN THE SCHEMA: capacity may not be recorded as returned unless the
  -- tickets are already recorded as void. Freeing the seat while the original
  -- ticket still admits is the one sequence that can OVERSELL; voiding first can
  -- only under-sell. The driver enforces the order, and this makes a driver that
  -- stops enforcing it fail loudly here instead of silently overselling.
  CHECK (capacity_returned_at IS NULL OR tickets_voided_at IS NOT NULL)
);

-- The cancellation runner and the staff path both look a void up by its order.
CREATE INDEX order_voids_order_idx ON order_voids (order_id);

-- +goose Down
-- Lock before the guard, as 0007 does: checking first leaves a window in which a
-- void commits between the check and the DROP, and destroying the record of a
-- reversal that really happened is worse than refusing to roll back.
LOCK TABLE order_voids IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM order_voids) THEN
    RAISE EXCEPTION 'cannot roll back 0025: comped-order voids exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE order_voids;
