-- Effective windows on price rules (TKT-152 / ADR-036 §4 step 2). This is what
-- makes an early-bird tier flip "without manual intervention": NOTHING RUNS.
-- No cron, no scheduled write, no job that can fail at 00:00 on an on-sale.
-- The same rows resolve differently as the evaluation instant crosses a bound.
--
-- The columns and the resolver's filter ship TOGETHER, deliberately. Landing
-- the columns alone would let an operator store a timed rule that the shipped
-- resolver ignores -- and every all-NULL test passes either way, so nothing
-- would catch it.
-- +goose Up
ALTER TABLE price_rules
    -- Half-open [effective_from, effective_until): the closed end is inclusive,
    -- the open end is not. Either bound NULL is unbounded on that side.
    ADD COLUMN effective_from  timestamptz,
    ADD COLUMN effective_until timestamptz;

-- A reversed window is unrepresentable. Without this an instant could be
-- simultaneously after `until` and before `from`, so BOTH provenance reasons
-- would apply to one rule with no stated precedence -- and the resolver's two
-- window branches would stop being mutually exclusive.
ALTER TABLE price_rules
    ADD CONSTRAINT price_rules_effective_window_check CHECK (
        effective_from IS NULL
        OR effective_until IS NULL
        OR effective_from < effective_until
    );

-- No index on the window columns, and that is a decision rather than an
-- omission (ADR-019: an index added without a read that needs it is write-path
-- tax for nothing). The candidate query cannot filter by time in SQL, because
-- all three of these need the row loaded:
--   * an expired rule must be reported as outside_window_past;
--   * a future rule must be reported as outside_window_future;
--   * a future wrong-currency rule must still fail the resolution now.
-- The subset read is still organizer + the five scope pairs, already served by
-- price_rules_scope. An index on columns the predicate never mentions would
-- only enlarge the write path.

-- +goose Down
-- Lock first, then check: checking first leaves a window where a writer commits
-- timed data between the guard and the DROP, and a "fail closed" guard that
-- silently loses data is worse than none. Same discipline as 0012.
LOCK TABLE price_rules IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM price_rules
               WHERE effective_from IS NOT NULL OR effective_until IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0013: price-rule effective-window data exists';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE price_rules
    DROP CONSTRAINT price_rules_effective_window_check,
    DROP COLUMN effective_until,
    DROP COLUMN effective_from;
