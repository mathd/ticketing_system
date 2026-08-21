-- TKT-257 (ADR-032 §Provider-neutral evidence reads): record what the PROVIDER says it
-- moved, distinctly from what payments asked it to move.
--
-- Until now `payment_operations.captured_amount` was written from the REQUEST — the charge
-- path and the status-resolve path both did it — and `payment_refund_legs.amount` was the
-- figure the leg BOUND, fixed before the provider call. So payments recorded, journalled
-- and published its own request as settled provider evidence, and a provider that moved a
-- different amount was invisible.
--
-- ADDITIVE, never a replace, and the reason matters more than the convenience:
--
--   1. A row written before this migration has no provider confirmation and never can.
--      NULL is the only honest answer for it. Back-filling from `request_amount` would
--      promote the very figure this ticket exists to stop treating as evidence, and
--      permanently — nothing downstream could ever tell a back-filled row from a confirmed
--      one.
--
--   2. `captured_amount` has live readers whose behaviour must not move: compensationAllowed
--      refuses a refund unless it is > 0, BindRefundLeg's ceiling sums against it, and
--      commerce's recovery runner decides whether to refund from the status body carrying
--      it. Repointing that column at a NULL-for-legacy value would make every pre-migration
--      captured operation permanently un-refundable — a production behaviour change wearing
--      the costume of a rename.
--
-- So the old columns keep their meaning and their readers, and confirmation is a new,
-- separately-named, nullable dimension beside them.
--
-- Money is BIGINT minor units + ISO currency; floats are banned on money paths.

-- +goose Up
ALTER TABLE payment_operations
  -- What the provider reported capturing. NULL means no provider figure was ever recorded:
  -- a legacy row, an unresolved operation, or a provider that did not say. Distinct from 0,
  -- which would be a provider affirming that nothing moved.
  ADD COLUMN confirmed_captured_amount bigint CHECK (confirmed_captured_amount >= 0),
  ADD COLUMN confirmed_currency varchar(3) CHECK (confirmed_currency ~ '^[A-Z]{3}$'),
  -- Both or neither. An amount without its currency is not a money value, and a currency
  -- with no amount confirms nothing — either half alone would read as evidence while
  -- carrying none.
  ADD CONSTRAINT payment_operations_confirmed_money_paired CHECK (
    (confirmed_captured_amount IS NULL AND confirmed_currency IS NULL)
    OR
    (confirmed_captured_amount IS NOT NULL AND confirmed_currency IS NOT NULL)
  );

ALTER TABLE payment_refund_legs
  -- What the provider reported returning for THIS leg, as opposed to `amount`, which is
  -- what the leg bound before the call.
  ADD COLUMN confirmed_amount bigint CHECK (confirmed_amount > 0),
  ADD COLUMN confirmed_currency varchar(3) CHECK (confirmed_currency ~ '^[A-Z]{3}$'),
  ADD CONSTRAINT payment_refund_legs_confirmed_money_paired CHECK (
    (confirmed_amount IS NULL AND confirmed_currency IS NULL)
    OR
    (confirmed_amount IS NOT NULL AND confirmed_currency IS NOT NULL)
  );

ALTER TABLE payment_compensations
  -- The whole-refund compensation path (recovery's) is the fourth money-moving write sink,
  -- and it must record the provider's own figure for the same reason the other three do.
  -- A void confirms nothing — nothing moved on the ledger — so this stays NULL for kind='void'.
  ADD COLUMN confirmed_amount bigint CHECK (confirmed_amount > 0),
  ADD COLUMN confirmed_currency varchar(3) CHECK (confirmed_currency ~ '^[A-Z]{3}$'),
  ADD CONSTRAINT payment_compensations_confirmed_money_paired CHECK (
    (confirmed_amount IS NULL AND confirmed_currency IS NULL)
    OR
    (confirmed_amount IS NOT NULL AND confirmed_currency IS NOT NULL)
  );

-- Deliberately NOT extended: the completion CHECK
-- `(status='refunded' AND completed_at IS NOT NULL AND fact_id IS NOT NULL)`.
--
-- "A completed leg carries provider confirmation" and "a pre-0006 row answers absent" are
-- contradictory statements about legs already completed when this migration runs. A CHECK
-- strong enough to enforce the first rejects the second, and the only ways out are a
-- discriminator column or a constraint that special-cases history — which is a constraint
-- that will be wrong again at the next migration.
--
-- So the rule lives where it can distinguish the two cases by construction: CompleteRefundLeg
-- requires confirmed money in its UPDATE, so a leg completed FROM NOW ON cannot settle
-- without it, while a row completed before this migration is simply never rewritten. Pinned
-- by TestCompleteRefundLegRequiresProviderConfirmation and
-- TestLegacyCompletedRefundLegSurvivesMigration in refund_legs_smoke_test.go.

-- +goose Down
-- Lock before the guard, as 0003 does: checking first leaves a window in which a completion
-- lands between the check and the DROP, and silently destroying evidence of money that left
-- the account is worse than refusing to roll back.
LOCK TABLE payment_refund_legs, payment_compensations, payment_operations IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM payment_refund_legs WHERE confirmed_amount IS NOT NULL)
     OR EXISTS (SELECT 1 FROM payment_compensations WHERE confirmed_amount IS NOT NULL)
     OR EXISTS (SELECT 1 FROM payment_operations WHERE confirmed_captured_amount IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back 0006: provider-confirmed money evidence exists';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE payment_refund_legs
  DROP CONSTRAINT payment_refund_legs_confirmed_money_paired,
  DROP COLUMN confirmed_currency,
  DROP COLUMN confirmed_amount;
ALTER TABLE payment_compensations
  DROP CONSTRAINT payment_compensations_confirmed_money_paired,
  DROP COLUMN confirmed_currency,
  DROP COLUMN confirmed_amount;
ALTER TABLE payment_operations
  DROP CONSTRAINT payment_operations_confirmed_money_paired,
  DROP COLUMN confirmed_currency,
  DROP COLUMN confirmed_captured_amount;
