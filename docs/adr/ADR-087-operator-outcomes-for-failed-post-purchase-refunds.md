# ADR-087: Operator outcomes for failed post-purchase refunds

Date: 2026-11-20

## Status

Accepted (TKT-525, owner decisions of 2026-10-12, the TKT-525 plan decisions).

Amends [ADR-032](ADR-032-stripe-behind-the-psp-port.md) §Amendment — a provider-failed refund is
never re-submitted (TKT-347): the post-purchase refund path now has an operator route. The rule that
no code path and no operator command re-submits a failed refund is unchanged.

Makes one narrow exception to [ADR-037](ADR-037-post-purchase-refund-money-protocol.md) §4 (bound
attempts count against both ceilings): a refund with a recorded `abandoned` outcome no longer counts
(§12). ADR-037 §4 stays as written, including its rule against locks across provider calls. Qualifies
[ADR-040](ADR-040-event-cancellation-bulk-refund-runs.md) §3 the same way.

Delivery: TKT-525 is one PR in two slices. Slice S1 is `refunded_outside` (§1–§9). Slice S2 is
`abandoned`: the Payments abandonment endpoint and marker write, the two ceiling exclusions, the
dispatch refusal, the Commerce outcome, the bulk skip and the Back Office replacement (§10–§16).

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
- `abandoned` releases the allowance in **both** ceilings: the Commerce quantity sum and the Payments
  captured-money sum exclude the abandoned attempt.
- After a recorded `abandoned` outcome, staff can submit a new refund under a new key through the
  Back Office. The platform never re-submits automatically, and no operator command re-submits.
- A bulk cancellation run that finds an abandoned refund skips it with no Payments call.
- The scope is staff refunds and bulk cancellation. Recovery and exchange are TKT-532 and TKT-533.

## Decision

### 1. One append-only outcome table with a generic subject

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

### 3. `refunded_outside` requires a read-only Payments proof first

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

### 4. The completion writes no money fact

The owner calls the outcome "not a payments fact", citing ADR-032. The ordinary Commerce completion
appends `order.refunded` to `order_facts` and forwards it to the Payments journal. Under this
reading, an outside completion does neither. **Consumers of the Payments journal or of Commerce
`order_facts` must read `refund_outcomes` for outside refunds.**

### 5. One short transaction in the ordinary lock order

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

### 7. Attribution is honest-operator evidence (ADR-003, ADR-021)

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
linked to the exact composite leg key, with the provider reference and a database timestamp. The
proof refuses a marked leg, so an outside completion cannot follow an abandonment that Payments has
committed. The Down locks `payment_operations`, then `payment_refund_legs`, then the marker table, and
refuses while a marker exists.

### 10. `abandoned`: Payments first, then Commerce

```
commerce record-refund-outcome <organizer-id> <refund-id> abandoned <operator> <reason>
```

The operator confirms that no outside transfer occurred and none is in flight. The command takes no
reference: Payments supplies the provider's `re_` for the failed refund.

1. Commerce inspects the refund in the organizer with no lock. It answers a missing or foreign
   refund, an exact replay, a conflicting outcome and a completed refund from Commerce alone, with
   no Payments call.
2. Commerce calls `POST /internal/psp/partial-refund/abandon` with the organizer, the order's stored
   charge key, the refund id as the refund key, and the refund's stored amount and currency. No
   Commerce lock is held.
3. After a valid 200 for exactly the refund's money, Commerce appends the `abandoned` outcome, with
   the returned `re_` as its external reference, in one short transaction. It takes the same locks
   and rechecks as §5 (one shared helper) and writes only the outcome. The refund stays `pending`.
   The order projection, the tickets, the capacity and the reversal state do not change.

The first committed Commerce outcome supplies the recorded operator name and reason. An exact replay
(same kind, operator name and reason) returns that record; the reference is not part of the command.

### 11. The Payments abandonment endpoint and its marker transaction

The endpoint takes the organizer, the charge `idempotency_key`, a canonical UUID `refund_key`, and
the expected `amount` and `currency`. It is behind the Payments internal credential only. It relies
on the authorized Commerce caller for the operator workflow.

1. It reads the leg, its charge and its marker. A missing leg is 404. A completed leg, or a leg whose
   money differs from the request, is 409. A committed marker is answered at once as a 200 replay
   with the stored reference, **without a provider call**, so a caller that lost the first answer can
   always finish, also when the provider is down.
2. For a new marker it asks the provider for the same proof as §3, with no lock held and no
   transaction open. A contradiction is 409; anything inconclusive is 502 and writes nothing.
3. One short transaction locks the `payment_operations` row (the lock `BindRefundLeg` decides the
   ceiling under), then the exact leg. It rechecks that the leg is `bound` and that its money, its
   provider key and the charge's PaymentIntent are the ones the proof used. A change is 409 and
   writes nothing. A marker committed meanwhile with the same reference is a replay; with another
   reference it is 409. Otherwise it inserts the marker.

No database lock spans a provider call (ADR-037 §4 is unchanged).

### 12. The two ceiling exclusions, and what still counts

- **Payments.** `BindRefundLeg`'s captured-money sum excludes a leg with a marker for the same
  organizer, charge key and refund key. Only that sum excludes it. The whole-refund exclusion
  (`BindCompensation`) still counts every leg, marked or not, so a charge with a marked leg cannot
  get a whole refund. The marked leg stays `bound` with its provider key.
- **Commerce.** `BindOrderRefund`'s quantity sum excludes a refund with a matching `abandoned`
  outcome: same organizer, subject kind `order_refund` and subject id. An outside completion and an
  ordinary pending refund still count. The release happens when the Commerce outcome commits.

This is the only exception to ADR-037 §4. A pending refund with no outcome still consumes both
ceilings, and so does a `refunded_outside` refund.

### 13. Dispatch refuses an abandoned leg; resolve-first covers the race

In `/internal/psp/partial-refund`, after the existing bind and completed-replay check, Payments reads
the marker in its own short statement immediately before `psp.Refund`. A marked leg answers the
existing 409 (`refund leg abandoned`) with no provider call. A read error fails closed (500).

A replay can pass that read just before a marker commits. It then reaches the Stripe adapter, which
resolves before it submits (ADR-032 §Refund): it lists the PaymentIntent's refunds, finds the stamped
`failed` refund that the marker was proven against, and answers the terminal 422 without a POST. The
existing provider idempotency key covers overlapping submissions under the provider's idempotency
contract. `TestPartialRefundDispatchRefusesAnAbandonedLeg` pins the 409 with no provider call. With
the marker read removed, the same fixture answers 422 after one listing GET, and still makes no POST.

### 14. Recovery after an uncertain answer

There is no request table. A conclusive Payments 404 or 409 on a new abandonment leaves both
services without a new record; the operator corrects the target, or reconciles the original leg, and
can retry an eligible command. A timeout, a 5xx, an invalid answer, or a Commerce commit failure after
Payments accepted leaves the stricter ceiling: Payments has released its allowance and Commerce keeps
the quantity reserved. The same command run again converges: Payments answers from its marker and
Commerce records once. An outside-completion command whose proof starts after the marker commits is
refused (§3), even while Commerce has no outcome yet. The remaining window is an outside command that
has its proof while a concurrent abandonment is about to commit. Concurrent contradictory commands for
one refund are outside scope; the runbook requires operators to agree the disposition before any
command or outside transfer, and to investigate a detected conflict before any further money action.

### 15. Bulk runs skip an abandoned refund (owner decision of 2026-10-12)

`LookupRefundByID` (and `BindOrderRefund`'s replay) carry the abandoned flag, scoped to the refund's
own organizer. The bulk runner checks it in **both** quantity branches, before it uses a fixed
quantity and before it calls the refund unit. It makes no Payments call. It classifies from the order
projection and the reversal state, which the abandonment did not change:

- Remaining quantity: `failed` / `refund_refused`, terminal.
- Zero remaining quantity: the existing reversal precedence. `already_refunded` when every reversal
  obligation is discharged; `reversal_outstanding` when one remains. 0012's success CHECK forbids a
  success with an obligation outstanding, so the owner's "zero means already refunded" is read
  through that CHECK.

If the abandonment commits between the runner's lookup and the Commerce bind, the shared refund unit
refuses (`refunds.ErrRefundAbandoned`) before any Payments call, and the runner applies the same
classification. If it commits after the bind, Payments' marker has committed first and dispatch
answers 409. The `(slot, order)` key, attempt charges and terminal report snapshots are unchanged.

### 16. Staff replacement through the Back Office (owner decision of 2026-10-12)

After a recorded `abandoned` outcome, staff can submit a new refund under a new key through the Back
Office. The platform never re-submits automatically, and no operator command re-submits.

A valid staff replay of the abandoned refund's key answers Commerce 409 with `code: refund_abandoned`
and sends nothing to Payments. A mismatched fingerprint and an ordinary Payments 409 stay generic
409s with no code. The handwritten Back Office client (`web/backoffice/src/lib/commerce.ts`) reads the
code only on a 409. `unresolvedRefund` (`order-console.ts`) then settles the saved retry, and the page
clears only the saved retry with the same organizer, order and key, and renders a blank refund form
with a fresh key. A generic refusal and a different saved key keep the existing retry behaviour.

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
  outside refund.
- An operator can close a failed refund that nobody will refund. Each service releases only that
  refund's allowance, and staff can refund again under a new key. The abandoned row stays `pending`
  and its leg stays `bound`: both are history, not work.
- A lost answer after Payments committed its marker leaves Payments' allowance released and
  Commerce's quantity reserved until the same command is run again. Find such a case with the
  runbook's leg query: a marker with no Commerce outcome.
- Like the outcome, the marker is honest-operator evidence (ADR-003, ADR-021), not tamper-evidence.

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
- S2, Payments ceilings and marker scope: `services/payments/internal/store/refund_leg_abandonments_smoke_test.go`.
- S2, Payments abandonment endpoint, recheck and dispatch refusal, real router and Stripe adapter:
  `services/payments/internal/api/partial_refund_abandon_smoke_test.go`.
- S2, Commerce ceiling, flag, write, replay, refusals and lock order:
  `services/commerce/internal/store/refund_abandoned_smoke_test.go`.
- S2, coordinator recovery (lost answer, commit failure, refused proof after the marker) and the
  abandon client: `services/commerce/internal/refundoutcome/abandon_smoke_test.go`, `client_test.go`.
- S2, bulk skip in both branches and the late abandonment:
  `services/commerce/internal/bulkrefund/runner_abandoned_smoke_test.go`.
- S2, staff `refund_abandoned`: `services/commerce/internal/api/refund_abandoned_smoke_test.go`.
- S2, Back Office: `web/backoffice/test/` (`api`, `order-console`, `mutation-pages`) and the browser
  spec `test/browser/refund-abandoned.mjs` (`make browser`, host Chrome).

## References

- [ADR-003](ADR-003-append-only-audit-trail.md), [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-032](ADR-032-stripe-behind-the-psp-port.md), [ADR-037](ADR-037-post-purchase-refund-money-protocol.md)
- [ADR-038](ADR-038-refund-reversal-ticket-voiding.md), [ADR-040](ADR-040-event-cancellation-bulk-refund-runs.md),
  [ADR-062](ADR-062-refund-reversal-reconciliation.md), [ADR-071](ADR-071-internal-mutation-ordering-honest-caller.md)
- [Order recovery runbook](../runbooks/order-recovery.md) §Failed post-purchase refund: operator outcome
