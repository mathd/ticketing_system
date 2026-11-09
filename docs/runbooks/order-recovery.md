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
| `voided=f`, attempts rising, `parked_at` null | Access is unavailable or has not issued the tickets yet. Retrying on backoff; nothing to do. |
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
