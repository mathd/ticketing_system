# Development

## Toolchain

Latest stable everything (see `conventions/dependencies-and-versions.md`): Go 1.26+,
Node 24+ with pnpm 11 (pinned via `packageManager`, auto-selected by corepack/pnpm),
Docker + Compose v2. No other host dependencies; `make lint-go` installs the pinned
golangci-lint release binary into `./bin` (sha256-verified against the release checksums;
`scripts/install-golangci-lint.sh`).

## Everyday loop

```bash
docker compose up -d --build --wait   # or: make up
make check                            # full local gate: lint + test + build + smoke
make lint / test / build / smoke      # individual stages
docker compose exec payments /app verify-journal    # verify the live money journal
docker compose exec access /app verify-lifecycle   # verify the live ticket lifecycle trail
```

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

## Conventions

- Money: integer minor units + ISO currency code; floats banned on money paths (ADR-001).
- Every entity carries a tenant/organizer id (ADR-002).
- Append-only trails on money/ticket paths from US-004 (ADR-003).
- Public read endpoints declare an ADR-004 TTL tier from birth.
- Commits/branches/PRs: see `conventions/`.
