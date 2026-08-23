-- +goose Up

-- TKT-255. An operator's record of unwinding a WEDGED exchange.
--
-- The wedge (found by TKT-167's adversarial review, pinned by two tests since): an exchange
-- records its basis, the inventory target claim then goes terminal — `expired`, or an
-- explicit `finalizing -> released` — and inventory refuses every transition out of a
-- terminal state. So `finalize` answers conflict and commerce answers 409 "exchange target
-- is unavailable", forever. The durable `order_exchanges` row then blocks a corrected
-- attempt through `order_exchanges_one_per_source` (0010) AND makes the source order
-- unrefundable, because `BindOrderRefund` counts ANY exchange row for the source with no
-- state predicate at all. The order is stuck in both directions and nothing in the service
-- can unstick it: 0010 says in its own comment that an exchange has no cancelled or
-- inactive state, which is exactly why the row is permanent.
--
-- WHY A DELETE AND NOT A TOMBSTONE COLUMN. A nullable `unwound_at` would have to be
-- excluded by every reader of the table, and there are two whose whole purpose is to treat
-- any row as live — the unique index (which cannot be made partial without changing what
-- "one exchange per order" means) and the refund count. A tombstone turns one invariant
-- into two predicates that must agree forever. The row is deleted and its evidence lands
-- here instead, which is why this table carries the pre-state rather than pointing at it.
--
-- NOTHING IS ORPHANED BY THAT DELETE, and it is the schema that guarantees it rather than
-- the application: no table anywhere references `order_exchanges`, and an UNSETTLED row —
-- the only kind that can be unwound — necessarily has `replacement_order_id IS NULL`
-- (0010's `order_exchanges_settlement_shape`) and `tickets_exchanged_at IS NULL`
-- (`order_exchanges_switch_after_settlement`). It owns no order, no switch, and no capacity
-- obligation. It is also invisible to the ADR-063 sweep, whose every claim conjunct and
-- gauge filters `settled_at IS NOT NULL` (0022).
--
-- NAME THE ADVERSARY (ADR-021). This is append-only by APPLICATION behaviour and it is
-- evidence about an HONEST operator — who unwound what, when, why they said they did, and
-- what the row held at the moment it was removed. It is NOT tamper-evident: anyone holding
-- commerce's database credentials can insert, alter or delete these rows, or delete an
-- `order_exchanges` row directly without leaving one. The signed payments journal remains
-- the evidence that money moved, which is precisely why the unwind refuses on PAYMENTS'
-- answer and never on a commerce flag. The Down guard below protects against an accidental
-- rollback, not against a writer.
CREATE TABLE order_exchange_unwinds (
  id                     uuid PRIMARY KEY,
  organizer_id           uuid NOT NULL,
  -- The exchange that was removed. NOT a foreign key, and that is the point: the row it
  -- names no longer exists. Kept so an operator can correlate this record with the
  -- idempotency key the buyer's original request carried.
  exchange_id            uuid NOT NULL,
  -- The source order DOES survive the unwind, so this one is a real foreign key. It is the
  -- column an operator actually searches by, because the order is what the buyer complains
  -- about.
  source_order_id        uuid NOT NULL REFERENCES orders(id),
  unwound_at             timestamptz NOT NULL DEFAULT now(),
  reason                 text NOT NULL
    CHECK (length(reason) BETWEEN 1 AND 500 AND btrim(reason) <> ''),
  -- The exchange's identity as the buyer's request expressed it. Reconstructing which
  -- request was abandoned is impossible from the exchange id alone, since that id is a
  -- SHA1 of (organizer, idempotency key) and does not invert.
  idempotency_key        text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  actor                  text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
  -- The money shape of the exchange as it stood, captured because the row is about to be
  -- destroyed and this is the part a later reader most needs: it says what the unwind
  -- CLAIMED had not moved. All three are nullable exactly as they are on `order_exchanges`,
  -- because a bound exchange with no basis has none of them — that shape is a faithful
  -- copy, not a claim.
  pre_delta_amount       bigint,
  pre_target_total       bigint CHECK (pre_target_total IS NULL OR pre_target_total >= 0),
  pre_source_total       bigint NOT NULL CHECK (pre_source_total >= 0),
  currency               varchar(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  -- Whether a basis had been recorded. This is the fact that decides whether money COULD
  -- have moved at all — the basis is persisted before any provider call — so it is the
  -- single most load-bearing thing about the removed row.
  pre_basis_recorded     boolean NOT NULL,
  -- The target claim the exchange held, so an operator can go and look at what inventory
  -- says about it afterwards. NULL when no basis was recorded, since the hold id is part of
  -- the basis.
  pre_target_hold_id     uuid,
  -- An unwind row can only exist for a row that was NOT settled, and this constraint is a
  -- database-tier restatement of the store guard's central predicate — it is what makes
  -- "an unwind row proves the exchange was unsettled" a true statement rather than a
  -- convention the Go code happens to follow. Same reasoning 0023 gives for its NOT NULL
  -- on `pre_recovery_parked_at`.
  CONSTRAINT order_exchange_unwinds_basis_shape CHECK (
    (pre_basis_recorded = false AND pre_delta_amount IS NULL AND pre_target_total IS NULL
      AND pre_target_hold_id IS NULL)
    OR
    (pre_basis_recorded = true AND pre_delta_amount IS NOT NULL AND pre_target_total IS NOT NULL
      AND pre_target_hold_id IS NOT NULL)
  ),
  -- The same money invariant `order_exchanges_delta_is_the_difference` (0010) enforces on
  -- the live row, preserved on the copy. A pre-state that does not satisfy the constraint
  -- the source row was under is not a faithful record of it.
  CONSTRAINT order_exchange_unwinds_delta_is_the_difference CHECK (
    pre_delta_amount IS NULL OR pre_delta_amount = pre_target_total - pre_source_total
  ),
  -- One unwind per exchange. A second one for the same id would mean either a duplicate
  -- record of one intervention or an exchange that was somehow re-created and re-unwound;
  -- both are things an operator needs to see refused rather than recorded twice.
  CONSTRAINT order_exchange_unwinds_one_per_exchange UNIQUE (organizer_id, exchange_id)
);

CREATE INDEX order_exchange_unwinds_source_idx
  ON order_exchange_unwinds (source_order_id, unwound_at DESC);

-- The settlement-in-flight marker (ai-review pass 1 [critical]).
--
-- An unwind takes the SOURCE ORDER's row lock, which is the lock BindOrderExchange and
-- BindOrderRefund both take. That is the right lock and it is not sufficient on its own,
-- because a resume RELEASES it when its bind transaction commits and only then finalizes,
-- charges and settles. So between those two moments a resume is invisible to the unwind's
-- lock, and an unwind could observe an unsettled row plus a clean payments answer and delete
-- the binding out from under a charge that is about to happen.
--
-- The ordering makes that narrow rather than routine: `completeExchangeFromBasis` calls
-- inventory's finalize BEFORE the provider, and a genuinely WEDGED exchange cannot pass
-- finalize — that refusal is the definition of wedged. The reachable case is an operator
-- unwinding a HEALTHY exchange that happens to be mid-flight, which the CLI cannot rule out
-- because commerce holds no copy of inventory's claim state and says so.
--
-- `settling_at` is written by the resume once finalize has SUCCEEDED, which is the exact
-- moment the exchange stops being wedged and starts being able to move money. The unwind
-- refuses while it is set. It is deliberately NOT a lease: nothing reclaims it, because a
-- crashed settlement leaves an exchange that a retry resumes and an operator must not
-- silently unwind — the marker going stale is a state a human should look at, and
-- `list-wedged-exchanges` shows it.
ALTER TABLE order_exchanges ADD COLUMN settling_at timestamptz;

-- NO index is added on `order_exchanges` for the wedged-exchange listing, deliberately, and
-- for the reason 0023 wrote down for its own listing: `list-wedged-exchanges` is a by-hand
-- operator command over a population bounded by how many exchanges are simultaneously stuck
-- — a handful of rows during an incident, not a scaling read path. ADR-019 requires that a
-- scoped read prove BOTH that the result is scoped and that the scan is; shipping an index
-- without that second proof is the "copying a query shape that scales ships a no-op" case
-- that ADR names. Considered and declined, not overlooked.

-- +goose Down

-- Fail closed once evidence exists. Rolling back would erase the only commerce-local record
-- that an operator ever abandoned an exchange — and unlike a parked order, the row it
-- describes is GONE, so this table is not a duplicate of state held elsewhere. It is the
-- only account of it. Same reasoning 0023 applies to its unpark evidence, and 0005 and 0012
-- to theirs.
--
-- THE LOCK COMES FIRST, and it is what makes "fail closed" true rather than merely intended
-- (ai-review [high]). Without it the guard reads an empty table, a concurrent unwind commits
-- its evidence row, and the DROP then waits for that writer and destroys the row it just
-- accepted — the exact outcome the guard exists to prevent, in the one window where it
-- matters. `ACCESS EXCLUSIVE` is the mode DROP TABLE will take anyway; taking it before the
-- check merely moves it earlier, so the check and the drop see the same table.
--
-- This is the pattern every other history-preserving Down in this service already follows
-- (0006, 0007, 0008, 0009, 0010, 0012, 0014, 0021, 0022). 0023 does NOT, and has the same
-- gap — pre-existing, out of this ticket's scope, and filed rather than silently fixed here.
LOCK TABLE order_exchange_unwinds, order_exchanges IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM order_exchange_unwinds) THEN
    RAISE EXCEPTION 'cannot roll back 0024: operator unwind evidence exists; export it before rolling back';
  END IF;
END
$$;
-- +goose StatementEnd

DROP TABLE order_exchange_unwinds;

-- The marker goes with the table it exists to protect. Unlike the evidence, dropping it
-- loses nothing an operator needs: it is transient state about a settlement in flight, and
-- rolling back to a schema with no unwind command means nothing can act on it anyway.
ALTER TABLE order_exchanges DROP COLUMN settling_at;
