-- +goose Up
-- Group/agency reservations (TKT-79 / ADR-027): a third claim kind with an explicit
-- expiry (not the cart TTL) and a named counterparty. Expiry rides the buyer lifecycle —
-- liveClaims keys on expires_at alone — so unconverted quantity returns to sale lazily,
-- by DB time, like any hold. Reservations may carry a channel; draw-down children
-- inherit it.
ALTER TABLE claims ADD COLUMN reservation_counterparty text;
ALTER TABLE claims DROP CONSTRAINT claims_claim_kind_check;
ALTER TABLE claims ADD CONSTRAINT claims_claim_kind_check CHECK (claim_kind IN ('buyer','operational','reservation'));
ALTER TABLE claims DROP CONSTRAINT claims_kind_shape;
ALTER TABLE claims ADD CONSTRAINT claims_kind_shape CHECK (
  (claim_kind = 'buyer' AND expires_at IS NOT NULL AND operational_purpose IS NULL AND operational_label IS NULL
    AND reservation_counterparty IS NULL)
  OR
  (claim_kind = 'operational' AND expires_at IS NULL
    AND operational_purpose IN ('house','artist','kill','other')
    AND operational_label IS NOT NULL AND length(trim(operational_label)) > 0
    AND channel_code IS NULL AND reservation_counterparty IS NULL)
  OR
  (claim_kind = 'reservation' AND expires_at IS NOT NULL
    AND reservation_counterparty IS NOT NULL AND length(trim(reservation_counterparty)) > 0
    AND operational_purpose IS NULL AND operational_label IS NULL)
);
ALTER TABLE claim_history
    DROP CONSTRAINT claim_history_action_check,
    ADD CONSTRAINT claim_history_action_check
        CHECK (action IN ('create','place','release','convert','finalize','confirm','expire','adjust_capacity','reserve','draw_down'));

-- +goose Down
-- Refuse to silently discard group reservations or their audit trail.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM claims WHERE claim_kind = 'reservation')
     OR EXISTS (SELECT 1 FROM claim_history WHERE action IN ('reserve','draw_down')) THEN
    RAISE EXCEPTION 'group reservations exist; resolve them before downgrading';
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE claim_history
    DROP CONSTRAINT claim_history_action_check,
    ADD CONSTRAINT claim_history_action_check
        CHECK (action IN ('create','place','release','convert','finalize','confirm','expire','adjust_capacity'));
ALTER TABLE claims DROP CONSTRAINT claims_kind_shape;
ALTER TABLE claims ADD CONSTRAINT claims_kind_shape CHECK (
  (claim_kind = 'buyer' AND expires_at IS NOT NULL AND operational_purpose IS NULL AND operational_label IS NULL)
  OR
  (claim_kind = 'operational' AND expires_at IS NULL
    AND operational_purpose IN ('house','artist','kill','other')
    AND operational_label IS NOT NULL AND length(trim(operational_label)) > 0
    AND channel_code IS NULL)
);
ALTER TABLE claims DROP CONSTRAINT claims_claim_kind_check;
ALTER TABLE claims ADD CONSTRAINT claims_claim_kind_check CHECK (claim_kind IN ('buyer','operational'));
ALTER TABLE claims DROP COLUMN reservation_counterparty;
