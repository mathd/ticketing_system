# Order, refund, and exchange recovery

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Parked recovery orders (TKT-146)

Commerce's recovery runner re-drives orders stranded in a non-terminal state. When an order
exhausts its budget (`MaxRecoveryAttempts`, 10), `ReleaseStuckOrder` sets `recovery_parked_at` and
stops: `ClaimStuckOrders` excludes parked rows, so **nothing in the service revisits a parked
order** (ADR-016 §Decision 1, deliberately — an order that failed ten re-drives should not keep
failing them on a timer). `ParkForReconciliation` parks the harder case, moving the order to
`reconciliation_required`.

Parking hands the problem to a human. These two commands are what the human uses.

```bash
docker compose exec commerce /app list-parked
# order=0f9c… status=release_pending attempts=10 parked_at=2026-08-19T14:02:11Z \
#   terminal_outcome=timeout last_error=psp unreachable
# order=1a3e… status=reconciliation_required attempts=4 parked_at=2026-08-18T09:41:55Z \
#   terminal_outcome=<none> last_error=captured payment whose claim is gone

docker compose exec commerce /app unpark-order 0f9c…  "psp restored; re-driving (OPS-4412)"
```

Both need only the service's usual `DATABASE_URL`. `list-parked` exits **zero** when nothing is
parked — "nothing to do" is not a failure, the same contract `reconcile-pins` states — and non-zero
only on a store failure, so a wrapper can tell an empty queue from a broken one.

**What `unpark-order` does, exactly.** It clears `recovery_parked_at`, resets `recovery_attempts`
to 0, makes the order due immediately, and clears any stale claim and lease. `recovery_last_error`
is **retained** as operator context. The attempts reset is not cosmetic: `ReleaseStuckOrder`
re-parks on `recovery_attempts >= MaxRecoveryAttempts`, so clearing the marker alone would buy
exactly one re-drive before the order parked again.

**What it does not do — and this is the part to be careful about.** It does not resolve the order.
It makes the order *drivable again* by the existing runner under the existing rules; whether the
re-drive succeeds depends on whatever was failing. It never touches `status`, never reads or writes
`terminal_outcome`, never calls the PSP, and writes no money column.

**It is not, however, a decision with no bearing on money.** A parked `reconciliation_required`
order may hold **captured** funds (ADR-016 §Consequences), and clearing its marker is what re-admits
it to the runner's `resolveReconciliation`. What happens there depends on the provider evidence:

- `captured` **with positive durable captured-amount evidence** → submitted for refund, and only
  *afterwards* does `inventory.Release` discover whether the claim was already confirmed, re-parking
  as *"refunded money against a confirmed claim"* when it was.
- `captured` **with no such evidence** (an operation predating payments migration 0002) → re-parks in
  one pass as *"operation predates durable provider evidence"*, without refunding.
- anything else → the shared provider-status decision table (voids release, refunded finishes,
  unknown retries).

That ordering is the runner's and predates these commands — any unparked `reconciliation_required`
row reaches it, and migration 0005's backfill created a population of them — but it is what an
operator is switching on.

So: **read `last_error` and establish what the order actually needs before unparking one.** If the
underlying condition has not been fixed, the order burns a fresh budget of ten attempts and parks
again. If it is a `reconciliation_required` row whose claim may have been confirmed or manually
repaired, resolve that first — unparking it asks the runner to re-decide on PSP evidence alone.

**It refuses three ways, distinguishably**, because during an incident the three call for different
next actions: the order does not exist (wrong id); the order is not parked (someone already did
this, or it was never parked); the order's status is not one the runner can claim. The third is
reachable only by a direct database write — no code path in the service produces a parked row with
a non-claimable status — and it is a fail-closed guard, because clearing the marker there would
look like a resolution while leaving the order just as unreachable.

**Every unpark is recorded** in `order_recovery_unparks`: the order, when, the operator's stated
reason, and the pre-unpark attempts, park timestamp and last error — the three values the unpark
destroys on the order row. The reason is required and cannot be blank; it is the only part of the
record a later reader cannot reconstruct.

**Scope of that claim (ADR-021).** This is evidence about an **honest operator**. It is append-only
by application behaviour, not tamper-evident: anyone holding commerce's database credentials can
insert, alter or delete these rows and nothing here detects it. The migration's `Down` refuses while
evidence exists, which protects against an accidental rollback — not against a writer.

### Knowing a parked order exists (TKT-263)

`list-parked` answers *which*, but only when someone runs it. Four gauges answer *how many* and
*how old* without being asked ([ADR-065](../adr/ADR-065-parked-recovery-order-observability.md)):

| Gauge | Reports |
|---|---|
| `commerce.recovery.parked` | Parked orders **excluding** `reconciliation_required` — attempt budget spent, no longer retried. |
| `commerce.recovery.parked.reconciliation_required` | Parked orders in `reconciliation_required`. Separate because these **may** hold captured money and what a re-drive does to one depends on provider evidence. |
| `commerce.recovery.parked.total` | Every parked order. The two above must sum to it. |
| `commerce.recovery.parked.oldest_age_seconds` | Age of the oldest park, measured from `recovery_parked_at`. |

They exported to the OTLP collector like every other metric; in the compose stack they land in the
`lgtm` Prometheus with dots rewritten as underscores (`commerce_recovery_parked`).

**Only parked rows are counted, and the distinction is load-bearing.** An **unparked**
`reconciliation_required` order is a queued compensation the runner is still driving — not a human's
inbox — so it is excluded. If it were counted, these numbers would rise exactly when recovery was
working.

**Reading them.** A steady small count is ordinary residue. A rising count means something upstream
stopped recovering. A flat count with a growing `oldest_age_seconds` means a specific order is stuck
and nobody has looked — which is the case this whole surface exists for, because nothing revisits a
parked order on its own.

**What they do not do.** Nothing fires: there is deliberately **no alert rule and no threshold**
(ADR-065 §4 gives the reasoning — a threshold chosen without production volumes would be muted
within a week). They never unpark anything, and they cannot tell you *which* order — that is still
`list-parked`, and unparking still calls for reading `last_error` first.

## Refund reversal reconciliation

A refund has two obligations beyond the money: the tickets must be **voided** and the seat's
**capacity returned**. Both are discharged after the money has already moved, so either can be left
outstanding by an access outage, an unset `ACCESS_URL`, or a refund that outruns issuance (access
answers `503` — "not yet", not "nothing to void").

When exchanged tickets leave too few eligible tickets, the exchanged tickets are already void and
Commerce logs `reversal obligation not discharged` with status 409. Commerce leaves this refund's
ticket-voiding and capacity-return obligations outstanding and retries until the row parks
(TKT-527, ADR-038 §5). The exchange may already have returned the same source capacity under its
own operation, so check the exchange's `capacity_returned_at` to see recorded exchange progress.

Since TKT-163 a background runner in commerce drives them to completion. **No operator action is
needed for the ordinary case**, and replaying the refund by hand is no longer the recovery path.

Reading the state of an outstanding reversal:

```sql
-- outstanding obligations, newest first
SELECT id, order_id, tickets_voided_at IS NOT NULL AS voided,
       capacity_returned_at IS NOT NULL AS capacity_back,
       reversal_attempts, reversal_next_attempt_at, reversal_parked_at, reversal_last_error
FROM order_refunds
WHERE status='completed'
  AND (tickets_voided_at IS NULL OR capacity_returned_at IS NULL)
ORDER BY created_at DESC;
```

| State | Means |
|---|---|
| `voided=f`, attempts rising, `parked_at` null | Ticket voiding is outstanding. Read the downstream status in the Commerce WARN `reversal obligation not discharged` for this `operation_id`. `503`: Access has not issued the tickets yet; status `0` with an error: Access is unreachable. Both retry on backoff; nothing to do. `409`: an exchange shortage (TKT-527) or refund-ID reuse; retries cannot succeed, and the row parks. |
| `voided=t`, `capacity_back=f`, `parked_at` null | The tickets no longer admit; the seat has not come back. Under-selling, which is the safe direction. Retrying. |
| `parked_at` set | **The runner has given up** after `reversal_attempts` reached 10 without progress. `reversal_last_error` says which half kept failing. This needs a human. |
| Both timestamps set | Complete. The row is no longer claimed or scanned. |

**A parked row is almost always a seated partial return.** Inventory refuses to return part of a
seated claim — nothing associates an issued ticket with a seat identity, so no subset of seats can be
derived (TKT-164) — and no number of retries changes that. The buyer has their money and the tickets
are void; only the seat is stuck. Unparking has no operator command yet (TKT-146, which owns the same
gap for parked recovery orders).

**Attempts reset on progress.** An outage of any length costs one attempt per pass while nothing
moves, and the first discharged obligation restores the full budget — so a long outage does not park
refunds that are recovering.

Metrics (commerce's first): `commerce.refund.reversal.outstanding`,
`commerce.refund.reversal.parked`, `commerce.refund.reversal.oldest_age_seconds`. Sustained nonzero
`outstanding` means a downstream is not recovering; any nonzero `parked` means work is owed that
nothing is driving. Alert on `parked`.

Tuning: `REFUND_REVERSAL_INTERVAL` (default `1m`) and `REFUND_REVERSAL_BATCH` (default `16`). A
restart drains immediately, so the interval bounds the steady state, not recovery from a deploy.

**`ACCESS_URL` is required for readiness.** Without it commerce boots and still refunds money, but
ticket voiding can never be discharged — by the runner or anything else — so `/readyz` answers `503`
with `access_configured: unhealthy` while `/healthz` stays green. Rationale, and why this does not
contradict ADR-021 §D6: [ADR-062](../adr/ADR-062-refund-reversal-reconciliation.md) §5.

## Exchange obligation sweep

`order_exchanges` carries two obligations — the switch (`tickets_exchanged_at`) and the source line's
capacity (`capacity_returned_at`). The tickets-switched callback discharges both on the happy path and
answers **502** when capacity is unresolved, which keeps access's message unacknowledged so JetStream
redelivers. The sweep (TKT-259, [ADR-063](../adr/ADR-063-exchange-reversal-reconciliation.md)) is the
backstop for rows redelivery gave up on — a dead-lettered message, or an `order.exchanged` never
consumed.

**The four states, and who owns each:**

| State | Meaning | Who resolves it |
|---|---|---|
| both timestamps set | complete | — |
| switched, capacity outstanding | safe under-sell; inventory refused or was down | **the sweep**, automatically |
| settled, switch not confirmed | access has not told commerce the old tickets stopped admitting | **access** — check its consumer and dead-letter queue |
| parked | no progress after the bounded budget | a human; investigate before unparking |

**The sweep never writes `tickets_exchanged_at`, and never claims a row that lacks it.** That marker
is access's fact, and migration 0011 gates the capacity return on it because freeing the seat while the
ticket still admits is the one ordering that can oversell. A settled exchange whose switch is
unconfirmed is *counted and monitored*, never claimed and never completed — ADR-063 §2. It accrues no
attempts and no error, so it can never park and can never block a migration rollback, and it becomes
actionable the moment access confirms, with no backoff to wait out. **A rising `awaiting_switch` is an
access incident, not an inventory one** — check access's consumer and its dead-letter queue, not
inventory.

**Attempts reset on progress.** An outage of any length costs one attempt per pass while nothing
moves, and the first discharged obligation restores the full budget.

Metrics: `commerce.exchange.reversal.outstanding`, `commerce.exchange.reversal.parked`,
`commerce.exchange.reversal.awaiting_switch`, `commerce.exchange.reversal.oldest_age_seconds`. The age
is measured from **settlement**, not row creation — an exchange is bound before it settles, and a bind
that never settled owes nothing. Alert on `parked` and on a sustained `awaiting_switch`.

Tuning: `EXCHANGE_REVERSAL_INTERVAL` (default `1m`) and `EXCHANGE_REVERSAL_BATCH` (default `16`). A
restart drains immediately, so the interval bounds the steady state, not recovery from a deploy.

Unparking has no operator command yet (TKT-146, which owns the same gap for parked recovery orders).

## Wedged exchange operator unwind

An exchange whose inventory **target claim went terminal** before settlement — `expired`, or an
explicit `finalizing -> released` — answers **409 "exchange target is unavailable"** on every retry,
forever. Its durable `order_exchanges` row then leaves the source order stuck in *both* directions:
`order_exchanges_one_per_source` blocks a corrected exchange, and `BindOrderRefund` counts any
exchange row for the source with no state predicate and refuses the refund. Nothing in the service
resolves it (TKT-255, [ADR-067](../adr/ADR-067-wedged-exchange-operator-unwind.md)).

**Two commands, and the second refuses more often than it acts:**

```
commerce list-wedged-exchanges
commerce unwind-exchange <organizer-id> <exchange-id> "<reason>"
```

`unwind-exchange` needs `DATABASE_URL`, `PAYMENTS_URL` and `PAYMENTS_INTERNAL_TOKEN` (falling back
to `INTERNAL_SERVICE_TOKEN`). It refuses immediately if payments is not configured — it cannot do
its job without asking payments, and finding that out after reading a row helps nobody.

**The listing reports CANDIDATES, not confirmed wedges.** Commerce holds no copy of inventory's
claim state, so it cannot tell a genuinely terminal target claim from an exchange that is in flight
right now — one is unsettled for a few hundred milliseconds and looks identical. Read the `age`
column, and **confirm in inventory that the target claim is terminal before unwinding**. The command
cannot check this for you, and it will happily unwind a healthy exchange.

**`settling=YES` means wait.** The exchange passed inventory's finalize, which is the moment it stops
being wedged and becomes able to move money, so `unwind-exchange` refuses it — for **five minutes**
from the timestamp shown. After that the marker stops vetoing and payments' own records decide, which
is what keeps a settlement that failed outright from stranding the order forever. An exchange that
genuinely moved money is still refused, ageing or not.

A marker much older than the window is not "now safe" — it is a settlement that crashed after
finalize. A retry of the buyer's request may still complete it. Read the timestamp and check payments
before unwinding.

**What `money=` in the listing means.** `impossible` means no provider call can have been made — the
basis is persisted *before* the provider is called, so an exchange with no basis never reached
payments, and an even exchange calls nobody. `possible` means only that a call could have been made;
`unwind-exchange` then asks payments and refuses if it was.

**The refusals, and where each one sends you:**

| Refusal | Meaning | What to do |
|---|---|---|
| `money moved` | payments records a provider movement for this exchange | **Stop.** The buyer paid. Compensating them is a product decision, not an unwind — see below. |
| `settlement in flight` | the exchange passed inventory's finalize within the last five minutes and may be at the provider right now | **Wait and re-run.** Past the window the marker stops vetoing and payments decides; a marker much older than that means a settlement crashed after finalize, so check payments before acting. |
| `indeterminate` | payments could not be asked, or gave no clean answer | Fix why payments could not answer, then re-run. **Do not** treat an unanswered question as a no. |
| `settled` | the exchange settled; it is not wedged | You are reading the listing wrong — a settled exchange's remaining obligations belong to the sweep above. |
| `not found` | no such organizer/exchange pair | Check the ids against the listing. |

**Only a 404 from payments permits an unwind.** Everything else refuses: a bound-but-unresolved
operation, a resolved-but-declined one, an uncompleted refund leg, a 5xx, a timeout. That is
deliberate under-approximation — a wedge that stays wedged is visible and reversible, and deleting a
charged buyer's binding is neither.

**A charged buyer is NOT compensated by this command, by design.** It refuses and leaves everything
intact. Choosing between refunding them and re-selling them a target has not been decided
(ADR-039 §2: an exchange has no safe partial state). If you hit this, the exchange row still carries
what the buyer was charged, which is what makes a manual resolution possible at all.

**After a successful unwind** the binding is gone: the source order can be exchanged again **with a
new idempotency key** (the old one derives the same exchange id) and can be refunded. Inventory is
untouched — the old target claim stays terminal and the source line's capacity was never released,
so nothing is oversold.

Every unwind writes a row to `order_exchange_unwinds` carrying the reason you gave and the state of
the exchange at the moment it was removed. That table is the only record the exchange ever existed;
migration 0024's Down refuses to roll back over it. Like everything else here it is
**honest-operator evidence, not tamper-evidence** (ADR-021): anyone with commerce's database
credentials can forge it, or delete an exchange row directly without leaving any.

## Failed post-purchase refund: operator outcome (TKT-525)

A staff refund or an event-cancellation (bulk) refund whose provider refund failed stays `pending`.
Its Payments leg stays `bound`, and both refund allowances stay reserved. The platform never
re-submits it (ADR-032, TKT-347). The staff order read shows the refund as `pending`; the bulk report
shows the order as `refund_refused`. Decision record:
[ADR-087](../adr/ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md).

**Agree on the disposition first.** Before you transfer money or run a command, agree with the
other operators what this refund's outcome is. Do not run two different commands for one refund. If
you find a conflict, stop and investigate before any further money action.

### The buyer was refunded outside the platform

```
commerce record-refund-outcome <organizer-id> <refund-id> refunded_outside "<operator name>" "<external reference>" "<reason>"
```

Run it with `docker compose exec commerce /app record-refund-outcome …`, as the other Commerce
commands. It needs `DATABASE_URL`, `PAYMENTS_URL` and `PAYMENTS_INTERNAL_TOKEN` (or
`INTERNAL_SERVICE_TOKEN`). It checks every argument and the Payments configuration before it opens
the database.

Before you run it:

1. Confirm that the outside transfer is **settled** and is for the **full stored amount** of this
   refund, in its stored currency. The command does not take an amount: it copies the refund's
   amount and currency, and prints them. Running the command is your confirmation.
2. Use the transfer's own reference (bank or PSP dashboard) as `<external reference>`.
3. Type your own name as `<operator name>`. It is the only personal attribution. The record also
   stores PostgreSQL `session_user`, which on this access path is always the shared `commerce` login.

What the command does:

1. It reads the refund in the organizer. A missing refund and another organizer's refund give the
   same refusal.
2. It asks Payments, read-only, to prove the failure: the leg is still bound, it has no abandonment
   marker, and the provider's own stamped refund for it is `failed` or `canceled` for the same amount
   and currency. It makes no writing HTTP call.
3. In one short transaction it appends the outcome to `refund_outcomes`, completes the refund with
   that outcome as its evidence, and advances the order's refund totals once.

The result line shows `amount`, `currency`, `external_ref`, `operator`, `session_user`,
`recorded_at` and `replay`.

After success: both allowances stay consumed. No Commerce `order.refunded` fact and no Payments
journal entry is written, so a report that reads only `order_facts` or the Payments journal does not
see this refund; read `refund_outcomes`. The reversal reconciler then voids the tickets and, only
after that, returns the capacity. Follow it in [Refund reversal reconciliation](#refund-reversal-reconciliation).
A later bulk run over the same slot uses the same refund key and reports `already_refunded` once the
reversal is complete.

**Run the same command again** after a timeout or a lost reply. An identical command returns the
original record (`replay=true`) with its original time and attribution, and asks Payments nothing.
A command that differs in kind, operator name, reference or reason is refused as a conflict.

| Refusal | Meaning | What to do |
|---|---|---|
| `no refund … for this organizer` | Wrong ids, or the refund belongs to another organizer | Check both ids. |
| `already carries a different outcome` | An outcome is recorded and it is not this one | Stop. Outcomes are append-only. Investigate before any money action. |
| `completed through payments` | The money moved through the platform | Nothing to record. |
| `payments refused the failure proof` (404 or 409) | No such leg, the leg completed, it has an abandonment marker, or the provider says the refund did not fail | Reconcile the original refund first. Do not transfer money. |
| `payments could not prove` (502, timeout, malformed answer, or different money) | No proof now | Nothing changed and both allowances stay reserved. Fix the cause and run the same command again. |
| `changed while the command ran` | The refund or its order changed between the read and the write | Run the same command again. |

**A refund whose provider refund has no stamp can never be proven.** A leg bound before the adapter
added the `compensation_key` stamp, or one the provider never answered, has no refund the proof can
find. The command refuses it, and both allowances stay reserved.

After an outside completion the refund is in the reversal backlog until its tickets are voided and
its capacity is returned. Watch `commerce.refund.reversal.outstanding` and
`commerce.refund.reversal.parked` ([Refund reversal reconciliation](#refund-reversal-reconciliation)).
A nonzero `parked` for this refund needs a human, as for any completed refund.

### Nobody will refund this attempt: `abandoned`

Use it when the buyer still has the money owed, no outside transfer occurred, and none is in flight.
Then staff can refund the buyer again under a **new** key through the Back Office.

```
commerce record-refund-outcome <organizer-id> <refund-id> abandoned "<operator name>" "<reason>"
```

Run it the same way, with the same configuration. It takes no reference: Payments supplies the
provider's `re_` for the failed refund, and the command records it as `external_ref`.

Before you run it, confirm that **no outside transfer occurred and none is in flight** for this
refund. If one did, use `refunded_outside` instead. `abandoned` releases the allowance, so running it
after an outside transfer opens the way to a second refund of the same money.

What the command does:

1. It reads the refund in the organizer, as for `refunded_outside`.
2. It asks Payments to abandon the leg. Payments proves, with the same read-only provider lookup,
   that the stamped provider refund is `failed` or `canceled` for the stored money, then writes an
   append-only marker on the exact leg. The leg stays `bound`. The marker releases that leg's amount
   in the Payments captured-money ceiling, and ordinary dispatch then refuses the leg (409) before
   any provider call.
3. Only after Payments answered 200, Commerce appends the `abandoned` outcome. The refund stays
   `pending`, and the order totals, the tickets and the capacity do not change. The Commerce quantity
   ceiling stops counting this refund when the outcome commits.

After success:

- An abandoned refund never enters the reversal backlog, and the reversal gauges do not change: it
  has no completion, so there is nothing to void or return.
- Staff refund the buyer again through the Back Office order console, under a new key. If the Back
  Office still holds a saved retry for the old key, submitting it answers "An operator recorded this
  refund as abandoned", retires that saved retry, and shows a blank refund form with a fresh key.
  The platform never re-submits automatically, and no operator command re-submits.
- A bulk cancellation run that reaches this order skips the abandoned refund with no Payments call.
  It reports `refund_refused` while quantity remains, and at zero quantity `already_refunded`, or
  `reversal_outstanding` while another refund's reversal is still owed.

**Run the same command again** after a timeout, a 5xx, a malformed answer, or a Commerce error.
Payments may already have written its marker; then its allowance is released while Commerce keeps
the quantity reserved. That is the stricter state, and it is safe. The same command converges:
Payments answers from its marker without asking the provider (also when the provider is down), and
Commerce records once. An identical command after success returns the original record
(`replay=true`). A different operator name or reason is a conflict.

| Refusal | Meaning | What to do |
|---|---|---|
| `no refund … for this organizer` | Wrong ids, or another organizer's refund | Check both ids. |
| `already carries a different outcome` | An outcome is recorded and it is not this one | Stop. Investigate before any money action. |
| `is completed` | The refund completed, through Payments or outside | Nothing to abandon. |
| `payments refused to abandon` (404 or 409) | No such leg (wrong target), the leg completed, its money differs, or the provider says the refund did not fail | Nothing was recorded in either service. Correct the target, or reconcile the original refund through ordinary refund reconciliation. |
| `UNCERTAIN: payments' answer … is unknown` | Timeout, 5xx, provider inconclusive, or an answer for other money | Commerce recorded nothing. Run the same command again. If Payments keeps answering 502, the provider proof is not available: see the unstamped case below. |
| `changed while the command ran` | The refund or its order changed between the read and the write | Run the same command again. |
| `FAILED: … was not recorded in Commerce` | A Commerce error after Payments may have accepted | Run the same command again. |

**After an abandonment, an outside completion is refused.** Once the Payments marker exists, the
`refunded_outside` proof answers 409 (`refund leg abandoned`), even while Commerce has no outcome
yet. Finish the abandonment with the same command instead.

**A Payments marker with no Commerce outcome** means an abandonment that Payments accepted and
Commerce has not recorded. Find it with the leg query below (`abandonment_ref` set), then check
Commerce:

```sql
-- commerce database
SELECT kind, external_ref, operator_name, recorded_at
FROM refund_outcomes
WHERE organizer_id = '<organizer-id>' AND subject_kind = 'order_refund' AND subject_id = '<refund-id>';
```

No row: run the same `abandoned` command again. Do not transfer money, and do not run
`refunded_outside`, until it has finished.

**The unstamped-leg case.** Proof is impossible for a leg whose provider refund has no compensation
stamp (bound before the adapter added it, or never answered by the provider). Both commands refuse
it (`refunded_outside` with "could not prove", `abandoned` with "UNCERTAIN" and Payments 502), and
both allowances stay reserved. No command in this release can close such a refund.

**Find the Payments leg** for a refund with the scoped leg key: the organizer, the order's checkout
idempotency key (`orders.idempotency_key`) and the refund id as the refund key.

```sql
-- payments database
SELECT l.status, l.amount, l.currency, l.bound_at, m.provider_ref AS abandonment_ref, m.abandoned_at
FROM payment_refund_legs l
LEFT JOIN payment_refund_leg_abandonments m
  ON (m.organizer_id, m.source_idempotency_key, m.refund_idempotency_key)
   = (l.organizer_id, l.source_idempotency_key, l.refund_idempotency_key)
WHERE l.organizer_id = '<organizer-id>'
  AND l.source_idempotency_key = '<order checkout key>'
  AND l.refund_idempotency_key = '<refund-id>';
```

**Roll forward, not back.** Commerce migration 0027 refuses to roll back while `refund_outcomes`
holds a row, and Payments migration 0007 refuses while `payment_refund_leg_abandonments` holds a row.
Both records are honest-operator evidence, not tamper-evidence (ADR-021): a writer with database
credentials can forge or delete them.
