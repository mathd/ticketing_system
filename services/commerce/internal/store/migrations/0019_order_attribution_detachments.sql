-- +goose Up
-- The record of every un-claim (TKT-225 / ADR-052).
--
-- A claim is the only NULL -> customer transition (TKT-223); this table is the
-- record of the only customer -> NULL one. It exists because an un-claim that
-- leaves no trace turns a support tool into a way to move purchases quietly: the
-- operation takes a completed purchase away from the account it belongs to, and
-- the only evidence it ever happened would otherwise be the absence of a value.
--
-- **The adversary, stated plainly (ADR-021).** These rows live in the same
-- database as the attribution they describe, so anyone who can write that database
-- can write, alter or delete them. This is accountability against a CARELESS
-- operator and against an application path that forgets — NOT tamper evidence
-- against a hostile database writer. The access service's lifecycle chain
-- (ADR-021) is the shape that resists the second adversary, and it protects
-- access's own domain, not commerce's. Do not describe this table as
-- tamper-evident.
--
-- `actor` is an operator-supplied LABEL, not an authenticated identity. This
-- slice's guard is the shared internal token (ADR-043), which authenticates the
-- caller as "something holding the service credential" and carries no individual.
-- Whoever adds the back-office surface can derive the actor from the staff
-- session, and at that point the column starts meaning what its name suggests.
CREATE TABLE order_attribution_detachments (
    id           uuid PRIMARY KEY,
    -- The caller's Idempotency-Key, and it is load-bearing rather than hygiene
    -- (ai-review [high]). A detached order is immediately re-claimable by design
    -- (ADR-052 § 4), so a lost response plus an ordinary retry would detach
    -- whoever claimed it in between — a customer the operator never reviewed,
    -- recorded under the reason they gave for a different one. The UNIQUE
    -- constraint is what makes the second attempt a replay instead of a second
    -- destructive act.
    --
    -- Scoped per order, not globally: two different orders may honestly be
    -- detached under the same operator key, and a global unique would refuse the
    -- second for no reason.
    idempotency_key text     NOT NULL,
    order_id     uuid        NOT NULL REFERENCES orders (id),
    -- The account the order was taken FROM. Kept even though the FK target may be
    -- deleted later: the point of the record is who held it at the time.
    customer_id  uuid        NOT NULL REFERENCES customer_accounts (id),
    -- Free text, deliberately. A fixed vocabulary would need to anticipate why a
    -- purchase was mis-claimed, and TKT-147 is already the open ticket for
    -- replacing a free-text reason with codes where that has proven necessary.
    reason       text        NOT NULL,
    actor        text        NOT NULL,
    detached_at  timestamptz NOT NULL DEFAULT now()
);

-- NOT NULL is not enough: '' and '   ' would satisfy it and record nothing. The
-- operation's whole value is answering "who did this, and why", so an empty answer
-- is a failed record rather than a permitted one. Enforced here rather than only
-- in Go so a psql session cannot write a blank one either.
ALTER TABLE order_attribution_detachments
    ADD CONSTRAINT order_attribution_detachments_reason_not_blank
        CHECK (btrim(reason) <> ''),
    ADD CONSTRAINT order_attribution_detachments_actor_not_blank
        CHECK (btrim(actor) <> ''),
    ADD CONSTRAINT order_attribution_detachments_key_not_blank
        CHECK (btrim(idempotency_key) <> '');

-- The replay guard. One key detaches one order once, however many times the
-- request arrives.
CREATE UNIQUE INDEX order_attribution_detachments_key_idx
    ON order_attribution_detachments (order_id, idempotency_key);

-- One order may be detached more than once — a buyer can re-claim after a detach
-- (ADR-052 § re-claim), and support may need to undo that too. So no unique
-- constraint on order_id; the index serves "what happened to this order", which is
-- the question an operator actually asks.
CREATE INDEX order_attribution_detachments_order_idx
    ON order_attribution_detachments (order_id, detached_at DESC);

-- Plain CREATE INDEX. ADR-020 records that CONCURRENTLY is still NOT adopted:
-- ADR-022 satisfied its first precondition, but they are conjunctive and the other
-- two remain false. Do not "improve" this line.

-- +goose Down
-- Refuses to run once evidence exists, mirroring 0012's stance on cancellation
-- history: a down migration that silently discards the only record of who took a
-- purchase away from whom is a data-loss bug wearing a rollback's clothes. An
-- empty table rolls back freely, which is the case that actually occurs — a
-- migration applied and reverted in the same development session.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM order_attribution_detachments) THEN
        RAISE EXCEPTION
            'refusing to drop order_attribution_detachments: % row(s) record who detached which order and why (TKT-225). Export them before rolling back.',
            (SELECT count(*) FROM order_attribution_detachments);
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE IF EXISTS order_attribution_detachments;
