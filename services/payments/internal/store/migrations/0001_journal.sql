-- +goose Up
CREATE TABLE journal_heads (
  organizer_id uuid PRIMARY KEY,
  last_sequence bigint NOT NULL DEFAULT 0,
  last_hash bytea NOT NULL DEFAULT decode(repeat('00', 32), 'hex')
);
CREATE TABLE journal_entries (
  fact_id uuid PRIMARY KEY,
  organizer_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK (sequence > 0),
  fact_type text NOT NULL,
  occurred_at timestamptz NOT NULL,
  buyer_id uuid NOT NULL,
  amount bigint NOT NULL CHECK (amount >= 0),
  currency varchar(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  payload jsonb NOT NULL DEFAULT '{}',
  previous_hash bytea NOT NULL CHECK (octet_length(previous_hash)=32),
  entry_hash bytea NOT NULL CHECK (octet_length(entry_hash)=32),
  key_id text NOT NULL,
  signature bytea NOT NULL CHECK (octet_length(signature)=32),
  UNIQUE (organizer_id, sequence)
);
CREATE FUNCTION reject_journal_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'journal entries are append-only'; END $$;
CREATE TRIGGER journal_no_update BEFORE UPDATE OR DELETE ON journal_entries
FOR EACH ROW EXECUTE FUNCTION reject_journal_mutation();

-- +goose Down
DROP TRIGGER journal_no_update ON journal_entries;
DROP FUNCTION reject_journal_mutation;
DROP TABLE journal_entries;
DROP TABLE journal_heads;
