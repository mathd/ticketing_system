# Configuration override register

This register covers every `${VAR}` interpolation in base [compose.yaml](../compose.yaml).
Values below are Compose defaults, not a universal list of binary defaults. No secret values
are included. [Configuration](configuration.md) explains validation, relationships, and limits.
Read the service entry point for additional code-only settings and CLI flags.

## Base Compose inputs

An input marked required is generated for local use by `make up`; do not substitute a shared
or historical default. A required public key is derived with its own corresponding signing seed.
The Compose file is the executable authority. Update this table with interpolation changes.

| Input | Compose default | Purpose |
|---|---|---|
| `ACCESS_ADMISSION_CONFLICT_DURABLE` | `access-admission-conflict-operator` | Admission-conflict operator durable |
| `ACCESS_DB_PASSWORD` | Required | PostgreSQL role password; applied by init on first boot |
| `ACCESS_EVENT_RETRY_BACKOFF` | Empty | Access consumer retry schedule; empty delegates to code default |
| `ACCESS_FEED_CURSOR_KEY` | Required | Access revocation-feed cursor HMAC; separate from QR/link signing |
| `ACCESS_LIFECYCLE_ALARM_DURABLE` | `access-lifecycle-integrity-operator` | Lifecycle alarm operator durable |
| `ACCESS_LIFECYCLE_ALARM_STREAM` | `PLATFORM` | Lifecycle and admission alarm stream |
| `ACCESS_LIFECYCLE_CHECKPOINT_INTERVAL` | `60s` | Lifecycle checkpoint worker interval |
| `ACCESS_LIFECYCLE_FAILURE_THRESHOLD` | `3` | Integrity failure count threshold |
| `ACCESS_LIFECYCLE_FAILURE_WINDOW` | `60s` | Integrity failure counting window |
| `ACCESS_LIFECYCLE_KID` | `access-lifecycle/local-v1` | Active lifecycle signing key ID |
| `ACCESS_LIFECYCLE_PRIVATE_KEY` | Required | Access Ed25519 signing seed; pair with matching public keyring |
| `ACCESS_LIFECYCLE_PUBLIC_KEYS` | Required | Access Ed25519 verification keyring |
| `ACCESS_POLICY_CONFLICT_DURABLE` | `access-policy-conflict-operator` | Policy-conflict operator durable |
| `ACCESS_QR_KID` | `access-qr/local-v1` | Active ticket credential key ID |
| `ACCESS_QR_PRIVATE_KEY` | Required | Access Ed25519 signing seed; pair with matching public keyring |
| `ACCESS_QR_PUBLIC_KEYS` | Required | Access Ed25519 verification keyring |
| `ACCESS_STAFF_WRITE_TOKEN` | Required | Service-specific back-office operation credential |
| `ACCESS_TICKET_LINK_KEY` | Required | Short-lived QR image URL HMAC |
| `CATALOG_DB_PASSWORD` | Required | PostgreSQL role password; applied by init on first boot |
| `CATALOG_ORGANIZER_ASSERTION_KID` | `catalog-org/local-v1` | Active organizer-assertion signing key ID |
| `CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY` | Required | Back-office organizer-assertion Ed25519 seed |
| `CATALOG_STAFF_WRITE_TOKEN` | Required | Service-specific back-office operation credential |
| `COMMERCE_CUSTOMER_ASSERTION_KEY` | Required | Server-side checkout customer binding |
| `COMMERCE_DB_PASSWORD` | Required | PostgreSQL role password; applied by init on first boot |
| `COMMERCE_ORGANIZER_ASSERTION_PUBLIC_KEYS` | Required | Commerce organizer-assertion verifier keyring |
| `COMMERCE_STAFF_WRITE_TOKEN` | Required | Service-specific back-office operation credential |
| `DB_CONN_MAX_IDLE_TIME` | `5m` | Per-process database pool bound; see configuration limits |
| `DB_CONN_MAX_LIFETIME` | `30m` | Per-process database pool bound; see configuration limits |
| `DB_MAX_IDLE_CONNS` | `10` | Per-process database pool bound; see configuration limits |
| `DB_MAX_OPEN_CONNS` | `25` | Per-process database pool bound; see configuration limits |
| `GATEWAY_PORT` | `8080` | Published gateway host port |
| `HOLD_TTL` | `10m` | Inventory default hold duration |
| `HTTP_IDLE_TIMEOUT` | `60s` | HTTP server timeout; see configuration limits |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | HTTP server timeout; see configuration limits |
| `HTTP_READ_TIMEOUT` | `15s` | HTTP server timeout; see configuration limits |
| `HTTP_WRITE_TIMEOUT` | `30s` | HTTP server timeout; see configuration limits |
| `INTERNAL_SERVICE_TOKEN` | Required | Shared testbed internal bearer credential; separate Payments credential |
| `INVENTORY_DB_PASSWORD` | Required | PostgreSQL role password; applied by init on first boot |
| `INVENTORY_STAFF_WRITE_TOKEN` | Required | Service-specific back-office operation credential |
| `JOURNAL_HISTORICAL_KEYS` | Empty | Retired journal verification keys; see key-rotation runbook |
| `JOURNAL_SIGNING_KEY` | Required | Active payments journal HMAC key |
| `NATS_ACCESS_PASSWORD` | Required | NATS identity password; scoped broker permissions |
| `NATS_ACCESS_TICKET_ISSUER_CONFIG` | `access-ticket-issuer.json` | nats-init consumer definition file; pair its BackOff with Access retry schedule |
| `NATS_ADMIN_PASSWORD` | Required | NATS identity password; scoped broker permissions |
| `NATS_CATALOG_PASSWORD` | Required | NATS identity password; scoped broker permissions |
| `NATS_COMMERCE_PASSWORD` | Required | NATS identity password; scoped broker permissions |
| `NATS_INVENTORY_PASSWORD` | Required | NATS identity password; scoped broker permissions |
| `NATS_PAYMENTS_PASSWORD` | Required | NATS identity password; scoped broker permissions |
| `OPENAPI_RESPONSE_VALIDATION_ENABLED` | `true` | Runtime response conformance check; disabling removes that check |
| `PAYMENTS_DB_PASSWORD` | Required | PostgreSQL role password; applied by init on first boot |
| `PAYMENTS_INTERNAL_TOKEN` | Required | Payments internal money-operation credential held by Commerce |
| `PAYMENTS_STATUS_REPLAY_RETENTION` | `24h` | Status-replay window; cannot extend or disable a bounded provider window |
| `POSTGRES_PASSWORD` | Required | PostgreSQL role password; applied by init on first boot |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | External origin for buyer-facing links |
| `STRIPE_SECRET_KEY` | `fake` | fake/empty selects fake; nonempty sk_test_ selects Stripe; live or malformed keys refuse startup |

## Commerce worker settings

Base Compose does not forward these worker overrides. To change them in a container, explicitly
pass them with a local overlay. The defaults below come from `workerConfigFromEnv` and
`defaultWorkerConfig` in [Commerce main](../services/commerce/cmd/commerce/main.go).
Durations and batch sizes must be positive; invalid or explicitly empty settings refuse startup.

| Interval / batch variables | Binary defaults | Work |
|---|---|---|
| `OUTBOX_DRAIN_INTERVAL`, `OUTBOX_DRAIN_BATCH` | `5s`, `32` | Publish Commerce outbox |
| `RECOVERY_INTERVAL`, `RECOVERY_BATCH` | `30s`, `16` | Checkout recovery |
| `CANCELLATION_REFUND_INTERVAL`, `CANCELLATION_REFUND_BATCH` | `10s`, `8` | Cancellation refund runs |
| `REFUND_REVERSAL_INTERVAL`, `REFUND_REVERSAL_BATCH` | `1m`, `16` | Refund reversal obligations |
| `EXCHANGE_REVERSAL_INTERVAL`, `EXCHANGE_REVERSAL_BATCH` | `1m`, `16` | Exchange obligations |

## Overlays and literal process environment

[compose.direct-ports.yaml](../compose.direct-ports.yaml) adds diagnostic ports, including
`POSTGRES_PORT`, `NATS_PORT`, `GRAFANA_PORT`, `PROM_PORT`, `OTLP_PORT`, `CATALOG_PORT`,
`INVENTORY_PORT`, `COMMERCE_PORT`, `PAYMENTS_PORT`, and `ACCESS_PORT`. Isolated test stacks
resolve these through [stack-env.sh](../scripts/stack-env.sh), which prints the slot and gateway.
Do not assume fixed smoke ports. See [Docker](docker.md) for the slot formula and overlay map.

Compose also sets literal `DATABASE_URL`, `NATS_URL`, service peer URLs, `PORT`, OTLP export
variables, and frontend `HOST`/`NODE_ENV` in the appropriate process. These are not additional
`${VAR}` overrides. Inspect the relevant service's `environment:` block before attempting a shell
override: a shell variable is not forwarded unless Compose declares it. Access DB URLs must
explicitly set `sslmode=disable` under the current Compose-only rule.

Runtime source locations: `shared/go/runtimecfg/` for shared validation; `services/*/cmd/*/`
for service settings; `scripts/env-bootstrap.sh` for dev credential generation; `scripts/stack-env.sh`
for test isolation. [NATS runbook](runbooks/nats.md) covers consumer/configuration coupling and
credential rotation. This register does not claim production secret management or venue qualification.
