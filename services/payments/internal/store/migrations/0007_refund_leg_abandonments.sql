-- TKT-525 (ADR-087): the append-only abandonment marker for one post-purchase refund leg.
--
-- A leg whose provider refund failed or was canceled stays `bound`: payments never re-submits a
-- failed refund (ADR-032 §Refund rule 3), and its allowance stays reserved against the
-- captured-money ceiling. An operator who establishes that nobody will refund that attempt
-- records `abandoned` through commerce (TKT-525 S2). This marker is the payments half of that
-- record. The leg row itself is not modified: it stays bound, with its provider key, so the
-- history of what was attempted is intact.
--
-- TKT-525 S1 creates the table and READS it: the read-only failure proof refuses a leg that
-- carries a marker, so an outside completion cannot follow an abandonment that payments has
-- already committed. S2 writes it.
--
-- Keyed by, and linked to, the exact composite leg key. One marker per leg.
--
-- ADR-021: honest-writer evidence, not tamper-evidence. A writer with payments database access
-- can forge or drop it, the same scope as payment_refund_legs itself.

-- +goose Up
-- Locks in the application's order: the operation row, then its legs (BindRefundLeg locks the
-- payment_operations row FOR UPDATE and then inserts the leg). The marker table does not exist
-- yet.
LOCK TABLE payment_operations, payment_refund_legs IN ACCESS EXCLUSIVE MODE;

CREATE TABLE payment_refund_leg_abandonments (
  organizer_id uuid NOT NULL,
  source_idempotency_key text NOT NULL,
  refund_idempotency_key text NOT NULL,
  -- The provider's own reference for the failed or canceled refund, corroborated against the
  -- leg's provider key, amount and currency before the marker is written.
  provider_ref text NOT NULL CONSTRAINT payment_refund_leg_abandonments_provider_ref_check
    CHECK (length(provider_ref) BETWEEN 1 AND 255 AND btrim(provider_ref) <> ''),
  abandoned_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payment_refund_leg_abandonments_pkey
    PRIMARY KEY (organizer_id, source_idempotency_key, refund_idempotency_key),
  CONSTRAINT payment_refund_leg_abandonments_leg_fkey
    FOREIGN KEY (organizer_id, source_idempotency_key, refund_idempotency_key)
    REFERENCES payment_refund_legs (organizer_id, source_idempotency_key, refund_idempotency_key)
);

-- Append-only, copied from journal_entries (0001) and settlement_entries (0004). Row-level
-- triggers do not fire on TRUNCATE, so the statement-level one is not optional.
-- +goose StatementBegin
CREATE FUNCTION reject_refund_leg_abandonment_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'refund leg abandonments are append-only'; END $$;
-- +goose StatementEnd
CREATE TRIGGER refund_leg_abandonments_no_update BEFORE UPDATE OR DELETE ON payment_refund_leg_abandonments
FOR EACH ROW EXECUTE FUNCTION reject_refund_leg_abandonment_mutation();
CREATE TRIGGER refund_leg_abandonments_no_truncate BEFORE TRUNCATE ON payment_refund_leg_abandonments
FOR EACH STATEMENT EXECUTE FUNCTION reject_refund_leg_abandonment_mutation();

-- +goose Down
-- Lock before the guard, in the Up's order and then the marker table. Checking first leaves a
-- window in which a marker commits between the check and the DROP, and the marker is the only
-- payments record that an allowance was released on an operator's word.
LOCK TABLE payment_operations, payment_refund_legs, payment_refund_leg_abandonments IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM payment_refund_leg_abandonments) THEN
    RAISE EXCEPTION 'cannot roll back 0007: refund leg abandonment markers exist; roll forward instead';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE payment_refund_leg_abandonments;
DROP FUNCTION reject_refund_leg_abandonment_mutation();
