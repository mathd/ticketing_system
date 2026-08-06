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
            -- jsonb_typeof, not merely IS NOT NULL: `->>` renders a JSON string
            -- and a JSON number identically, so a presence check accepts
            -- "garbage" and then the cast below fails at INSERT time with a
            -- type error instead of a constraint violation -- or, for a numeric
            -- string, silently succeeds. Require the JSON type.
            -- coalesce on EVERY jsonb_typeof, without exception. The first
            -- version of this constraint coalesced two of them and not these
            -- three, and the same UNKNOWN-passes-a-CHECK hole reopened
            -- immediately -- a fix composing into the defect it had just
            -- removed. Caught by this migration's own test, twice.
            AND coalesce(jsonb_typeof(fee_resolution_snapshot -> 'face_value'), 'absent') = 'number'
            AND coalesce(jsonb_typeof(fee_resolution_snapshot -> 'passed_on_fees'), 'absent') = 'number'
            AND coalesce(jsonb_typeof(fee_resolution_snapshot -> 'total_amount'), 'absent') = 'number'
            -- The stored envelope must agree with the columns it explains. A
            -- snapshot that says one thing while the row charges another is
            -- worse than no snapshot: it is a provenance document that lies, and
            -- TKT-217 settles real money from it.
            AND (fee_resolution_snapshot ->> 'face_value')::bigint = face_value_amount
            AND (fee_resolution_snapshot ->> 'total_amount')::bigint = total_amount
            -- And it must be internally consistent: the fees the buyer paid are
            -- exactly the difference between what they were charged and the face
            -- value. Without this a snapshot claiming 999 in fees on a row whose
            -- columns differ by 300 is accepted, and a settlement run reading the
            -- snapshot would attribute money the buyer never paid.
            AND (fee_resolution_snapshot ->> 'passed_on_fees')::bigint
                = total_amount - face_value_amount
            AND (fee_resolution_snapshot ->> 'passed_on_fees')::bigint >= 0
        )
    );

-- channel_code is an idempotency TERM, not a decoration (TKT-215 ai-review).
-- The replay path refuses a key reused "with different terms" by comparing
-- quantity, ticket type and the seat set. Channel selects which fee rules apply,
-- so two requests under one key differing only by channel are different sales --
-- and without this column the second silently receives the first one's quote.
ALTER TABLE reservations
    ADD COLUMN channel_code text
        CHECK (channel_code IS NULL OR length(channel_code) BETWEEN 1 AND 100);
-- Nullable and not backfilled: NULL is the default/public context, which is
-- exactly what every pre-existing reservation was sold through.

-- The exchange source needs BOTH numbers, and this column is why (TKT-215
-- ai-review, [high]). Repointing the exchange at the face value fixed the delta
-- and BROKE the money facts: exchangeFacts publishes SourceTotal as the gross
-- `order.exchange.reversed` leg, so a fee-carrying order reversed 9100 against
-- an original charge of 9400 and the payments journal stopped agreeing with the
-- money that actually moved. Two correct-looking fixes composing into a new
-- defect.
--
-- Face drives the delta (0010's CHECK ties delta_amount to source_total);
-- gross drives the reversal fact. Backfilled from source_total, which is exact:
-- no exchange before this migration involved a fee.
ALTER TABLE order_exchanges
    ADD COLUMN source_gross_total bigint;
UPDATE order_exchanges SET source_gross_total = source_total WHERE source_gross_total IS NULL;
ALTER TABLE order_exchanges
    ALTER COLUMN source_gross_total SET NOT NULL;
ALTER TABLE order_exchanges
    ADD CONSTRAINT order_exchanges_gross_at_least_face CHECK (source_gross_total >= source_total);

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
    IF EXISTS (SELECT 1 FROM order_exchanges WHERE source_gross_total <> source_total) THEN
        RAISE EXCEPTION 'cannot roll back 0014: exchanges exist whose gross differs from their face value';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE order_exchanges
    DROP CONSTRAINT order_exchanges_gross_at_least_face,
    DROP COLUMN source_gross_total;
ALTER TABLE reservations
    DROP COLUMN channel_code;
ALTER TABLE reservations
    DROP CONSTRAINT reservations_fee_snapshot_shape,
    DROP COLUMN fee_resolution_snapshot,
    DROP CONSTRAINT reservations_face_value_bounds,
    DROP COLUMN face_value_amount;
