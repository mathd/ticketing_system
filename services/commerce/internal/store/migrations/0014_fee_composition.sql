-- Fee composition on the sale (TKT-215 / ADR-046). Two columns, and the second
-- one exists because of a defect found at plan review rather than because the
-- ticket asked for it.
--
-- total_amount becomes the GROSS charge: face value + passed-on fees. That is
-- what the PSP captures (api/server.go charge), what order_facts records, and
-- what the payments journal fact carries -- three sites that all want the money
-- that actually moved.
--
-- But `exchanges` compares total_amount against a PRICE-ONLY target
-- (store/exchanges.go ExchangeDelta = targetTotal - sourceTotal, where the
-- target is resolution.total(quantity)). Once total_amount includes fees, an
-- EVEN exchange computes a negative delta and refunds the service fee:
--
--     face 4550 + fee 300 = 4850 stored ; target 4550 ; delta = -300
--     -> settleExchangeDelta issues a 300-cent partial refund for an exchange
--        that should move no money at all.
--
-- So face_value_amount is stored explicitly and the exchange source reads THAT.
-- The backfill is exact rather than approximate: before this migration no
-- reservation carries a fee, so face_value_amount = total_amount is true for
-- every existing row by construction, and exchange behaviour is UNCHANGED
-- rather than newly corrected.
-- +goose Up
ALTER TABLE reservations
    ADD COLUMN face_value_amount bigint;

-- Exact for every pre-existing row: no fee has ever been charged, so the total
-- IS the face value.
UPDATE reservations SET face_value_amount = total_amount WHERE face_value_amount IS NULL;

ALTER TABLE reservations
    ALTER COLUMN face_value_amount SET NOT NULL;

-- NOT NULL rather than nullable, deliberately: a nullable face value would make
-- every reader handle "unknown", and the one reader that forgot would silently
-- fall back to the gross total -- which is precisely the bug this column exists
-- to prevent.
ALTER TABLE reservations
    ADD CONSTRAINT reservations_face_value_bounds CHECK (
        face_value_amount >= 0 AND face_value_amount <= total_amount
    );
-- face <= total holds by construction: total = face + passed_on and passed_on is
-- never negative. ABSORBED fees deliberately do not appear in either number --
-- they are borne by the organizer out of the face value, so they change what the
-- organizer nets, not what the buyer pays.

-- The fee provenance snapshot. Same discipline as 0006's price snapshot: a
-- SNAPSHOT, not a reference. A fee rule can later be closed or superseded, and a
-- foreign key would let that rewrite what a buyer was charged.
--
-- Name the adversary (ADR-021): honest-writer consistency, not tamper-evidence.
-- Anyone who can write to commerce's database can replace this document.
ALTER TABLE reservations
    ADD COLUMN fee_resolution_snapshot jsonb;

-- Nullable, and NOT backfilled. Rows written before this migration were sold
-- with no fee concept at all, and stamping them with an empty breakdown would
-- fabricate a resolution that never happened -- the same reasoning 0006 applies
-- to pre-existing price rows. The staff paths (operational conversion, group
-- draw-down) keep writing NULL for the same reason they keep writing a NULL
-- price snapshot: they are deliberately outside this ticket's scope.

-- Structural guard over the JSONB itself. The application does full contract
-- validation; this is the database refusing an obviously broken document.
--
-- The envelope is {resolution, breakdown, face_value, passed_on_fees,
-- total_amount}: catalog's document kept verbatim under `resolution` so the
-- losing candidates survive, plus the arithmetic commerce performed on it.
-- Storing only the computed breakdown would discard the answer to "why was I
-- charged this fee"; storing only the resolution would make every later reader
-- redo the arithmetic and risk disagreeing with the amount actually captured.
ALTER TABLE reservations
    ADD CONSTRAINT reservations_fee_snapshot_shape CHECK (
        fee_resolution_snapshot IS NULL
        OR (
            jsonb_typeof(fee_resolution_snapshot) = 'object'
            -- coalesce, not a bare comparison: `->` on a MISSING key yields SQL
            -- NULL, jsonb_typeof(NULL) is NULL, and `NULL = 'object'` is
            -- UNKNOWN -- which a CHECK constraint ACCEPTS. Without the coalesce
            -- the whole AND chain below goes unknown and an envelope with no
            -- resolution document at all sails through. Found by this
            -- migration's own test rather than in review.
            AND coalesce(jsonb_typeof(fee_resolution_snapshot -> 'resolution'), 'absent') = 'object'
            AND coalesce(jsonb_typeof(fee_resolution_snapshot -> 'breakdown'), 'absent') = 'array'
            AND (fee_resolution_snapshot ->> 'face_value') IS NOT NULL
            AND (fee_resolution_snapshot ->> 'passed_on_fees') IS NOT NULL
            AND (fee_resolution_snapshot ->> 'total_amount') IS NOT NULL
            -- The stored envelope must agree with the columns it explains. A
            -- snapshot that says one thing while the row charges another is
            -- worse than no snapshot: it is a provenance document that lies.
            AND (fee_resolution_snapshot ->> 'face_value')::bigint = face_value_amount
            AND (fee_resolution_snapshot ->> 'total_amount')::bigint = total_amount
        )
    );

-- No index on the snapshot. The trace starts from a reservation or order
-- identity, and no subset query over fee provenance has a requirement yet; an
-- index added without a read that needs it is write-path tax on the sale path.

-- +goose Down
-- Lock before the guard: checking first leaves a window where a reserve commits
-- a snapshot between the check and the drop, and a "fail closed" guard that
-- silently destroys money provenance is worse than none. Same discipline as 0006.
LOCK TABLE reservations IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reservations WHERE fee_resolution_snapshot IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 0014: reservation fee snapshots exist';
    END IF;
    -- The face-value column is safe to drop only while it is redundant. Once a
    -- fee has been charged, face_value_amount carries information total_amount
    -- no longer does, and dropping it would silently re-break the exchange
    -- delta for every fee-carrying order.
    IF EXISTS (SELECT 1 FROM reservations WHERE face_value_amount <> total_amount) THEN
        RAISE EXCEPTION 'cannot roll back 0014: reservations exist whose face value differs from their total';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE reservations
    DROP CONSTRAINT reservations_fee_snapshot_shape,
    DROP COLUMN fee_resolution_snapshot,
    DROP CONSTRAINT reservations_face_value_bounds,
    DROP COLUMN face_value_amount;
