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
unconfirmed is *counted and monitored*, never claimed and never completed by the sweep (ADR-063 §2).
It accrues no attempts or error, so it can never park or block migration 0022's rollback. It becomes
actionable the moment access confirms, with no backoff to wait out. **A rising `awaiting_switch` is an
access incident, not an inventory one** — check access's consumer and its dead-letter queue, not
inventory. If Access records `exchange_refused` because a source ticket was admitted, see
[Settled exchange without a ticket switch](#settled-exchange-without-a-ticket-switch-tkt-498).

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

`unwind-exchange` needs `DATABASE_URL` set to the intended Commerce database, `PAYMENTS_URL`, and
the dedicated `PAYMENTS_INTERNAL_TOKEN`. The token must have at least 32 bytes and must differ from
`INTERNAL_SERVICE_TOKEN`. There is no fallback to the shared token. Before it opens the database, the
command checks that the Payments URL is present and validates the token. It refuses immediately if
payments is not configured — it cannot do its job without asking payments, and finding that out
after reading a row helps nobody.

Before you run the command, confirm that `PAYMENTS_URL` reaches the Payments service with no
redirect. The current client follows redirects and accepts a 404 from the final address as absence
(TKT-589).

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
| `settled` | the exchange settled; it is not wedged | A settled exchange's remaining obligations belong to the sweep above. If Access refused its switch because a source ticket was already admitted, use [Settled exchange without a ticket switch](#settled-exchange-without-a-ticket-switch-tkt-498). |
| `not found` | no such organizer/exchange pair | Check the ids against the listing. |

**If the exchange has a recorded nonzero delta, the Payments lookup must return 404 before Commerce
can unwind.** Commerce refuses an unresolved operation, a declined operation, an uncompleted refund
leg, and an unavailable or inconclusive answer. This is a deliberate under-approximation. A wedge
remains visible and reversible, while deleting a charged buyer's binding removes evidence needed for
recovery. If the exchange has no recorded basis or its delta is zero, Commerce uses local evidence.
The command still checks the Payments URL and dedicated credential before it opens the database.

**A charged buyer of an UNSETTLED exchange is NOT compensated by this command, by design.** It
refuses and leaves everything intact. Choosing between refunding them and re-selling them a target
has not been decided (ADR-039 §2: an exchange has no safe partial state). If you hit this, the exchange row still carries
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

## Settled exchange without a ticket switch (TKT-498)

Access refuses the ticket switch of a settled exchange when a source ticket was already admitted.
The exchange then stays settled and unswitched: the delta moved, the replacement order exists, and
the target capacity stays held. The buyer used the source ticket, so the replacement must never
issue. `unwind-exchange` refuses a settled exchange, so it does not apply. The remedy is to
**abandon** the exchange. Decision record: [ADR-039 §2](../adr/ADR-039-exchange-settles-the-difference.md),
amended by TKT-498.

Access records the refusal as a failed-event record with reason `exchange_refused`
([ticket lifecycle](./ticket-lifecycle.md)). Commerce keeps the exchange in the reversal backlog
(`commerce.exchange.reversal.outstanding` and `awaiting_switch`) until the abandonment completes.

### Before the first use: rollout order

Do these steps once, in this order, when the release that adds `abandon-exchange` is deployed:

1. Deploy Payments first. Only the new Payments accepts the `order.exchange.abandoned` journal fact.

   ```bash
   docker compose up -d --build payments
   ```

2. Run Commerce migration 0028 and replace the Commerce container. The `commerce` service waits for
   the `commerce-migrate` job, so one command does both:

   ```bash
   docker compose up -d --build commerce
   ```

3. After the old Commerce container exits, wait **at least 60 seconds** before the first
   `abandon-exchange`. The longest Commerce outbound HTTP timeout is 30 seconds. The wait lets every
   staff or bulk refund request that the old container sent to Payments finish. Only the new
   Commerce checks replacement-ticket issuance before it sends a replacement refund (see
   [Replacement refunds](#replacement-refunds-of-an-abandoned-exchange)).

If you deploy Commerce before Payments, the command fails closed: Payments refuses the unknown fact,
and the abandonment stays started with its target returned. Deploy Payments, then run the same
command again. After the first reservation exists, roll forward only: migration 0028's Down refuses
to run over started, failed or completed abandonments.

### 1. List the candidates

```bash
docker compose exec commerce /app list-settled-unswitched-exchanges <organizer-id>
# organizer=4b1e… exchange=9c0d… source_order=21fa… replacement_order=7e3b… target_hold=c55a… \
#   settled_at=2026-11-20T09:30:00Z quantity=2 delta=+1000 EUR abandonment=none
```

The command needs only `DATABASE_URL`, which must not be empty, and reads Commerce only, so it works
while Access, Payments and Inventory are down. It exits **zero** when there is nothing to list. Rows are sorted by
settlement time, then by exchange id.

**The rows are candidates.** Commerce cannot see Access's tickets. A row can be a switch whose
Commerce callback is late, or an exchange that Access refused. The listing cannot tell them apart.
`abandon-exchange` asks Access before it changes anything (step 2).

| Column | Meaning |
|---|---|
| `delta` | The signed face-value difference. `+` is an upgrade the buyer paid, `-` a downgrade refunded to the buyer, `0` an even exchange. |
| `abandonment=none` | No abandonment has started. |
| `abandonment=started` | An abandonment is reserved, with its start time and the operator who typed the reservation. |
| `abandonment=refund-failed` | The provider refused the upgrade's delta refund. `refund_failed_ref` is the provider reference. The buyer is still charged and the target is still held. TKT-588 owns this state (see below). |
| `abandonment=target-returned` | The target capacity came back. Only the journal entry and the terminal step are left. |

A completed abandonment does not appear in the listing.

**The listing omits inconsistent rows.** A settled, unswitched exchange whose reservations belong to
another organizer, whose replacement uses another reservation or hold, whose quantities differ, or
whose replacement is not completed is not a candidate. The backlog gauges still count it. If the
`outstanding` gauge counts an exchange that the listing does not show, query the table directly:

```sql
SELECT id, source_order_id, replacement_order_id, target_hold_id, settled_at, abandonment_started_at
FROM order_exchanges
WHERE organizer_id = '<organizer-id>' AND settled_at IS NOT NULL
  AND tickets_exchanged_at IS NULL AND abandoned_at IS NULL
ORDER BY settled_at, id;
```

`abandon-exchange` refuses an inconsistent row. Treat it as an incident and do not edit the row.

### 2. Abandon the exchange

**Agree on the disposition first.** Agree with the other operators that this exchange is to be
abandoned, and who runs the command.

```bash
docker compose exec commerce /app abandon-exchange <organizer-id> <exchange-id> "<operator name>" "<reason>"
```

The command checks its arguments and its configuration before it opens the database or calls a
service. It needs all of these, and the `commerce` container has them:

- `DATABASE_URL`, not empty. An empty value would select the shell's PostgreSQL defaults.
- `ACCESS_URL`, `PAYMENTS_URL` and `INVENTORY_URL`, each an absolute `http` or `https` base URL with
  a host name, a port in 1–65535 if it names one, no query, and no `#` anywhere.
- `INTERNAL_SERVICE_TOKEN` for Access and inventory.
- `PAYMENTS_INTERNAL_TOKEN` for Payments. It must be set and must differ from
  `INTERNAL_SERVICE_TOKEN`. There is no fallback to the shared token.

The operator name is 1–200 and the reason 1–500 characters after trimming.

The command first prints `abandon-exchange trace_id=<id>` on stderr. Every service request log
stamps that trace id on this run's calls, so you can find them. The run can take up to 29 min 5 s
when every dependency is slow: up to 159 calls of 10 s each (three inspections of up to 51
replacement refund legs), the delta refund of up to 35 s, and 2 minutes of database work. The refund
waits longer because Payments answers it only after its provider calls; 35 s is just over Payments'
30 s default response-write deadline. A run stopped at a deadline keeps its recorded progress; run the
same command again.

The command reads Access about the source and then the replacement, checks the money and journal
evidence, reserves the abandonment, refunds an upgrade's delta, returns the target claim, and
journals one `order.exchange.abandoned` fact. [ADR-039 §2](../adr/ADR-039-exchange-settles-the-difference.md#amended-by-tkt-498-a-settled-exchange-whose-source-was-admitted-is-abandoned)
gives the reasons for this order.

What each branch leaves:

| Delta | Money | Recorded on the exchange |
|---|---|---|
| Upgrade (`+`) | The buyer receives the delta back: one refund leg against the delta charge, and one `payment.refunded` fact. | `abandonment_refund_fact_id`, the Payments fact of that refund. |
| Downgrade (`-`) | No money moves. The buyer keeps the settlement's refund. | `abandonment_written_off_amount`, the delta the platform writes off. |
| Even (`0`) | No money moves. | Neither. |

In every branch the target claim stays `confirmed` with all of its quantity returned, and the pool's
confirmed count drops by the exchange quantity. The source order keeps its admitted tickets and its
capacity. The replacement issues no ticket. The journal fact's `amount` is the target gross that the
abandonment removes from the recorded exchange sale. Its payload carries the source order, the
exchange, the source gross it restores, the signed delta, and the refund fact (upgrade) or the
write-off (downgrade).

The output is the terminal record:

```
organizer=… exchange=… source_order=… replacement_order=… target_hold=… quantity=2 delta=+1000 EUR \
  operator="Ada Operator" session_user="commerce" reason="…" started_at=… target_returned_at=… \
  abandoned_at=… refund_fact=… replay=false
```

### Ownership, retries and replay

- **The first trimmed operator name and reason pair owns the abandonment.** Only that exact pair can
  resume it or replay it. Any other pair is refused, before and after completion. The listing shows
  the operator; the reason is in `order_exchanges.abandonment_reason`.
- **Run the same command again** after any `INCOMPLETE` answer. Each step keeps its recorded
  progress, and the refund and the capacity return use one stable key, so a retry does not refund or
  return twice.
- **An identical command after success** prints the original record with `replay=true` and its
  original times. It reads Commerce only and calls no other service.
- **An identical command that overlaps a successful one** also prints the original record with
  `replay=true` when it finds the terminal marker, and calls no other service after that.

### Refusals and what to do

| Output starts with | Meaning | What to do |
|---|---|---|
| `REFUSED: no exchange … for this organizer` | No such organizer and exchange pair. | Check both ids against the listing. |
| `REFUSED: the abandonment of exchange … belongs to another operator name and reason` | Another pair started this abandonment. Nothing changed. | Repeat the first pair exactly. |
| `STOPPED: the provider refused the upgrade's delta refund` | The provider failed the delta refund for good. | Stop. See [The provider refused the delta refund](#the-provider-refused-the-delta-refund-tkt-588). |
| `REFUSED: exchange … is not a candidate` | The exchange is not settled, Access switched its tickets, its rows are inconsistent, or its gross journal facts disagree with it. A started abandonment whose rows became inconsistent is refused the same way before any further step. Nothing changed. | Not settled: use the [wedged exchange unwind](#wedged-exchange-operator-unwind). Switched: the [exchange obligation sweep](#exchange-obligation-sweep) owns it. Inconsistent rows or facts: treat as an incident. |
| `REFUSED: Access does not prove an admitted source with an unissued replacement` | Access reports the source not admitted or not fully issued, or reports replacement tickets. Nothing changed. | Replacement tickets mean that the switch happened and its Commerce callback is late. Follow the failed-event procedure in [ticket lifecycle](./ticket-lifecycle.md). A source that is not admitted is not this procedure's case. |
| `REFUSED: payments evidence for exchange … needs reconciliation first` | The delta charge or the downgrade refund does not match the exchange, a refund of the replacement completed, or Payments holds a leg for a pending refund of the replacement. | Reconcile by hand. Money can have moved on the replacement through the TKT-586 checkout-key alias. Do not abandon until it is understood. |
| `INCOMPLETE: exchange … was not abandoned` | A dependency was unavailable, gave an answer that proves nothing (a redirect, a `404` that is not Payments' own "not found" answer, an answer that is not exactly what the dependency writes, such as a field named twice, a case variant of a field name, a null field or a malformed journal hash, or an Access admission with no issued ticket), or refused one request. Every recorded step stays. | Clear the cause and run the **same** command again. |

### The provider refused the delta refund (TKT-588)

For an upgrade, a provider's terminal failure of the delta refund stops the command. The platform
never submits that refund again (ADR-032, ADR-087). The exchange is then in this state:

- settled, with the abandonment started and the provider's `re_` reference recorded;
- the Payments refund leg bound, with its amount still counted against the delta charge;
- the buyer still charged for the delta;
- the target claim still held;
- the replacement inert: no money, no tickets, no capacity;
- no terminal marker, so the exchange stays in the listing (`abandonment=refund-failed`) and in the
  reversal backlog.

The same command reports the stored failure and calls nothing. `record-refund-outcome` does not apply:
it records outcomes of order refunds, not of an exchange. TKT-588 owns the next step. Until it ships,
leave the exchange as it is.

### Replacement refunds of an abandoned exchange

- **After the abandonment starts**, the replacement order cannot bind or replay a refund. A staff
  refund answers `409 only a completed order can be refunded`, and a cancellation run reports
  `not_refundable`.
- **After the abandonment starts**, the replacement order cannot bind or replay a void either. A
  staff void answers `409 only a completed, unexchanged order can be voided`, and a cancellation run
  reports `refund_refused`. An existing void row stays as history.
- **A refund of an exchange replacement whose switch Commerce has not recorded reaches Payments only
  after Access reports issued replacement tickets.** This applies to staff refunds and cancellation
  runs. Without issued tickets, or when Access cannot answer, a staff refund answers
  `502 refund unresolved`, a cancellation run reports `unavailable`, and the refund stays `pending`.
  A replacement with `tickets_exchanged_at` set needs no Access read, so an Access outage does not
  block its refunds. The check exists because of the TKT-586 checkout-key alias. Payments looks up the
  exact (organizer, key) pair. It does not translate the replacement's key `exchange:<id>` to the
  exchange's `exchange-charge:<id>` operation. The risk is a checkout in the same organizer that used
  the literal key `exchange:<id>`: a replacement refund would refund that checkout's charge. When you
  reconcile, look for that checkout, not for the exchange's own charge.
- **A pending replacement refund for which Payments answered 404 does not stop the abandonment, and it
  stays `pending` after it.** `record-refund-outcome` cannot close it: both of its outcomes need
  Payments' proof of a provider failure, and there is no Payments leg to prove.

### After success

- The exchange leaves the listing and the reversal backlog gauges (`outstanding`, `awaiting_switch`
  and the oldest age).
- The exchange, both orders and the original journal facts stay as history. The source order still
  cannot be refunded, voided or exchanged again, because its exchange row still exists.
- A replay of the exchange request answers `200` with status `abandoned`. The other fields keep the
  original exchange and money values as history, and the replay repairs nothing. The exchange stays
  settled and unswitched for good. Tell the buyer the outcome through support. While the abandonment
  is started but not finished, the replay still answers `switch_pending`.
- A redelivery of the original `order.exchanged` event is refused again by Access. It issues no
  ticket.

### What the record proves

The abandonment record is **honest-operator evidence, not tamper-evidence** (ADR-021):

- The typed operator name is the operator's own attestation. `session_user` is the database login
  of the process that ran the command, which is the shared `commerce` login in the container. It
  does not identify a person.
- A write-once trigger refuses any change to an abandonment field that has a value. It protects
  against application defects only. A writer with privileges on the Commerce database can disable it
  and change or delete the fields.
- A restore of the Commerce database can roll the record back. The Payments journal entry is
  hash-chained, so `payments verify-journal` detects an edit of the journal itself, but a coordinated
  restore of both databases removes both records.

## Failed post-purchase refund: operator outcome (TKT-525)

A staff refund or an event-cancellation (bulk) refund whose provider refund failed stays `pending`.
Its Payments leg stays `bound`, and both refund allowances stay reserved. The platform never
re-submits it (ADR-032, TKT-347). The staff order read shows the refund as `pending`. The bulk report
row shows `outcome` `failed` with `failure.code` `refund_refused`. Decision record:
[ADR-087](../adr/ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md).

Find the exact refund under the intended organizer. Commerce staff order detail is
`GET /internal/orders/<order-id>` on Commerce. The request needs the Commerce staff credential and an
organizer assertion for the intended organizer. Commerce takes the organizer from the assertion, not
from the request. An order of another organizer returns `404`.

For a staff refund, open staff order detail, identify the refund attempt, and read its `refund_id`,
stored `amount` in minor units, and `currency`.

For a bulk refund, use the procedure below only for a money refund that the provider failed:

1. Read the organizer's cancellation report with
   `GET /internal/cancellation-refunds/<run-id>?organizer_id=<organizer-id>`. Send the Commerce
   shared internal credential in `X-Internal-Token`. Give curl the header on standard input and
   `--noproxy '*'`, as in the [report example](payments.md#event-cancellation-bulk-refunds-tkt-159).
   Do not put the token in a command line. Read the order rows after the run completes.
   Record the run's `slot_id`. If the page does not contain the order and returns
   `next_after_order_id`, use that value as `after_order_id` in the next request. Repeat until you
   find the order or the report returns no cursor.
2. Find the row of the order. A row can include `refund_id`. A row with `outcome` `failed` and
   `failure.code` `reversal_outstanding` has a refund whose money moved and whose ticket or capacity
   reversal is outstanding. Do not record an outcome for it. Use
   [refund reversal reconciliation](#refund-reversal-reconciliation) instead.
3. Continue only for a row with `outcome` `failed` and `failure.code` `refund_refused`. This code also
   covers other refusals. For example, Payments refused the refund for a different cause, an operator
   already recorded the refund as `abandoned`, or the refund did not return the whole order. Thus the
   row alone does not prove a provider failure.
4. Open staff order detail for the row's `order_id`. Select the refund whose `idempotency_key` is
   `cancel:<slot_id>:<order_id>`, with the run's `slot_id` and the row's `order_id`. Its `actor` must
   also be `system:event-cancellation`. Use the actor only to confirm the selection. A staff refund
   can have the same actor text under a different key.
5. Continue only if that refund has `status` `pending` and `abandoned` `false`. If no such refund
   exists, stop. This procedure does not apply to the order.
6. Read the `refund_id`, stored `amount` in minor units, and `currency` of that refund.

**Agree on the disposition first.** Before you transfer money or run a command, agree with the
other operators what this refund's outcome is. Do not run two different commands for one refund. If
you find a conflict, stop and investigate before any further money action.

**Before you send money outside the platform**, verify that the provider marked the exact original
refund `failed` or `canceled` for the stored amount and currency. From a client on the platform
network, use the Payments service address. Base Compose uses `http://payments:8080` on its network.
The Commerce container's `PAYMENTS_URL` is a setting for the Commerce command. For local work, the
opt-in `compose.direct-ports.yaml` overlay makes Payments available from the host at
`http://127.0.0.1:${PAYMENTS_PORT:-8093}`. Both the Payments and Commerce containers use distroless
images without a shell or `curl`.

For `organizer_id`, use the refund's organizer. For `idempotency_key`, read the order's stored
checkout key from the intended Commerce database. Staff order detail shows the refund attempt's own
`idempotency_key`. Use the checkout key from the Commerce query for the Payments request.

```sh
docker compose exec postgres psql -U postgres -d commerce
```

```sql
SELECT o.idempotency_key
FROM orders o
JOIN reservations r ON r.id = o.reservation_id
WHERE o.id = '<order-id>' AND r.organizer_id = '<organizer-id>';
```

For `refund_key`, copy `refund_id` exactly as staff order detail shows it, as a canonical lowercase
UUID. Set `ORGANIZER_ID`, `CHECKOUT_KEY`, and `REFUND_ID` to these three values. Export the dedicated
`PAYMENTS_INTERNAL_TOKEN` in your shell. For base Compose, use a throwaway, digest-pinned curl
container on the `ticketing_default` network. The image runs curl as its entry point. Your host
shell writes the header to the container's standard input, and curl reads it with `--header @-`.
`printf` is a shell builtin, so the token is in no command line, and it is not in the container's
environment or configuration. `--noproxy '*'` sends the request directly to Payments, also when
Docker gives the container proxy settings.

```sh
printf 'X-Internal-Token: %s\n' "$PAYMENTS_INTERNAL_TOKEN" |
  docker run --rm -i --network ticketing_default \
    curlimages/curl:8.23.0@sha256:3fb7e13e5d179a845c97d78204f3f1064d1907037f26a3ebf91710571151fd93 \
    --silent --show-error --fail-with-body --noproxy '*' --header @- --get \
    --data-urlencode "organizer_id=$ORGANIZER_ID" \
    --data-urlencode "idempotency_key=$CHECKOUT_KEY" \
    --data-urlencode "refund_key=$REFUND_ID" \
    --write-out '\nHTTP %{http_code}\n' \
    http://payments:8080/internal/psp/partial-refund/failure-proof
```

Continue only when curl exits `0` and prints `HTTP 200` with the provider reference and matching
amount and currency. Stop if the provider refund is still pending or the proof is missing,
contradictory, or inconclusive. The proof is read-only: after a successful proof, Commerce still
shows the refund as `pending` until you record the outcome below
([ADR-087](../adr/ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md) §3).

Run either command with `docker compose exec commerce /app record-refund-outcome …`. This uses the
`commerce` container's Compose values. It ignores values set in your shell unless you pass each one
with `-e`, for example
`docker compose exec -e PAYMENTS_URL=<url> commerce /app record-refund-outcome …`.
Set `DATABASE_URL` to the intended Commerce database. Set `PAYMENTS_URL` to an absolute `http` or
`https` base URL with a host name and, if it names a port, a port in 1–65535. Do not include a query
or a fragment. Set `PAYMENTS_INTERNAL_TOKEN` to the dedicated credential that this Payments service
accepts. It must have at least 32 bytes and must differ from `INTERNAL_SERVICE_TOKEN`. There is no
fallback to the shared token. Both forms check their arguments and these three settings before they
open the Commerce database. A local configuration refusal creates no reservation. If a migration
fails, follow
[ADR-087's rollout recovery](../adr/ADR-087-operator-outcomes-for-failed-post-purchase-refunds.md#consequences).

The command sends the Payments credential only to `PAYMENTS_URL` and does not follow a redirect.
It accepts a Payments `200` only when the body is exactly the evidence that Payments writes: one
object with `provider_ref`, `amount` and `currency`, each once, with these exact names and no null
value; a `re_` reference of at most 255 bytes; a positive amount in minor units; three uppercase
letters for the currency; and no more than 64 KiB. The refusal of a `200` does not show its body.

### The buyer was refunded outside the platform

```
commerce record-refund-outcome <organizer-id> <refund-id> refunded_outside "<operator name>" "<external reference>" "<reason>"
```

Before you run it:

1. Confirm that the outside transfer is **settled** and is for the **full stored amount** of this
   refund, in its stored currency. The command does not take an amount: it copies the refund's
   amount and currency, and prints them. Running the command is your confirmation.
2. Use the transfer's own reference (bank or PSP dashboard) as `<external reference>`.
3. Type your own name as `<operator name>`. It is the only personal attribution. The record also
   stores PostgreSQL `session_user`, which on this access path is always the shared `commerce` login.

What the command does (ADR-087 §3–§6): it asks Payments, read-only, to prove that the leg is still
bound and unmarked and that the provider's stamped refund is `failed` or `canceled` for the stored
money. Then, in one short transaction, it appends the outcome, completes the refund with that outcome
as its evidence, and advances the order's refund totals once. It makes no writing HTTP call.

The result line shows `amount`, `currency`, `external_ref`, `operator`, `session_user`,
`recorded_at` and `replay`.

After success: both allowances stay consumed. No Commerce `order.refunded` fact and no Payments
journal entry is written, so a report that reads only `order_facts` or the Payments journal does not
see this refund; read `refund_outcomes`. The reversal reconciler then voids the tickets and, only
after that, returns the capacity. Follow it in [Refund reversal reconciliation](#refund-reversal-reconciliation).
If the outside transfer completed an earlier cancellation refund, a later run for the same slot uses
the same cancellation key and replays that refund without calling Payments. It reports
`already_refunded` after the order is fully refunded and all reversals are complete. After a partial
staff refund, a cancellation run can refund the remaining quantity under the cancellation key and
report `refunded`.

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
| `payments could not prove` with `payments redirect refused (HTTP <status>)` | The answer to the request at `PAYMENTS_URL` was a redirect (3xx). The command did not follow it and did not send the credential to another address. | Nothing changed and both allowances stay reserved. Check the routing from `PAYMENTS_URL` to the Payments service: it must reach Payments with no redirect. Correct the routing or the URL, then run the same command again. |
| `payments could not prove` with `payments returned invalid refund evidence` | A `200` answer is not exactly the evidence that Payments writes. This can occur when `PAYMENTS_URL` is correct. | Nothing changed and both allowances stay reserved. Find which service sent the answer: Payments, or a proxy or another service on the route. Correct the cause, then run the same command again. |
| `changed while the command ran` | The refund or its order changed between the read and the write | Run the same command again. |
| `an abandonment is reserved` | An operator started `abandoned` for this refund | Do not record it as outside. Finish the abandonment with the original command. |

**A refund whose provider refund has no stamp can never be proven.** A leg bound before the adapter
added the `compensation_key` stamp, or one the provider never answered, has no refund the proof can
find. The command refuses it, and both allowances stay reserved.

After an outside completion the refund is in the reversal backlog until its tickets are voided and
its capacity is returned. Watch `commerce.refund.reversal.outstanding` and
`commerce.refund.reversal.parked` ([Refund reversal reconciliation](#refund-reversal-reconciliation)).
A nonzero `parked` for this refund needs a human, as for any completed refund.

### Nobody will refund this attempt: `abandoned`

Use `abandoned` when the buyer is still owed the refund, no outside transfer occurred, and none is in
flight. If an outside transfer is in flight, wait for a definitive result before recording either
outcome. Use `refunded_outside` only after the full stored amount settles in the stored currency.
After `abandoned`, staff can refund the buyer again under a **new** key through the Back Office.

```
commerce record-refund-outcome <organizer-id> <refund-id> abandoned "<operator name>" "<reason>"
```

It takes no reference: Payments supplies the provider's `re_` for the failed refund, and the command
records it as `external_ref`.

`abandoned` releases the allowance, so running it after an outside transfer opens the way to a second
refund of the same money.

**Starting `abandoned` commits you to it.** The first run reserves the abandonment in Commerce
(`order_refund_abandonment_dispositions`, state `pending`) before it calls Payments. From then on,
`refunded_outside` is refused for this refund. The reservation keeps the operator name and reason of
the run that started it, and the outcome is attributed to that operator. To finish it, anyone runs
the command again with exactly that operator name and reason.

If the configuration passed the local checks, Payments can still refuse the credential after
Commerce reserves the abandonment. The reservation can then stay `pending`. Correct the credential
and repeat the original `abandoned` command with the same operator name and reason.

What the command does (ADR-087 §10): reserve in Commerce; Payments proves the terminal failure with
the same read-only lookup and writes an append-only marker on the exact leg, which releases that
leg's amount in the Payments ceiling; then Commerce appends the `abandoned` outcome and finalizes the
reservation in one transaction, which releases the quantity. The refund stays `pending`, and the
order totals, the tickets and the capacity do not change.

After success:

- An abandoned refund never enters the reversal backlog, and the reversal gauges do not change: it
  has no completion, so there is nothing to void or return.
- Staff refund the buyer again through the Back Office order console, under a new key. If the Back
  Office still holds a saved retry for the old key, submitting it answers "An operator recorded this
  refund as abandoned", retires that saved retry, and shows a blank refund form with a fresh key.
  This also works when another staff member of the organizer holds the page: the order console
  reads Commerce's staff order detail, and when it confirms the abandonment of that exact key it
  retires the saved retry, so the next refund carries that staff member's identity and a fresh key.
  The platform never re-submits automatically, and no operator command re-submits. While the
  abandonment is only reserved, a staff retry of the old key still goes to Payments.
- A bulk cancellation run gets no new cancellation refund for this order, also after a staff
  replacement. It reports `refund_refused` while quantity remains, and at zero quantity
  `already_refunded`, or `reversal_outstanding` while a reversal is still owed. Staff own any further
  refund of the order.

**Run the same command again** after any refusal, timeout, 5xx, malformed answer or Commerce error.
The reservation stays `pending` until the run that finishes it; a 404 or 409 describes one request,
and an overlapping request can still have written the marker. If Payments wrote its marker, its
allowance is released while Commerce keeps the quantity counted: the stricter state. Payments
answers the next run from its marker without asking the provider (also when the provider is down),
and Commerce finishes once. An identical command after success returns the original record
(`replay=true`). A different operator name or reason is a conflict.

| Refusal | Meaning | What to do |
|---|---|---|
| `no refund … for this organizer` | Wrong ids, or another organizer's refund | Check both ids. |
| `already carries a different outcome, or its abandonment was started with another operator name or reason` | An outcome is recorded and it is not this one, or the reservation has another attribution | Repeat the original operator name and reason, or stop and investigate before any money action. |
| `is completed` | The refund completed, through Payments or outside | Nothing to abandon. A pending reservation on it is inert. |
| `payments refused this abandon request` (404 or 409) | No such leg, the leg completed, its money differs, or the provider says the refund did not fail | The reservation stays. Correct the cause, or reconcile the original refund; then run the same command again. |
| `UNCERTAIN: payments' answer … is unknown` | Timeout, 5xx, provider inconclusive, or an answer for other money | Run the same command again. If Payments keeps answering 502, see the unstamped case below. |
| `UNCERTAIN` with `payments redirect refused (HTTP <status>)` | The answer to the request at `PAYMENTS_URL` was a redirect (3xx). The command did not follow it and did not send the credential to another address. | The reservation stays `pending` and the quantity stays counted. The Payments marker can exist even on the first attempt. A redirect leaves the outcome of the initial request unknown. Check the routing from `PAYMENTS_URL` to the Payments service: it must reach Payments with no redirect. Correct the routing or the URL, then run the same command again. |
| `UNCERTAIN` with `payments returned invalid refund evidence` | A `200` answer is not exactly the evidence that Payments writes. This can occur when `PAYMENTS_URL` is correct. | The reservation stays `pending` and the quantity stays counted. The Payments marker can exist: Payments can have written it before the answer. Find which service sent the answer: Payments, or a proxy or another service on the route. Correct the cause, then run the same command again. |
| `changed while the command ran` | The refund or its order changed between the read and the write | Run the same command again. |
| `FAILED: … was not recorded in Commerce` | A Commerce error after Payments may have accepted | Run the same command again. |

**Inspect one refund's disposition** in both databases. The Payments leg query is below.

```sql
-- commerce database
SELECT d.state, d.operator_name, d.reason, d.reserved_at, o.kind, o.external_ref, o.recorded_at
FROM order_refunds r
LEFT JOIN order_refund_abandonment_dispositions d ON (d.organizer_id, d.refund_id) = (r.organizer_id, r.id)
LEFT JOIN refund_outcomes o
  ON (o.organizer_id, o.subject_kind, o.subject_id) = (r.organizer_id, 'order_refund', r.id)
WHERE r.organizer_id = '<organizer-id>' AND r.id = '<refund-id>';
```

- `pending`, no outcome, and a Payments marker (`abandonment_ref` set): Payments accepted and
  Commerce did not finish. Run the same `abandoned` command again. Do not transfer money until it
  has finished.
- `pending`, no outcome, no marker: nothing released yet. Both allowances stay reserved. If the
  proof never becomes conclusive (an unstamped leg), it stays pending.
- An outcome `refunded_outside` together with a Payments marker on the same leg is an invariant
  violation. Stop all further money actions on the order and its charge, and investigate.

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

**Detect a late provider refund** for a refund closed with either outcome (ADR-087 §13). A
dispatch admitted before the outside proof or the abandonment marker can still send one POST under
the leg's stored provider key. The provider deduplicates it within its 24 h key retention; a process
that stalls longer can create a new refund, and nothing in the platform stops or records it as an
error. Run this check as part of provider reconciliation for every refund closed with
`refunded_outside` or `abandoned`:

1. Read the leg's compensation stamp and its charge reference:

   ```sql
   -- payments database
   SELECT l.provider_idempotency_key AS compensation_stamp, o.provider_payment_ref AS payment_intent,
          l.status, l.fact_id, l.provider_ref
   FROM payment_refund_legs l
   JOIN payment_operations o
     ON (o.organizer_id, o.idempotency_key) = (l.organizer_id, l.source_idempotency_key)
   WHERE l.organizer_id = '<organizer-id>'
     AND l.source_idempotency_key = '<order checkout key>'
     AND l.refund_idempotency_key = '<refund-id>';
   ```

2. List the PaymentIntent's refunds at the provider (`GET /v1/refunds?payment_intent=<payment_intent>`
   in Stripe, all pages) and keep each refund whose `metadata.compensation_key` equals
   `compensation_stamp`.
3. A `succeeded` refund in that list, or a leg with `status = 'refunded'` and a `fact_id` (a
   `payment.refunded` journal fact), is an invariant violation for an attempt that an operator
   closed as refunded outside or abandoned. Stop all further money actions on the order and its
   charge. A journal fact alone does not prove that money moved: the fact can record a success the
   provider reported just before the refund failed. If the journal fact is the only signal, use the
   completed leg's `provider_ref` for `GET /v1/refunds/<re_ id>`. Confirm the current refund state
   before you reconcile with the buyer and the provider.

**Roll forward, not back.** Commerce migration 0027 refuses to roll back while `refund_outcomes` or
`order_refund_abandonment_dispositions` holds a row, and Payments migration 0007 refuses while
`payment_refund_leg_abandonments` holds a row. The records are honest-operator evidence, not
tamper-evidence (ADR-021): a writer with database credentials can forge or delete them, and a
coordinated rollback can erase them. The `session_user` on an outcome is the shared `commerce` login;
the typed operator name is the personal attribution. Both migration numbers were unreleased while
TKT-525 was in review: recreate a disposable database that applied an earlier draft of 0027 or 0007,
so that goose applies the current definitions from Commerce 26 and Payments 6.
