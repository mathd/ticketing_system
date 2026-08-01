-- Event-cancellation bulk refunds (TKT-159, ADR-040).
--
-- Two tables: a durable RUN, and a per-order LEDGER that is both the work queue and the
-- final report. They are the same rows on purpose — "exactly one recorded outcome per
-- order", resumability, and no-double-refund are then one mechanism instead of three that
-- can disagree.
--
-- The run is driven by a commerce background runner in bounded batches (the
-- `internal/recovery` lifecycle, copied rather than merged: checkout recovery and
-- cancellation reporting have different eligibility, terminal states and retention). No
-- transaction and no row lock spans the book, and no external call happens while any lock
-- is held.
--
-- Money is BIGINT minor units + ISO currency; floats are banned on money paths.
--
-- Scope of the guarantees below: honest-writer concurrency and crash/retry. These rows are
-- NOT tamper-evident — anyone with commerce database write access can alter them. The
-- signed, append-only payments journal is the evidence that money moved; ADR-021's limits
-- on it are unchanged by this migration.
-- +goose Up
CREATE TABLE cancellation_refund_runs (
  organizer_id uuid NOT NULL,
  id uuid NOT NULL,
  slot_id uuid NOT NULL,
  idempotency_key text NOT NULL,
  request_fingerprint text NOT NULL,
  -- The OPERATOR's attribution. Deliberately not what the individual refunds bind under:
  -- those use fixed constants, so a second run replays the first attempt instead of
  -- colliding on order_refunds.request_fingerprint, which covers actor and reason.
  actor text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
  reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed')),
  -- Bounds the RESERVATION set this run will page through, recorded in the same transaction
  -- as the row so the book is finite and durable before any work starts. It does NOT bound
  -- order completion: whether an order is completed is necessarily sampled when its page is
  -- read (see incomplete_at_enumeration below).
  cutoff_at timestamptz NOT NULL,
  -- Keyset cursor over the slot's reservations, advanced across the WHOLE page (including
  -- reservations with no completed order) so no later state change can leave a gap behind
  -- the cursor.
  cursor_created_at timestamptz,
  cursor_reservation_id uuid,
  enumeration_completed_at timestamptz,
  -- Orders on the slot that were NOT completed when their page was enumerated. Named for
  -- enumeration, not the cutoff: the cutoff bounds the RESERVATION set (which is what makes
  -- the run finite and resumable), while an order's completion is necessarily sampled when
  -- its page is read. An order that completes after its page has been passed is not in this
  -- run — that is what this count tells the operator.
  incomplete_at_enumeration integer NOT NULL DEFAULT 0 CHECK (incomplete_at_enumeration >= 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  PRIMARY KEY (organizer_id, id),
  UNIQUE (organizer_id, idempotency_key),
  CONSTRAINT cancellation_refund_runs_cursor_shape CHECK (
    (cursor_created_at IS NULL) = (cursor_reservation_id IS NULL)
  ),
  -- `completed` is not a label the runner can apply early: it requires enumeration to be
  -- finished, and the runner only stamps completed_at once no row is left unresolved.
  CONSTRAINT cancellation_refund_runs_completion_shape CHECK (
    (status = 'completed') = (completed_at IS NOT NULL)
    AND (completed_at IS NULL OR enumeration_completed_at IS NOT NULL)
  )
);

-- The runner's queue: unfinished runs, oldest first.
CREATE INDEX cancellation_refund_runs_queue_idx
  ON cancellation_refund_runs (created_at, id) WHERE status <> 'completed';

CREATE TABLE cancellation_refund_orders (
  organizer_id uuid NOT NULL,
  run_id uuid NOT NULL,
  order_id uuid NOT NULL REFERENCES orders(id),
  currency varchar(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  -- Fixed before any external call and never recomputed. Recomputing after the money moved
  -- reads back a different remainder, which would change the refund's request fingerprint
  -- and turn a crash-resume into a 409 against its own earlier attempt.
  requested_quantity integer CHECK (requested_quantity > 0),
  refund_id uuid,
  outcome text CHECK (outcome IN ('refunded','already_refunded','failed')),
  failure_code text CHECK (failure_code IN ('no_captured_money','not_refundable','refund_refused','reversal_outstanding','unavailable','internal')),
  failure_reason text CHECK (length(failure_reason) BETWEEN 1 AND 500),
  -- A SNAPSHOT at outcome time, not a live projection: the report has to stay readable
  -- after a later refund of the same order moves the order's own numbers. Do not "fix"
  -- the apparent drift by joining to orders at read time.
  money_refunded boolean NOT NULL DEFAULT false,
  tickets_voided boolean NOT NULL DEFAULT false,
  capacity_returned boolean NOT NULL DEFAULT false,
  refunded_quantity integer NOT NULL DEFAULT 0 CHECK (refunded_quantity >= 0),
  refunded_amount bigint NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0),
  -- A lease, not a lock: written in a short claim transaction and committed BEFORE the
  -- provider call, so nothing holds a database lock across an external call.
  claim_id uuid,
  lease_until timestamptz,
  -- Charged per claim, and bounded. It exists for ONE class of failure: the ambiguous kind,
  -- where the money may or may not have moved (a provider timeout, an unavailable journal, a
  -- completion that did not persist). Finalizing those terminally is what strands money with
  -- the tickets still valid; retrying them forever is what stops a run from ever completing.
  -- A definite refusal is still terminal on the first attempt and never consumes this.
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  -- Recorded when this row first resolves its quantity: a completed cancellation refund for
  -- the order ALREADY existed, so a previous run refunded it and this one only replays it.
  -- Durable rather than inferred, because a retried attempt cannot tell "a previous run did
  -- this" from "this run did it before being interrupted" — and that difference is the whole
  -- distinction between `refunded` and `already_refunded`.
  prior_run boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  -- One row per order per run: a second outcome for the same order in the same run is not
  -- rejected by review, it is impossible.
  PRIMARY KEY (organizer_id, run_id, order_id),
  FOREIGN KEY (organizer_id, run_id) REFERENCES cancellation_refund_runs(organizer_id, id),
  CONSTRAINT cancellation_refund_orders_claim_shape CHECK (
    (claim_id IS NULL) = (lease_until IS NULL)
  ),
  CONSTRAINT cancellation_refund_orders_terminal_shape CHECK (
    (outcome IS NULL) = (completed_at IS NULL)
  ),
  -- A failure must say why, and a success must not pretend to have one.
  CONSTRAINT cancellation_refund_orders_failure_shape CHECK (
    (outcome = 'failed' AND failure_code IS NOT NULL AND failure_reason IS NOT NULL)
    OR (outcome <> 'failed' AND failure_code IS NULL AND failure_reason IS NULL)
    OR outcome IS NULL
  ),
  -- The ADR-039 rule, enforced by the database rather than by the runner's good manners:
  -- a successful outcome means EVERY obligation is discharged. An order whose money came
  -- back but whose tickets are still valid cannot be written as `refunded`.
  CONSTRAINT cancellation_refund_orders_success_is_complete CHECK (
    outcome NOT IN ('refunded','already_refunded')
    OR (money_refunded AND tickets_voided AND capacity_returned)
  ),
  -- `refunded` means THIS run moved money, so it has a refund to point at.
  CONSTRAINT cancellation_refund_orders_refunded_has_refund CHECK (
    outcome <> 'refunded' OR (refund_id IS NOT NULL AND requested_quantity IS NOT NULL)
  )
);

-- The claim scan: unresolved rows whose lease is absent or expired, oldest first. The
-- primary key already orders report pages by (organizer, run, order_id).
CREATE INDEX cancellation_refund_orders_queue_idx
  ON cancellation_refund_orders (lease_until, created_at, order_id) WHERE outcome IS NULL;

-- Enumeration pages the slot's book through this index. Without it the scan is a
-- sequential read of every reservation the organizer ever made: a filter that is not
-- backed by an index is not a scoped read, it only looks like one.
CREATE INDEX reservations_cancellation_book_idx
  ON reservations (organizer_id, slot_id, created_at, id);

-- +goose Down
-- Lock before the guard, as 0007 does: checking first leaves a window in which a run
-- commits between the check and the DROP, and silently destroying the record of who was
-- repaid on a cancelled event is worse than refusing to roll back.
LOCK TABLE cancellation_refund_orders, cancellation_refund_runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM cancellation_refund_runs)
     OR EXISTS (SELECT 1 FROM cancellation_refund_orders) THEN
    RAISE EXCEPTION 'cannot roll back 0012: cancellation refund runs exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX reservations_cancellation_book_idx;
DROP TABLE cancellation_refund_orders;
DROP TABLE cancellation_refund_runs;
