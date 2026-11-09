# Architecture overview

This page describes the current Docker Compose system. Binding decisions live in
[`adr/`](./adr/); the product scope and ticket decomposition live in
[`product/prd-v1.md`](./product/prd-v1.md).

## Running system

```mermaid
flowchart TB
    subgraph Clients
        BUYER[Buyer browser]
        STAFF[Back-office staff browser]
        GATE[Enrolled scanner device]
    end

    subgraph Edge
        GW[gateway<br/>Go, explicit public route table<br/>web apps + /api/* + health]
    end

    subgraph Frontends
        SF[storefront<br/>Astro 7 SSR + React, ADR-006]
        BO[back office<br/>Astro 7 SSR, staff sessions and role checks, ADR-042]
        SC[scanner<br/>React/Vite via nginx under /scanner/]
    end

    subgraph Services["Go services (ADR-002)<br/>chi, /healthz, healthcheck subcommand"]
        CAT[catalog<br/>public-read cache]
        INV[inventory<br/>contention hot path, availability cache]
        COM[commerce]
        PAY[payments<br/>payment operations and journal]
        ACC[access]
    end

    subgraph Data
        PG[(PostgreSQL 18.6<br/>one DB + role per service, ADR-007)]
        NATS[[NATS JetStream<br/>PLATFORM stream, publisher ACLs, ADR-072]]
    end

    subgraph Schema
        MIG[one-shot migrate jobs<br/>one per service, ADR-022]
    end

    subgraph Observability
        LGTM[grafana/otel-lgtm<br/>collector + Tempo + Loki + Prometheus + Grafana]
    end

    BUYER --> GW
    STAFF --> GW
    GATE --> GW
    GW --> SF
    GW --> BO
    GW --> SC
    GW -->|/api/catalog| CAT
    GW -->|/api/inventory| INV
    GW -->|/api/commerce| COM
    GW -->|/api/payments| PAY
    GW -->|/api/access| ACC

    SF -.->|SSR API calls| GW
    BO -.->|public API calls| GW
    BO -.->|credentialed operator calls| CAT
    BO -.->|credentialed operator calls| INV
    BO -.->|credentialed refund calls| COM
    BO -.->|credentialed redelivery calls| ACC
    SC -.->|scan API via gateway<br/>scanner token + QR credential| GW

    CAT -->|platform.catalog.performance.published| NATS
    NATS -->|durable consumer provisions capacity| INV
    COM -->|platform.commerce.order.completed| NATS
    NATS -->|durable consumer issues/delivers tickets| ACC

    CAT & INV & COM & PAY & ACC --> PG
    MIG --> PG
    MIG -.->|must complete before server starts| CAT & INV & COM & PAY & ACC
    CAT & INV & COM & PAY & ACC -->|OTLP: traces, metrics, logs| LGTM
    GW -->|OTLP| LGTM
```

In base `compose.yaml`, the gateway is the only published application port. It routes `/` to the
storefront, `/admin/` to the back office, `/scanner/` to the scanner, and explicit `/api/*`
prefixes to the five Go services. The local development overlay also publishes the services on
loopback for diagnostics; those ports are not public entry points. Internal service routes remain
edge-denied.

## Service ownership

| Component | Owns | Module |
|---|---|---|
| `catalog` | organizers/tenants, venues, seat maps, events, performances, series/seasons, festivals, rule definitions | `services/catalog` |
| `inventory` | every reservation model (GA, seats, entitlements, lodging, wristbands), holds, allocations; single-writer contention hot path | `services/inventory` |
| `commerce` | customer identity, cart, pricing/fee/promo evaluation, orders, and post-purchase lifecycle | `services/commerce` |
| `payments` | fake and Stripe test-mode PSP adapters, payment operations, append-only money journal, and settlement | `services/payments` |
| `access` | ticket issuance and delivery, scanning and redemption, passes, and wristband validation | `services/access` |
| `gateway` | the only public web entry point; explicit route registration; health fan-out | `gateway` |
| `storefront` | buyer discovery, account, reservation, checkout, and ticket-bundle UI; SSR page-data cache | `web/storefront` |
| `backoffice` | staff sessions, role-gated authoring, order lookup/refund, channel allocation administration | `web/backoffice` |
| `scanner` | browser UI for connected and offline admission workflows | `web/scanner` |
| `shared/go` | shared runtime, observability, cache-tier, rate-limit, mail, and event-consumer packages; additions require an ADR | `shared/go` |

## Synchronous service calls

The diagram emphasizes public entry points and event delivery. These direct HTTP dependencies
also run inside the Compose network. They do not pass through the public gateway.

| Caller → service | Purpose | Credential |
|---|---|---|
| Commerce → Catalog | Resolve products, prices, fees, and rules | `INTERNAL_SERVICE_TOKEN` |
| Commerce → Inventory | Hold, claim, release, and exchange inventory | `INTERNAL_SERVICE_TOKEN` |
| Commerce → Payments | Payment/refund operations and money evidence | `PAYMENTS_INTERNAL_TOKEN` |
| Commerce → Access | Void and replace tickets for post-purchase operations | `INTERNAL_SERVICE_TOKEN` |
| Inventory → Catalog | Resolve publications and seat identity/pins | `INTERNAL_SERVICE_TOKEN` |
| Access → Commerce | Resolve delivery addresses and order seats; acknowledge committed ticket switches | `INTERNAL_SERVICE_TOKEN` |
| Access → Catalog | Resolve performance time zones for ticket bundles | `INTERNAL_SERVICE_TOKEN` |

These service clients use `X-Internal-Token`. See each service's `cmd/<service>/main.go`,
Commerce's `internal/api/server.go`, Inventory's `internal/consumer/catalog.go`, and Access's
`internal/delivery/redelivery.go`, `internal/catalog/zones.go` and `internal/consumer/`. Commerce can boot without
`ACCESS_URL`, but its `/readyz` refuses readiness until that variable is set. This check proves
configuration, not Access reachability.

Back-office SSR calls use distinct `X-Catalog-Staff-Write-Token`,
`X-Inventory-Staff-Write-Token`, `X-Commerce-Staff-Write-Token`, or
`X-Access-Staff-Write-Token` headers. Tenant-scoped calls also carry a server-minted organizer
assertion. The back office receives no shared `INTERNAL_SERVICE_TOKEN`. Read the matching
`web/backoffice/src/lib/{catalog,inventory,commerce,access}.ts` client and receiving router before
changing a route's authorization. [Configuration](configuration.md) describes credential setup.

## Cross-cutting invariants

- Every entity carries a tenant/organizer id (ADR-002).
- Money uses integer minor units and ISO currency; floats are banned on money paths (ADR-001).
  Commerce currently refuses non-EUR prices. Catalog can store other ISO codes. Multi-currency
  selling remains a requirement tracked by TKT-10, not implemented checkout behavior.
  See `services/commerce/internal/api/catalog_pricing.go`, the resolved-price validation.
- Payments records money facts in its append-only journal. Access records ticket lifecycle events
  through its chained append path (ADR-003, ADR-021).
- Commerce commits a completed order before publishing the identifier-only event consumed by
  Access. Access issues signed QR tickets and keeps the `issued`/`delivered` lifecycle trace;
  it resolves the buyer email from Commerce only while dispatching delivery (ADR-012).
- Connected scans require an enrolled device token. Access verifies the scanner's organizer scope
  and the QR signature before recording the decision. Offline occurrences reconcile through the
  same lifecycle rules (ADR-025).
- Catalog owns staff accounts. The back office owns process-local sessions and role enforcement;
  unsafe catalog calls also require its catalog-only credential and a server-minted organizer
  assertion where the operation is tenant-scoped (ADR-042, ADR-058).
- Direct back-office writes to Inventory and Commerce use separate, least-privilege staff
  credentials. The back office takes the organizer and actor from its authenticated server-side
  session rather than the submitted form (ADR-057, ADR-042 as amended by TKT-194).
- Public reads use the TTL tiers in ADR-004. The storefront caches SSR page data, Catalog caches its
  event, season, and festival public reads, and Inventory caches public availability. The two Go
  caches invalidate after committed writes and expose operator kill switches (ADR-044, ADR-045).
- Inventory remains the only writer for admission claims. Display caches may be stale within their
  tier; claim transactions decide availability and prevent oversell (ADR-005, ADR-010).
- All cross-service calls propagate W3C trace context (`obs.Client`); all servers emit
  structured JSON logs with `trace_id`.
- Migrations run through each binary's `migrate` subcommand as one-shot Compose jobs. Server mode
  never applies schema changes (ADR-022).
- NATS operates with per-user authentication and publisher/subscriber access control lists (ADR-072). Services
  receive individual credentials and publish only to authorized subjects.

## Deployment topology

Docker Compose is the only runtime topology. The root [`compose.yaml`](../compose.yaml) runs the
`ticketing` project. Smoke and browser gates create isolated projects with shifted ports and their
own databases, streams, and credentials. No cloud infrastructure or application release pipeline
exists; GitHub Actions runs `make check`.

## Decisions

See the [decision status and topic index](adr/README.md).

## Component and shared-package entry points

This page owns the service/frontend map; separate service READMEs are not required. Each service's
`cmd/<service>/main.go` dispatches server and CLI commands; `internal/api/` handles HTTP,
`internal/store/` owns persistence, and `api/openapi.yaml` defines its declared contract.
[Contract map](contracts.md) lists all generated consumers.

`shared/go/` contains `cachetier`, `cmdline`, `contract`, `domainevent`, `durableconsumer`,
`fakepsp`, `httpx`, `mail`, `obs`, `organizerassertion`, `ratelimit`, and `runtimecfg`.
Verify current package directories before adding shared policy. A new shared package needs an ADR
under the working agreement.
