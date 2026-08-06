-- +goose Up
-- Customer accounts (TKT-220 / US-A1, ADR-049).
--
-- Commerce owns orders and buyer_pii, so the buyer principal lives here rather
-- than in catalog, on the gateway, or in a sixth service. That is NOT ADR-042's
-- argument copied across: staff went to catalog because catalog owns organizers
-- and staff administer an organizer. A customer administers nothing.
--
-- buyer_pii is deliberately untouched and unrelated. It is keyed by buyer_id
-- (minted per reservation) and rewritten on every checkout with ON CONFLICT DO
-- UPDATE — an order-time snapshot, not an identity. Making it the account would
-- make the password follow the most recent checkout.
--
-- Nothing here references orders. Attaching an order to an account is TKT-221.
CREATE TABLE customer_accounts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- email keeps the buyer's original spelling for display; email_key is the
    -- normalized (trimmed, lower-cased) form every lookup goes through.
    -- Conflating them is the failure where registration succeeds and the account
    -- can never sign in.
    email         text NOT NULL CHECK (length(trim(email)) > 0),
    -- UNIQUE across the whole table, not per organizer: a customer buys across
    -- organizers, so "which organizer are you signing in to?" has no answer at
    -- the storefront's sign-in form.
    email_key     text NOT NULL UNIQUE CHECK (email_key = lower(trim(email_key))),
    -- A bcrypt modular-crypt string. The CHECK is a tripwire, not security: it
    -- makes a plaintext password written straight into this column fail loudly
    -- instead of becoming a credential that silently never matches.
    password_hash text NOT NULL CHECK (password_hash LIKE '$2%$%'),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Sign-in resolves an account by email_key on every attempt. The UNIQUE
-- constraint above already provides the index; no second one is added, because a
-- redundant index costs writes and buys nothing (ADR-019: an index is justified
-- by the scan it removes, and this scan is already removed).

-- +goose Down
-- Refuse to silently discard credentials: dropping this table locks every
-- customer out with no record of who existed, and the passwords cannot be
-- recovered.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM customer_accounts) THEN
        RAISE EXCEPTION 'customer accounts exist; export or delete them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE IF EXISTS customer_accounts;
