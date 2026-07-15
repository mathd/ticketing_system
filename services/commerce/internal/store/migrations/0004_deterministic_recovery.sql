-- +goose Up
-- Deterministic recovery for stuck checkouts (ADR-016 §Decisions 1-3).
--
-- Until now a checkout that died mid-protocol parked forever: commerce had no
-- background work at all, so `created` / `confirmation_pending` orders were advanced
-- only by a byte-identical replay that nothing generated. The buyer's claim stayed
-- `finalizing`, which inventory counts against availability and exempts from expiry, so
-- the seat leaked permanently.

-- The terminal PSP answer, recorded BEFORE a release is attempted. Without it the
-- release is un-restartable: a crash after releasing but before marking the order
-- leaves no evidence the outcome was ever known, and the next pass cannot tell a
-- released claim from one that was never released (ADR-016 §Decision 2).
ALTER TABLE orders ADD COLUMN terminal_outcome text
  CHECK (terminal_outcome IN ('declined','timeout','not_attempted'));

-- Claim/lease, mirroring the completion outbox: the claim id is what stops a claimant
-- whose lease lapsed mid-drive from mutating its successor's row.
ALTER TABLE orders ADD COLUMN recovery_claim_id uuid;
ALTER TABLE orders ADD COLUMN recovery_lease_until timestamptz;
ALTER TABLE orders ADD COLUMN recovery_attempts integer NOT NULL DEFAULT 0;
-- Backoff gate. Claiming is oldest-first, so without it one unrecoverable order at the
-- head is re-selected every pass and starves every newer one.
ALTER TABLE orders ADD COLUMN recovery_next_attempt_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE orders ADD COLUMN recovery_last_error text;
-- Parked: exhausted attempts, or awaiting compensation that does not exist yet. Never
-- claimed again; stays visible to an operator via recovery_last_error.
ALTER TABLE orders ADD COLUMN recovery_parked_at timestamptz;

-- The runner's only query path.
CREATE INDEX orders_recovery_claimable_idx
  ON orders (recovery_next_attempt_at)
  WHERE status IN ('created','confirmation_pending','release_pending')
    AND recovery_parked_at IS NULL;

-- `orders.status` has never had a CHECK constraint — the vocabulary lived only as
-- string literals scattered across Go, which is how `release_pending` came to be
-- returned to buyers while being written nowhere. Pin it now that recovery depends on
-- the set being closed: an unknown status would silently fall out of every claim query.
--
-- `release_pending` is included because this migration makes it real (it was previously
-- a response-only lie). `reconciliation_required` is new: captured money whose claim can
-- never be confirmed, waiting on compensation from TKT-56.
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (
  status IN ('created','payment_unknown','confirmation_pending','release_pending',
             'completed','declined','timeout','reconciliation_required')
);

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
DROP INDEX orders_recovery_claimable_idx;
ALTER TABLE orders DROP COLUMN recovery_parked_at;
ALTER TABLE orders DROP COLUMN recovery_last_error;
ALTER TABLE orders DROP COLUMN recovery_next_attempt_at;
ALTER TABLE orders DROP COLUMN recovery_attempts;
ALTER TABLE orders DROP COLUMN recovery_lease_until;
ALTER TABLE orders DROP COLUMN recovery_claim_id;
ALTER TABLE orders DROP COLUMN terminal_outcome;
