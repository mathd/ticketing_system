# ADR-087: Operator outcomes for failed post-purchase refunds

Date: 2026-11-20

## Status

Accepted (TKT-525, owner decisions of 2026-10-12, plan decisions D7–D13, D15 and D16).

Amends [ADR-032](ADR-032-stripe-behind-the-psp-port.md) §Amendment — a provider-failed refund is
never re-submitted (TKT-347): the post-purchase refund path now has an operator route. The rule that
no code path and no operator command re-submits a failed refund is unchanged.

Delivery: TKT-525 is one PR in two slices (D7). This revision records slice S1,
`refunded_outside`. Slice S2 (`abandoned`, the abandonment marker write, the ceiling exclusions, the
bulk skip and the Back Office replacement) is not delivered by S1, and this ADR claims nothing about
it beyond the schema that S1 creates for it.

## Context

A staff refund or an event-cancellation (bulk) refund that the provider marks `failed` or
`canceled` stays `pending` in Commerce. Its Payments leg stays `bound`. ADR-037 §4 counts bound
attempts against both ceilings, so the failed attempt keeps its quantity and its amount reserved.
ADR-032 (TKT-347) forbids a re-submission, and no surface can record what an operator did outside
the platform. So the refund has no possible end, and the buyer can hold valid tickets for money that
came back by another route.

The owner decided on 2026-10-12:

- An operator command writes a new append-only outcome record in Commerce (ADR-003). The failed leg
  is not modified. The record is "not a payments fact" (ADR-032). It holds the refund id, the kind
  `refunded_outside` or `abandoned`, the amount in minor units and the currency, an external
  reference, the operator, the reason and the time.
- `refunded_outside` does not release the refund allowance. The money left, so the quantity and the
  amount stay counted against both ceilings, and the reversal runs.
- The scope is staff refunds and bulk cancellation. Recovery and exchange are TKT-532 and TKT-533.

## Decision

### 1. One append-only outcome table with a generic subject (D8)

Commerce migration 0027 adds `refund_outcomes`, keyed by `(organizer_id, subject_kind, subject_id)`.
TKT-525 writes only `subject_kind='order_refund'`. The table has no foreign key to `order_refunds`,
so TKT-532 and TKT-533 can add their own subject kinds. Row and statement triggers refuse `UPDATE`,
`DELETE` and `TRUNCATE`. Amount and currency are copied from the refund row; the operator never types
them, so an outcome covers the whole stored refund or nothing.

### 2. An outside completion is linked to its evidence

0027 replaces the 0007 completion CHECK. It finds that CHECK by relation and by its exact set of
referenced columns, and stops unless exactly one matches. `order_refunds` gets:

- `outcome_subject_kind`, a plain `NOT NULL` column with the constant default `order_refund` and a
  CHECK for that value. A constant default adds the column without a table rewrite; a stored
  generated column would rewrite the table under `ACCESS EXCLUSIVE`.
- `outside_completion_kind`, nullable.
- A composite foreign key `(organizer_id, outcome_subject_kind, id, outside_completion_kind)` to the
  outcome's `(organizer_id, subject_kind, subject_id, kind)`. A `NULL` kind skips it, so ordinary
  rows are unaffected. The `kind` column in the key is also what keeps a pending row from needing
  an outcome.
- `order_refunds_completion_evidence`, which admits three states, each with explicit `NULL` tests: a
  pending row with no evidence and no completion time; a completed row with a Payments fact and no
  outside kind; and a completed row with no Payments fact and the kind `refunded_outside`. An
  `abandoned` outcome cannot complete a refund.

The Down locks `order_refunds` and then `refund_outcomes`, then refuses while any outcome exists.
An empty Down restores the 0007 rule under the name `order_refunds_completion_check`.

### 3. `refunded_outside` requires a read-only Payments proof first (D13)

Commerce cannot tell from its own pending row whether money moved: Payments can complete a leg
before Commerce records it. So the command asks Payments to prove the failure before it writes:

```
GET /internal/psp/partial-refund/failure-proof?organizer_id=&idempotency_key=&refund_key=
```

- The charge key is the order's stored checkout key. The refund key is the Commerce refund id, which
  Payments requires as a canonical UUID; that excludes exchange downgrade keys.
- Payments reads the leg, its charge and its abandonment marker in one statement. It refuses a
  completed leg or a marked leg (409) before any provider call.
- The provider lookup only lists and reads. `psp.RefundFailureProver` is an interface separate from
  `psp.PSP`; the Stripe adapter implements it with the same listing loop, match rule (`classify`)
  and status mapping (`mapRefundStatus`) that `resolveRefund` uses. The proof scans the whole
  listing. One corroborated stamped refund in `failed` or `canceled` is a proof and returns its
  `re_` reference. A matching `succeeded` refund, or more than one stamped refund, contradicts the
  failure (409). A pending refund, no stamped refund, a stamped refund that does not corroborate
  (other PaymentIntent, amount or currency), an incomplete or malformed listing, an unavailable
  provider, and a provider with no prover (the fake PSP) are inconclusive (502).
- After the provider answers, Payments reads the leg, its charge and its marker again. Any change
  refuses (409).
- Error bodies are static. The provider idempotency key never crosses the boundary.

The CLI requires the proof's amount and currency to equal the refund's. It makes no writing HTTP
call.

### 4. The completion writes no money fact (D15)

The owner calls the outcome "not a payments fact", citing ADR-032. The ordinary Commerce completion
appends `order.refunded` to `order_facts` and forwards it to the Payments journal. Under this
reading, an outside completion does neither. **Consumers of the Payments journal or of Commerce
`order_facts` must read `refund_outcomes` for outside refunds.**

### 5. One short transaction in the ordinary lock order (D16)

The command inspects the refund in the organizer with no lock, asks Payments with no lock (ADR-037
§4), then writes in one transaction. It locks the refund row and then the order row, the order
`CompleteOrderRefund` uses. Under those locks it rechecks the source reservation's organizer, the
order's charge key, and the refund's order, amount and currency against what it inspected. It
decides replay or conflict against any outcome committed in the meantime, refuses a refund that
Payments completed, appends the outcome, sets the outside completion evidence, and advances the
order projection with the same arithmetic as `CompleteOrderRefund` (one shared helper).
`CompleteOrderRefund`, the ordinary `order.refunded` append and fact forwarding are unchanged.

0027 locks only `order_refunds` (and `refund_outcomes` on Down). Locking `orders` first could
deadlock with a live completion that holds a refund row and waits for its order. So the rollout
needs no Commerce writer drain.

### 6. Replay, conflict and scope

A missing refund and a refund outside the organizer, on either the refund row or its source
reservation, give the same refusal. An exact repeat (same kind, operator name, external reference
and reason) returns the original record with its original time and attribution, and calls nothing.
Any other repeat is a conflict. A refund that Payments completed is ineligible. All four are
answered from Commerce alone, with no Payments call.

### 7. Attribution is honest-operator evidence (D9, ADR-003, ADR-021)

The command records the typed operator name and PostgreSQL `session_user`, which the database reads
on the writing connection. On the documented access path (`docker compose exec commerce …`),
`session_user` is the shared `commerce` service login. **The typed operator name is the only
personal attribution.** Name the adversary: a writer with Commerce database access can forge or
remove an outcome, disable the triggers, or complete a refund directly, and a coordinated rollback
can erase the record. The record is evidence of what an honest operator attested to. The Payments
proof endpoint is behind the Payments internal credential only and relies on its authorized Commerce
caller for the operator workflow.

### 8. Reversal and bulk use the existing paths (ADR-038, ADR-040, ADR-062)

An outside completion is a `completed` refund, so the reversal reconciler claims it and drives the
tickets and then the capacity, with its ordering guard and retries unchanged. A pending refund is
still never claimed. A later bulk run over the slot derives the same refund key
(`cancel:<slot>:<order>`), replays the completed refund with no Payments call, and reports
`already_refunded` once the reversal is complete.

### 9. Payments schema for S2

Payments migration 0007 adds the append-only `payment_refund_leg_abandonments` marker, keyed by and
linked to the exact composite leg key, with the provider reference and a database timestamp. S1 only
reads it: the proof refuses a marked leg, so an outside completion cannot follow an abandonment that
Payments has committed. The Down locks `payment_operations`, then `payment_refund_legs`, then the
marker table, and refuses while a marker exists.

## Consequences

- An operator can close a failed staff or bulk refund that was refunded outside the platform. Both
  allowances stay consumed and the tickets are voided before the capacity returns.
- Proof is impossible for a leg whose provider refund has no compensation stamp (bound before the
  stamp existed, or never answered by the provider). The command refuses it, and the allowances stay
  reserved.
- Stripe documents refund listing filtered by PaymentIntent and the `failed` and `canceled`
  statuses. That historical failed and canceled refunds stay visible in that listing under the
  deployed API version is not verified live; the recorded fixtures assume it.
- Rollout: run the Payments migration and replace every old Payments process, then run the Commerce
  migration and update Commerce. Enable the command only when both services are current. The
  `migrate` subcommand has a 30-second deadline (ADR-008); if a migration fails on lock contention or
  on that deadline, rerun the failed job from the previous version before you continue.
- Roll forward, not back, once either migration holds outcome or marker history. Both Downs refuse.
- The quantity and money reports that read only `order_facts` or the Payments journal do not see an
  outside refund (D15).

## Verification

- Commerce migration: `services/commerce/internal/store/migration_0027_smoke_test.go`.
- Payments migration: `services/payments/internal/store/migration_0007_smoke_test.go`.
- Proof adapter: `services/payments/internal/psp/stripe_refund_proof_test.go`.
- Proof endpoint, real router and Stripe adapter against a recording peer:
  `services/payments/internal/api/partial_refund_failure_proof_smoke_test.go`.
- Commerce store, scope, rechecks, lock order and ceiling:
  `services/commerce/internal/store/refund_outcomes_smoke_test.go`.
- Command coordinator, proof client, reversal and bulk replay:
  `services/commerce/internal/refundoutcome/`.
- CLI validation before any connection: `services/commerce/cmd/commerce/refund_outcomes_test.go`.
- Same-key pins after a terminal 422: `TestStaffRefundRepeatAfterTerminal422ReusesTheOriginalKeys`,
  `TestDowngradeRepeatAfterTerminal422ReusesTheOriginalKeys` and
  `TestPartialRefundRepeatAfterTerminal422ReusesTheStoredProviderKey`.

## References

- [ADR-003](ADR-003-append-only-audit-trail.md), [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-032](ADR-032-stripe-behind-the-psp-port.md), [ADR-037](ADR-037-post-purchase-refund-money-protocol.md)
- [ADR-038](ADR-038-refund-reversal-ticket-voiding.md), [ADR-040](ADR-040-event-cancellation-bulk-refund-runs.md),
  [ADR-062](ADR-062-refund-reversal-reconciliation.md), [ADR-071](ADR-071-internal-mutation-ordering-honest-caller.md)
- [Order recovery runbook](../runbooks/order-recovery.md) §Failed post-purchase refund: operator outcome
