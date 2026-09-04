-- Keep Catalog's stored seat identities inside the 200-character request
-- limit shared by Commerce and Inventory. Refuse the migration if an older
-- database contains a longer identity. Truncation would change the stable key
-- used downstream.
-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM seat_map_seats
        WHERE char_length(seat_identity) > 200
    ) THEN
        RAISE EXCEPTION 'cannot enforce seat identity limit: an existing identity is longer than 200 characters';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE seat_map_seats
    ADD CONSTRAINT seat_map_seats_identity_length
    CHECK (char_length(seat_identity) BETWEEN 1 AND 200);

-- +goose Down
ALTER TABLE seat_map_seats
    DROP CONSTRAINT seat_map_seats_identity_length;
