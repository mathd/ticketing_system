-- Price-rule hierarchy and deterministic unit-price resolution (TKT-151 /
-- ADR-036). Rules are polymorphically scoped: one row attaches to exactly one
-- of five levels (ADR-036 §1), and the level is part of the identity, not a
-- descriptor -- see the composite index below.
--
-- Rows are append-mostly under HONEST-WRITER consistency, not tamper-evidence
-- (ADR-021 / ADR-036 §3): the store's write gate makes a malformed rule
-- unrepresentable, but anyone with catalog DB access can still write one.
--
-- TKT-152 adds effective_from / effective_until and their half-open window
-- semantics. They are absent here on purpose: a column that accepts a timed
-- rule the shipped resolver ignores would pass every all-NULL test.
-- +goose Up
CREATE TABLE price_rules (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id            uuid NOT NULL REFERENCES organizers (id),
    scope_level             text NOT NULL
        CHECK (scope_level IN ('ticket_type', 'slot', 'series', 'event', 'venue')),
    -- No FK: the target table depends on scope_level, so referential integrity
    -- is traded for a provable query plan (ADR-036 §3) and paid back at the
    -- write path by the store's INSERT ... SELECT gate.
    scope_id                uuid NOT NULL,
    -- Tagged union with one member today. Widening it later (basis points,
    -- deltas) is additive; ADR-036 §2 requires any such action to stay
    -- integer-safe.
    action_kind             text NOT NULL CHECK (action_kind = 'absolute'),
    -- Upper bound matches the OpenAPI Money.amount cap (2^53-1), so every
    -- consumer including the storefront can represent it exactly. Unbounded
    -- here would let a write succeed and the contract-declared read 500 on it
    -- (ADR-028) -- the money path failing at read time because of a write we
    -- allowed. Note ticket_types.price_amount still lacks this bound: that is
    -- pre-existing and is TKT-154's, deliberately not fixed here.
    amount                  bigint NOT NULL
        CHECK (amount >= 0 AND amount <= 9007199254740991),
    currency                char(3) NOT NULL,
    -- Operators express intent through priority; the resolver's final
    -- tie-break on id exists only so row order can never decide a price.
    priority                integer NOT NULL DEFAULT 0,
    force_ancestor_override boolean NOT NULL DEFAULT false,
    created_at              timestamptz NOT NULL DEFAULT now()
);

-- The resolution predicate matches (scope_level, scope_id) PAIRS, never
-- scope_id alone: UUID uniqueness is per table, so an untyped match could load
-- an unrelated event's rule for a ticket type that happened to share its id.
-- Plain CREATE INDEX -- CONCURRENTLY is not adopted in this repo (ADR-020).
CREATE INDEX price_rules_scope ON price_rules (organizer_id, scope_level, scope_id);

-- No uniqueness on (organizer_id, scope_level, scope_id): several competing
-- rules at one scope are exactly what priority and the id tie-break resolve.

-- +goose Down
-- Fail closed rather than silently discard pricing data, mirroring 0004/0006.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM price_rules) THEN
        RAISE EXCEPTION 'cannot roll back 0012: price-rule data exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX price_rules_scope;
DROP TABLE price_rules;
