# Ticket lifecycle integrity

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Access ticket lifecycle trail operations

The trail is chained per ticket and checkpointed per organizer
([ADR-021](../adr/ADR-021-ticket-lifecycle-trail-integrity.md)). **Read §The trust boundary before
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
[ADR-025](../adr/ADR-025-admission-events-and-offline-reconciliation.md) §D1/§D2).** A slot whose
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
[ADR-025](../adr/ADR-025-admission-events-and-offline-reconciliation.md) §Claims, the admission-conflict
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
is an [ADR-017](../adr/ADR-017-domain-event-schema-evolution.md) §3 decision on this contract — a
required field fires §3b independently of whether any consumer exists — not a payload tweak.

What that test is **not**: proof that the payload contains no PII, and not a constraint on anyone
who can write to the Access database. It is a producer-schema check against honest application
changes (ADR-021 §The trust boundary). `device_occurred_at` is device-*claimed* and correlates with
a physical gate event, so the payload is bounded, not anonymous.

### Exchange entitlement switches (TKT-166)

An exchange voids the source order's tickets and issues the replacement set **in one access
transaction** (`store.SwitchExchange`, [ADR-039 §3](../adr/ADR-039-exchange-settles-the-difference.md)).
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

- **An admitted source ticket is refused before the target hold.** Commerce reads admission through
  access before repricing or money moves. The rule covers single-admission tickets and passes with
  any `entry`; an unused pass remains exchangeable. Access checks `ticketAdmittedUnion`,
  which includes quarantine admission evidence as well as lifecycle events (ADR-039 §2).

  Access checks again under the source ticket row lock when it switches the tickets. A scan can land
  after commerce's read, so this backstop can still refuse after settlement. That leaves a
  settled-but-unswitched exchange, visible in the query above. ADR-067 does not resolve this state:
  `unwind-exchange` refuses settled exchanges. TKT-498 owns the open operator remedy.

  The failure record says `exchange_refused`, **not** `issuance_retries_exhausted`, and it is
  published on the first delivery. Retrying a fact about admission history cannot change it, and
  filing it under exhaustion would send an operator looking for a broken dependency.

  Note what this state costs: the buyer paid the difference and gets no replacement. That is a
  **stranded paid exchange**, and it is the deliberate trade — the alternative available without a
  product decision is admitting the same entitlement twice. Fencing the source tickets during
  `switch_pending` would remove the strand, and would do it by **denying a legitimate holder entry
  at the gate** while an exchange is mid-flight. For a ticketing system that is the worse failure,
  which is why TKT-498 owns the operator decision rather than changing gate behavior here.

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
