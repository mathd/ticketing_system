-- The channel axis on price rules (TKT-237 / ADR-046 §4 and §8, applied to
-- prices). Fees have resolved per channel since TKT-214; prices never have, and
-- the PRD's cross-epic invariant ("every price/fee decision resolves through the
-- TKT-5 hierarchy") assumed a symmetry that did not exist.
--
-- The column and the resolver's channel step ship TOGETHER, for the reason 0013
-- wrote down when it added the window columns: landing the column alone would
-- let an operator store a channel-specific rule that the shipped resolver
-- ignores, and every all-NULL test passes either way, so nothing would catch it.
--
-- NULL means CHANNEL-AGNOSTIC -- eligible in every channel including the
-- default/public one -- so every existing row keeps resolving exactly as it did
-- before this migration. That is not a migration convenience; it is ADR-046 §4's
-- semantics, and the resolver depends on it.
--
-- No FK to `channels` (TKT-235's registry), and none is coming. That registry is
-- a LOOKUP, NOT A CONSTRAINT: an unregistered-but-legal code must stay
-- authorable and resolvable, exactly as it stays sellable in inventory. Adding a
-- reference here would quietly revoke the guarantee TKT-235 shipped one ticket
-- ago.
--
-- Name the adversary (ADR-021): honest-writer consistency. The CHECK stops an
-- operator mistake and a buggy write path. Anyone with catalog DB access writes
-- what they like.

-- +goose Up
ALTER TABLE price_rules
    -- Bound mirrors fee_rules.channel_code (0016), split_schedules.channel_code
    -- (0017), channels.code (0018) and inventory's claims.channel_code /
    -- channel_allocations.channel_code (0004). Six places now store a channel
    -- code and all six must agree, or a code legal in one is unusable in
    -- another. Exact and case-sensitive: nothing trims or folds (ADR-024).
    ADD COLUMN channel_code text
        CHECK (channel_code IS NULL OR length(channel_code) BETWEEN 1 AND 100);

-- No index on channel_code, and that is a decision -- the same one 0016 made for
-- fee_rules. Channel is filtered by the pure comparator in Go, never in SQL: the
-- candidate query selects on (organizer_id, scope_level, scope_id) and hands the
-- whole candidate set to the resolver, which is what lets provenance report why
-- each loser lost. An index here would serve a predicate that does not exist,
-- and adding that predicate would push scope_id into a post-index Filter and
-- redden the ADR-019 plan assertion (pricing_smoke_test.go).

-- +goose Down

-- Refuse to roll back over channel data, keyed on a NON-NULL value rather than
-- on row count. A row-count guard would be VACUOUS-IN-REVERSE here: legacy rows
-- are expected and all carry NULL, so counting rows would refuse every rollback
-- including the ones that are safe -- the mirror image of the drift 0007's index
-- caused (migration_smoke_test.go's header). 0013 set this precedent for exactly
-- the same shape: a nullable column added to a populated table.
LOCK TABLE price_rules IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM price_rules WHERE channel_code IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0019: per-channel price-rule data exists';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE price_rules
    DROP COLUMN channel_code;
