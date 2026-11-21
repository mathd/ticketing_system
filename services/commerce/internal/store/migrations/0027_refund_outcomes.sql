-- Operator outcomes for failed post-purchase refunds (TKT-525, ADR-087).
--
-- A staff or bulk refund whose provider refund failed stays `pending` forever: its leg stays
-- bound in payments, both allowances stay reserved, and nothing in the service can finish it.
-- The owner decision of 2026-10-12 adds an operator command that writes a NEW append-only
-- record of what a human established outside the platform. The failed leg is not modified.
-- This migration adds that record and the one completion path it may evidence.
--
-- TWO OUTCOME KINDS, ONE TABLE.
--
--   refunded_outside  The money went back to the buyer by a route outside the platform. The
--                     refund completes with this outcome as its evidence (no payments fact),
--                     both allowances stay consumed, and the reversal reconciler drives the
--                     tickets and the capacity as for any completed refund.
--   abandoned         Nobody will refund this attempt. The refund stays `pending`. TKT-525 S2
--                     writes this kind; the vocabulary is here so the table is created once.
--
-- GENERIC SUBJECT, NO FOREIGN KEY TO order_refunds. The key is
-- (organizer_id, subject_kind, subject_id). TKT-525 writes only subject_kind='order_refund'.
-- TKT-532 (recovery) and TKT-533 (exchange) can add their own subject kinds without a second
-- outcome table. The link goes the other way: an outside completion on order_refunds names its
-- outcome through a composite foreign key, below.
--
-- Money is BIGINT minor units + ISO currency; floats are banned on money paths.
--
-- ADR-021, name the adversary: this is evidence of what an HONEST operator attested to. A
-- writer with commerce database access can forge a row, disable the triggers, or drop the
-- table, and a coordinated rollback can erase it. `operator_name` is the typed name and is the
-- only personal attribution; `session_user_name` is the PostgreSQL login, which on the
-- documented access path is the shared `commerce` service login (ADR-087). This migration
-- references no database role.

-- +goose Up
-- Lock ONLY order_refunds (TKT-525 D16). refund_outcomes does not exist yet, and nothing else is
-- touched. The live completion path (CompleteOrderRefund) locks the refund row and THEN the order
-- row, so a migration that took orders first could hold orders while a completion holds a refund
-- row and waits on the order: a lock cycle. Out-of-band placement (ADR-022) does not quiesce an
-- older revision that is still serving, so the migration must not depend on that.
LOCK TABLE order_refunds IN ACCESS EXCLUSIVE MODE;

CREATE TABLE refund_outcomes (
  organizer_id uuid NOT NULL,
  subject_kind text NOT NULL
    CONSTRAINT refund_outcomes_subject_kind_check CHECK (subject_kind IN ('order_refund')),
  subject_id uuid NOT NULL,
  kind text NOT NULL
    CONSTRAINT refund_outcomes_kind_check CHECK (kind IN ('refunded_outside', 'abandoned')),
  -- Copied from the subject by the command, never typed by the operator: the outcome covers the
  -- whole stored refund or nothing.
  amount bigint NOT NULL CONSTRAINT refund_outcomes_amount_check CHECK (amount > 0),
  currency varchar(3) NOT NULL
    CONSTRAINT refund_outcomes_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
  -- refunded_outside: the outside transfer reference the operator typed.
  -- abandoned: the provider refund reference payments returned (S2).
  external_ref text NOT NULL CONSTRAINT refund_outcomes_external_ref_check
    CHECK (length(external_ref) BETWEEN 1 AND 200 AND btrim(external_ref) <> ''),
  operator_name text NOT NULL CONSTRAINT refund_outcomes_operator_name_check
    CHECK (length(operator_name) BETWEEN 1 AND 200 AND btrim(operator_name) <> ''),
  -- Written from `session_user` on the inserting connection, never from an argument.
  session_user_name text NOT NULL CONSTRAINT refund_outcomes_session_user_name_check
    CHECK (length(session_user_name) BETWEEN 1 AND 200),
  reason text NOT NULL CONSTRAINT refund_outcomes_reason_check
    CHECK (length(reason) BETWEEN 1 AND 500 AND btrim(reason) <> ''),
  recorded_at timestamptz NOT NULL DEFAULT now(),
  -- One outcome per subject. A second, different disposition is a conflict to refuse, not a
  -- history to append.
  CONSTRAINT refund_outcomes_pkey PRIMARY KEY (organizer_id, subject_kind, subject_id),
  -- The foreign-key target for an outside completion. It includes `kind`, so only an outcome of
  -- the kind the refund names can evidence it.
  CONSTRAINT refund_outcomes_evidence_key UNIQUE (organizer_id, subject_kind, subject_id, kind)
);

-- Append-only, the same shape as payments' journal_entries and settlement_entries. Row-level
-- triggers do not fire on TRUNCATE, so the statement-level one is not optional.
-- +goose StatementBegin
CREATE FUNCTION reject_refund_outcome_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'refund outcomes are append-only'; END $$;
-- +goose StatementEnd
CREATE TRIGGER refund_outcomes_no_update BEFORE UPDATE OR DELETE ON refund_outcomes
FOR EACH ROW EXECUTE FUNCTION reject_refund_outcome_mutation();
CREATE TRIGGER refund_outcomes_no_truncate BEFORE TRUNCATE ON refund_outcomes
FOR EACH STATEMENT EXECUTE FUNCTION reject_refund_outcome_mutation();

-- Drop 0007's completion CHECK. 0007 left it unnamed, so PostgreSQL named it, and that name is
-- an artifact of declaration order. Find it by what it IS: a CHECK on order_refunds whose
-- referenced columns are exactly {status, completed_at, payment_fact_id}. The status vocabulary
-- CHECK references only `status` and stays. Exactly one match, or the migration stops: dropping
-- the wrong constraint, or none, would leave a completion rule nobody chose.
-- +goose StatementBegin
DO $$
DECLARE matches text[];
BEGIN
    SELECT array_agg(c.conname::text) INTO matches
    FROM pg_constraint c
    WHERE c.conrelid = 'order_refunds'::regclass
      AND c.contype = 'c'
      AND (SELECT array_agg(a.attname::text ORDER BY a.attname::text)
             FROM pg_attribute a
            WHERE a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey))
          = ARRAY['completed_at', 'payment_fact_id', 'status'];
    IF coalesce(cardinality(matches), 0) <> 1 THEN
        RAISE EXCEPTION 'cannot apply 0027: expected exactly one completion CHECK on order_refunds over (status, completed_at, payment_fact_id), found %',
            coalesce(cardinality(matches), 0);
    END IF;
    EXECUTE format('ALTER TABLE order_refunds DROP CONSTRAINT %I', matches[1]);
END $$;
-- +goose StatementEnd

-- A plain column with a constant default, not a generated column. PostgreSQL 11+ adds a
-- constant default without a table rewrite; a STORED generated column rewrites the table under
-- ACCESS EXCLUSIVE. The CHECK holds it to the one value it can have, so it carries the subject
-- kind into the foreign key below.
ALTER TABLE order_refunds
  ADD COLUMN outcome_subject_kind text NOT NULL DEFAULT 'order_refund'
    CONSTRAINT order_refunds_outcome_subject_kind_check CHECK (outcome_subject_kind = 'order_refund'),
  -- NULL unless the refund completed by an operator outcome. MATCH SIMPLE: a NULL here skips the
  -- foreign key, so every ordinary row is unaffected.
  ADD COLUMN outside_completion_kind text,
  ADD CONSTRAINT order_refunds_outside_completion_outcome_fkey
    FOREIGN KEY (organizer_id, outcome_subject_kind, id, outside_completion_kind)
    REFERENCES refund_outcomes (organizer_id, subject_kind, subject_id, kind),
  -- Three admissible states, each with explicit NULL tests:
  --   pending:                 no completion time and no evidence of either kind.
  --   completed by payments:   a completion time and a payments fact, no operator outcome.
  --   completed outside:       a completion time and a matching `refunded_outside` outcome,
  --                            no payments fact.
  -- An `abandoned` outcome cannot appear here: it leaves its refund pending.
  --
  -- The third branch tests `outside_completion_kind IS NOT NULL` before comparing it. Without
  -- that, a completed row with neither fact nor kind makes the comparison NULL, the whole
  -- expression NULL, and a CHECK accepts NULL: the evidence-free completion this rule exists
  -- to refuse would pass (found by TestMigration0027CompletionEvidenceRule).
  ADD CONSTRAINT order_refunds_completion_evidence CHECK (
    (status = 'pending' AND completed_at IS NULL
      AND payment_fact_id IS NULL AND outside_completion_kind IS NULL)
    OR
    (status = 'completed' AND completed_at IS NOT NULL
      AND payment_fact_id IS NOT NULL AND outside_completion_kind IS NULL)
    OR
    (status = 'completed' AND completed_at IS NOT NULL
      AND payment_fact_id IS NULL AND outside_completion_kind IS NOT NULL
      AND outside_completion_kind = 'refunded_outside')
  );

-- +goose Down
-- Lock BEFORE the history check, in the Up's order: order_refunds, then refund_outcomes.
-- Checking first leaves a window in which an outcome commits between the check and the DROP,
-- and a guard that then destroys the only record of an operator's money attestation is worse
-- than none.
LOCK TABLE order_refunds, refund_outcomes IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM refund_outcomes) THEN
    RAISE EXCEPTION 'cannot roll back 0027: refund outcome history exists; roll forward instead';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE order_refunds
  DROP CONSTRAINT order_refunds_completion_evidence,
  DROP CONSTRAINT order_refunds_outside_completion_outcome_fkey,
  DROP COLUMN outside_completion_kind,
  DROP COLUMN outcome_subject_kind,
  -- 0007's completion rule, restored with an explicit name. With no outcome history no row can
  -- be an outside completion (the foreign key needs an outcome), so every row satisfies it.
  ADD CONSTRAINT order_refunds_completion_check CHECK (
    (status = 'pending' AND completed_at IS NULL AND payment_fact_id IS NULL)
    OR
    (status = 'completed' AND completed_at IS NOT NULL AND payment_fact_id IS NOT NULL)
  );
DROP TABLE refund_outcomes;
DROP FUNCTION reject_refund_outcome_mutation();
