-- +goose Up
-- Attribute an order to a customer account (TKT-221 / US-A2, ADR-049 § TKT-221).
--
-- NULL means GUEST, and the column is nullable for that domain reason — not as a
-- migration convenience. Buying without an account is the default this system was
-- built around and stays first-class: the storefront's checkout works signed out,
-- no route redirects to sign-in, and `guest_order_ref` remains how a guest reaches
-- their tickets. A NOT NULL column here would be a statement that every purchase
-- has an account behind it, which is exactly the opposite of TKT-21's first COS.
--
-- Nothing is backfilled. Every order that existed before this migration was made
-- by a guest as far as this system can know, and inventing an account for it would
-- be fabricating a fact.
--
-- The FK is real, not decorative: an attribution naming an account that does not
-- exist is unusable by the wallet TKT-222 builds, and it would fail silently
-- there rather than loudly here.
ALTER TABLE orders ADD COLUMN customer_id uuid REFERENCES customer_accounts (id);

-- No index. The read this column exists for — "every order belonging to customer
-- X" — is TKT-222's, and ADR-019's rule is that an index is justified by the scan
-- it removes. Adding one now would be paying write cost on the checkout hot path
-- for a query nothing issues yet, and TKT-222 has to prove the scan is actually
-- scoped anyway.

-- +goose Down
-- Dropping this discards which purchases belonged to which account. That is
-- recoverable in principle — the orders survive — but not from anything left
-- behind here, so it refuses while any attribution exists rather than silently
-- turning every account's history into guest orders.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orders WHERE customer_id IS NOT NULL) THEN
        RAISE EXCEPTION 'orders are attributed to customer accounts; exporting or clearing them is a deliberate act, not a rollback';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE orders DROP COLUMN customer_id;
