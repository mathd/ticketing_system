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

## Conventions

- Money: integer minor units + ISO currency code; floats banned on money paths (ADR-001).
- Every entity carries a tenant/organizer id (ADR-002).
- Append-only trails on money/ticket paths from US-004 (ADR-003).
- Public read endpoints declare an ADR-004 TTL tier from birth.
- Commits/branches/PRs: see `conventions/`.
