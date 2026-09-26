-- +goose Up
CREATE TABLE scanner_local_decisions (
  occurrence_id uuid PRIMARY KEY,
  ticket_id uuid NOT NULL REFERENCES tickets(id),
  organizer_id uuid NOT NULL,
  occurred_at timestamptz NOT NULL,
  decision text NOT NULL CHECK (decision = 'revocation_refused')
);

-- +goose Down
-- Refusal records are operator evidence and cannot be discarded by rollback.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM scanner_local_decisions) THEN
    RAISE EXCEPTION 'migration 0014 is irreversible while scanner refusal records exist';
  END IF;
  DROP TABLE scanner_local_decisions;
END $$;
-- +goose StatementEnd
