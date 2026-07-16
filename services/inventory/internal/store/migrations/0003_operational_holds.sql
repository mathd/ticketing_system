-- +goose Up
-- Operational holds (TKT-77 / ADR-023): claims gain a kind; operational claims have no
-- TTL (expires_at NULL) and carry a named purpose. The kind-shape check makes NULL expiry
-- impossible on buyer claims, so every expiry predicate can key on expires_at alone.
ALTER TABLE claims ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE claims ADD COLUMN claim_kind text NOT NULL DEFAULT 'buyer' CHECK (claim_kind IN ('buyer','operational'));
ALTER TABLE claims ADD COLUMN operational_purpose text;
ALTER TABLE claims ADD COLUMN operational_label text;
ALTER TABLE claims ADD CONSTRAINT claims_kind_shape CHECK (
  (claim_kind = 'buyer' AND expires_at IS NOT NULL AND operational_purpose IS NULL AND operational_label IS NULL)
  OR
  (claim_kind = 'operational' AND expires_at IS NULL
    AND operational_purpose IN ('house','artist','kill','other')
    AND operational_label IS NOT NULL AND length(trim(operational_label)) > 0)
);

-- Append-only history of claim mutations. History begins at this migration; earlier
-- claims carry no baseline rows. Application-level append-only guard only — this is an
-- audit convenience, not tamper evidence against a database writer (ADR-021).
CREATE TABLE claim_history (
    id uuid PRIMARY KEY,
    organizer_id uuid NOT NULL,
    claim_id uuid NOT NULL REFERENCES claims(id),
    related_claim_id uuid REFERENCES claims(id),
    action text NOT NULL CHECK (action IN ('create','place','release','convert','finalize','confirm','expire')),
    actor text NOT NULL CHECK (length(trim(actor)) > 0),
    reason text NOT NULL CHECK (length(trim(reason)) > 0),
    quantity integer NOT NULL CHECK (quantity > 0),
    quantity_after integer NOT NULL CHECK (quantity_after >= 0),
    status_after text NOT NULL,
    idempotency_key text,
    request_fingerprint text,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX claim_history_claim ON claim_history(claim_id, occurred_at);
-- The registry for operational mutations (place/release/convert): staff idempotency keys
-- live here, not on claim rows, so they cannot collide with the buyer-hold key namespace.
CREATE UNIQUE INDEX claim_history_request ON claim_history(organizer_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION claim_history_append_only() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'claim_history is append-only';
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER claim_history_no_rewrite
  BEFORE UPDATE OR DELETE ON claim_history
  FOR EACH ROW EXECUTE FUNCTION claim_history_append_only();

-- +goose Down
DROP TRIGGER claim_history_no_rewrite ON claim_history;
DROP FUNCTION claim_history_append_only();
DROP TABLE claim_history;
-- Refuse to silently discard operational reservations.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM claims WHERE claim_kind = 'operational') THEN
    RAISE EXCEPTION 'operational claims exist; resolve them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE claims DROP CONSTRAINT claims_kind_shape;
ALTER TABLE claims DROP COLUMN operational_label;
ALTER TABLE claims DROP COLUMN operational_purpose;
ALTER TABLE claims DROP COLUMN claim_kind;
ALTER TABLE claims ALTER COLUMN expires_at SET NOT NULL;
