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
docker compose exec payments /app verify-journal  # verify the live money journal
```

Go code is a `go.work` workspace: one module per service + `gateway`, `shared/go`, `smoke`.
TS code is a pnpm workspace (`web/scanner`; the storefront is static HTML pending ADR-006).

## Testing model

- **Unit tests** live next to code; kept minimal at the scaffold layer (shared middleware).
- **The integration seam is the gateway**: `smoke/` asserts everything observable from
  outside — health fan-out, web shells, trace propagation, JetStream persistence, DB
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
Failure-record publication happens before termination; a failed publication leaves the source
message eligible for redelivery. Counters use the low-cardinality `reason` and `stage` labels.

To recover, inspect the failed record, repair the producer or downstream dependency, locate the
original event in the durable `PLATFORM` stream by `source_event_id`, and republish that original
envelope with a new `Nats-Msg-Id` replay suffix. Keep the event's own `id`: Access issuance and
delivery are idempotent on that identifier. Never manufacture a payload from the failed record;
it deliberately does not retain attacker-controlled event data. If the failure subject itself is
unavailable through all six deliveries, JetStream's max-deliver advisory is the operator signal
to restore the stream and replay the original message.

## Conventions

- Money: integer minor units + ISO currency code; floats banned on money paths (ADR-001).
- Every entity carries a tenant/organizer id (ADR-002).
- Append-only trails on money/ticket paths from US-004 (ADR-003).
- Public read endpoints declare an ADR-004 TTL tier from birth.
- Commits/branches/PRs: see `conventions/`.
