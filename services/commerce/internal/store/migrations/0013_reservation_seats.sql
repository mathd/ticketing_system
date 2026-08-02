-- +goose Up
-- Seated reservations (TKT-173): a reservation can now name specific seats rather
-- than a quantity, so the row has to remember which ones.
--
-- Why the column has to exist at all: the reserve handler answers a REPLAYED
-- reservation from the persisted row and replays inventory with the *pinned* terms,
-- never re-resolved ones — that is what makes "the price you were quoted is the price
-- you are charged" survive a retry. Inventory fingerprints seat-hold idempotency on
-- the canonical seat set (seatFingerprint), so a seated reservation that does not
-- persist its seats cannot be replayed at all: the retry would send a different
-- request and be refused as an idempotency conflict.
--
-- Why jsonb and not text[]: `database/sql` + the pgx/v5 stdlib driver — the stack every
-- service here uses — WRITES a text[] happily and cannot READ one back
-- (`unsupported Scan, storing driver.Value type string into type *[]string`). The write
-- succeeding is what makes that trap expensive: the defect surfaces first in the replay
-- path, which is the only reader. jsonb round-trips as []byte + encoding/json, which is
-- how price_resolution_snapshot in this same table already works, and it matches the
-- reasoning already recorded in seatFingerprint: this data is JSON-encoded rather than
-- delimiter-joined because a seat identity may itself contain the delimiter.
--
-- Nullable with no default and no backfill: every existing row is a GA or staff-created
-- reservation with no seat information to recover, and NULL is the honest record of
-- that. Presence of the array is the reservation-kind signal.
ALTER TABLE reservations ADD COLUMN seat_identities jsonb;

-- +goose Down
-- Refuse to silently discard the seat sets: dropping this column makes every seated
-- reservation unreplayable, and the data cannot be reconstructed from anywhere else
-- (inventory holds the claim, but the mapping back to a reservation is this row).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reservations WHERE seat_identities IS NOT NULL) THEN
        RAISE EXCEPTION 'seated reservations exist; resolve them before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE reservations DROP COLUMN IF EXISTS seat_identities;
