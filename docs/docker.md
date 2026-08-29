# Docker & Compose

One stack file at the repo root (`compose.yaml`, project `ticketing`):

| Service | Image / build | Notes |
|---|---|---|
| postgres | `postgres:18.4-bookworm` (digest-pinned) | init script creates 5 DBs + roles, CONNECT revoked cross-service (ADR-007) |
| nats | `nats:2.14.4-alpine3.22` (digest-pinned), `-js -sd /data` | file-backed JetStream on a named volume; monitoring on 8222 |
| nats-init | `natsio/nats-box:0.19.7` (digest-pinned, one-shot) | provisions the `PLATFORM` stream at stack init (ADR-007) |
| lgtm | `grafana/otel-lgtm:0.30.0` (digest-pinned) | collector + Tempo + Loki + Prometheus + Grafana (:3000) |
| `<svc>`-migrate | `build/go.Dockerfile`, `command: ["migrate"]` | one per Go service, from the `x-migrate-job` anchor; each service waits on its own with `service_completed_successfully` (ADR-022) |
| access-lifecycle-backfill | `build/go.Dockerfile` (one-shot) | runs after `access-migrate`; backfills the lifecycle trail |
| catalog…access | `build/go.Dockerfile` (arg `PKG`) | distroless static; healthcheck = `/app healthcheck` subcommand (no shell in image) |
| gateway | `build/go.Dockerfile` | only published app port (:8080) |
| storefront | `web/storefront/Dockerfile` | Astro 7 SSR standalone build on Node, `/healthz` |
| backoffice | `web/backoffice/Dockerfile` | Astro 7 SSR back-office shell behind `/admin/` (ADR-006); root build context for the pnpm lockfile |
| scanner | `web/scanner/Dockerfile` | pnpm build stage → nginx under `/scanner/`, `/healthz` |

Published host ports are env-overridable (`GATEWAY_PORT`, `POSTGRES_PORT`, `NATS_PORT`,
`GRAFANA_PORT`, `PROM_PORT`, `OTLP_PORT`) — published infra ports bind to `127.0.0.1` only; that's how `make smoke` runs an isolated copy
beside your dev stack. `scripts/stack-env.sh` derives a **slot** as `cksum("$ROOT/$STACK") % 40` —
from the checkout path *and* the stack name, so the smoke and browser stacks in one worktree
normally land on different slots — then shifts both the project name (`ticketing-<stack>-<slot>`)
and every port by it (gateway `18080+slot`, postgres `15432+slot`, and so on). Two worktrees can
therefore smoke at once. Do not assume the literals `ticketing-smoke` or 18080 in scripts; read the
exported values instead.

Gotchas already paid for:

- Distroless images have no shell/curl: container healthchecks exec the service binary's
  `healthcheck` subcommand.
- nginx must listen on `[::]` too — healthcheck probes hit `127.0.0.1` explicitly.
- Go images build from the repo root context (go.work workspace); the scanner also builds
  from the root context to see the pnpm workspace lockfile.
- OTel export cadence is tightened via env (`OTEL_METRIC_EXPORT_INTERVAL=5000` etc.) so
  telemetry is queryable within seconds locally.
