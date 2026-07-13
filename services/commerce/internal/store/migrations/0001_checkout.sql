-- +goose Up
CREATE TABLE reservations (
  id uuid PRIMARY KEY, organizer_id uuid NOT NULL, hold_id uuid NOT NULL UNIQUE,
  slot_id uuid NOT NULL, ticket_type_id uuid NOT NULL, buyer_id uuid NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0), unit_amount bigint NOT NULL CHECK(unit_amount >= 0),
  total_amount bigint NOT NULL CHECK(total_amount >= 0), currency varchar(3) NOT NULL,
  status text NOT NULL CHECK(status IN ('held','finalizing','completed','failed','unknown')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE buyer_pii (
  buyer_id uuid PRIMARY KEY, name text NOT NULL, email text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE order_facts (
  fact_id uuid PRIMARY KEY, order_id uuid NOT NULL, organizer_id uuid NOT NULL, buyer_id uuid NOT NULL,
  fact_type text NOT NULL, amount bigint NOT NULL, currency varchar(3) NOT NULL,
  occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE orders (
  id uuid PRIMARY KEY, reservation_id uuid NOT NULL REFERENCES reservations(id), status text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE orders; DROP TABLE order_facts; DROP TABLE buyer_pii; DROP TABLE reservations;
