# Development

## Toolchain

Latest stable everything (see `conventions/dependencies-and-versions.md`): Go 1.26+,
Node 24+ with pnpm 11 (pinned via `packageManager`, auto-selected by corepack/pnpm),
Docker + Compose v2. No other host dependencies; `make lint-go` installs the pinned
golangci-lint release binary into `./bin` (sha256-verified against the release checksums;
`scripts/install-golangci-lint.sh`).

## Everyday loop

```bash
make up                               # bootstraps .env once, then compose up -d --build --wait
make check                            # full local gate: lint + test + build + smoke
make lint / test / build / smoke      # individual stages
docker compose exec payments /app verify-journal    # verify the live money journal
docker compose exec access /app verify-lifecycle   # verify the live ticket lifecycle trail
docker compose exec inventory /app reconcile-pins  # reclaim seat pins left by expired holds
```

### Internal service credential (TKT-83)

No default `INTERNAL_SERVICE_TOKEN` ships in the repo. `make up` generates a random one into a
gitignored `.env` (chmod 600, other entries preserved); compose reads `.env` natively, so every
later `docker compose` command works unchanged. To supply your own, set the variable in `.env`
or the shell. Services refuse to start — before touching any dependency — when the token is
absent, empty, or the retired historical value `local-service-token`; that literal survives in
the code only as a denylist entry and in tests proving its rejection. Deleted or broken `.env`?
Run `make up` again. The smoke harness is independent: `scripts/smoke.sh` generates its own
credential per invocation, so CI needs no secret.

Go code is a `go.work` workspace: one module per service + `gateway`, `shared/go`, `smoke`.
TS code is a pnpm workspace: the Astro 7 SSR/React storefront in `web/storefront`, the Astro 7 SSR
back office in `web/backoffice` (served under `/admin/`, ADR-042), and the React/Vite scanner in
`web/scanner` (ADR-006).

## Testing model

- **Unit tests** live next to code; kept minimal at the scaffold layer (shared middleware).
- **The integration seam is the gateway**: `smoke/` asserts everything observable from
  outside — health fan-out, web applications, trace propagation, JetStream persistence, DB
  credential isolation, metrics ingestion. `make smoke` owns the stack lifecycle
  (isolated project `ticketing-smoke`, shifted ports, trap-based teardown).
- **The gate polices itself**: `scripts/gate-selftest.sh` seeds one failure per stage in a
  disposable git worktree and requires each to fail. CI runs both jobs.
- **Two smoke build paths** (TKT-42): `make smoke` packages host-built artifacts (fast,
  per-PR); `make smoke-hermetic` runs the original in-Docker builds — weekly in CI and on
  PRs touching the build files. See docs/testing.md §Smoke build paths.

## Observability

Every service calls `obs.Setup()`: OTLP export (traces, metrics, logs) to the `lgtm`
container + structured JSON on stdout with `trace_id`/`span_id`. Grafana at :3000.
Cross-service calls must use `obs.Client()` so W3C trace context propagates.

## Access failed-event recovery

Access classifies invalid `order.completed` envelopes as permanent and records only their
source identifier/fingerprint plus a bounded reason on
`platform.access.ticket-issuance.failed`. Transient issuance or delivery failures retry four
times on the configured JetStream backoff, then produce the same sanitized terminal record.
Failure-record publication happens before termination. Processing is bounded at four attempts,
while a failed terminal-record publication leaves the source message eligible for redelivery
without a JetStream delivery ceiling. Counters use the low-cardinality `reason` and `stage` labels.

To recover, inspect the failed record, repair the producer or downstream dependency, locate the
original event in the durable `PLATFORM` stream by `source_event_id`, and republish that original
envelope with a new `Nats-Msg-Id` replay suffix. Keep the event's own `id`: Access issuance and
delivery are idempotent on that identifier. Never manufacture a payload from the failed record;
it deliberately does not retain attacker-controlled event data. If the failure subject itself is
unavailable through all six deliveries, JetStream's max-deliver advisory is the operator signal
to restore the stream and replay the original message.

## Inventory catalog-event quarantine operations

Future-schema catalog events (version skew, ADR-017 §5b) are no longer parked against the
durable's ack window: inventory persists the raw envelope to the bounded
`catalog_event_quarantine` table (cap 10 000 unresolved rows) and acks the original, so variants
this binary *does* understand keep flowing (TKT-68). Readiness still latches false on the first
quarantined event and stays down across restarts while unresolved rows exist — the alarm is
Postgres-backed, not ack-window-backed.

To recover: deploy an inventory binary whose schema registry covers the quarantined variants,
run `inventory reprocess-quarantine` (one-shot subcommand; republishes the stored envelopes
byte-identically to their original subjects with deterministic `Nats-Msg-Id`s, marks rows only
after the broker accepts), then restart inventory — startup confirms no unresolved rows remain
and readiness returns. Rows the running binary still cannot read stay unresolved and keep
readiness down; `reinjected_at` means broker republication succeeded, never that inventory
business processing completed (that is `consumed_events`' job). Reinjected rows are pruned
7 days after reinjection; unresolved rows never age out. If the quarantine itself fills, new
future-schema events fall back to delayed NAKs — a deliberate, loud stall at an
inventory-owned bound, not a drop.

## Seat-pin reconciliation (TKT-112)

A seated hold pins its seats in catalog (`seat_map_pins`, `pinned_by = 'hold:<claim_id>'`) and
unpins them when the hold is released, or best-effort when the next mutation on that pool sweeps
the expiry (ADR-031 §4 — no worker, deliberately). A pool that is never touched again after an
on-sale therefore keeps the pins of holds that simply timed out. Those pins fail **safe**: they
block a now-unnecessary seat-map edit with a 409, they never orphan or oversell a seat. Left
alone they accumulate and slowly degrade an organizer's ability to edit a published map.

`inventory reconcile-pins` is the one-shot cleanup:

```bash
docker compose exec inventory /app reconcile-pins
# inventory reconcile-pins: scanned=812 reclaimed=37 live=770 unknown=2 malformed=0 other=3
```

It drains catalog's pin table in keyset pages over the internal read
(`GET /internal/seat-map-pins`), asks inventory for one verdict per `hold:` reference, and unpins
the dead ones through the same family-locked batch route a release uses. Requires the service's
usual `DATABASE_URL`, `CATALOG_URL` and `INTERNAL_SERVICE_TOKEN`; it never reads catalog's
database (ADR-010).

**Three dispositions leave the pin in place on purpose**, and the counters name each:

- `live` — the claim still consumes its seats: held and unexpired, finalizing, or **confirmed**.
  A confirmed seated claim keeps its pin because the seat is sold.
- `unknown` — the pin names a claim this inventory database does not have. Reported, never
  reclaimed: that is the shape an inventory database restored *behind* catalog presents, and
  since `hold:` pins cover sold seats too, unpinning one could let an edit orphan a sold seat.
- `malformed` — `pinned_by` is `hold:<not-a-uuid>`. The column is free-form, so this is
  reachable; it cannot be correlated to a claim, so it is a human's problem.

The run **exits zero** when `unknown` or `malformed` pins remain — that is the expected fail-safe
state, the same contract `reprocess-quarantine` has for rows awaiting a newer binary. A non-zero
exit means a store, transport, or unpin failure; every unpin it did apply is idempotent, so the
fix is to rerun. Deciding a claim is dead **settles that pool's lazy expiry under the pool lock**
(the ordinary `sweepExpired`), so a run also expires other due holds in the pools it touches and
settles any pending capacity cut there — the same thing the next hold on that pool would have
done.

Scope, stated the way ADR-021 requires: this is **honest-writer reconciliation**. It guards
against our own bugs and against concurrent honest writers. A writer with catalog database access
can insert or delete pins at will and nothing here detects it; a writer with inventory database
access can forge the claims the verdict is derived from. It is not tamper-evidence.

**Known limitation (TKT-143).** A pin whose `seat_identity` or `pinned_by` is megabyte-scale can
stop a run at that row. The command pages the pin table and shrinks the page on overflow, but a
single row that exceeds the 4 MiB response cap cannot be read, and the keyset cursor cannot
advance past a row it never read — so later pins stay unreclaimed until the offending pin is
removed by hand. Nothing this system writes can produce such a row (`pinned_by` is `hold:` plus a
uuid; identities are composed server-side from labels), and catalog enforces no length limit on
those columns, which is what TKT-143 is for. The failure is loud and names the cursor; no pin is
wrongly removed.

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
*how old* without being asked ([ADR-065](adr/ADR-065-parked-recovery-order-observability.md)):

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

## Access ticket lifecycle trail operations

The trail is chained per ticket and checkpointed per organizer
([ADR-021](adr/ADR-021-ticket-lifecycle-trail-integrity.md)). **Read §The trust boundary before
saying anything about what it protects.**

**What it is worth, and the only phrasing to quote.** Against an adversary who can write to the
Access database but holds no lifecycle private key, modification, insertion and reordering are
cryptographically evident. **Targeted rollback is not detected, at any checkpoint interval, and
neither is a compromised current key.** Every control below keeps its state inside the Access
database, which is what that adversary owns, so none of them constrain them. Closing that needs an
attestation outside this instance — TKT-11's. "The lifecycle trail is tamper-evident", with no
adversary named, is an overstatement; three clauses of ADR-021 made it before two review passes
caught them.

**Keys.** `ACCESS_LIFECYCLE_*`, under the `access-lifecycle/` namespace, with key material distinct
from `access-qr/` — a leaked credential-signing key must not also authorize rewriting history
(§D4). The namespace is enforced in code: QR material is rejected here and vice versa.

**Rotation.** Quiesce, run `access seal-lifecycle-epoch` (retains each head's current signature
under the outgoing key), add the new key to `ACCESS_LIFECYCLE_PUBLIC_KEYS` while **keeping the old
one** — history stays signed under it — then move `ACCESS_LIFECYCLE_KID` and the seed. Sealing
needs no private key: a head's stored signature already binds ticket, sequence, version and key id,
so it *is* the epoch signature for its key. Epoch signatures raise the cost of a current-key
compromise from "re-sign a head" to "re-sign a head and delete these rows". They do **not** contain
one: the rows are deletable, and because ADR-021 surrenders global ticket-set completeness, nothing
says a row must exist, so a deleted one is indistinguishable from a ticket that never had one.

**Backfill.** `access-lifecycle-backfill` is a one-shot Compose job that adopts pre-0003 history as
the chain's baseline, and `access` waits on it. It is separate from `access-migrate` because it
signs a head per ticket — cost scales with history, and ADR-008's surviving 30-second deadline
bounds the migrate job the service depends on, so a slow backfill there would stop Access booting
(§D9, amended for ADR-022). Interrupt it freely: it chains one ticket per transaction and resumes.
It cannot prove legacy rows were honest; existing history is adopted, not audited.

**Checkpoint freshness.** Default interval 60s (`ACCESS_LIFECYCLE_CHECKPOINT_INTERVAL`). Watch
`access.lifecycle.checkpoint.last_success` (updated on no-change passes too, so idle is
distinguishable from dead) and `access.lifecycle.checkpoint.pending_oldest_age_seconds`. Staleness
is the only signal there is — and note what it is a signal *of*: the worker stopped. It is **not**
evidence of tampering, because the checkpoint detects nothing today; it is the structure TKT-11
will anchor. Freshness is deliberately **not** wired into `/readyz`: that is the container's health
probe, and closing every turnstile because scaffolding stalled is the customer-denying failure §D6
exists to avoid.

**Alarms — and the gap you must close yourself.** Degraded admissions and integrity denials publish
to `platform.access.lifecycle-integrity.alarm` via a transactional outbox committed with the
decision itself, so an admission cannot happen without an owed alarm. Access **refuses to boot**
unless `ACCESS_LIFECYCLE_ALARM_DURABLE` exists and filters that subject.

**Be precise about what that check buys: an alarm is RETAINED, not read.** This repository ships
**no consumer** for that durable — `nats-init` creates it and nothing drains it. So the default
stack passes the boot check and is *still unmonitored*. ADR-021 §D6's "an unmonitored deployment
must not run this scheme in fail-open" is a **deployment obligation TKT-67 cannot discharge**: no
boot-time check can prove a human will act on a page. **Attach the durable to your on-call adapter
before running fail-open in production.**

Three signals, none of them gates:
- `access.lifecycle.alarm.durable_pending` — alarms sitting unread. Sustained non-zero means nobody
  is collecting: this is the closest thing to detecting the forbidden unmonitored deployment.
  **Alert on it.**
- `access.lifecycle.alarm.oldest_unpublished_age_seconds` — alarms we have not managed to publish.
- `access.lifecycle.alarm.dead_lettered` — alarms that will never be delivered. Every one is a
  degraded admission nobody will hear about. **Non-zero is always wrong.**

**Degraded mode** (`access set-lifecycle-mode <organizer> <normal|operator_deny|operator_admit>
<operator>`). A ticket whose chain fails verification is admitted **once**, alarmed and
quarantined; every later scan of it is denied, whatever the organizer's mode. Three distinct
first-time failures inside `ACCESS_LIFECYCLE_FAILURE_WINDOW` flip the organizer to
`operator_deny`, so continuing to admit becomes a human's explicit choice rather than a default
taken at 3am. Valid tickets keep scanning throughout.

**These bound our bugs — a canonicalization drift, a botched rotation — and nothing else.** A
database-write adversary deletes the quarantine row and resets the window between scans. Against
them fail-open is unbounded, full stop, and this section is the wording to reuse if any of it ever
reaches a dashboard.

**Pass admission policy (TKT-87,
[ADR-025](adr/ADR-025-admission-events-and-offline-reconciliation.md) §D1/§D2).** A slot whose
catalog `re_entry_policy` is `multi` or `count_limited` admits through repeatable occurrence-keyed
`entry`/`exit` events; such a ticket never gains `redeemed`. Access learns the policy from the
`re_entry` field riding additively on `platform.catalog.performance.published` (no schema bump —
ADR-017 §2), projected into `slot_re_entry_policies` by the `access-slot-policy` durable; **a slot
access knows nothing about is `single` — today's semantics, never fail-open**. Live policy denials
(`entry_limit_reached`, `exit_required`, `not_inside`, `exit_not_applicable`,
`occurrence_required`) append **nothing** to the trail.

**Derived pass-policy conflicts — the only phrasing to quote.** Pass reconciliation records
factual `entry`/`exit` only; it never mints `duplicate_admit` (that type is scoped to single-entry
tickets). A policy conflict — a `requires_exit` re-entry without a prior exit, an entry beyond
`max_entries` — is a **derived projection re-evaluated as late cross-device events arrive, ordered
by claimed device time, which is not attested**. It is alarmed conservatively on
`platform.access.admission-policy-conflict.alarm` (schema 1) as raise/withdraw pairs sharing one
`conflict_id`, and it is **revisable: an alarm can be withdrawn, an appended event cannot**. The
`pass_policy_conflicts` table is a rebuildable diff cache for that alarm stream — it is never
consulted by an admission decision and proves nothing. Access refuses to boot unless
`ACCESS_POLICY_CONFLICT_DURABLE` exists and filters the subject; the same
retained-not-read caveat as the other alarm classes applies. Quarantine admissions remain
operator-only: they do not surface in `GET /orders/{ref}/tickets` (the §D10 open question,
answered here).

**Admission conflicts — the third alarm class, and the only phrasing to quote.** When
reconciliation of a **single-entry** ticket finds an offline admit the authoritative trace would
have rejected, access appends `duplicate_admit` and owes an alarm on
`platform.access.admission-conflict.alarm` (**schema 1**), committed in the **same transaction** as
the append. **Scope that atomicity claim before quoting it:** on the honest Access reconciliation
path the lifecycle append and the owed outbox row commit together, so our own code cannot record
the admission and skip the alarm — it says nothing against a writer with database access, who can
delete the outbox row at leisure (ADR-021 §The trust boundary). This is deliberately
**not** an ADR-021 integrity alarm: there the chain is broken, here **the chain is valid and the
world disagreed with it**. Access refuses to boot unless `ACCESS_ADMISSION_CONFLICT_DURABLE` exists
and filters the subject, and the same **retained-not-read** caveat as the other two classes applies —
the boot check proves the alarm has somewhere durable to land, never that anyone reads it.

Name the adversary before quoting any of this. Per
[ADR-025](adr/ADR-025-admission-events-and-offline-reconciliation.md) §Claims, the admission-conflict
alarm **"bounds operational skew and our bugs; it is visibility, not containment against the database
adversary."** It makes a double-admit *visible* at reconciliation; it does not prevent the physical
admission that already happened, and an occurrence that never syncs is not represented at all.

The payload carries four bounded identifiers, one device-claimed timestamp and one boolean —
`alarm_id`, `organizer_id`, `ticket_id`, `occurrence_id`, `device_occurred_at`, `skew_flagged` — and
no buyer, guest reference or raw scanner-operator identity (ADR-025 §D9 as amended by TKT-119;
ADR-003 §D3). Be precise about what is enforced: `TestReconcileConflictAppendsDuplicateAdmitAndOwesAlarm`
pins the exact key set of the persisted envelope at both levels, decodes every value at both levels
into the scalar it is contracted to be — rejecting `null` explicitly, which every one of those
decodes otherwise accepts — so a new field or a nested object cannot arrive unnoticed; and it pins
`conflictAlarmData`'s json tags **and field types** at the source, which is the only place a `,omitempty` field can be caught, since such a field is simply
absent from any fixture that leaves it zero. There is deliberately **no `reason` field**: unlike the policy-conflict class
(whose payload carries `rule`), this class has exactly one
condition (ADR-025 §D2 scopes `duplicate_admit` to single-entry tickets, where any occurrence beyond
the one `redeemed` is the conflict), so **the subject is the reason** and a one-valued enum would
carry no information. If a second condition is ever added to this class, introducing a reason space
is an [ADR-017](adr/ADR-017-domain-event-schema-evolution.md) §3 decision on this contract — a
required field fires §3b independently of whether any consumer exists — not a payload tweak.

What that test is **not**: proof that the payload contains no PII, and not a constraint on anyone
who can write to the Access database. It is a producer-schema check against honest application
changes (ADR-021 §The trust boundary). `device_occurred_at` is device-*claimed* and correlates with
a physical gate event, so the payload is bounded, not anonymous.

### Exchange entitlement switches (TKT-166)

An exchange voids the source order's tickets and issues the replacement set **in one access
transaction** (`store.SwitchExchange`, [ADR-039 §3](adr/ADR-039-exchange-settles-the-difference.md)).
Two transactions would each be individually correct and jointly wrong: void-then-issue opens a
window where **neither** ticket admits, issue-then-void one where **both** do. Neither is
recoverable by a retry, because the harm lands during the window.

Operationally that means:

- **`exchanged` is a lifecycle event like any other.** It goes through `appendLifecycle`, it is
  once-per-ticket, and `access verify-lifecycle` asserts one-to-one coverage over it. A direct
  INSERT into `lifecycle_events` reads as tampering — the same rule as every other event type.
- **A stuck exchange leaves the OLD tickets valid.** If the switch transaction rolls back, the
  `consumed_events` receipt rolls back with it, so the event is still owed and JetStream redelivers.
  The buyer keeps a working entitlement throughout. The recovery path is the ordinary sanitized
  failed-event procedure above; nothing exchange-specific is needed.
- **`switched, capacity outstanding` is a real state.** Commerce sets `tickets_exchanged_at`
  *before* asking inventory to return the old capacity, so the safety ordering is checkable
  (ADR-039 §3b). A crash in between leaves `capacity_returned_at` NULL — under-sold, visible, and
  retried by the unacknowledged message. Query it with:

  ```sql
  SELECT id, source_order_id, settled_at, tickets_exchanged_at, capacity_returned_at
  FROM order_exchanges
  WHERE settled_at IS NOT NULL AND capacity_returned_at IS NULL;
  ```

- **A gate refusing `exchanged` is not an integrity problem.** The verdict is its own
  (`DecisionExchanged`), distinct from `refunded`, and it is checked **before** chain verification —
  the degraded posture admits once (ADR-021 §D6), and an exchanged ticket must not be the one it
  admits. The buyer holding it has a live replacement under the **same** guest-order link.

- **A used source ticket refuses the switch.** If any source ticket carries `redeemed` or a pass
  `entry`, `SwitchExchange` returns `ErrSourceTicketsAlreadyAdmitted` and switches nothing —
  otherwise voiding a used ticket and issuing a fresh one would admit the same entitlement twice.
  The exchange stays settled-but-unswitched, which is visible in the query above and is what every
  exchange looked like before TKT-166. Resolving one is a **human decision**, not a retry: whether a
  used ticket may be exchanged at all is still open (ADR-039 §2, TKT-169). The failure record
  says `exchange_refused`, **not** `issuance_retries_exhausted`, and it is published on the first
  delivery — retrying a fact about history cannot change it, and filing it under exhaustion would
  send an operator looking for a broken dependency.

  Note what this state costs: the buyer paid the difference and gets no replacement. That is a
  **stranded paid exchange**, and it is the deliberate trade — the alternative available without a
  product decision is admitting the same entitlement twice. Fencing the source tickets during
  `switch_pending` would remove the strand, and would do it by **denying a legitimate holder entry
  at the gate** while an exchange is mid-flight. For a ticketing system that is the worse failure,
  which is exactly why it is a decision (TKT-169) and not a fix.

- **The buyer's link shows the old tickets too, without QR codes.** Replacement tickets share the
  source order's guest reference deliberately, so one link covers the whole story. The storefront
  suppresses the QR for any ticket whose history contains `exchanged` or `refunded` and labels it
  unmistakably — otherwise the buyer is handed four identical numbered codes and discovers at the
  gate which two work.

**When a switch exhausts its retries**, the terminal failure record and the republish procedure
above are the recovery, and they converge: a republished event finds its `consumed_events` receipt,
so the switch is a no-op, and the callback runs **anyway** — `processExchanged` does not branch on
whether the switch was fresh. That is deliberate and pinned by a test; without it a capacity return
lost to an outage could never be recovered.

## Event-cancellation bulk refunds (TKT-159)

Refunds every completed, not-fully-refunded order on one slot, with a per-order outcome. Internal
and on demand — it does **not** require the slot to be in a cancelled state, and never reads catalog.

```bash
# start a run (Idempotency-Key is required; the same key replays the same run)
curl -X POST "$COMMERCE_URL/internal/slots/$SLOT_ID/cancellation-refunds" \
  -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" -H "Idempotency-Key: cancel-$SLOT_ID-1" \
  -H 'Content-Type: application/json' \
  -d '{"organizer_id":"'"$ORGANIZER_ID"'","actor":"ops@example.test","reason":"event cancelled"}'

# read the report (202 while it is still running, with the counts; 200 when complete, with the rows)
curl "$COMMERCE_URL/internal/cancellation-refunds/$RUN_ID?organizer_id=$ORGANIZER_ID"
```

Reading the outcomes:

| Outcome | Means |
|---|---|
| `refunded` | This run returned the money **and** the tickets are void **and** the seat is back. |
| `already_refunded` | The order was already fully refunded, with every obligation discharged. No provider call was made. |
| `failed` + `reversal_outstanding` | The money is back but a reversal is not done. `money_refunded`/`tickets_voided`/`capacity_returned` say which half. A **seated** order stays here permanently (TKT-164). |
| `failed` + `no_captured_money` | A zero-price (comped) order. It has no money leg — and therefore got **no reversal at all**, so its tickets still admit. |
| `failed` + `refund_refused` / `unavailable` / `not_refundable` / `internal` | The refund did not happen. Failures are terminal **for that run**; retry by starting another run, which is safe. |

`incomplete_at_enumeration` on the report is the count of orders that existed on the slot at the run's
cutoff but were not completed, so this run could not refund them. Non-zero means a later run may owe
somebody money.

Tuning: `CANCELLATION_REFUND_INTERVAL` (default `10s`) and `CANCELLATION_REFUND_BATCH` (default `8`).
The batch bounds one claim and, through it, the lease.

Why a second run cannot double-refund, and why the refund key ignores the run:
[ADR-040](adr/ADR-040-event-cancellation-bulk-refund-runs.md) §3.

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
contradict ADR-021 §D6: [ADR-062](adr/ADR-062-refund-reversal-reconciliation.md) §5.

## Exchange obligation sweep

`order_exchanges` carries two obligations — the switch (`tickets_exchanged_at`) and the source line's
capacity (`capacity_returned_at`). The tickets-switched callback discharges both on the happy path and
answers **502** when capacity is unresolved, which keeps access's message unacknowledged so JetStream
redelivers. The sweep (TKT-259, [ADR-063](adr/ADR-063-exchange-reversal-reconciliation.md)) is the
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
resolves it (TKT-255, [ADR-067](adr/ADR-067-wedged-exchange-operator-unwind.md)).

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

## Journal signing key rotation

The payments money journal is signed with HMAC-SHA256 under a **keyring**: one active key that new
entries are signed with, plus retired keys retained so their era stays verifiable (ADR-016 §Decision
8, ADR-032 §Keyring configuration, rotation and retirement).

```
JOURNAL_KEY_ID           active key id, e.g. local-v2
JOURNAL_SIGNING_KEY      active secret, >=16 bytes, raw (not base64)
JOURNAL_HISTORICAL_KEYS  retired keys: kid=<secret>,kid=<secret> — secrets base64.RawStdEncoding
```

**To rotate**, in one deployment (never two):

1. Encode the **outgoing** secret: `printf %s "$OLD_SECRET" | base64 | tr -d '=\n'`
   (unpadded standard alphabet — `base64.RawStdEncoding`; a padded value is rejected. Keep the
   `\n` in `tr`: GNU `base64` wraps at 76 columns, so a secret over 57 bytes otherwise carries an
   embedded newline. `openssl base64 -A` avoids the wrapping entirely.)
2. Set `JOURNAL_HISTORICAL_KEYS` to `"<old-kid>=<that value>"`, appending to any existing list.
3. Set `JOURNAL_KEY_ID`/`JOURNAL_SIGNING_KEY` to the new key. **`JOURNAL_SIGNING_KEY` is RAW — do
   not base64 it.** The asymmetry is deliberate (historical keys share one delimited variable and
   need an encoding; the active key does not), and it is the one step here that fails *silently*:
   a base64 blob is over 16 bytes and passes every check, so the service boots and signs real money
   facts under a key nobody wrote down. **Startup now rejects this outright when the pasted value is
   the base64 of a key in `JOURNAL_HISTORICAL_KEYS` (below) — it is no longer silent.**
4. Deploy, then run `verify-journal` — it must pass over the now mixed-key chain.

Steps 2 and 3 must land in the **same** deployment: a new active key without the old one in the
ring makes every pre-rotation entry fail verification.

**Startup rejects the classic mis-paste.** If `JOURNAL_SIGNING_KEY` **decodes** to a key listed in
`JOURNAL_HISTORICAL_KEYS` — i.e. you pasted step 1's output into step 3 — payments refuses to start
and names the key it matched. That is the one rotation error the steps above can actually produce,
and it used to be silent.

It compares *decoded bytes*, so it does not matter which encoding your tooling produced: the step-1
pipeline above strips padding, a bare `base64` keeps it, and both are caught.

Three honest bounds — the guarantee is exactly this and no wider:

- **Standard base64 only**, padded or unpadded. ADR-032 fixes the keyring's encoding as standard
  base64, so a URL-safe (`-_`) paste is a different mistake and is deliberately outside this
  guarantee rather than half-covered.
- It catches a key that decodes to a secret **in the ring**. Base64 of some other key, or any other
  wrong-but-plausible secret, is not detectable — nothing in this package can make that detectable.
- It cannot tell the mistake from intent. A raw key that happens to decode to a ring member is
  rejected too. That needs a key both arbitrary and a valid encoding of a secret already in the
  ring; the error names what to change, and there is deliberately no override — an escape hatch on
  a fail-closed check is worth less than the case it would serve.
- It is not a security control. This ring is secret material and every holder can forge under every
  kid in it (ADR-021 §the trust boundary). It catches an honest operator's paste error.

Payments also logs the active key id at startup and before `verify-journal`:

```json
{"msg":"journal signing key loaded","journal_key_id":"local-v2"}
```

The key **id** only, never anything derived from the key. An earlier draft of this feature logged a
truncated HMAC of the key as a "fingerprint" so operators could compare it; that was withdrawn
because a deterministic tag over a fixed public message is an **offline oracle** for guessing a
symmetric secret — anyone who can read logs could test candidate keys — and nothing here requires
`JOURNAL_SIGNING_KEY` to be high-entropy. Do not reintroduce a key-derived value into logs or
metrics. (The readable development default this once warned about is gone: ai-review S5 removed
every checked-in signing-key default, `make up` generates one per clone, and the binary refuses
the retired literal forever.)

**To retire** a key, drop its entry from `JOURNAL_HISTORICAL_KEYS` — but only once **no retained
entry references it**, including backups and archives expected to stay auditable. Retiring a key
that is still referenced deliberately makes that era permanently unverifiable; `verify-journal`
then fails naming the unknown key id. Nothing prevents this at startup (validating the ring's
structure is not scanning the journal's contents), and nothing runs `verify-journal` for you in a
deployed environment — so run it after any retirement.

**What the ring is, said plainly.** These are **secrets**, not public keys. This is the opposite of
the access lifecycle keyring (§Access ticket lifecycle trail operations), where a verifier holds
only public keys and genuinely cannot rewrite history. Here **anyone holding the ring can forge
under every key id in it**. Rotation buys one thing: retiring a key no longer invalidates its
history. It does not produce a verifier that lacks signing power, and `verify-journal` must never be
described to an auditor as if it did. Modification and insertion by a database writer who holds no
key are evident; targeted rollback and current-key compromise are not, and remain TKT-11's.

## Orphan-prevention correction wave (TKT-183)

Catalog emits `performance.published` **schema 5** for a seated slot bound to a seat-map
version with `orphan_prevention_enabled` on. TKT-179 shipped that setting *before* this
transport existed, so slots published in between were emitted at **schema 4**, had
`event_emitted_at` set, and will never be emitted again — re-POSTing publish is idempotent.
Inventory holds an ordinary seated pool for them, with no flag and no adjacency, while the
back office insists the rule is on. The wave repairs exactly those.

```
docker compose exec catalog /app reemit-orphan-prevention
```

**Rollout order is the reason this is a separate revision — do not reorder it:**

1. TKT-181's inventory (schema-5 arm + projection) and TKT-182's rule are fully deployed.
2. Roll out this catalog binary.
3. Run the command.
4. Once every pre-TKT-183 catalog replica has drained, **run it again**.

Step 4 is a stopping condition, not a correctness precondition. The wave keeps no
correction state, so each run reconciles every published slot currently bound to an enabled
version — a slot published at schema 4 by an undrained old replica is picked up by the next
run. Re-running is safe and expected: the correction identity is deterministic per
publication, so repeats converge instead of multiplying events (inventory's
`consumed_events` and JetStream's dedup window absorb them).

It prints `corrected=<n>`, the number of publications re-emitted. Any publish failure aborts
the run and surfaces the error — the count never claims a repair it cannot prove. A second
run reporting the same count is normal and means nothing is wrong; it does **not** mean the
first run failed.

**What it does not tell you.** `corrected=` counts *emissions*, not repaired pools. Inventory
applies them asynchronously. To confirm the repair, check the pool: `orphan_prevention_enabled`
true and a populated `seat_claim_adjacency` for that pool. And per ADR-021: this is
honest-writer consistency — anyone who can write to catalog can change bindings or forge
candidates, and anyone who can write to inventory can undo the repair.

**Do not run it against an inventory that predates TKT-181.** Schema 5 reaching such a replica
is quarantined and acked, latches the consumer unready, and needs `reprocess-quarantine`
**plus a restart** by an operator (see *Inventory catalog-event quarantine operations*).

## Back-office sign-in (TKT-190)

`/admin/` is behind a staff session. Three paths stay anonymous and nothing else does:
`/admin/login`, `/admin/healthz` (Compose probes it **directly on the container**, so gating
it would make the back office unhealthy and the whole stack would fail to start) and
`/admin/_astro/*` (the hashed assets the login page itself needs). An unknown admin path is
gated exactly like a real one, so an anonymous caller cannot map the surface.

**Roles (TKT-197).** `admin`, `box_office`, `finance` — the vocabulary is the `StaffRole` enum in
catalog's contract and nowhere else. `admin` reaches everything the back office exposes; the other
two reach the venue list and sign-out and little else today, because the surfaces they exist for
(order console TKT-193/194, settlement TKT-23) are not built. That is expected, not a misconfiguration
— widening a route's roles to give someone something to do is the wrong fix.

The route→role table is `web/backoffice/src/lib/authorization.ts`, read by both the gate and the
navigation. A page added under `src/pages/` without a row there **fails the build**; so does a row
naming a page that no longer exists.

**If one account suddenly gets a 500 at sign-in while everyone else works**, its stored `role` is
outside the vocabulary. That is deliberate: an unrecognised role must not authenticate, and it is
reported as a server-side problem rather than "wrong password" so nobody resets a password that was
never wrong. Check `staff_accounts.role` against the enum.

**Provision an account** — there is no seeded default credential, deliberately (TKT-83 removed
the last checked-in default). The password is read from stdin only; a `--password` flag would
put it in shell history, the process table and any log that captures argv:

```bash
printf '%s' "$PASSWORD" | docker compose exec -T catalog /app provision-staff \
  --organizer-id 00000000-0000-0000-0000-000000000001 \
  --identifier ada@example.test --role admin
```

`--role` is validated against the contract vocabulary, so a typo fails here rather than at that
person's first sign-in days later.

It prints the new staff id and nothing else. Provisioning is **create-only**: a colliding
identifier fails rather than resetting a live account's password. `--role` is stored but
interpreted nowhere until TKT-191.

**Sessions are in-process and are not persisted.** A back-office restart signs everyone out,
and a second replica would not share them — accepted for a single-replica Compose staff tool,
and reversible without moving the enforcement point (ADR-042). The absolute lifetime is eight
hours; it does not slide, so a stolen cookie cannot be kept alive by using it. One staff member
holds at most **five** concurrent sessions: signing in on a sixth device ends the oldest. That
cap is what bounds the session map, not the expiry sweep.

**Signing out invalidates server-side.** Replaying the captured cookie afterwards fails —
that, not the browser being told to drop it, is what the smoke suite asserts.

**What this does and does not gate.** It gates the back-office UI. Since **TKT-191** catalog also
refuses any write without `CATALOG_STAFF_WRITE_TOKEN`, so the API is no longer open either — but
catalog authenticates the **back office**, not the individual staff member. Which staff member may
drive which write is decided in the back office, and today the answer is "any signed-in one".
Per-role authorization is TKT-197. Do not read "catalog writes are authenticated" as "catalog
enforces who may write what" (ADR-042 § TKT-191 amendment).

**These credentials are generated separately by `make up`:** `INTERNAL_SERVICE_TOKEN` (shared;
opens every service's internal surface), `CATALOG_STAFF_WRITE_TOKEN` (catalog writes only; held by
catalog and the back office), and `INVENTORY_STAFF_WRITE_TOKEN` (TKT-244/ADR-057; opens exactly the
two operations the channel-allocation editor needs), alongside the commerce, payments and access
keys `scripts/env-bootstrap.sh` documents at the top. One leaking does not imply another, which is
the entire reason each gets its **own** `/dev/urandom` draw rather than a shared one. Catalog fails
startup without its two; inventory **refuses to start** when `INVENTORY_STAFF_WRITE_TOKEN` equals
`INTERNAL_SERVICE_TOKEN`, because equal values collapse the blast-radius boundary while looking
configured.

**If `make up` fails on a missing credential, the bootstrap has fallen behind Compose.** That is
what TKT-227 fixed: TKT-244 made `INVENTORY_STAFF_WRITE_TOKEN` mandatory in `compose.yaml` without
adding it to `scripts/env-bootstrap.sh`, so interpolation failed with a message telling the
developer to run `make up` — the command that had just failed. `make check` never saw it, because
the smoke stage builds its environment from `scripts/stack-env.sh` instead.

```
make check-required-env    # every required var in compose.yaml is generated, non-empty
```

It reads **both** of Compose's required forms, which do not mean the same thing: `${VAR:?msg}`
fails when the variable is unset **or empty**, while `${VAR?msg}` fails only when it is unset. So
the check is not "was a name assigned" but "would Compose accept what the bootstrap left" — a
name-only comparison passes on `VAR=` while `make up` still dies.

It bootstraps into a throwaway sandbox (never your `.env`), prints names and reasons but never
values, and runs in CI inside the `gate-selftest` job, where three seeded mutations — a deleted
name, a name generated empty, and a requirement written in the alternate `${VAR?}` form — prove it
can still fail. It is deliberately **not** in `make check`, which never starts the stack it
protects. When you add a required variable to `compose.yaml`, add it to `env-bootstrap.sh` in the
same change.

**An existing `.env` is preserved.** A run adds only what is missing; it never rotates a value you
already have. So a developer who once set `INVENTORY_STAFF_WRITE_TOKEN` equal to
`INTERNAL_SERVICE_TOKEN` by hand keeps that value and inventory will keep refusing to start —
delete the line from `.env` and re-run `make up` to get a fresh independent draw.

**Verifying a change here needs a real browser.** The smoke suite submits the login and logout
forms through the real gateway and Astro SSR layer, but it sets `Origin` itself — it cannot
prove a browser sends it on a same-origin POST, nor that a browser honours `SameSite`. Run
`make browser` and add a spec to `test/browser/` (see the ticket DoD and
`learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md`).

## The browser-submit gate (TKT-228)

`make browser` brings up a smoke-shaped stack on its own ports, runs every `test/browser/*.mjs`
spec against it through real Chrome, and tears it down. It is **not** part of `make check` — it
drives the host's browser, so CI cannot run it and a developer without Chrome must still be able
to pass the gate.

```
make browser              # up, run every spec, tear down
./scripts/browser.sh up   # leave the stack up for iterating
./scripts/browser.sh run  # re-run the specs against it
./scripts/browser.sh down
```

A spec gets `BASE` (the gateway URL) and `POSTGRES_CONTAINER` (for the operator-style `psql`
reads a mailbox or an audit check needs) from the script; it must never hardcode a port. Ports and
the compose project name come from `scripts/stack-env.sh`, shared with `scripts/smoke.sh` so the
two stacks can be up at once and cannot collide.

## Customer accounts (TKT-220)

Buyers may create an **optional** account on the storefront. Guest checkout is unchanged and stays
the default: no storefront route redirects to sign-in, and buying never requires an account. See
ADR-049.

**Where things live.** Accounts are `customer_accounts` in **commerce** (migration `0015`), reached
through two public contract operations — `POST /api/commerce/customers` (register) and
`POST /api/commerce/customers/authenticate`. Neither takes a credential, which is why the storefront
container still holds exactly one environment variable (`GATEWAY_URL`) and **no service token**. Do
not "fix" that by giving it one.

**Sign in / register / sign out** are at `/{locale}/account/sign-in`, `/{locale}/account/register`
and (POST only) `/{locale}/account/sign-out`. There is no provisioning CLI and no seeded account —
registration is public, unlike staff.

**Password recovery exists (TKT-226).** `/{locale}/account/forgot-password` mails a link;
`/{locale}/account/reset-password?token=…` redeems it. See § *Password recovery* below. There is
still **no email verification and no magic link** — an account can register an address it does not
own.

**Registering an address that already exists answers 409 and says so.** That is a deliberate,
recorded disclosure — an unauthenticated membership oracle over the customer base (ADR-049 §2).
Rate limiting it is **TKT-224**. Now that mail exists the standard mitigation (answer 201, mail the
owner) has become available and **§2 is revisitable** — deliberately not done in TKT-226, because it
forces registration to stop returning a principal and breaks the register→signed-in flow. See
ADR-049 § *TKT-226 amendment*.

**A wrong password and an unknown address are the same answer and the same cost.** If you are
debugging a sign-in and want to know which it was, the answer is deliberately unavailable from
outside; look at `customer_accounts` directly.

**Sessions are in-process and are not persisted**, exactly like the back office: a storefront
restart signs every customer out, a second replica would not share them, the eight-hour lifetime is
absolute and does not slide, and one customer holds at most **five** concurrent sessions. The cap is
what bounds the session map, not the expiry sweep.

**The customer session cookie is scoped to `/`, not to an account subtree** — a deliberate departure
from the back office's `/admin`-scoped cookie, argued in ADR-049 §5. It is therefore attached to
same-origin requests to `/api/*`, `/admin/` and `/scanner/`.

> **Standing constraint:** request logging must never log the `Cookie` header. Today nothing does —
> `shared/go/obs/requestlog.go` records method, path, status and duration only — and ADR-049 §5
> accepts the cookie's scope *on that basis*. Adding header logging anywhere in the gateway or the
> services invalidates that argument and needs the cookie re-scoped, not a note.

**Verifying a change here needs a real browser**, for the same reason as the back office and with
one extra: `security.checkOrigin` defaults to **true** in Astro 7 and the storefront had never set
it, because it had no form until this ticket. `make check` renders storefront pages and never
submits one, so the whole "SSR rejects the write before the handler runs" class is invisible to it.
Drive `make up` and submit register, sign-in and sign-out.

### Attributing a purchase to an account (TKT-221)

A checkout made while signed in is attached to the account; a guest checkout is not, and guest stays
the default. See ADR-049 § *TKT-221 amendment*.

**The checkout no longer goes straight to commerce.** It posts to the storefront's own bridge at
**`/checkout`**, which attaches a commerce-signed assertion server-side when a session is live and
forwards everything else untouched. The session cookie is `httpOnly` and the checkout runs in a
browser island, so nothing else could have done it. If checkout starts failing, `/checkout` is the
first place to look — and note it passes the storefront's origin check, **including for guests**.

**There is now a fourth credential**, `COMMERCE_CUSTOMER_ASSERTION_KEY`, held only by commerce and
generated independently by `make up` alongside the other three. **Commerce refuses to start if any
two of its three credentials are equal** — the error names the pair and never echoes a value.

**Rotating that key signs everyone out of attribution.** There is no multi-key verification: every
assertion minted under the old key stops verifying immediately, so signed-in buyers get guest orders
until they sign in again. Rotate deliberately.

**An expired session at checkout is a guest checkout, not an error.** That is on purpose: breaking a
working checkout because the buyer had once been signed in is worse than an unattributed order they
can still reach by order reference.

**`GET /orders/{id}` reports `customer_id` and it is informational only** — that read is public and
answers for any order id. It is not an ownership check.

### The wallet (TKT-222)

`/{locale}/account` lists the signed-in customer's completed purchases, newest first, 20 to a page,
each linking to the existing ticket page. See ADR-049 § *TKT-222 amendment*.

**If the wallet shows purchases with no event name**, catalog's display-name resolver is failing —
commerce logs a `WARN` and renders the rows anyway, deliberately: a row the buyer can still open
beats a wallet that will not load. Look for `wallet display names unavailable`.

**If the wallet says "could not be loaded"**, the commerce read itself failed. That is a different
state from an empty wallet, and the page distinguishes them on purpose.

**A customer asking for another customer's wallet gets 404, not 403** — the same answer as an id that
does not exist. That is deliberate non-disclosure, not a bug: 403 would confirm the account exists.

### Claiming a past guest order (TKT-223)

A signed-in buyer can attach a completed guest order to their account from the ticket page. See
ADR-049 § *TKT-223 amendment*.

> **A claim can be undone since TKT-225** — see *Undoing a claim* below. It is an operator action,
> not something the buyer can do, and it is recorded.

**The proof is the order reference alone**, deliberately — matching the email would refuse a buyer
who signed up with a different address. That means anyone holding a leaked reference can claim the
order, which made **TKT-202** (every service logged the reference via the URL path) a safety issue
for this feature and not just logging hygiene. TKT-202 is closed: the request log, the contract-drift
log and the OTel span attribute all sanitise declared capability segments. References already written
to retained logs before that change remain valid, so ADR-052's detach path is still the recovery.

**Every refusal is the same 404.** "No such order", "not completed" and "already claimed by somebody
else" are indistinguishable on purpose. If a buyer reports that claiming does not work, the answer is
in `orders` — check `status` and `customer_id` for that `guest_order_ref`.

**Paging is keyset on `created_at`, never `updated_at`.** `updated_at` is rewritten by checkout
retries, recovery and the refund runner, and a cursor on a mutable key makes rows jump pages. If
someone "optimizes" the sort key, that is the bug to look for.

### Undoing a claim (TKT-225)

The recourse for "someone else claimed my order". See ADR-052.

```bash
curl -sS -X POST "$COMMERCE_URL/internal/orders/$ORDER_ID/unclaim" \
  -H "X-Internal-Token: $COMMERCE_INTERNAL_TOKEN" \
  -H "Idempotency-Key: support-4471" \
  -H 'Content-Type: application/json' \
  -d '{"actor":"staff:amy","reason":"claimed by the wrong account, buyer confirmed by phone"}'
```

It answers `200` with the order id and **the customer it was detached from** — record that value.
It is gone from `orders` the moment the call succeeds, and it is the only way to know who to hand
the order back to if you detached the wrong one.

**Direct to commerce, not through the gateway.** `/api/commerce/internal/` is edge-denied by
construction, and a smoke test pins that for this route.

**`Idempotency-Key` is required, and it is not ceremony.** Because a detached order is immediately
claimable again, a retry without a key could detach whoever claimed it in between — someone you
never reviewed, recorded under your reason. Reuse the same key when retrying; use a NEW key when you
genuinely mean to detach again (a second mis-claim on the same order).

**`actor` and `reason` are required and are recorded** in `order_attribution_detachments`. Blank or
whitespace-only values are refused by the handler *and* by a database `CHECK` — an un-claim that
says nothing about itself is a failed record, not a permitted one.

> **What the record is worth.** These rows live in the commerce database, so anyone who can write it
> can alter or delete them: **accountability against a careless operator, not tamper evidence
> against a hostile one** (ADR-021). And `actor` is a label the caller supplies — the internal token
> carries no individual identity. Do not describe this trail as tamper-evident.

**Every refusal is `404 "order is not detachable"`** — no such order, not completed, or already
unattributed. As with the claim, the answer is in `orders`: check `status` and `customer_id`. A
malformed request (blank actor/reason, bad uuid) is `400`, which is a different problem.

**A detached order is immediately claimable again, by anyone holding the reference.** That is
deliberate (ADR-052 § 4): blocking re-claim would block the rightful buyer, and would not stop an
attacker who can register another account. Tell the buyer to claim it promptly. What bounds abuse is
ADR-051's rate limiting on the claim path — and the underlying exposure was fixed by TKT-202, which
stops this platform emitting the reference into its own logs and traces (it cannot un-leak references
already retained elsewhere).

**Detaching is not transferring.** It restores `NULL` and stops. There is no operation that moves an
order from one account to another in one step (TKT-9/TKT-160). If you need to move an order, detach
it and have the correct buyer claim it.

### Password recovery and the mail path (TKT-226)

`/{locale}/account/forgot-password` asks for a link; the mailed link opens
`/{locale}/account/reset-password?token=…`. See ADR-050.

> **Nothing in this system has ever sent an email.** The only `mail.Sender` is
> `shared/go/mail.Fake`, which validates a message, keeps it in memory and returns success.
> `make up`, the smoke stack and `make check` all run against it, deliberately — the gate must not
> need a network or a provider account. **A buyer on a local stack will never receive anything.**
> To see what *would* have been sent, read the row (below).

**Where a message actually is.** `mail_outbox` in the commerce database. One row per message,
`sent_at IS NULL` until the drainer's sender accepted it:

```sql
SELECT id, recipient, subject, sent_at, attempts, last_error, dead_lettered_at
  FROM mail_outbox ORDER BY created_at DESC LIMIT 20;
-- the reset link itself, when you need to follow it locally:
SELECT body FROM mail_outbox WHERE recipient = 'buyer@example.test' ORDER BY created_at DESC LIMIT 1;
```

> **That table holds PII and live credentials in plaintext.** The body of a reset message *is* a
> working reset link until it is redeemed or expires. Nothing prunes the table — retention is
> **TKT-33**. Treat a dump of it as you would a password file.

**"The reset link never arrived."** In order: is there a row? Is `sent_at` set? Is
`dead_lettered_at` set — that row is quarantined and will never be retried, and `last_error` says
why. A row that is present and unsent with rising `attempts` means the sender is refusing; a row
that never appears means the address has no account, and **that is indistinguishable from the
outside on purpose**.

> **`last_error` starting with `DELIVERED-BUT-UNCONFIRMED` means the opposite of what the row
> looks like.** That row has `sent_at IS NULL` and is dead-lettered, but the sender *accepted* the
> message — possibly on every attempt — and only the database write recording that failed. **Do not
> tell a customer nothing was sent, and do not resend by hand without asking them first.** Every
> other dead-lettered row means what it says: nothing left.

**The drainer logs nothing from the message** — not the recipient, not the subject, not the body,
on any path including failure. The `message_id` is the operator's handle. Do not "improve" those log
lines: for a reset the body is a credential and the recipient is the fact the endpoint refuses to
disclose.

**A reset request answers 202 for every address**, known or not. There is no 404 and adding one
would be a security regression, not a usability fix. The answer is identical in status and bytes; it
is **not** identical in cost (a known address commits two rows), and ADR-050 records that residual
rather than claiming otherwise.

**`PUBLIC_BASE_URL` is what reset links are built from** — the same variable and meaning access
already uses for ticket delivery. Commerce **never** derives it from the request: a link base taken
from the `Host` header lets a caller mail a victim a genuine reset link pointing at the attacker's
site. Unset degrades to a startup WARN and undeliverable mail; every other operation still serves.

**Tokens are single-use, one hour, and SHA-256 in the database.** Not bcrypt — a salted hash cannot
be looked up, only verified, so finding the row would be a full-table scan of KDF operations.
Requesting a new link invalidates the previous one, and redeeming one invalidates the customer's
others.

**A completed reset signs that customer out everywhere — in the storefront process.** That is the
half that makes a reset meaningful: changing the credential does not touch the session map. It works
because the storefront route calls `destroyAllSessionsForCustomer`. **A reset completed by calling
commerce directly signs nobody out**, and a storefront restart or a second replica is outside it
either way (ADR-049 §4).

**Verifying a change here needs a real browser**, for the reason the rest of this section's features
do: `make check` renders storefront pages and never submits one. Submit both reset forms.

## Cache kill-switch (TKT-210)

Both in-memory read caches — inventory's availability cache (ADR-044) and catalog's public-read
cache (ADR-045) — can be **bypassed on a running process, without a restart or redeploy**. This is
ADR-004's incident switch: use it when a cache is suspected of serving something wrong, or to
compare behaviour with it out of the way.

The surface is `/internal/cache-control` on each service, reached **directly on its loopback port**,
not through the gateway — the edge denies `/api/<svc>/internal/*` with 404 by construction. Auth is
the shared `X-Internal-Token`; a wrong or missing token gets 401.

```bash
# inspect
curl -s -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" localhost:8091/internal/cache-control
curl -s -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" localhost:8090/internal/cache-control
# -> {"enabled":true,"entries":42}

# disable (purges immediately; every read then goes to the database)
curl -s -X PUT -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" \
     -H 'Content-Type: application/json' -d '{"enabled":false}' \
     localhost:8091/internal/cache-control

# restore
curl -s -X PUT -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" \
     -H 'Content-Type: application/json' -d '{"enabled":true}' \
     localhost:8091/internal/cache-control
```

Ports: inventory `8091`, catalog `8090` (`INVENTORY_PORT` / `CATALOG_PORT`; the smoke suite shifts
both per checkout).

**Read this before using it in an incident:**

- **It is not durable. A restart comes back ENABLED.** If you disable a cache and then roll the
  service, the cache is back and nothing will tell you. Re-check the state after any restart.
- **It is process-local.** The response describes *the process you asked*, and says nothing about
  another replica. With more than one, address each.
- **It is not a rate limiter, and disabling makes database load go UP, not down.** Every read that
  was being served from memory now reaches Postgres. Each cache still bounds its concurrent source
  calls, but that is a ceiling on concurrency, not on load. Disabling a cache during a traffic spike
  is a way to make an incident worse.
- **It does not purge anything downstream** — not the storefront's SSR cache, not a browser, not a
  future CDN. Those expire on their own tiers (`Age` keeps them from stacking; ADR-045).
- **Re-enabling starts cold**, so expect a reload wave proportional to how hot the data was.
- `entries` is **cardinality, not bytes**. A small count can hold a large payload — catalog's event
  list is one entry.
- The token authenticates a **shared machine credential**. There is no operator identity here and no
  durable record of who toggled what.

## Scanner device enrolment (ai-review S1)

`POST /api/access/scans` and `/scans/reconciliations` admit only **enrolled gate
devices**. Each device holds its own token; there is no shared scanner secret, because
the scanner is a static SPA and anything baked into it ships to every phone that loads
`/scanner/`.

```
access enrol-scanner <organizer-id> "north gate"   # prints the token ONCE
access list-scanners <organizer-id>                # live + revoked, with last-seen
access revoke-scanner <device-id>                  # the answer to a lost phone
```

Run them in the access container: `docker compose exec access /app enrol-scanner …`.

The token is printed once and stored **hashed** (SHA-256), so it cannot be read back
out of the database. An operator who loses it revokes the device and enrols a new one —
which is the correct workflow anyway, and the reason the credential is per device.

**Pairing.** Open `/scanner/` on the device; an unpaired scanner shows a pairing screen
instead of the scan form. Paste the token. It is kept in that browser's `localStorage`
and survives reloads.

**What an operator sees when it goes wrong.** A revoked or unpaired device gets `401`
and the app returns to the pairing screen with the reason — never the ticket-rejection
screen. That distinction is deliberate: the person at the turnstile has a perfectly good
ticket, and "Rejected" would be the wrong instruction. Offline scans already queued on
the device are **kept**, not discarded, and sync once it is paired again.

**What this does and does not constrain** (ADR-021 — name the adversary). It stops a
caller who does not hold an enrolled device's token from admitting, redeeming or
rewriting scan history. It constrains nobody with write access to the access database,
who can enrol their own device.

## Finding misconfigured rule currencies (TKT-243)

A price or fee rule whose `currency` differs from the currency of a ticket type it applies to is
invalid configuration, not a rule that quietly does not apply: resolution **fails** on it
(ADR-036 §2). Price resolution filters by **channel before** it checks currency (TKT-237, and that
order is deliberate — the alternative made one misconfigured `pos` rule return 500 for every
`reseller` and public request), so such a rule is **invisible until a sale arrives on its channel**,
and then it fails closed at the worst possible moment.

`catalog validate-rules` finds them first, without needing a sale on each channel:

```bash
docker compose exec catalog /app validate-rules
# catalog validate-rules: 1 currency mismatch pair(s)
#   price rule_id=… organizer_id=… ticket_type_id=… scope=venue/… channel=pos \
#     rule_currency=USD ticket_currency=EUR window=[unbounded,unbounded)
```

It needs only the service's usual `DATABASE_URL`. It **reports and decides nothing**: exit code is
`0` whether or not it finds anything, and it performs no writes. Findings are for a human to act on,
so do not wire it into a gate — a failing build is not the intended answer to a misconfigured rule.

**One finding is one `(rule, ticket type)` pair, not one rule.** A rule attaches to one of five
scope levels with no foreign key (ADR-036 §3), so a venue-scoped rule covers every ticket type under
that venue, each with its own currency — one rule can be correct for one ticket type and wrong for
another at the same time. That asymmetry is also why write-time validation cannot do this job
(ADR-036 §4 step 1).

**Two things it deliberately does not filter**, because filtering them would reintroduce the blind
spot it exists to close:

- **channel** — it never joins the `channels` registry. The registry is a lookup, not a constraint:
  a code that was never registered sells exactly like one that was, so consulting it would miss the
  rules most likely to be wrong.
- **`effective_from`** — a rule whose window has not opened yet is precisely what is being hunted.
  It will price the moment it opens and nothing today would notice.

**One thing it deliberately excludes.** Rules whose window has already **closed** are not reported.
They are inert and **unrecoverable**: `currency` is immutable and `effective_until` can only be
shortened, so no write can rescue such a row (ADR-036 §4 step 1, ADR-046 §8). An operator handed
that finding could do nothing with it, and a report that accumulates unfixable rows forever teaches
people to ignore the fixable ones beside it.

**Split schedules are not swept** — they carry no currency at all (shares are basis points, which
are currency-independent; ADR-047), so they cannot mismatch.

**What this does and does not constrain** (ADR-021 — name the adversary). It is an **operator aid
under honest-writer assumptions**. It reads the same tables a writer with catalog database access
writes, so it is not an integrity control and proves nothing against someone who can insert or alter
rules directly. It catches configuration mistakes, not tampering.

## When a scheduled workflow fails (TKT-213)

Two workflows run on a weekly cron and are **not** part of the per-PR gate:

- `hermetic-smoke` (Mondays 06:00 UTC) — the full in-Docker build path. `make check` uses the fast
  host-built path, so this is what keeps the two honest.
- `security` (Mondays 07:00 UTC) — re-scans every dependency even when nothing changed, which is how
  a newly-disclosed CVE in untouched code gets caught.

Both used to fail into the void. `hermetic-smoke` was red on `main` for nine days in August 2026 and
was found only because an unrelated PR happened to touch `compose.yaml` and trip its path filter.

**Now a failed scheduled run opens a GitHub issue** labelled `scheduled-workflow-failure`, carrying a
link to the failed run. Repeated failures comment on the same issue rather than opening new ones — one
issue per outage, not per run and not per matrix leg (`security` has eight `govulncheck` legs). A
later scheduled run that is genuinely green comments and closes it.

**PR-triggered failures do not open issues.** A red check on a PR is already in front of its author.

### Things worth knowing before you edit either workflow

- **`permissions:` at job level replaces the workflow-level block; it does not merge.** This repo's
  `default_workflow_permissions` is `read`, so the notifier jobs declare `issues: write` explicitly.
  Omit it and the step 403s — a notifier that "runs" and tells nobody, which is the failure this whole
  mechanism exists to prevent. If you add a checkout step to a notifier job in `security.yaml`, add
  `contents: read` back too.
- **Adding a top-level job? Add it to both notifier jobs' `needs:`.** The recovery job does not trust
  `needs:` alone — it asks the run how many jobs concluded `failure`, `cancelled` or `timed_out`
  (`gh run view --json jobs`) and closes the issue only when **exactly one job is still in flight**
  (itself) **and exactly one finished job did not succeed** (the sibling notifier, which is skipped on
  every run where the other one fires).
  Anything else — `failure`, `cancelled`, `timed_out`, `action_required`, `stale`, a conclusion GitHub
  adds later, a *second* unfinished job, or a *second* skipped job — blocks the close. The last two
  are what catch a top-level job omitted from `needs:`: still queued, or skipped by a false `if` or a
  missing input. Allowing skips freely would have closed the outage while a scan that was supposed to
  run never did — in `security.yaml`, a silently-unperformed CVE sweep reported as clean.

  **Known gap:** the guard takes one snapshot and does not retry. If a job row is transiently
  unsettled when it looks, this run declines to close and the issue stays open until the next
  scheduled run clears it. Self-correcting, and preferred over retry logic inside an alarm path.

  It reads `status`, not just `conclusion`, and that is not cosmetic: **`gh run view --json jobs`
  renders a running job's conclusion as the empty string, not JSON null**, so a guard written against
  `null` treats its own still-running job as unacceptable and never closes anything. Four versions of
  this guard were wrong before this one — by display-name prefix, by counting only failures, by
  requiring exactly one JSON null, and by that empty-string confusion. Change it carefully.
- **Both notifier jobs carry `!cancelled()`.** GitHub re-evaluates a running job's `if` during
  cancellation and keeps the job when it still holds, so without it a cancelled run could still write
  to the issue — contradicting the rule that a cancelled run reports nothing.
- **The notifier is inlined in each workflow rather than shared as a script**, deliberately. A shared
  script needs `actions/checkout`, which would run the notifier *from the commit under test* — so a
  bad `main` would break the alarm whose job is to report that `main` is bad. The cost is a
  near-duplicate block in the two files: change one, change the other.
- **The issue is matched by a marker keyed on the workflow's file path**, not its display name, so
  renaming a workflow does not orphan its open issue.

### Verifying a change to it

```bash
bash scripts/verify-scheduled-notifier.sh all        # both workflows
bash scripts/verify-scheduled-notifier.sh hermetic   # just one
```

Extracts the `run:` blocks verbatim from the named workflow and drives them against the real GitHub
API with a throwaway label — create, dedupe over three failures, the guard **refusing** to close
while a job failed, close, and the no-op path — cleaning up after itself.

It also exercises the guard's predicate directly against **synthetic job payloads**, because the live
state cannot be staged from real run data: a completed historical run has nothing in flight, while the
guard always has exactly one job running when it asks. That gap hid a real defect for a review round.
The synthetic cases cover each blocking conclusion, an unknown future conclusion, a second unfinished
job, and an empty jobs array — and the suite fails if its copy of the predicate drifts from either
workflow's.

**Run `all`, not one.** The two workflows carry independent inlined copies, so a suite that reads
only `hermetic.yaml` says nothing about `security.yaml` — which is exactly the coupling the
change-both-copies rule above creates. Every assertion is fatal: an earlier version used
`[ x = y ] && echo ok`, which under `set -e` cannot fail the script, and it printed a success banner
while the issue body was missing its link to the failed run.

**What it does not cover, and nothing local does:** GitHub's own evaluation of `if:`, `needs:`,
`permissions:` and matrix aggregation. Nothing in `make check` parses or executes workflow YAML, so a
green gate says nothing about this mechanism. The wiring is confirmed by watching a real run: on a PR
touching these files, both notifier jobs must appear as **skipped**.

## Conventions

- Money: integer minor units + ISO currency code; floats banned on money paths (ADR-001).
- Every entity carries a tenant/organizer id (ADR-002).
- Append-only trails on money/ticket paths from US-004 (ADR-003).
- Public read endpoints declare an ADR-004 TTL tier from birth.
- Commits/branches/PRs: see `conventions/`.
