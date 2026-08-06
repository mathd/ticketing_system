-- Fee-rule hierarchy and per-code resolution (TKT-214 / ADR-046). Rules are
-- polymorphically scoped exactly as price_rules are: one row attaches to exactly
-- one of ADR-036 §1's five levels, and the level is part of the identity, not a
-- descriptor -- see the composite index below.
--
-- A SEPARATE TABLE, not a widened price_rules, and the reason is semantic rather
-- than aesthetic: a price resolution has one winner, a fee resolution has one
-- winner PER FEE CODE. Folding additive multi-code rows, two value columns and a
-- channel selector into the table that prices real sales would make the price
-- comparator carry cases it can never have.
--
-- Rows are append-mostly under HONEST-WRITER consistency, not tamper-evidence
-- (ADR-021 / ADR-036 §3): the store's write gate makes a malformed rule
-- unrepresentable, but anyone with catalog DB access can still write one.
-- +goose Up
CREATE TABLE fee_rules (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id            uuid NOT NULL REFERENCES organizers (id),
    scope_level             text NOT NULL
        CHECK (scope_level IN ('ticket_type', 'slot', 'series', 'event', 'venue')),
    -- No FK, for the same reason price_rules has none: the target table depends
    -- on scope_level, so referential integrity is traded for a provable query
    -- plan (ADR-036 §3) and paid back at the write path by an INSERT ... SELECT
    -- gate.
    scope_id                uuid NOT NULL,
    -- The additive stream this rule belongs to. One winner per code; codes never
    -- compete with each other. Opaque and case-sensitive on purpose (ADR-046
    -- § Fee codes): a registry and a normalization policy are TKT-17's, and a
    -- vocabulary invented here would decide that story. Bounded so an unbounded
    -- string cannot become a storage or index problem.
    fee_code                text NOT NULL CHECK (length(fee_code) BETWEEN 1 AND 64),
    basis                   text NOT NULL
        CHECK (basis IN ('per_ticket_fixed', 'per_order_fixed', 'percentage_bps')),
    -- Upper bound matches the OpenAPI Money.amount cap (2^53-1) so every
    -- consumer, the storefront included, can represent it exactly. Unbounded
    -- here would let a write succeed and a contract-declared read 500 on it
    -- (ADR-028) -- the money path failing at read time because of a write we
    -- allowed.
    amount                  bigint CHECK (amount >= 0 AND amount <= 9007199254740991),
    -- Basis points, 0..10000 -- i.e. 0%..100%. The upper bound is not decoration:
    -- a rate above 10000 is a fee larger than the thing it is a fee on, which is
    -- never a configuration anyone meant.
    rate_bps                integer CHECK (rate_bps >= 0 AND rate_bps <= 10000),
    -- Uppercase-enforced, like price_rules.currency and unlike the older
    -- ticket_types.currency: the contract requires ^[A-Z]{3}$, so a lowercase
    -- code would resolve fine and then 500 the declared read (ADR-028).
    currency                char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- Who bears it. This changes nothing about eligibility or precedence; it
    -- decides what commerce does with the number (TKT-215) and what the
    -- settlement ledger has to balance (TKT-217).
    incidence               text NOT NULL CHECK (incidence IN ('passed_on', 'absorbed')),
    -- NULL = channel-agnostic: eligible in EVERY channel, including the
    -- default/public one. A non-NULL code is eligible only on an exact string
    -- match. Length bound mirrors inventory's claims.channel_code (ADR-024),
    -- which is the only other place a channel code is stored; the two must agree
    -- or a code legal in one is unusable in the other.
    channel_code            text CHECK (channel_code IS NULL OR length(channel_code) BETWEEN 1 AND 100),
    priority                integer NOT NULL DEFAULT 0,
    force_ancestor_override boolean NOT NULL DEFAULT false,
    -- Half-open [effective_from, effective_until), identical to price rules.
    effective_from          timestamptz,
    effective_until         timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    -- The tagged union, enforced. A fixed basis carries an amount and no rate; a
    -- percentage basis carries a rate and no amount. Without this both columns
    -- are independently nullable and a row can claim a basis whose value is
    -- missing -- which the resolver would then have to guess about on a money
    -- path.
    CONSTRAINT fee_rules_basis_shape CHECK (
        (basis IN ('per_ticket_fixed', 'per_order_fixed')
             AND amount IS NOT NULL AND rate_bps IS NULL)
        OR (basis = 'percentage_bps'
             AND rate_bps IS NOT NULL AND amount IS NULL)
    ),
    -- A reversed window is unrepresentable. Without it an instant could be
    -- simultaneously after `until` and before `from`, so BOTH provenance reasons
    -- would apply to one rule with no stated precedence -- and the resolver's two
    -- window branches would stop being mutually exclusive.
    CONSTRAINT fee_rules_effective_window_check CHECK (
        effective_from IS NULL
        OR effective_until IS NULL
        OR effective_from < effective_until
    )
);

-- The resolution predicate matches (scope_level, scope_id) PAIRS, never scope_id
-- alone: UUID uniqueness is per table, so an untyped match could load an
-- unrelated event's rule for a ticket type that happened to share its id
-- (ADR-036 §3). Plain CREATE INDEX -- CONCURRENTLY is not adopted (ADR-020).
CREATE INDEX fee_rules_scope ON fee_rules (organizer_id, scope_level, scope_id);

-- No index on fee_code, channel_code or the window columns, and that is a
-- decision rather than an omission (ADR-019: an index without a read that needs
-- it is write-path tax for nothing). The candidate read filters on organizer +
-- the five scope pairs and nothing else -- code, channel and time are all
-- resolved in memory, because an expired rule must still be LOADED to be
-- reported as outside_window_past, and a wrong-currency rule for another channel
-- must still be loaded to fail the resolution.

-- +goose Down
-- Lock before the guard: checking first leaves a window where a writer commits a
-- rule between the check and the drop, and a "fail closed" guard that silently
-- destroys fee configuration is worse than none. ACCESS EXCLUSIVE is what DROP
-- TABLE needs anyway, so taking it up front costs nothing and makes the check
-- and the drop one atomic decision. Same discipline as 0012/0013.
LOCK TABLE fee_rules IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM fee_rules) THEN
        RAISE EXCEPTION 'cannot roll back 0016: fee-rule data exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX fee_rules_scope;
DROP TABLE fee_rules;
