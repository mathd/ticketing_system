# Docker & Compose

One stack file at the repo root (`compose.yaml`, project `ticketing`):

| Service | Image / build | Notes |
|---|---|---|
| postgres | `postgres:18.4` | init script creates 5 DBs + roles, CONNECT revoked cross-service (ADR-007) |
| nats | `nats:2.14-alpine` `-js -sd /data` | file-backed JetStream on a named volume; monitoring on 8222 |
| nats-init | `natsio/nats-box` (one-shot) | provisions the `PLATFORM` stream at stack init (ADR-007) |
| lgtm | `grafana/otel-lgtm` | collector + Tempo + Loki + Prometheus + Grafana (:3000) |
| catalog…access | `build/go.Dockerfile` (arg `PKG`) | distroless static; healthcheck = `/app healthcheck` subcommand (no shell in image) |
| gateway | `build/go.Dockerfile` | only published app port (:8080) |
| storefront | `web/storefront/Dockerfile` | Astro 7 SSR standalone build on Node, `/healthz` |
| scanner | `web/scanner/Dockerfile` | pnpm build stage → nginx under `/scanner/`, `/healthz` |

Published host ports are env-overridable (`GATEWAY_PORT`, `POSTGRES_PORT`, `NATS_PORT`,
`GRAFANA_PORT`, `PROM_PORT`, `OTLP_PORT`) — published infra ports bind to `127.0.0.1` only; that's how `make smoke` runs an isolated copy
(project `ticketing-smoke`, ports 18080/15432/14222/13000/19090/14318) beside your dev stack.

Gotchas already paid for:

- Distroless images have no shell/curl: container healthchecks exec the service binary's
  `healthcheck` subcommand.
- nginx must listen on `[::]` too — healthcheck probes hit `127.0.0.1` explicitly.
- Go images build from the repo root context (go.work workspace); the scanner also builds
  from the root context to see the pnpm workspace lockfile.
- OTel export cadence is tightened via env (`OTEL_METRIC_EXPORT_INTERVAL=5000` etc.) so
  telemetry is queryable within seconds locally.
