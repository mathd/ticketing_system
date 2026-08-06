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
    -- 'legacy_unattributed' exists only for captures that predate this migration.
    -- Their composition is not recoverable -- the journal records the amount, not
    -- which part was face value and which was owed to whom -- so the backfill at
    -- the bottom records what IS true: the whole amount, attributed to nobody.
    -- Guessing would be worse than admitting, and omitting them would make the
    -- invariant a claim about future writes rather than about the table.
    entry_kind      text NOT NULL CHECK (entry_kind IN ('face_value', 'fee', 'legacy_unattributed')),
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
    -- A fee entry with NO payee is money collected and UNATTRIBUTED: a fee whose
    -- code had no split schedule when the sale happened. It is a real state, not
    -- a defect to refuse -- TKT-215 shipped fees before TKT-216 shipped
    -- schedules, so every fee sold in that window is exactly this. Refusing it
    -- would have broken those sales at CHECKOUT, after the buyer committed.
    -- Recording it keeps the ledger balanced AND makes the gap queryable, which
    -- is what an operator needs in order to fix it.
    CONSTRAINT settlement_entries_shape CHECK (
        (entry_kind = 'fee'
             AND fee_code IS NOT NULL AND incidence IS NOT NULL AND amount >= 0
             AND ((payee_id IS NOT NULL AND payee_kind IS NOT NULL AND payee_display_name IS NOT NULL)
                  OR (payee_id IS NULL AND payee_kind IS NULL AND payee_display_name IS NULL
                      AND payee_external_ref IS NULL)))
        OR (entry_kind IN ('face_value', 'legacy_unattributed')
             AND payee_id IS NULL AND payee_kind IS NULL
             AND payee_display_name IS NULL AND payee_external_ref IS NULL
             AND fee_code IS NULL AND incidence IS NULL)
    ),
    -- One line per payee per fee code per capture. A duplicate would double-pay.
    --
    -- NULLS NOT DISTINCT is load-bearing, not decoration. Ordinary UNIQUE treats
    -- NULLs as distinct, and the two rows this most needs to stop are exactly the
    -- ones with NULLs: a second face_value line (NULL payee, NULL fee code), and a
    -- second UNATTRIBUTED line for one fee code (NULL payee). Both are BALANCED --
    -- they can sum to the capture perfectly -- so the deferred balance trigger
    -- below cannot see them. A sum cannot see shape.
    UNIQUE NULLS NOT DISTINCT (capture_fact_id, entry_kind, payee_id, fee_code)
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
    fact       uuid;
    captured   bigint;
    total      numeric;
    rows_n     integer;
    f_type     text;
    f_org      uuid;
    f_currency char(3);
    f_order    text;
BEGIN
    fact := NEW.capture_fact_id;
    -- The foreign key says the fact EXISTS. It does not say the fact is a capture,
    -- nor that this ledger is about the same money: a balanced set hung off an
    -- authorization attributes money that never moved, and one naming another
    -- organizer, order or currency balances against a different unit entirely.
    -- "Settlement iff capture" is the claim, so the claim is what gets checked.
    SELECT amount, fact_type, organizer_id, currency, payload ->> 'order_id'
      INTO captured, f_type, f_org, f_currency, f_order
      FROM journal_entries WHERE fact_id = fact;
    IF captured IS NULL THEN
        RAISE EXCEPTION 'settlement % references no journal fact', fact;
    END IF;
    IF f_type <> 'payment.captured' THEN
        RAISE EXCEPTION 'settlement % attaches to a % fact, which moved no money', fact, f_type;
    END IF;
    IF NEW.organizer_id <> f_org OR NEW.currency <> f_currency OR NEW.order_id::text IS DISTINCT FROM f_order THEN
        RAISE EXCEPTION 'settlement % names organizer/order/currency %/%/%, but the fact says %/%/%',
            fact, NEW.organizer_id, NEW.order_id, NEW.currency, f_org, f_order, f_currency;
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

-- Captures that predate this ledger. The triggers above govern INSERT only, so
-- without this the invariant would hold for every future capture and be FALSE of
-- the table -- and "every captured cent is attributed" would be a claim about the
-- code rather than about the data.
--
-- Refusing to apply instead was the first attempt and it was wrong: any database
-- that has ever completed a checkout holds such facts, so the migration bricked
-- every real upgrade. Adversarial review caught it.
--
-- These rows are validated by the SAME deferred balance trigger as everything
-- else -- one line per capture, for the full captured amount -- so the backfill
-- cannot quietly produce an unbalanced ledger.
-- The order_id is read from a jsonb payload, and 0001 never constrained payloads
-- to carry one. A capture written by /internal/facts, or by direct SQL, can have
-- it missing or malformed -- and an unguarded cast would abort the whole
-- migration with an opaque error, which is the bricking failure again wearing a
-- different hat. Backfill what is interpretable, then say precisely what was not.
-- +goose StatementBegin
DO $$
DECLARE unmappable bigint;
BEGIN
    INSERT INTO settlement_entries
          (organizer_id, order_id, capture_fact_id, entry_kind, amount, currency)
    SELECT organizer_id, (payload ->> 'order_id')::uuid, fact_id, 'legacy_unattributed', amount, currency
      FROM journal_entries
     WHERE fact_type = 'payment.captured'
       AND payload ->> 'order_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';

    -- Anything left is a captured fact this ledger cannot describe. That is a
    -- broken journal row, not a normal upgrade, so it stops the migration and
    -- names itself -- unlike the earlier blanket refusal, which stopped upgrades
    -- that were merely ordinary.
    SELECT count(*) INTO unmappable
      FROM journal_entries je
     WHERE je.fact_type = 'payment.captured'
       AND NOT EXISTS (SELECT 1 FROM settlement_entries se WHERE se.capture_fact_id = je.fact_id);
    IF unmappable > 0 THEN
        RAISE EXCEPTION 'cannot apply 0004: % captured fact(s) carry no usable order_id in their '
            'payload and cannot be attributed; inspect them before migrating', unmappable;
    END IF;
END $$;
-- +goose StatementEnd

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
