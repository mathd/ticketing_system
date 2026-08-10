-- The sales-channel registry (TKT-235 / epic TKT-17). A channel finally becomes
-- a defined thing instead of a string four tables happen to agree on.
--
-- Before this, `channel_code` was an exact opaque string stored independently in
-- inventory's claims and channel_allocations (0004) and catalog's fee_rules
-- (0016) and split_schedules (0017), each carrying a comment saying the four
-- bounds must agree. Nothing said what a channel WAS, what it was called, or
-- whether it was still in use.
--
-- THE REGISTRY IS A LOOKUP, NOT A CONSTRAINT, and that is the load-bearing
-- decision here. No foreign key points at this table from any of those four
-- columns, and none is coming. ADR-024 refuses an FK from claims to allocation
-- rows so that historical attribution survives a channel being retired; the same
-- argument covers the rest. A code that has never been registered keeps selling
-- exactly as it does today, and a test in inventory pins that (TKT-235's
-- unregistered-code characterization test). The registry answers "what is this
-- channel called and is it still offered" — it never answers "may this sell".
--
-- Codes are EXACT and case-sensitive (ADR-024, ADR-046 §4). 'POS', 'pos' and
-- ' pos ' are three different channels. Nothing here trims, folds or normalizes,
-- and nothing downstream may either: a normalized registry would disagree with
-- four unnormalized columns, which is worse than no registry.
--
-- Name the adversary (ADR-021): this is HONEST-WRITER consistency. The
-- constraints below stop an operator mistake and a buggy write path. Anyone with
-- catalog DB access writes whatever they like. Nothing here is tamper-evident.

-- +goose Up

CREATE TABLE channels (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id uuid NOT NULL REFERENCES organizers (id),
    -- Bound mirrors claims.channel_code / channel_allocations.channel_code
    -- (inventory 0004) and fee_rules.channel_code (0016) /
    -- split_schedules.channel_code (0017). All five must agree or a code legal
    -- in one place is unusable in another.
    code         text NOT NULL CHECK (length(code) BETWEEN 1 AND 100),
    -- Bounded like every other display string in this schema; 1..200 matches
    -- payees.display_name (0017), the nearest sibling. An unbounded text column
    -- on an operator-writable table is a storage and index problem waiting
    -- (TKT-143 tracks the class).
    display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    -- A CHECK enum rather than payee_kinds-style rows, and unlike ADR-047 the
    -- reason is that these four are not extensible by data. A new kind changes
    -- what the platform DOES -- a reseller channel means a partner credential,
    -- a presale channel means unlock codes -- so it lands with code, in the
    -- OpenAPI enum and the generated Go and TypeScript types, not as an INSERT.
    -- Making it a row would advertise an extensibility that does not exist.
    kind         text NOT NULL CHECK (kind IN ('web', 'pos', 'presale', 'reseller')),
    -- Disable, never delete. There is no DELETE endpoint: a deleted channel
    -- whose code sits on live claims is exactly the orphaning the no-FK rule
    -- exists to avoid, and nothing would cascade or complain.
    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Written explicitly by the store on every update. There is no trigger --
    -- catalog has none for this, anywhere -- so a store that forgets leaves a
    -- stale value rather than failing loudly.
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Per organizer, not global: two organizers may both call a channel 'pos'.
    -- Case-sensitive by construction, since the column is not normalized -- so
    -- 'POS' and 'pos' coexist, deliberately.
    CONSTRAINT channels_code_per_organizer UNIQUE (organizer_id, code)
);

-- The public read is `WHERE organizer_id = $1 AND enabled ORDER BY code`. A
-- partial index on the enabled rows keeps that off a sequential scan as the
-- registry grows across tenants (ADR-019: a scoped read is only scoped if an
-- index backs the filter). `code` is in the index so the ordering comes from it
-- rather than a sort.
CREATE INDEX channels_enabled_by_organizer
    ON channels (organizer_id, code)
    WHERE enabled;

-- +goose Down

-- Refuse to roll back over data, like 0012/0013/0016. The lock and the check are
-- one atomic decision: without it a concurrent INSERT lands between them.
LOCK TABLE channels IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM channels) THEN
        RAISE EXCEPTION 'cannot roll back 0018: channel registry data exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX channels_enabled_by_organizer;
DROP TABLE channels;
