-- +goose Up
-- Password recovery and the mail outbox (TKT-226 / ADR-050).
--
-- Two tables in one migration because neither is useful alone: a reset token nobody
-- can be told about is not a recovery path, and a mail outbox with nothing to send is
-- dead code. They are also written in ONE transaction by the request path, so a
-- deployment that had one and not the other could not serve the operation at all.

-- The recovery credential.
--
-- WHY THE COLUMN IS A SHA-256 AND NOT A BCRYPT HASH, stated because the file six
-- inches away (0015) stores a bcrypt password and the analogy is the wrong one:
--
--   * A bcrypt hash carries its own salt, so it can only be VERIFIED against a
--     candidate, never LOOKED UP. Finding this row by bcrypt would mean comparing the
--     presented token against every row in the table at cost 10 — a full table scan of
--     KDF operations, on an unauthenticated public endpoint.
--   * A work factor buys nothing here anyway. A password is low-entropy and guessable,
--     which is what a slow KDF defends. This token is 32 bytes from crypto/rand: there
--     is no dictionary, so there is nothing to slow down.
--
-- What the hash DOES buy is the only thing claimed for it: the database never holds a
-- usable token, so a reader of this table (a backup, a replica, an operator's SELECT)
-- cannot mint a reset. Name the adversary (ADR-021): this stops a READER. It stops
-- nothing against a WRITER, who can insert a row whose hash they chose. That adversary
-- already owns customer_accounts and does not need this table.
CREATE TABLE password_reset_tokens (
    -- SHA-256 of the raw token, hex. PRIMARY KEY, so lookup is the index and a
    -- collision is impossible rather than merely unlikely.
    token_hash  text PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    customer_id uuid NOT NULL REFERENCES customer_accounts(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Absolute expiry, stamped by the issuer. Not an interval computed at read time:
    -- shortening the TTL constant must not retroactively extend tokens already mailed,
    -- and lengthening it must not resurrect expired ones.
    expires_at  timestamptz NOT NULL,
    -- NULL until redeemed. Single-use is enforced by the conditional UPDATE's
    -- `used_at IS NULL` predicate, not by deleting the row: a deleted row cannot tell
    -- an operator that a reset happened, and "no such token" and "already used" would
    -- become indistinguishable in the trail as well as in the response.
    used_at     timestamptz
);

-- The claimable set: unused, unexpired, newest first. Partial, because a redeemed or
-- expired token is never selected again and would only bloat the index.
--
-- This index exists for the ISSUING path, which invalidates a customer's outstanding
-- tokens before minting a new one (ADR-019: an index is justified by the scan it
-- removes, and that is this scan). The redemption path looks up by primary key.
CREATE INDEX password_reset_tokens_live_idx
    ON password_reset_tokens (customer_id)
    WHERE used_at IS NULL;

-- The durable mail queue.
--
-- Shape and protocol are deliberately the SAME as completion_outbox (0003): claim with
-- a lease and a claim id, exponential backoff on failure, dead-letter after bounded
-- attempts, partial index on the claimable set. That table is proven and its drainer
-- has survived three review passes; copying it is cheaper and safer than inventing a
-- second queue discipline.
--
-- It is a SEPARATE TABLE rather than a row type on completion_outbox because that
-- table's columns are order-event-specific (order_id NOT NULL REFERENCES orders,
-- subject, a frozen ADR-009 envelope) and none of them mean anything for a message.
--
-- WHY A QUEUE AT ALL, given the only sender in this repo is a fake that cannot fail —
-- this is the paragraph that stops someone deleting it later:
--
--   Inline sending cannot satisfy the enumeration-parity criterion. An unknown address
--   does no send and therefore cannot fail; a known address can. So inline-and-honest
--   makes a send failure an account-existence oracle, and inline-and-silent is the
--   "the reset never arrived and nobody was told" outcome. Enqueueing is the only shape
--   where both paths return the same answer, because NEITHER has attempted delivery
--   when the response is written.
CREATE TABLE mail_outbox (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The recipient and the composed message. This table holds PII and a
    -- reset link in plaintext, which is inherent: something has to hold the message
    -- until it is sent. Retention and deletion are TKT-33 and are NOT solved here.
    recipient    text NOT NULL CHECK (length(trim(recipient)) > 0),
    subject      text NOT NULL CHECK (length(trim(subject)) > 0),
    body         text NOT NULL CHECK (length(body) > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- NULL until the sender returned success. The unsent set is exactly
    -- `sent_at IS NULL` (ack before marking, ADR-016 §Decision 6).
    sent_at      timestamptz,
    -- Identifies the current claimant. Release and retirement are conditional on it, so
    -- a claimant whose lease expired mid-send cannot clear or retire the lease of the
    -- drainer that superseded it.
    claim_id     uuid,
    lease_until  timestamptz,
    attempts     integer NOT NULL DEFAULT 0,
    -- Backoff gate. Claiming is oldest-first, so without this a few permanently-failing
    -- rows at the head starve every newer message forever.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error   text,
    -- Terminal quarantine. An exhausted row stops being claimed at all, so one poison
    -- message cannot block the queue. It stays visible to an operator via last_error.
    dead_lettered_at timestamptz
);

CREATE INDEX mail_outbox_claimable_idx
    ON mail_outbox (next_attempt_at)
    WHERE sent_at IS NULL AND dead_lettered_at IS NULL;

-- +goose Down
-- Unlike 0015, this drops without complaint. Nothing here is unrecoverable: a reset
-- token is a short-lived credential a buyer can mint again, and an undrained message
-- is one nobody received — losing it costs a re-request, not an account. The rule 0015
-- protects (credentials cannot be reconstructed) does not apply to either table.
DROP TABLE IF EXISTS mail_outbox;
DROP TABLE IF EXISTS password_reset_tokens;
