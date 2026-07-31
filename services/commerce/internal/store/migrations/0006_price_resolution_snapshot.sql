-- Reservation price provenance (TKT-153 / ADR-036 §5). A completed order can be
-- traced to the rule that priced it, without consulting the current catalog row.
--
-- A SNAPSHOT, not a reference. A price rule can later be closed or superseded
-- (TKT-152 made windows mutable at exactly one field), and a foreign key would
-- let that rewrite what a buyer was charged. Copying the document keeps the
-- record true. It also keeps `candidates` -- the losing rules and their reasons,
-- which are the actual answer to "why was I charged this?" and which no set of
-- discrete columns preserves without ~15 nullable columns and a winner/fallback
-- union.
--
-- Name the adversary (ADR-021): this is HONEST-WRITER consistency, not
-- tamper-evidence. Anyone who can write to commerce's database can replace the
-- snapshot. What it protects against is ordinary editing of the catalog rule,
-- which is the realistic way history would otherwise rot.
-- +goose Up
ALTER TABLE reservations
    ADD COLUMN price_resolution_snapshot jsonb;

-- Nullable, and NOT backfilled. Rows written before this migration were priced
-- from the ticket type's raw column, and stamping them 'no_eligible_rule' would
-- fabricate a resolution that never happened. The staff paths
-- (convertOperational, group draw-down) are deliberately out of this ticket's
-- scope and keep writing NULL -- the same state as every pre-existing row,
-- which is why leaving them is coherent rather than sloppy.

-- Trace projections, generated from the immutable snapshot so a support query
-- never writes a JSON path and no mutable value is duplicated.
ALTER TABLE reservations
    ADD COLUMN price_resolver_version integer
        GENERATED ALWAYS AS ((price_resolution_snapshot ->> 'resolver_version')::integer) STORED,
    ADD COLUMN price_rule_id uuid
        GENERATED ALWAYS AS ((price_resolution_snapshot #>> '{winner,rule_id}')::uuid) STORED,
    ADD COLUMN price_rule_scope_level text
        GENERATED ALWAYS AS (price_resolution_snapshot #>> '{winner,scope_level}') STORED,
    ADD COLUMN price_fallback_reason text
        GENERATED ALWAYS AS (price_resolution_snapshot ->> 'fallback_reason') STORED;

-- Independent structural guard, expressed over the JSONB itself rather than
-- over the generated columns: PostgreSQL restricts what a CHECK may reference,
-- and depending on the generated columns here would couple this constraint to
-- that rule for no benefit. The application does full contract validation; this
-- is the database refusing an obviously broken document.
--
-- The XOR is the same invariant commerce validates before persisting: exactly
-- one of a winner and a fallback reason.
ALTER TABLE reservations
    ADD CONSTRAINT reservations_price_snapshot_shape CHECK (
        price_resolution_snapshot IS NULL
        OR (
            jsonb_typeof(price_resolution_snapshot) = 'object'
            AND (price_resolution_snapshot ->> 'resolver_version') IS NOT NULL
            AND (
                ((price_resolution_snapshot #>> '{winner,rule_id}') IS NOT NULL
                 AND (price_resolution_snapshot ->> 'fallback_reason') IS NULL)
                OR
                ((price_resolution_snapshot #>> '{winner,rule_id}') IS NULL
                 AND (price_resolution_snapshot ->> 'fallback_reason') IS NOT NULL)
            )
        )
    );

-- No index on the snapshot. The trace starts from an order or reservation
-- identity, and no subset query over provenance has a requirement yet; an index
-- added without a read that needs it is write-path tax on the sale path
-- (ADR-019's own caveat, applied against adding one).

-- +goose Down
-- Lock before the guard: checking first leaves a window where a reserve commits
-- a snapshot between the check and the column drop, and a "fail closed" guard
-- that silently destroys money provenance is worse than none.
LOCK TABLE reservations IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reservations WHERE price_resolution_snapshot IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0006: reservation price-resolution snapshots exist';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE reservations
    DROP CONSTRAINT reservations_price_snapshot_shape,
    DROP COLUMN price_fallback_reason,
    DROP COLUMN price_rule_scope_level,
    DROP COLUMN price_rule_id,
    DROP COLUMN price_resolver_version,
    DROP COLUMN price_resolution_snapshot;
