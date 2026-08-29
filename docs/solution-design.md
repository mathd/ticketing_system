# Solution Design

This document is the high-level design index for the ticketing-system testbed. Detailed product
scope lives in [`product/`](./product/), binding technical decisions in [`adr/`](./adr/), and the
running topology in [`architecture.md`](./architecture.md). Code and committed contracts remain
the source of truth for implementation details.

## Purpose

The system exercises AI-assisted delivery against a non-trivial event-ticketing domain. M1 proves
one complete GA path: catalog publication, contention-safe reservation, fake-PSP checkout,
append-only money facts, ticket issuance/delivery, and gate redemption. M2 adds owner-prioritized
capabilities from the existing backlog rather than changing the platform stack.

## Design boundaries

| Area | Current design |
|---|---|
| Services | Five Go services—Catalog, Inventory, Commerce, Payments, Access—behind a thin Go gateway (ADR-002) |
| Frontends | Astro SSR/React Storefront, Astro SSR Back Office behind `/admin/` with role-gated staff sessions (ADR-006/042), and React/Vite Scanner |
| Persistence | PostgreSQL 18.4, one database and role per service (ADR-007) |
| Events | NATS JetStream `PLATFORM` stream; versioned identifier-only domain events (ADR-007/009) |
| Contracts | Hand-maintained OpenAPI specifications validated against requests/responses (ADR-009) |
| Money | Integer minor units plus ISO currency; append-only journal facts (ADR-001/003/011) |
| Tickets | Access-owned signed QR credentials and immutable lifecycle history (ADR-012) |
| Operations | Docker Compose only; OTLP traces, metrics, and logs through `grafana/otel-lgtm` |

Service ownership and cross-service flows are diagrammed in
[`architecture.md`](./architecture.md). A stack deviation requires an ADR; domain implementation
must begin from an accepted product spec or ticket context.

## Runtime and data contracts

- Public traffic enters through the gateway's explicit route table. Internal service routes are not
  generic gateway passthroughs.
- Catalog publication provisions Inventory through
  `platform.catalog.performance.published`.
- Commerce completion triggers Access through `platform.commerce.order.completed`.
- Each service owns its schema and credentials; cross-service reads use HTTP or committed events.
- OpenAPI source files live at `services/*/api/openapi.yaml`; generated models and Storefront types
  are drift-checked by `make check`.
- Configuration and operational recovery procedures live in
  [`configuration.md`](./configuration.md) and [`development.md`](./development.md).

## Quality and evolution

The repository gate is `make check`: generation drift, Go/TypeScript lint and tests, builds, and an
isolated Compose smoke suite. The smoke seam covers real PostgreSQL, JetStream, gateway routing,
contracts, browser assets, and observability. Architecture decisions are appended under
[`adr/`](./adr/); milestone state is maintained in [`ROADMAP.md`](./ROADMAP.md).

There is no cloud environment, infrastructure-as-code estate, production deployment pipeline,
real PSP, or external documentation system in the current scope. Those are future decisions, not
implicit parts of this design.
