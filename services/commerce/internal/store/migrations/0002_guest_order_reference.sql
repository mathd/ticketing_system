-- +goose Up
ALTER TABLE orders ADD COLUMN guest_order_ref uuid UNIQUE;

-- +goose Down
ALTER TABLE orders DROP COLUMN guest_order_ref;
