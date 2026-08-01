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
    -- prevents, and it is enforced here rather than by a read: a unique index is the only
    -- thing that holds under concurrency. Not partial — there is nothing to predicate on,
    -- because an exchange has no cancelled or inactive state (ADR-039 §6, TKT-166).
    -- The BASIS is persisted before any money moves (ai-review F3): target hold, target
    -- total and the signed delta. A retry after a provider call that succeeded and a
    -- later step that failed must settle against the SAME numbers — re-resolving the
    -- price or re-taking the hold on replay can produce a different basis, or fail
    -- outright on an expired claim, leaving a charged buyer with an unsettled exchange.
    target_hold_id uuid,
    replacement_reservation_id uuid,
    -- The FULL priced basis, not just the total (ai-review pass 2). The replacement
    -- reservation is written from these, so a price change between the basis and the
    -- replacement cannot produce a row whose total disagrees with quantity × unit, or a
    -- provenance snapshot describing a different basis from the money that moved.
    target_unit_amount bigint CHECK (target_unit_amount >= 0),
    target_slot_id uuid,
    target_price_snapshot jsonb,
    basis_at timestamptz,
    CONSTRAINT order_exchanges_basis_shape CHECK (
        (basis_at IS NULL AND target_hold_id IS NULL AND replacement_reservation_id IS NULL
         AND target_total IS NULL AND delta_amount IS NULL AND target_unit_amount IS NULL
         AND target_slot_id IS NULL)
        OR
        (basis_at IS NOT NULL AND target_hold_id IS NOT NULL AND replacement_reservation_id IS NOT NULL
         AND target_total IS NOT NULL AND delta_amount IS NOT NULL AND target_unit_amount IS NOT NULL
         AND target_slot_id IS NOT NULL)
    ),
    -- Settlement can only follow a persisted basis, and only names an order.
    CONSTRAINT order_exchanges_settlement_shape CHECK (
        (settled_at IS NULL AND replacement_order_id IS NULL)
        OR
        (settled_at IS NOT NULL AND replacement_order_id IS NOT NULL AND basis_at IS NOT NULL)
    ),
    -- The defining money invariant, enforced by the database rather than trusted from the
    -- application (ai-review F4). Without it a regression or a repair can persist
    -- target 1000 / source 5000 / delta 9000 and the row still claims to be settled.
    CONSTRAINT order_exchanges_delta_is_the_difference CHECK (
        delta_amount IS NULL OR delta_amount = target_total - source_total
    ),
    -- The total is a product, and the basis says so rather than leaving it implied.
    CONSTRAINT order_exchanges_total_is_the_product CHECK (
        target_total IS NULL OR target_total = target_unit_amount * quantity
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
