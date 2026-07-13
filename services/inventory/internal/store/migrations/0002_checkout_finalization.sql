-- +goose Up
ALTER TABLE claims ADD COLUMN ticket_type_id uuid;
ALTER TABLE claims ADD COLUMN unit_amount bigint CHECK (unit_amount >= 0);
ALTER TABLE claims ADD COLUMN currency varchar(3);
ALTER TABLE claims DROP CONSTRAINT claims_status_check;
ALTER TABLE claims ADD CONSTRAINT claims_status_check
  CHECK (status IN ('held','finalizing','confirmed','released','expired'));

-- +goose Down
ALTER TABLE claims DROP CONSTRAINT claims_status_check;
ALTER TABLE claims ADD CONSTRAINT claims_status_check
  CHECK (status IN ('held','confirmed','released','expired'));
ALTER TABLE claims DROP COLUMN currency;
ALTER TABLE claims DROP COLUMN unit_amount;
ALTER TABLE claims DROP COLUMN ticket_type_id;
