-- Exchanges (TKT-158, ADR-039).
--
-- An exchange is a reversal AND a sale, and that is why it is not an order_refunds row
-- plus an ordinary checkout: composing those would refund the old gross amount and
-- capture the new gross amount — TWO provider movements — while an exchange makes
-- exactly ONE net movement for the difference. What the provider does and what the
-- trail records are different questions, and both gross legs are still journalled.
--
-- Progress is nullable timestamps, the same shape ADR-038 §6 settled on for refunds:
-- `settled_at` (this ticket) and `tickets_exchanged_at` (TKT-166). Independent storage,
-- SAFETY-ORDERED execution — the entitlement switch happens after settlement, and the
-- old capacity is not returned until the old tickets stop admitting, which is the one
-- ordering that cannot oversell.
--
-- Money is integer minor units; floats are banned. delta_amount is SIGNED: positive is
-- an upgrade the buyer pays, negative a downgrade refunded to them, zero settles nothing.
-- +goose Up
CREATE TABLE order_exchanges (
    organizer_id uuid NOT NULL,
    id uuid NOT NULL,
    source_order_id uuid NOT NULL REFERENCES orders(id),
    -- The replacement line. NULL until settlement confirms it; a target ticket type is
    -- known at bind time, a target ORDER only once one exists.
    replacement_order_id uuid REFERENCES orders(id),
    target_ticket_type_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint text NOT NULL,
    quantity integer NOT NULL CHECK (quantity > 0),
    source_total bigint NOT NULL CHECK (source_total >= 0),
    target_total bigint CHECK (target_total >= 0),
    delta_amount bigint,
    currency varchar(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- Staff attribution, as on order_refunds: an operation that moves money cannot be
    -- less attributable than one that moves a seat.
    actor text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    created_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz,
    tickets_exchanged_at timestamptz,
    PRIMARY KEY (organizer_id, id),
    UNIQUE (organizer_id, idempotency_key),
    -- One live exchange per source order. An order reversed twice is the failure this
    -- prevents, and it is enforced here rather than by a read: a partial unique index is
    -- the only thing that holds under concurrency.
    CONSTRAINT order_exchanges_settlement_shape CHECK (
        (settled_at IS NULL AND replacement_order_id IS NULL AND target_total IS NULL AND delta_amount IS NULL)
        OR
        (settled_at IS NOT NULL AND replacement_order_id IS NOT NULL AND target_total IS NOT NULL AND delta_amount IS NOT NULL)
    ),
    -- The entitlement cannot switch before the money settles (TKT-166 sets the second).
    CONSTRAINT order_exchanges_switch_after_settlement CHECK (
        tickets_exchanged_at IS NULL OR settled_at IS NOT NULL
    )
);
CREATE UNIQUE INDEX order_exchanges_one_per_source ON order_exchanges (source_order_id);

-- +goose Down
LOCK TABLE order_exchanges IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM order_exchanges) THEN
        RAISE EXCEPTION 'cannot roll back 0010: exchanges exist';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE order_exchanges;
