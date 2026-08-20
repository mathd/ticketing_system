-- +goose Up
-- Presale unlock codes (TKT-239 / ADR-064): WHO may sell on a channel, alongside
-- ADR-024's HOW MUCH and ADR-054's WHEN. A window says the presale opens Tuesday;
-- a code says only these buyers get in.
--
-- A code grants ACCESS and nothing else. It is not a discount and changes no
-- price -- promotions are TKT-7's, and a code that moved money would put this
-- table on the money path, which it is emphatically not.
--
-- WHY THE GATE LIVES HERE AND NOT ON CATALOG'S CHANNEL REGISTRY. The obvious
-- home for "is this channel gated" is catalog's channels row (TKT-235), and it
-- is the wrong one for two independent reasons. That registry is deliberately A
-- LOOKUP, NOT A CONSTRAINT -- 0018_channels.sql says so at length and
-- TestAnUnregisteredChannelCodeStillSells pins that an unregistered code still
-- sells. And inventory has NO read path to catalog's tables at all (ADR-002):
-- separate services, separate databases, no client, no consumer, no replica. A
-- gate there would mean a cross-service read on the claim path, which is exactly
-- what ADR-010's single-serialization-point rule exists to prevent. So the flag
-- sits on the allocation row the claim path ALREADY reads under the pool lock.
-- Accepted cost: gating is configured per allocation, not once per channel.

-- requires_code defaults FALSE so every allocation that exists today keeps
-- selling exactly as it does today. The feature is off until an operator turns
-- it on, and a test pins that an unflagged allocation ignores codes entirely.
ALTER TABLE channel_allocations
    ADD COLUMN requires_code boolean NOT NULL DEFAULT false;

-- Identity is (organizer_id, channel_code, code) -- a code is scoped to one
-- channel of one organizer and SPANS EVERY SLOT in that channel's presale. That
-- span is the point (one code for the whole on-sale) and it is also the reason
-- redemption cannot be serialized by the pool row the way a channel cap is: see
-- the lock note in channel_allocations.go. Codes are EXACT and case-sensitive,
-- like channel codes (ADR-024, ADR-046 section 4) -- no citext, no lower(), no
-- trimming. 'VIP', 'vip' and ' vip ' are three different codes.
--
-- No foreign key to catalog's channels, and none to inventory's
-- channel_allocations either: an allocation is per-pool while a code is
-- per-channel, so there is no single row to point at, and ADR-024 refuses the FK
-- anyway so that a retired channel does not break history.
CREATE TABLE presale_codes (
    organizer_id    uuid NOT NULL,
    -- Bound mirrors claims.channel_code and channel_allocations.channel_code
    -- (0004) and catalog's fee_rules/split_schedules/channels. All must agree or
    -- a code legal in one place is unusable in another.
    channel_code    text NOT NULL CHECK (length(channel_code) BETWEEN 1 AND 100),
    code            text NOT NULL CHECK (length(code) BETWEEN 1 AND 100),
    -- NULL = unlimited redemptions. A cap of 0 would be a code that can never be
    -- used, which is what deleting it expresses better.
    max_redemptions integer CHECK (max_redemptions IS NULL OR max_redemptions > 0),
    -- Half-open [opens_at, closes_at), same convention and same clock_timestamp()
    -- reasoning as ADR-054's channel window. A code's window and its channel's
    -- window are independent: both must admit the instant.
    opens_at        timestamptz,
    closes_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organizer_id, channel_code, code),
    -- A reversed window is unrepresentable, for the reason 0013 gives: otherwise
    -- an instant is simultaneously before the open and after the close and
    -- "closed" acquires two incompatible reasons.
    CONSTRAINT presale_codes_window_order
        CHECK (opens_at IS NULL OR closes_at IS NULL OR opens_at < closes_at)
);

-- Lets an operator resolve a code WITHOUT knowing its channel, which is what
-- makes the internal read able to distinguish "unknown code" from "code issued
-- on another channel" -- a distinction the PUBLIC refusal deliberately hides.
CREATE INDEX presale_codes_by_code ON presale_codes(organizer_id, code);

-- The claim's citation of the code it redeemed. Plain text, NO FOREIGN KEY, for
-- exactly the reason claims.channel_code has none (0004): historical attribution
-- must survive the code being edited or deleted. A claim records what happened,
-- and what happened does not change when configuration does.
ALTER TABLE claims ADD COLUMN presale_code text
  CHECK (presale_code IS NULL OR length(presale_code) BETWEEN 1 AND 100);

-- A code citation without a channel is incoherent: a code is scoped to a channel,
-- so a claim citing one must say which channel it sold on. The reverse is fine --
-- most channel claims cite no code.
ALTER TABLE claims ADD CONSTRAINT claims_presale_code_needs_channel
  CHECK (presale_code IS NULL OR channel_code IS NOT NULL);

-- Backs the derived redemption count, which sums consumed quantity over
-- (organizer_id, channel_code, presale_code) and then filters on the
-- confirmed-or-live predicate. Partial, because ordinary claims never carry a
-- code and have no business enlarging this index.
--
-- ADR-019: a scoped read is only scoped if an index backs the filter, and
-- proving it takes two assertions -- the result is scoped AND the scan is. Both
-- are in TestRedemptionCountIsIndexBacked, which asserts this index BY NAME:
-- with it dropped the planner still avoids a seq scan by using the
-- organizer_id/idempotency_key unique index and filtering the rest in the heap,
-- so "not a seq scan" passes while the count degrades to O(organizer's claims).
-- The count runs on the claim hot path under TWO locks, so that is a contention
-- problem, not merely a slow query.
CREATE INDEX claims_presale_usage
  ON claims(organizer_id, channel_code, presale_code, status, expires_at)
  WHERE presale_code IS NOT NULL;

-- +goose Down
-- Keyed on the presence of REAL STATE, not on row counts of things that always
-- exist. Every allocation predating this migration has requires_code = false, so
-- a row-count guard on channel_allocations would refuse every rollback including
-- the safe ones -- the vacuous-in-reverse shape 0013 inherited from catalog's
-- 0019. IS TRUE is the guard; the default is not state.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM channel_allocations WHERE requires_code IS TRUE) THEN
    RAISE EXCEPTION 'gated channel allocations exist; ungate them before downgrading';
  END IF;
  IF EXISTS (SELECT 1 FROM presale_codes) THEN
    RAISE EXCEPTION 'presale codes exist; delete them before downgrading';
  END IF;
  IF EXISTS (SELECT 1 FROM claims WHERE presale_code IS NOT NULL) THEN
    RAISE EXCEPTION 'claims cite presale codes; downgrading would erase that attribution';
  END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX claims_presale_usage;
ALTER TABLE claims DROP CONSTRAINT claims_presale_code_needs_channel;
ALTER TABLE claims DROP COLUMN presale_code;
DROP TABLE presale_codes;
ALTER TABLE channel_allocations DROP COLUMN requires_code;
