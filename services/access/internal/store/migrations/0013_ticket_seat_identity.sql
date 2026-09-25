-- +goose Up
ALTER TABLE tickets
  ADD COLUMN seat_identity text NULL
  CHECK (seat_identity IS NULL OR (char_length(seat_identity) BETWEEN 1 AND 200 AND btrim(seat_identity) <> ''));

-- +goose Down
-- Unconditionally irreversible, like Access migrations 0002-0012. A ticket's
-- seat identity records the association made at issuance; dropping it would erase
-- that history and cannot be repaired from the lifecycle trail.
-- +goose StatementBegin
DO $$
BEGIN
  RAISE EXCEPTION 'migration 0013 is irreversible: ticket seat identities record issuance associations';
END $$;
-- +goose StatementEnd
