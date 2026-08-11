-- Enrolled scanner devices (ai-review S1).
--
-- POST /scans and POST /scans/reconciliations accepted an admission decision from
-- anyone who could reach the gateway. Combined with a captured QR that is a burned
-- ticket at someone else's door; reconciliations rewrite a night's scan history in
-- bulk. The scanner README named the gap and deferred it; the deferral was the
-- finding.
--
-- Why a device table and not a shared token. The scanner is a STATIC SPA served by
-- nginx, so a credential compiled into it ships in a public JavaScript bundle to
-- every phone that loads /scanner/ — a doormat key. Per-device credentials are also
-- what an operator actually needs: when a gate phone is lost mid-event the answer
-- has to be revoking that phone, not rotating a secret shared by every door.
--
-- The token is stored HASHED, for the same reason the password reset tokens are
-- (services/commerce/internal/store/password_reset.go): a database read must not
-- yield a working credential. SHA-256 rather than bcrypt, deliberately — this is a
-- 256-bit random value, not a human-chosen password, so there is no dictionary to
-- slow down and the scan path checks it on every admission at a busy door.
--
-- Named the adversary (ADR-021): this constrains a caller who does not hold an
-- enrolled device's token. It constrains nobody with write access to this database,
-- who can enrol their own device — the same trust boundary every other table here
-- sits inside.

-- +goose Up
CREATE TABLE scanner_devices (
    id            uuid PRIMARY KEY,
    organizer_id  uuid NOT NULL,
    label         text NOT NULL,
    -- hex-encoded SHA-256 of the enrolment token. UNIQUE so a hash collision or a
    -- duplicated enrolment is a write failure rather than two devices sharing an
    -- identity, which would make revocation ambiguous.
    token_hash    text NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz,
    -- Advisory only: written on a best-effort basis by the scan path so an operator
    -- can tell a live gate from a forgotten enrolment. Never read by any decision.
    last_seen_at  timestamptz,
    CONSTRAINT scanner_devices_label_not_blank CHECK (btrim(label) <> '')
);

-- The authentication lookup, and the only index that path needs: it arrives with a
-- token and nothing else. Partial on the live rows so a revoked device is not merely
-- rejected but absent from the index the hot path scans.
CREATE INDEX scanner_devices_live_token ON scanner_devices (token_hash) WHERE revoked_at IS NULL;

-- The operator listing ("which devices does this organizer have"), which is the only
-- other access pattern.
CREATE INDEX scanner_devices_by_organizer ON scanner_devices (organizer_id, created_at DESC);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  -- Every migration in this service refuses to roll back, and this one is not an
  -- exception for a different reason: dropping the table does not merely lose the
  -- enrolment records, it makes every scan route unauthenticated again — the exact
  -- state ai-review S1 is about — while looking like an ordinary rollback.
  --
  -- Three tests here assert that the topmost migration cannot be undone, which is
  -- how a reversible one is caught before it ships (TestRepeatableAdmissionMigrationIsIrreversible
  -- and its siblings). Revoke the devices instead: `access revoke-scanner`.
  RAISE EXCEPTION 'cannot roll back scanner device enrolment: it would leave the scan routes unauthenticated. Revoke devices instead (access revoke-scanner)';
END $$;
-- +goose StatementEnd
