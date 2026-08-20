-- +goose Up

-- TKT-146. An operator's record of returning a parked recovery order to the claimable
-- set. ReleaseStuckOrder parks a row once recovery_attempts reaches MaxRecoveryAttempts
-- and ClaimStuckOrders excludes parked rows, so before this table nothing in the service
-- could resolve one: a human was told to act and given no instrument.
--
-- NAME THE ADVERSARY (ADR-021). This is append-only by APPLICATION behaviour and it is
-- evidence about an HONEST operator — who unparked what, when, and why they said they
-- did. It is NOT tamper-evident: anyone holding commerce's database credentials can
-- insert, alter or delete these rows, and nothing here detects that. The Down guard
-- below protects against an accidental rollback, not against a writer.
CREATE TABLE order_recovery_unparks (
  id                      uuid PRIMARY KEY,
  order_id                uuid NOT NULL REFERENCES orders(id),
  unparked_at             timestamptz NOT NULL DEFAULT now(),
  reason                  text NOT NULL
    CHECK (length(reason) BETWEEN 1 AND 500 AND btrim(reason) <> ''),
  -- The recovery state as it stood BEFORE the unpark. Captured because the unpark
  -- destroys it on the order row: recovery_attempts is reset to 0 and
  -- recovery_parked_at is cleared, so without this the evidence of what was resolved
  -- is gone the moment it is resolved.
  pre_recovery_attempts   integer NOT NULL CHECK (pre_recovery_attempts >= 0),
  -- NOT NULL is load-bearing, not incidental nullability housekeeping. An unpark row
  -- can only exist for a row that WAS parked, so this constraint is a second,
  -- database-tier enforcement of the store guard's "is it parked?" predicate — it is
  -- what makes "an unpark row proves the order was parked" a true statement rather
  -- than a convention the Go code happens to follow.
  pre_recovery_parked_at  timestamptz NOT NULL,
  -- Nullable: an order parked by ReleaseStuckOrder always carries a cause, but the
  -- column itself is nullable on orders and this is a faithful copy, not a claim.
  pre_recovery_last_error text
);

CREATE INDEX order_recovery_unparks_order_idx
  ON order_recovery_unparks (order_id, unparked_at DESC);

-- NO index is added on orders for the parked-order listing, deliberately. `list-parked`
-- is a by-hand operator command over a population bounded by MaxRecoveryAttempts
-- exhaustion — a handful of rows, not a scaling read path. ADR-019 requires that a
-- scoped read prove BOTH that the result is scoped and that the scan is; shipping an
-- index without that second proof is the "copying a query shape that scales ships a
-- no-op" case that ADR names. Considered and declined, not overlooked.

-- +goose Down

-- Fail closed once evidence exists. Rolling back would erase the only commerce-local
-- record that an operator ever intervened, and there is no honest way to translate it
-- into the pre-0023 schema — the same reasoning 0005_psp_recovery.sql applies to its
-- own durable recovery evidence, and 0012 to cancellation refund runs.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM order_recovery_unparks) THEN
    RAISE EXCEPTION 'cannot roll back 0023: operator unpark evidence exists; export it before rolling back';
  END IF;
END
$$;
-- +goose StatementEnd

DROP TABLE order_recovery_unparks;
