# Configuration

All configuration is environment variables, injected by Compose (`compose.yaml`); there are
no config files in the services.

## Go services (all five)

| Variable | Meaning | Default |
|---|---|---|
| `PORT` | listen port | `8080` |
| `DATABASE_URL` | own Postgres database (ADR-007: one DB + role per service) | — (required) |
| `NATS_URL` | JetStream bus | — (required) |
| `DB_MAX_OPEN_CONNS` | maximum open database connections per service | `25` |
| `DB_MAX_IDLE_CONNS` | maximum idle database connections; must not exceed open | `10` |
| `DB_CONN_MAX_LIFETIME` | maximum lifetime of a pooled connection | `30m` |
| `DB_CONN_MAX_IDLE_TIME` | maximum time a pooled connection remains idle | `5m` |
| `OPENAPI_RESPONSE_VALIDATION_ENABLED` | enforce ADR-028 response-drift fail-closed at runtime; off skips the response buffer and schema walk, and drifted payloads reach the client unchecked (TKT-125) | `true` |
| `ACCESS_EVENT_RETRY_BACKOFF` | Access-only comma-separated event retry intervals | `1s,5s,30s,2m,5m,10m` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | collector (lgtm container) | SDK default |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BLRP_SCHEDULE_DELAY` | export cadence (tightened locally) | SDK defaults |

## HTTP servers (services and gateway)

| Variable | Meaning | Default |
|---|---|---|
| `HTTP_READ_HEADER_TIMEOUT` | deadline for reading request headers | `5s` |
| `HTTP_READ_TIMEOUT` | deadline for reading the complete request | `15s` |
| `HTTP_WRITE_TIMEOUT` | deadline for writing the response | `30s` |
| `HTTP_IDLE_TIMEOUT` | keep-alive idle timeout | `60s` |

Durations use Go duration syntax and must be positive. Database counts must be positive. A process
fails at startup when a value is invalid; silently falling back would hide a broken capacity
contract. Compose forwards these variables to every applicable process.

## Gateway

The HTTP variables above, `PORT`, `OTEL_EXPORTER_OTLP_ENDPOINT`, plus one URL per registered route:
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

## Back-office staff sign-in

No environment variable configures it, and that is the point: the back office reaches catalog's
public `POST /staff/authenticate` **through the gateway** (`GATEWAY_URL`, already set), exactly
like every other call it makes. It deliberately does **not** hold `INTERNAL_SERVICE_TOKEN` — that
one shared value also opens commerce's refunds and inventory's operational holds, and a
public-facing SSR process is the wrong place for it (ADR-042).

`scripts/smoke.sh` provisions a throwaway account per run and passes it to the test process as
`SMOKE_STAFF_IDENTIFIER` / `SMOKE_STAFF_PASSWORD`. The password is generated per run and never
written to a file or the log.

## Access QR keys

`ACCESS_QR_PRIVATE_KEY` is the raw-base64 Ed25519 signing seed used only for issuance;
`ACCESS_QR_KID` must be an `access-qr/`-namespaced key identifier. `ACCESS_QR_PUBLIC_KEYS`
is the comma-separated `access-qr/<kid>=<raw-base64-public-key>` verifier keyring used by
the scan API. Access fails closed at startup if the keyring is invalid or does not include
the active key.

## Payments has its own internal credential

`PAYMENTS_INTERNAL_TOKEN` opens payments' internal surface — every charge, void, refund
and partial refund. It is **not** `INTERNAL_SERVICE_TOKEN`, which every service holds:
before ai-review S8 a compromise of any one service yielded the whole platform's money
surface. Commerce is payments' only caller and holds both; it refuses to start when any
two of its four credentials are equal, and payments refuses to start when its own equals
the shared one.

This is a reduction, not a solution — compromising commerce still reaches payments.
Per-caller credentials or mTLS is the finish.

## Ticket bundle links

`ACCESS_TICKET_LINK_KEY` signs the short-lived QR **image** URLs the ticket bundle hands
out (ai-review S2). Distinct from `ACCESS_QR_PRIVATE_KEY`: it proves a URL was minted
recently, not that a credential admits at a gate.

The order ref remains the bundle's bearer credential — forwarding "here are your
tickets" is a feature. What expires is the image URL, which is what ends up in a
screenshot, a referrer header or a proxy log. The bundle page mints fresh links on every
load, so a buyer never meets the expiry.

## Scanner devices

The scan routes require `X-Scanner-Token` from an enrolled device (ai-review S1). No
environment variable configures it: devices are enrolled per organizer with
`access enrol-scanner` and stored hashed. See `docs/development.md` § Scanner device
enrolment.

## Database passwords have no defaults

`POSTGRES_PASSWORD` and one password per service role (`CATALOG_DB_PASSWORD`,
`INVENTORY_DB_PASSWORD`, `COMMERCE_DB_PASSWORD`, `PAYMENTS_DB_PASSWORD`,
`ACCESS_DB_PASSWORD`) are required. Until ai-review S11 the superuser's password was the
committed literal `postgres` and each role's was its own **name**, so anything that could
reach the published port owned every database by typing the obvious thing.

`make up` generates all six into `.env`; the isolated stacks generate their own per run.
They are applied on **first boot only** — the init scripts run against an empty data
directory — so rotate with `make down` (which takes the volume) or an `ALTER ROLE`.

## Published ports

`compose.yaml` publishes **only the gateway**. Direct loopback access to PostgreSQL,
NATS, Grafana, Prometheus and the five services lives in `compose.direct-ports.yaml`,
which `make up`, `scripts/smoke.sh` and `scripts/browser.sh` add (ai-review S11). Local
work is unchanged; a deployment built from `compose.yaml` no longer inherits a loopback
door into every internal surface.

## Signing keys have no defaults

`JOURNAL_SIGNING_KEY`, `ACCESS_QR_PRIVATE_KEY` / `ACCESS_QR_PUBLIC_KEYS` and
`ACCESS_LIFECYCLE_PRIVATE_KEY` / `ACCESS_LIFECYCLE_PUBLIC_KEYS` are **required**: Compose
refuses to start without them, and the binaries refuse the three values that used to be
their defaults, forever — the way `INTERNAL_SERVICE_TOKEN` refuses `local-service-token`
(TKT-83). Those three are in this repository's history, so they are published key material,
not secrets. A forged QR signed with the old seed passed `ed25519.Verify` at the gate, and a
forged checkpoint passed `access verify-lifecycle`.

`make up` generates all five per clone into `.env` (`scripts/env-bootstrap.sh`); the
isolated smoke and browser stacks generate their own per run (`scripts/stack-env.sh`).
Each value gets its own draw, so one leaking never implies another, and the Ed25519 seed
and its public keyring are always written together — a fresh seed beside an old public key
breaks verification, and a fresh public key beside an old seed leaves the old seed a valid
signer.

`access keygen` prints one fresh `<seed> <public key>` pair for either namespace; the KID
decides which. Use it for a manual rotation.
