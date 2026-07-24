-- +goose Up
-- PSP-backed commerce recovery (TKT-115 / ADR-032 Slice 3): resolve payment_unknown via
-- provider-neutral PSP status and refund reconciliation_required orders holding captured
-- money, through the payments /internal/psp/* surface.

-- `no_side_effect` joins terminal_outcome: a PSP-status-proven resolution where the hold
-- was released without a provider decision (void, external cancellation, replay-proven
-- never-created). Distinct from `declined`/`timeout` — those keep recording the
-- provider's exact terminal answer even when it arrives via the status path, or the
-- audit column stops distinguishing a decline from a timeout (ADR-032 amendment, TKT-115).
ALTER TABLE orders DROP CONSTRAINT orders_terminal_outcome_check;
ALTER TABLE orders ADD CONSTRAINT orders_terminal_outcome_check
  CHECK (terminal_outcome IN ('declined','timeout','not_attempted','no_side_effect'));

-- `refunded` joins the closed status vocabulary: captured money whose claim could not be
-- delivered, returned via POST /internal/psp/refund. Terminal — never claimed again.
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (
  status IN ('created','payment_unknown','confirmation_pending','release_pending',
             'completed','declined','timeout','reconciliation_required','refunded')
);

-- The claimable set grows: payment_unknown is resolvable (PSP status exists now) and an
-- UNPARKED reconciliation_required row is a queued compensation, not a human's inbox.
DROP INDEX orders_recovery_claimable_idx;
CREATE INDEX orders_recovery_claimable_idx
  ON orders (recovery_next_attempt_at)
  WHERE status IN ('created','payment_unknown','confirmation_pending','release_pending',
                   'reconciliation_required')
    AND recovery_parked_at IS NULL;

-- Re-open EXACTLY the two populations parked because this slice did not exist yet. Both
-- wear status='reconciliation_required' (ParkForReconciliation rewrote the source
-- status), so the reasons are the discriminator — matched exactly, not by pattern, to
-- avoid resurrecting rows parked for attempt exhaustion or manual reconciliation.
-- recovery_last_error is deliberately RETAINED as operator context; attempts reset so
-- the newly-actionable rows get a full retry budget.
UPDATE orders
SET recovery_parked_at = NULL,
    recovery_attempts = 0,
    recovery_next_attempt_at = now(),
    recovery_claim_id = NULL,
    recovery_lease_until = NULL
WHERE status = 'reconciliation_required'
  AND recovery_parked_at IS NOT NULL
  AND recovery_last_error IN (
    'payment result unknown; needs PSP status (TKT-56)',
    'captured payment whose claim is gone; needs void/refund (TKT-56)'
  );

-- +goose Down
-- The vocabulary rollback would silently falsify durable recovery evidence if any row
-- already uses it — fail loudly instead of translating. Same for an UNPARKED
-- reconciliation_required row (a queued compensation this migration's backfill may have
-- re-opened): the pre-0005 index cannot claim it and the cleared park marker no longer
-- represents it as awaiting a human, so rolling back would strand it invisibly
-- (ai-review B4). Re-park such rows explicitly before rolling back.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM orders WHERE status='refunded' OR terminal_outcome='no_side_effect') THEN
    RAISE EXCEPTION 'cannot roll back 0005: rows carry refunded/no_side_effect evidence';
  END IF;
  IF EXISTS (SELECT 1 FROM orders WHERE status='reconciliation_required' AND recovery_parked_at IS NULL) THEN
    RAISE EXCEPTION 'cannot roll back 0005: unparked reconciliation_required rows would be stranded; park them first';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX orders_recovery_claimable_idx;
CREATE INDEX orders_recovery_claimable_idx
  ON orders (recovery_next_attempt_at)
  WHERE status IN ('created','confirmation_pending','release_pending')
    AND recovery_parked_at IS NULL;

ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (
  status IN ('created','payment_unknown','confirmation_pending','release_pending',
             'completed','declined','timeout','reconciliation_required')
);
ALTER TABLE orders DROP CONSTRAINT orders_terminal_outcome_check;
ALTER TABLE orders ADD CONSTRAINT orders_terminal_outcome_check
  CHECK (terminal_outcome IN ('declined','timeout','not_attempted'));
