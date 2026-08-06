-- Payees and split schedules (TKT-216 / ADR-047). Who a fee is owed to, and in
-- what share.
--
-- The shares are the point. Money is whole cents and shares are basis points,
-- and the two do not divide: a 1¢ fee at 3333/3333/3334 floors to 0/0/0 and
-- loses the cent. The allocator (services/payments/internal/splits) is what
-- makes the arithmetic exact; this schema is what makes an UNBALANCED set
-- impossible to author in the first place.
--
-- Name the adversary (ADR-021). The deferred trigger below is HONEST-WRITER
-- consistency: it stops an operator mistake and a buggy write path, not an
-- adversary. Anyone who can set `session_replication_role = replica`, or run
-- ALTER TABLE ... DISABLE TRIGGER, commits whatever they like. Do not describe
-- this as making imbalance impossible.
-- +goose Up

-- Kinds are ROWS, not a CHECK enum, and that is a decision (ADR-047 §2). A
-- referenced kind must exist, so a typo cannot silently route money to a
-- category nobody watches -- it fails the foreign key instead. Adding a kind is
-- then data rather than a migration, which is what "extensible" in the ticket
-- means. The residual risk is misclassification, never arithmetic.
CREATE TABLE payee_kinds (
    code  text PRIMARY KEY CHECK (length(code) BETWEEN 1 AND 40),
    label text NOT NULL CHECK (length(label) BETWEEN 1 AND 200)
);
INSERT INTO payee_kinds (code, label) VALUES
    ('system', 'The platform'),
    ('venue',  'The venue'),
    ('artist', 'The performer or rights holder');

CREATE TABLE payees (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id       uuid NOT NULL REFERENCES organizers (id),
    kind               text NOT NULL REFERENCES payee_kinds (code),
    display_name       text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    -- The operator's own identifier for this payee in whatever system actually
    -- pays them. Opaque here on purpose: payout execution is not this epic's.
    external_reference text CHECK (external_reference IS NULL OR length(external_reference) BETWEEN 1 AND 200),
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- The tenant key a composite foreign key can point at, so a schedule cannot
    -- reference another organizer's payee (see split_schedule_parts).
    UNIQUE (id, organizer_id)
);

CREATE TABLE split_schedules (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organizer_id            uuid NOT NULL REFERENCES organizers (id),
    -- Same five levels, same derivation, same comparator as fee rules
    -- (ADR-036 §1). A split resolves through the hierarchy exactly as the fee
    -- it splits does.
    scope_level             text NOT NULL
        CHECK (scope_level IN ('ticket_type', 'slot', 'series', 'event', 'venue')),
    -- No FK: the target table depends on scope_level. Paid back at the write
    -- path by the same INSERT ... SELECT gate price and fee rules use.
    scope_id                uuid NOT NULL,
    fee_code                text NOT NULL CHECK (length(fee_code) BETWEEN 1 AND 64),
    -- No currency column. Shares are basis points, which are
    -- currency-independent; a column no code reads would only invite a
    -- mismatch check nobody needs.
    channel_code            text CHECK (channel_code IS NULL OR length(channel_code) BETWEEN 1 AND 100),
    priority                integer NOT NULL DEFAULT 0,
    force_ancestor_override boolean NOT NULL DEFAULT false,
    effective_from          timestamptz,
    effective_until         timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT split_schedules_effective_window_check CHECK (
        effective_from IS NULL OR effective_until IS NULL OR effective_from < effective_until
    ),
    UNIQUE (id, organizer_id)
);

CREATE TABLE split_schedule_parts (
    schedule_id  uuid NOT NULL,
    payee_id     uuid NOT NULL,
    organizer_id uuid NOT NULL,
    share_bps    integer NOT NULL CHECK (share_bps >= 0 AND share_bps <= 10000),
    PRIMARY KEY (schedule_id, payee_id),
    -- COMPOSITE foreign keys, carrying organizer_id into both: without them a
    -- schedule could name another organizer's payee, and settlement would pay
    -- money to a stranger. The tenant is part of the reference, not a field
    -- somebody remembers to check.
    FOREIGN KEY (schedule_id, organizer_id) REFERENCES split_schedules (id, organizer_id) ON DELETE CASCADE,
    FOREIGN KEY (payee_id, organizer_id)    REFERENCES payees (id, organizer_id)
);

-- The resolution predicate matches (scope_level, scope_id) PAIRS, never
-- scope_id alone: UUID uniqueness is per table (ADR-036 §3). Plain CREATE
-- INDEX (ADR-020).
CREATE INDEX split_schedules_scope ON split_schedules (organizer_id, scope_level, scope_id);
-- Parts are always read for a known schedule, so the primary key already serves
-- them; no second index.

-- The balance rule, enforced for EVERY writer rather than only the handler.
--
-- DEFERRED, and it has to be: a schedule is a header plus N parts, so it is
-- unbalanced for the whole of its own creating transaction. Checking per
-- statement would make authoring impossible. Checking at COMMIT is what makes
-- "no unbalanced schedule exists" true of the database rather than of one code
-- path.
-- +goose StatementBegin
CREATE FUNCTION split_schedule_check_one(target uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    total integer;
    parts integer;
BEGIN
    IF target IS NULL THEN
        RETURN;
    END IF;
    -- The schedule may have been deleted in this transaction; then there is
    -- nothing to balance and the cascade has already removed its parts.
    IF NOT EXISTS (SELECT 1 FROM split_schedules WHERE id = target) THEN
        RETURN;
    END IF;
    SELECT COALESCE(sum(share_bps), 0), count(*) INTO total, parts
      FROM split_schedule_parts WHERE schedule_id = target;
    IF parts = 0 THEN
        RAISE EXCEPTION 'split schedule % has no parts', target;
    END IF;
    IF total <> 10000 THEN
        RAISE EXCEPTION 'split schedule % shares sum to %, not 10000', target, total;
    END IF;
END $$;

CREATE FUNCTION split_schedule_must_balance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- BOTH sides, and this is the whole point (TKT-216 ai-review, [high]).
    --
    -- The first version validated COALESCE(NEW.schedule_id, OLD.schedule_id),
    -- which on an UPDATE is always the DESTINATION. Moving a part from A to B
    -- therefore never revalidated A, and one ordinary transaction could commit
    -- an unbalanced schedule:
    --
    --   A={P1:5000,P2:5000}  B={P3:5000,P4:5000}
    --   move P2 A->B   (validates B, now 15000)
    --   delete B/P4    (validates B, now 10000 -- passes)
    --   commit         -> A is 5000 and was never checked
    --
    -- Reproduced against a real database before fixing. Every key stayed valid;
    -- what broke was the guarantee this trigger exists to make, and settlement
    -- would have been handed a snapshot its allocator refuses.
    PERFORM split_schedule_check_one(NEW.schedule_id);
    IF OLD.schedule_id IS DISTINCT FROM NEW.schedule_id THEN
        PERFORM split_schedule_check_one(OLD.schedule_id);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER split_schedule_parts_balance
    AFTER INSERT OR UPDATE OR DELETE ON split_schedule_parts
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION split_schedule_must_balance();

-- A header with NO parts at all would never fire the row trigger above, so it
-- gets its own: an empty schedule is not "unsplit", it is a schedule that
-- resolves to nothing while looking authored.
-- +goose StatementBegin
CREATE FUNCTION split_schedule_header_must_have_parts() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM split_schedules WHERE id = NEW.id) THEN
        RETURN NULL; -- created and removed inside one transaction
    END IF;
    IF NOT EXISTS (SELECT 1 FROM split_schedule_parts WHERE schedule_id = NEW.id) THEN
        RAISE EXCEPTION 'split schedule % has no parts', NEW.id;
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER split_schedules_have_parts
    AFTER INSERT ON split_schedules
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION split_schedule_header_must_have_parts();

-- +goose Down
LOCK TABLE split_schedules IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM split_schedules) OR EXISTS (SELECT 1 FROM payees) THEN
        RAISE EXCEPTION 'cannot roll back 0017: payee or split-schedule data exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER split_schedules_have_parts ON split_schedules;
DROP FUNCTION split_schedule_header_must_have_parts;
DROP TRIGGER split_schedule_parts_balance ON split_schedule_parts;
DROP FUNCTION split_schedule_must_balance;
DROP FUNCTION split_schedule_check_one;
DROP INDEX split_schedules_scope;
DROP TABLE split_schedule_parts;
DROP TABLE split_schedules;
DROP TABLE payees;
DROP TABLE payee_kinds;
