-- Keep Catalog's seat_map_pins bounded to prevent unbounded memory and response growth (TKT-143).
-- Refuse the migration if an existing database contains an over-limit row.
-- Truncation would change stable identity and idempotency keys used downstream.
-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    bad_id uuid;
    bad_len int;
BEGIN
    SELECT id, char_length(seat_identity) INTO bad_id, bad_len
    FROM seat_map_pins
    WHERE char_length(seat_identity) < 1 OR char_length(seat_identity) > 200
    LIMIT 1;
    IF bad_id IS NOT NULL THEN
        RAISE EXCEPTION 'cannot enforce seat_map_pins_identity_length: row % has seat_identity length % outside permitted range 1..200', bad_id, bad_len;
    END IF;

    SELECT id, char_length(pinned_by) INTO bad_id, bad_len
    FROM seat_map_pins
    WHERE char_length(pinned_by) < 1 OR char_length(pinned_by) > 45
    LIMIT 1;
    IF bad_id IS NOT NULL THEN
        RAISE EXCEPTION 'cannot enforce seat_map_pins_pinned_by_length: row % has pinned_by length % outside permitted range 1..45', bad_id, bad_len;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE seat_map_pins
    ADD CONSTRAINT seat_map_pins_identity_length
    CHECK (char_length(seat_identity) BETWEEN 1 AND 200);

ALTER TABLE seat_map_pins
    ADD CONSTRAINT seat_map_pins_pinned_by_length
    CHECK (char_length(pinned_by) BETWEEN 1 AND 45);

-- +goose Down
ALTER TABLE seat_map_pins
    DROP CONSTRAINT seat_map_pins_identity_length;

ALTER TABLE seat_map_pins
    DROP CONSTRAINT seat_map_pins_pinned_by_length;
