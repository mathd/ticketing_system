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
    --
    -- The CHECK ties the key to ITS OWN email, not merely to a normalized shape.
    -- `email_key = lower(trim(email_key))` — which is what this was first written
    -- as — accepts email='alice@example.test' beside email_key='bob@example.test':
    -- normalized, self-consistent, and a row that displays one buyer while
    -- reserving another's unique key and being unreachable from every application
    -- lookup (ai-review, TKT-220 [medium]).
    --
    -- Two normalizers must now agree — this one and customerEmailKey in
    -- customer.go — for every input the contract admits, or a legitimate
    -- registration is refused by a constraint.
    --
    -- `COLLATE "C"` is what makes that true rather than probable. Postgres's
    -- `lower()` is COLLATION-DEPENDENT and this deployment pins no locale: on a
    -- Turkish-locale database `lower('I')` is a dotless ı where Go's ASCII fold
    -- gives 'i', so a capital I in an address would be refused (ai-review pass 2).
    -- Under the C collation `lower()` touches exactly A-Z, which is precisely what
    -- customerEmailKey does, on every server.
    --
    -- The trim character set is spelled out for the same reason. Bare `btrim`
    -- removes only the ASCII space, while Go's strings.TrimSpace removes Unicode
    -- whitespace — so an address padded with U+2003 EM SPACE, written directly,
    -- could satisfy this CHECK with a key that keeps those bytes while every
    -- application lookup strips them and asks for a different key (ai-review
    -- pass 3). Both sides now trim exactly these six ASCII bytes.
    --
    -- It is a no-op on the application path: RegisterCustomer trims before
    -- inserting. It is kept so a direct writer cannot slip a padded value past
    -- the comparison.
    --
    -- NAMED, because this one references two columns: Postgres makes any such
    -- CHECK a TABLE-level constraint and auto-names it `customer_accounts_check`,
    -- which says nothing about what it enforces and shifts if another table-level
    -- constraint is ever added. A test that has to identify which rule refused a
    -- write needs a name that means something.
    email_key     text NOT NULL UNIQUE
                  CONSTRAINT customer_accounts_email_key_matches_email
                  CHECK (email_key = lower(btrim(email, E' \t\n\r\v\f') COLLATE "C")),
    -- A bcrypt modular-crypt string. The CHECK is a tripwire, not security: it
    -- makes a plaintext password written straight into this column fail loudly
    -- instead of becoming a credential that silently never matches.
    -- Named for the same reason as the one above: a test proving WHICH rule
    -- refused a write should not depend on a name Postgres generated.
    password_hash text NOT NULL
                  CONSTRAINT customer_accounts_password_hash_is_bcrypt
                  CHECK (password_hash LIKE '$2%$%'),
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
