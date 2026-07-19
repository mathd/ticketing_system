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
TS code is a pnpm workspace: the Astro 7 SSR/React storefront in `web/storefront` and the
React/Vite scanner in `web/scanner` (ADR-006).

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

## Conventions

- Money: integer minor units + ISO currency code; floats banned on money paths (ADR-001).
- Every entity carries a tenant/organizer id (ADR-002).
- Append-only trails on money/ticket paths from US-004 (ADR-003).
- Public read endpoints declare an ADR-004 TTL tier from birth.
- Commits/branches/PRs: see `conventions/`.
