# ADR-007: PostgreSQL per service, NATS JetStream as the event bus

Date: 2026-07-12

## Status

Accepted (approved at the TKT-25 plan gate, 2026-07-12)

## Context

US-001 (TKT-25) mandates that the databases and event bus be chosen before the scaffold lands.
The inventory hot path needs real transactional primitives (row/advisory locks for contention-safe
claims, US-003); the ADR-003 append-only journal needs a store it can trust; ADR-002 requires an
event bus for the domain-event stream. The estate is Docker Compose only, run by one owner + AI
agents. Per the platform convention, all versions are latest-stable at implementation time
(Postgres 18.4, NATS 2.x as of this writing).

## Possible Solutions

- **Database — one shared DB:** rejected: erodes ADR-002 service boundaries (cross-service joins
  become possible, then habitual).
- **Database — engine per service (mixed):** rejected: operational tax with no payoff at this scale.
- **Database — SQLite per service:** rejected: no concurrent-writer story for the inventory hot path.
- **Database — PostgreSQL, one cluster, one database + role per service (chosen).**
- **Bus — Kafka/Redpanda:** rejected for now: operational weight unjustified before TKT-24's
  analytics volume exists; revisit there with evidence.
- **Bus — Postgres outbox only:** rejected: an outbox is a publishing pattern, not a bus; services
  may still use outboxes internally to publish reliably (later stories).
- **Bus — NATS JetStream (chosen).**

## Decision

1. **PostgreSQL (18.4), one Compose cluster, one database + one role per service.** CONNECT is
   revoked from PUBLIC on every service database: a service's credentials physically cannot reach
   another service's data (asserted by the smoke suite). Fitness note: Postgres suits the ADR-003
   journal (append-only tables), but the journal's design — sequence numbering, hash chaining,
   signing, projections, pseudonymity — is **deferred to US-004**, not settled here.
2. **Shared-cluster trade-off (recorded deliberately):** one cluster gives no independent failure,
   upgrade, resource or backup boundaries between services. **Separation triggers:** contention
   evidence from US-003/TKT-4 load tests isolating inventory, or divergent tuning/upgrade needs on
   the payments journal. Database-per-service makes that split a connection-string change, not a
   data migration.
3. **NATS JetStream** is the platform event bus: single small binary, persistent streams for the
   ADR-003/TKT-24 domain-event stream, subject hierarchy (`platform.>` stream exists from US-001)
   fits per-domain/per-tenant routing.

## Consequences

- **Positive:**
    - Inventory claims get real DB-level concurrency control in US-003 without new infrastructure.
    - Boundary enforcement is physical (credential isolation), not conventional.
    - The domain-event stream is durable from day one; TKT-24's pipeline consumes it without
      re-platforming.
- **Negative:**
    - One Postgres cluster is a shared fate until a separation trigger fires.
    - JetStream stream/consumer topology is a new operational surface the team must learn.

## References

- [PRD](../product/prd-v1.md) (US-001 planning note) · [ADR-002](./ADR-002-services-from-day-one.md) ·
  [ADR-003](./ADR-003-append-only-audit-trail.md) · TKT-25 plan thread (board)
