# Configuration

All configuration is environment variables, injected by Compose (`compose.yaml`); there are
no config files in the services.

## Go services (all five)

| Variable | Meaning | Default |
|---|---|---|
| `PORT` | listen port | `8080` |
| `DATABASE_URL` | own Postgres database (ADR-007: one DB + role per service) | — (required) |
| `NATS_URL` | JetStream bus | — (required) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | collector (lgtm container) | SDK default |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BLRP_SCHEDULE_DELAY` | export cadence (tightened locally) | SDK defaults |

## Gateway

`PORT`, `OTEL_EXPORTER_OTLP_ENDPOINT`, plus one URL per registered route:
`CATALOG_URL`, `INVENTORY_URL`, `COMMERCE_URL`, `PAYMENTS_URL`, `ACCESS_URL`,
`STOREFRONT_URL`, `SCANNER_URL`. The gateway refuses to start if a registered route's
env var is missing — the route table is explicit by design.

## Compose host ports (dev defaults / smoke)

All published ports bind to `127.0.0.1`. `GATEWAY_PORT` 8080/18080 · `POSTGRES_PORT` 5432/15432 · `NATS_PORT` 4222/14222 ·
`GRAFANA_PORT` 3000/13000 · `PROM_PORT` 9090/19090 · `OTLP_PORT` 4318/14318.

## Secrets

Local stack uses throwaway credentials (per-service DB passwords equal to the role name,
`postgres/postgres` superuser). Nothing here is production-grade; a real secrets story
arrives with the first deployed environment. Never commit real credentials —
see `conventions/security.md`.

## Access QR keys

`ACCESS_QR_PRIVATE_KEY` is the raw-base64 Ed25519 signing seed used only for issuance;
`ACCESS_QR_KID` must be an `access-qr/`-namespaced key identifier. `ACCESS_QR_PUBLIC_KEYS`
is the comma-separated `access-qr/<kid>=<raw-base64-public-key>` verifier keyring used by
the scan API. Access fails closed at startup if the keyring is invalid or does not include
the active key. Compose supplies a development-only matching key pair.
