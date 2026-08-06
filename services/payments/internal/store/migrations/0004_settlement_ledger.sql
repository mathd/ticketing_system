-- The settlement ledger (TKT-217 / ADR-048): who is owed what out of a capture.
--
-- One identity holds it together, and it holds for every input:
--
--     (face - absorbed) + passed_on + absorbed  =  face + passed_on  =  captured
--
-- Absorbed fees appear on BOTH sides: they reduce the organizer's line and are
-- still owed to a payee, out of money the buyer already paid. A ledger that
-- records only passed-on fees balances against the charge and is wrong about who
-- earned what.
--
-- Name the adversary (ADR-021): HONEST-WRITER CONSISTENCY, not tamper-evidence.
-- These rows are NOT in the journal's hash chain. A writer with payments database
-- access can drop the triggers below and rewrite any row. What this schema stops
-- is an application bug and an operator mistake. Joining the chain is a real
-- option and is named as future work in ADR-048; it is not claimed here.
-- +goose Up
CREATE TABLE settlement_entries (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id    uuid NOT NULL,
    order_id        uuid NOT NULL,
    -- The captured journal fact this settles. The ledger explains ONE money
    -- movement, and this is which one.
    capture_fact_id uuid NOT NULL REFERENCES journal_entries (fact_id),
    entry_kind      text NOT NULL CHECK (entry_kind IN ('face_value', 'fee')),
    -- Payee identity is SNAPSHOTTED, not referenced. A payee's display name or
    -- external reference is editable in catalog, and a settlement row must keep
    -- saying who was paid at the time they were paid -- the same discipline the
    -- price and fee snapshots already apply. There is deliberately no foreign
    -- key: catalog is a different service and a different database.
    payee_id           uuid,
    payee_kind         text,
    payee_display_name text,
    payee_external_ref text,
    fee_code           text,
    incidence          text CHECK (incidence IS NULL OR incidence IN ('passed_on', 'absorbed')),
    -- SIGNED. The organizer's line goes negative when they absorbed more than
    -- the face value -- a real if misconfigured sale, and the ledger's job is to
    -- be true rather than to police configuration. Fee lines are non-negative:
    -- a payee is never owed a negative amount.
    amount   bigint NOT NULL,
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- The two shapes, enforced rather than described: a fee line names a payee,
    -- a fee code and an incidence; the organizer's line names none of them.
    CONSTRAINT settlement_entries_shape CHECK (
        (entry_kind = 'fee'
             AND payee_id IS NOT NULL AND payee_kind IS NOT NULL
             AND payee_display_name IS NOT NULL AND fee_code IS NOT NULL
             AND incidence IS NOT NULL AND amount >= 0)
        OR (entry_kind = 'face_value'
             AND payee_id IS NULL AND payee_kind IS NULL
             AND payee_display_name IS NULL AND payee_external_ref IS NULL
             AND fee_code IS NULL AND incidence IS NULL)
    ),
    -- One line per payee per fee code per capture. A duplicate would double-pay.
    UNIQUE (capture_fact_id, entry_kind, payee_id, fee_code)
);

CREATE INDEX settlement_entries_order ON settlement_entries (organizer_id, order_id);

-- Append-only, copied VERBATIM from journal_entries (0001_journal.sql) rather
-- than written afresh. TKT-216 wrote a new integrity trigger from first
-- principles in this same repo and shipped a TRUNCATE hole that review had to
-- find; row-level triggers do not fire on TRUNCATE, so the statement-level one
-- is not optional.
-- +goose StatementBegin
CREATE FUNCTION reject_settlement_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'settlement entries are append-only'; END $$;
-- +goose StatementEnd
CREATE TRIGGER settlement_no_update BEFORE UPDATE OR DELETE ON settlement_entries
FOR EACH ROW EXECUTE FUNCTION reject_settlement_mutation();
CREATE TRIGGER settlement_no_truncate BEFORE TRUNCATE ON settlement_entries
FOR EACH STATEMENT EXECUTE FUNCTION reject_settlement_mutation();

-- The set-balance rule. A per-row CHECK cannot see a set, and the application
-- builder can be bypassed by direct SQL, so this is the level that makes
-- "every capture is fully attributed" true of the DATABASE.
--
-- DEFERRED: the entries and the capture fact are written in one transaction, so
-- the set is incomplete for the whole of it. Checking per statement would make
-- writing the ledger impossible.
--
-- numeric aggregation, not bigint: the organizer line is signed and fee lines
-- can be large, and a sum that overflowed while checking for imbalance would be
-- the worst possible failure of this constraint.
-- +goose StatementBegin
CREATE FUNCTION settlement_must_balance() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    fact     uuid;
    captured bigint;
    total    numeric;
    rows_n   integer;
BEGIN
    fact := NEW.capture_fact_id;
    SELECT amount INTO captured FROM journal_entries WHERE fact_id = fact;
    IF captured IS NULL THEN
        RAISE EXCEPTION 'settlement % references no journal fact', fact;
    END IF;
    SELECT COALESCE(sum(amount), 0), count(*) INTO total, rows_n
      FROM settlement_entries WHERE capture_fact_id = fact;
    IF rows_n = 0 THEN
        RAISE EXCEPTION 'settlement set for % is empty', fact;
    END IF;
    IF total <> captured THEN
        RAISE EXCEPTION 'settlement for % sums to %, but % was captured', fact, total, captured;
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER settlement_entries_balance
    AFTER INSERT ON settlement_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION settlement_must_balance();

-- And the other direction: a CAPTURE with no settlement at all. Without this,
-- the invariant only holds for captures that happened to write entries -- which
-- is the state this ticket exists to make unreachable. It constrains the generic
-- /internal/facts endpoint too, which allowlists payment.captured; that is
-- correct rather than incidental, because a captured fact with no settlement is
-- exactly what is being forbidden, wherever it comes from.
-- +goose StatementBegin
CREATE FUNCTION capture_must_settle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.fact_type <> 'payment.captured' THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM settlement_entries WHERE capture_fact_id = NEW.fact_id) THEN
        RAISE EXCEPTION 'captured fact % has no settlement entries', NEW.fact_id;
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER journal_capture_must_settle
    AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION capture_must_settle();

-- +goose Down
LOCK TABLE settlement_entries IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM settlement_entries) THEN
        RAISE EXCEPTION 'cannot roll back 0004: settlement entries exist';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER journal_capture_must_settle ON journal_entries;
DROP FUNCTION capture_must_settle;
DROP TRIGGER settlement_entries_balance ON settlement_entries;
DROP FUNCTION settlement_must_balance;
DROP TRIGGER settlement_no_truncate ON settlement_entries;
DROP TRIGGER settlement_no_update ON settlement_entries;
DROP FUNCTION reject_settlement_mutation;
DROP INDEX settlement_entries_order;
DROP TABLE settlement_entries;
