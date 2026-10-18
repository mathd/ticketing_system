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
| `ACCESS_EVENT_RETRY_BACKOFF` | Access-only comma-separated event retry intervals. Must agree with the `backoff` of the file `NATS_ACCESS_TICKET_ISSUER_CONFIG` selects, or access refuses its durable at startup (TKT-327) | `1s,5s,30s,2m,5m,10m` |
| `NATS_ACCESS_TICKET_ISSUER_CONFIG` | `nats-init` only: the file in `deploy/nats/consumers/` it provisions `access-ticket-issuer` from. `scripts/stack-env.sh` sets `access-ticket-issuer.smoke.json` beside its short `ACCESS_EVENT_RETRY_BACKOFF` | `access-ticket-issuer.json` |
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

**Access and the gateway need `HTTP_WRITE_TIMEOUT` of at least `30s`** (TKT-557, ADR-085 §2.4b).
Both processes refuse to start when the resolved value is below `30s`. The error names the variable
and the minimum. The check runs before the process opens any resource or listener. The scanner can
show a result up to 30 s after a scan, so a shorter write deadline would lose more committed answers.
Compose forwards one value to both processes, and Compose itself accepts `29s`: the processes refuse
it. The other services keep their current policy.

Equality is allowed, and it does not close the race. The gateway starts its write deadline before
the forwarded Access request starts its own, so with equal values the gateway deadline can expire
first and lose a response that Access committed. `HTTP_READ_TIMEOUT` is a request-read deadline, not
a handler deadline: nothing guarantees a total handler time.

## Gateway

The HTTP variables above, `PORT`, `OTEL_EXPORTER_OTLP_ENDPOINT`, plus one URL per registered route:
`CATALOG_URL`, `INVENTORY_URL`, `COMMERCE_URL`, `PAYMENTS_URL`, `ACCESS_URL`,
`STOREFRONT_URL`, `SCANNER_URL`, `BACKOFFICE_URL`. The gateway refuses to start if a registered
route's env var is missing. The route table is explicit by design.

## Commerce: ACCESS_URL and the refund reversal runner

`ACCESS_URL` is how commerce voids a refunded order's tickets. The binary still **starts** without
it — a refund returns the money and records the obligation either way — but its absence fails
**readiness**: `/readyz` answers `503` with `access_configured: unhealthy` while `/healthz` stays
green, so a misconfigured deployment is not routed traffic and is not mistaken for a dead process.
Without it nothing, including the reconciler, can ever discharge ticket voiding
([ADR-062](adr/ADR-062-refund-reversal-reconciliation.md) §5).

`REFUND_REVERSAL_INTERVAL` (default `1m`) and `REFUND_REVERSAL_BATCH` (default `16`) tune the runner
that drives outstanding reversals. Operator surface and the meaning of each state:
`docs/development.md` § Refund reversal reconciliation.

`EXCHANGE_REVERSAL_INTERVAL` (default `1m`) and `EXCHANGE_REVERSAL_BATCH` (default `16`) tune the
equivalent sweep for outstanding exchange obligations. Both are plain defaults with no compose `:?`
marker — they are tuning knobs, not credentials. Operator surface and the meaning of each state:
`docs/development.md` § Exchange obligation sweep.

## Compose host ports (dev defaults / smoke)

All published ports bind to `127.0.0.1`. `GATEWAY_PORT` 8080/18080 · `POSTGRES_PORT` 5432/15432 · `NATS_PORT` 4222/14222 ·
`GRAFANA_PORT` 3000/13000 · `PROM_PORT` 9090/19090 · `OTLP_PORT` 4318/14318.

## Secrets

The local stack uses per-clone throwaway credentials generated into `.env` by `make up`; isolated
smoke and browser stacks generate separate values for every run. None is production-grade. Never
commit real credentials or reuse local values outside development. See
[`conventions/security.md`](conventions/security.md).

## Back-office staff sign-in

No environment variable configures it, and that is the point: the back office reaches catalog's
public `POST /staff/authenticate` **through the gateway** (`GATEWAY_URL`, already set), exactly
like every other call it makes. It deliberately does **not** hold `INTERNAL_SERVICE_TOKEN` — that
one shared value opens most services' internal surfaces, including inventory's operational holds,
and a public-facing SSR process is the wrong place for it (ADR-042). Commerce's staff refund, void
and order read take neither that token nor a tenant from the request: they require
`COMMERCE_STAFF_WRITE_TOKEN` and the staff session's organizer assertion, which commerce verifies
with `COMMERCE_ORGANIZER_ASSERTION_PUBLIC_KEYS` (TKT-287).

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

This is a reduction, not a solution: compromising commerce still reaches payments. For the
Payments boundary, per-caller credentials or mTLS is the remaining production control. The shared
`INTERNAL_SERVICE_TOKEN` elsewhere also remains a testbed credential model, not a production
service-identity design.

## Ticket bundle links

`ACCESS_TICKET_LINK_KEY` signs the short-lived QR **image** URLs the ticket bundle hands
out (ai-review S2). Distinct from `ACCESS_QR_PRIVATE_KEY`: it proves a URL was minted
recently, not that a credential admits at a gate.

The order ref remains the bundle's bearer credential: forwarding "here are your tickets" is a
feature. What expires is the image URL, which is what ends up in a screenshot, a referrer header,
or a proxy log. The bundle page mints fresh links on every successful load, so a buyer with a valid
order reference can replace an expired image URL by reloading the bundle.

## Access: CATALOG_URL and ticket time zones

`CATALOG_URL` is the Catalog base URL that Access reads each ticket's performance time zone
from when it serves the ticket bundle (TKT-365,
[ADR-083](adr/ADR-083-access-reads-performance-time-zones-from-catalog.md)). Access refuses to
start without it. Access sends the shared `INTERNAL_SERVICE_TOKEN` and asks for at most 20
performances per request, all requests within one 1.5 s deadline. A request that fails or does
not finish in time leaves the zones it asked for `null`; zones from requests that succeeded are
kept. The bundle still loads, and the storefront shows a `null` zone's history times in labelled
UTC.

## Scanner devices

The scan routes require `X-Scanner-Token` from an enrolled device (ai-review S1). No
environment variable configures it: devices are enrolled per organizer with
`access enrol-scanner` and stored hashed. See `docs/development.md` § Scanner device
enrolment.

**Provisional pre-decision bounds** (TKT-557, ADR-085 §8 ticket 0). Access limits four steps that
run before any ticket decision to **500 ms each**: the device lookup, the initial policy read of a
live scan, and the single and pass transaction acquisitions. A step that passes its bound answers
`503`. The advisory device Touch after a successful lookup has its own **500 ms** bound and never
changes the answer. Both values are **PROVISIONAL**: TKT-557 ships them without load proof, and
TKT-560 qualifies them. They are code constants, not settings; a changed value needs an ADR-085
amendment. **Venue use after TKT-558 requires TKT-560 Done and the owner's selection of the bound
and of the acceptable offline-spill rate.** Nothing enforces that order: the operator must follow it.
The bound sets how many healthy but slow scans go offline at normal peak, not only latency.

## Access: `DATABASE_URL` must set `sslmode=disable`

Every Access command that opens its database refuses `DATABASE_URL` unless the string sets
`sslmode=disable` **explicitly** and the parsed configuration enables no TLS and no TLS fallback
(TKT-557, ADR-085 decision D21). This covers the server, `migrate`, and every database command of the
CLI (`lifecycle-backfill`, `verify-lifecycle`, `seal-lifecycle-epoch`, `set-lifecycle-mode` and the
scanner commands). `healthcheck` and `keygen` open no database. The refusal comes before any dial,
and its text does not contain the URL.

- The last `sslmode` in the string decides, as in pgx, in URL and keyword form. A keyword value can
  be quoted; text inside a value is never read as a key.
- `PGSSLMODE` in the environment cannot supply the setting, and it cannot override it.
- A later `ssl=true` in a URL turns TLS on, so that URL is refused too.

The three Access URLs in `compose.yaml` set it. Other services are not affected.

**Why, and what it costs.** Access answers a connect-time restart (SQLSTATE 57P03) or a full server
(53300) with a pre-decision `503` only when every connect attempt fails with a known class. With the
pgx default (`prefer`), a failed TLS attempt adds an error with no type, and the answer would be
`500`; after TKT-558 a routine database restart would then stop the gates. The Compose PostgreSQL has
no TLS, so `prefer` already ends in plaintext there: this costs no encryption. It is a
**Compose-only restriction**. A deployment with database TLS needs a new ADR and a new real-driver
probe first.

**DNS is availability only inside Compose.** A device lookup or scan step whose connect fails on DNS
answers `503`, because in Compose the name `postgres` is the database service and a DNS failure means
it is stopped or disconnected. Outside that topology a wrong host name gives the same error, and the
scanner can then admit offline. The check above does not prove that the host name is correct.

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

`make up` generates each private secret independently per clone into `.env`
(`scripts/env-bootstrap.sh`); the isolated smoke and browser stacks generate their own per run
(`scripts/stack-env.sh`). Each Ed25519 public key is derived from its corresponding seed, and the
pair is written together. A fresh seed beside an old public key breaks verification; a fresh
public key beside an old seed leaves the old seed a valid signer.

`access keygen` prints one fresh `<seed> <public key>` pair for either namespace; the KID
decides which. Use it for a manual rotation.
