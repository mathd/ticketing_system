-- +goose Up
-- Back-office staff accounts (TKT-190 / US-B1, ADR-042).
--
-- Catalog owns organizers and tenant-scoped configuration (ADR-002), so the humans
-- who administer an organizer live here rather than in a sixth service or on the
-- gateway. This table holds credentials only; sessions live in the back-office
-- process and are deliberately not persisted (ADR-042 -- a restart signs everyone
-- out, which is correct for a single-replica Compose staff tool and leaves no schema
-- to migrate away from when it stops being).
--
-- role is stored but NOT interpreted anywhere yet: TKT-191 defines the role matrix
-- and enforces it. A CHECK here would pin the vocabulary before the ticket that owns
-- it has chosen one, so the column only refuses emptiness.
--
-- identifier_key is UNIQUE across the whole table, not per organizer. Sign-in is by
-- identifier alone, so two organizers holding "admin@example.com" would make "who is
-- signing in?" ambiguous. v1 has a single organizer (migration 0002); multi-organizer
-- sign-in needs an organizer selector on the login form and is out of scope here.
CREATE TABLE staff_accounts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id   uuid NOT NULL REFERENCES organizers (id),
    -- identifier keeps the operator's original spelling for display; identifier_key
    -- is the normalized (trimmed, lower-cased) form every lookup goes through.
    identifier     text NOT NULL CHECK (length(trim(identifier)) > 0),
    identifier_key text NOT NULL UNIQUE CHECK (identifier_key = lower(trim(identifier_key))),
    role           text NOT NULL CHECK (length(trim(role)) > 0),
    -- A bcrypt modular-crypt string. The CHECK is a tripwire, not security: it makes
    -- a plaintext password written straight into this column fail loudly instead of
    -- becoming a credential that silently never matches.
    password_hash  text NOT NULL CHECK (password_hash LIKE '$2%$%'),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Sign-in resolves an account by identifier_key on every request that establishes a
-- session. The UNIQUE constraint above already provides the index; no second one is
-- added, because a redundant index costs writes and buys nothing (ADR-019: an index
-- is justified by the scan it removes, and this scan is already removed).

-- +goose Down
-- Refuse to silently discard credentials: dropping this table locks every staff
-- member out with no record of who existed, and the passwords cannot be recovered.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM staff_accounts) THEN
        RAISE EXCEPTION 'staff accounts exist; export or delete them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE IF EXISTS staff_accounts;
