-- +goose Up
-- Partner (reseller) API credentials — the platform's THIRD machine identity
-- class (TKT-240 / ADR-056), beside staff sessions (ADR-042), the shared internal
-- service token (ADR-043) and customer sessions (ADR-049).
--
-- Shape follows access's scanner_devices (migration 0009 there) rather than
-- catalog's staff bcrypt, and the difference is the point: a scanner device and a
-- reseller both present a MACHINE credential on EVERY request, where a staff
-- password is checked once per session. bcrypt is deliberately slow, and a
-- deliberately slow hash on the on-sale hot path is a self-inflicted denial of
-- service. It is also unlookupable: a bcrypt hash salts per row, so authenticating
-- would mean scanning every credential. A SHA-256 over a 32-byte random token is
-- one indexed lookup, and the entropy — not a KDF's cost — is what makes it
-- unguessable. That reasoning holds ONLY because the secret is generated, never
-- chosen by a human; do not reuse this shape for anything a person types.
--
-- **The adversary, stated plainly (ADR-021).** Storing the hash protects the
-- credential against disclosure AT REST — a database dump, a slow-query log, a
-- backup — and makes revocation immediate. It does NOT protect against replay of a
-- live stolen token (TLS, idempotency keys and rate limiting carry that), and it
-- does NOT protect against an adversary who can WRITE this database: such a writer
-- can insert their own credential row or clear revoked_at at will. This table is
-- honest-writer security, not tamper evidence. Do not describe it otherwise.
CREATE TABLE reseller_credentials (
    id           uuid PRIMARY KEY,
    -- The reseller's identity, carried onto every claim and order it creates so
    -- settlement (ADR-048) can split by it. Deliberately NOT a foreign key to a
    -- resellers table: no such entity exists yet, and inventing one here would put
    -- a second definition of "who is a partner" beside catalog's channel registry.
    reseller_id  uuid NOT NULL,
    -- The credential's scope. BOTH halves are authority: they are read FROM this
    -- row and compared against whatever the request claims, never taken from the
    -- request. ADR-053 records what the other arrangement costs — catalog's staff
    -- credential can enumerate and mutate across tenants precisely because the
    -- organizer arrives in the request body.
    organizer_id uuid NOT NULL,
    -- Exact, unnormalized, like every other channel code in the platform (ADR-024).
    -- No FK: the registry lives in catalog's database and ADR-024 forbids an FK
    -- from historical attribution to it. Issuance validates registry membership;
    -- the column records what was issued.
    channel_code text NOT NULL CHECK (length(channel_code) BETWEEN 1 AND 100),
    -- SHA-256 hex of the presented token. UNIQUE because it is the authentication
    -- lookup key: the credential id is an administrative handle for revocation and
    -- never travels in a request, so there is one fewer caller-supplied input to
    -- get wrong.
    token_hash   text NOT NULL UNIQUE,
    label        text NOT NULL CHECK (length(btrim(label)) > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);

-- One live credential per (organizer, channel, reseller). Rotation is enrol-then-
-- revoke, so the partial index counts only live rows: a revoked credential must not
-- block issuing its replacement.
CREATE UNIQUE INDEX reseller_credentials_one_live
    ON reseller_credentials (organizer_id, channel_code, reseller_id)
    WHERE revoked_at IS NULL;

-- Per-reseller attribution on the sale (TKT-240). Both columns are NULLABLE and
-- not backfilled: NULL is "not a partner sale", which is exactly what every
-- pre-existing reservation and order was.
--
-- No foreign key to reseller_credentials. Attribution is HISTORICAL -- who sold
-- this, at the time -- and an FK would let revoking or rotating a credential
-- rewrite or block the record of past sales. Same reasoning ADR-024 applies to
-- channel codes on claims, and the same reasoning behind the channel_code column
-- these sit beside.
--
-- reseller_id, not credential_id: a partner that rotates its credential after a
-- leak is the same partner, and settlement (ADR-048) splits by the partner.
ALTER TABLE reservations
    ADD COLUMN reseller_id uuid,
    -- A reseller sale is a CHANNELLED sale by construction: the credential is
    -- issued for one channel and the handler takes the channel from it. Without
    -- this, a row could claim a reseller with no channel, and settlement would
    -- have a sale it cannot attribute to an allocation.
    ADD CONSTRAINT reservations_reseller_implies_channel
        CHECK (reseller_id IS NULL OR channel_code IS NOT NULL);

ALTER TABLE orders
    ADD COLUMN channel_code text
        CHECK (channel_code IS NULL OR length(channel_code) BETWEEN 1 AND 100),
    ADD COLUMN reseller_id uuid,
    ADD CONSTRAINT orders_reseller_implies_channel
        CHECK (reseller_id IS NULL OR channel_code IS NOT NULL);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reseller_credentials) THEN
        RAISE EXCEPTION 'refusing to drop reseller_credentials: % row(s) exist. '
            'Dropping them would silently revoke every partner''s access and destroy '
            'the record of which reseller sold what. Revoke deliberately instead.',
            (SELECT count(*) FROM reseller_credentials);
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE reseller_credentials;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orders WHERE reseller_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM reservations WHERE reseller_id IS NOT NULL) THEN
        RAISE EXCEPTION 'refusing to drop reseller attribution: partner sales exist. '
            'Dropping these columns would destroy the record of which reseller sold '
            'what, which settlement splits by (ADR-048).';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE orders DROP CONSTRAINT orders_reseller_implies_channel,
    DROP COLUMN reseller_id, DROP COLUMN channel_code;
ALTER TABLE reservations DROP CONSTRAINT reservations_reseller_implies_channel,
    DROP COLUMN reseller_id;
