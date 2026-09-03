# ADR-072: NATS JetStream publisher access control lists

Date: 2026-09-03

## Status

Accepted (TKT-170)

## Context

The platform uses NATS JetStream with the `PLATFORM` stream as its event bus ([ADR-007](ADR-007-postgres-nats.md)).
Until this decision, NATS operated with no authentication. Any client with network reach to the broker could
publish to any subject. For example, a forged `platform.commerce.order.completed` event caused the access service
to mint Ed25519-signed tickets valid at physical gate turnstiles ([ADR-012](ADR-012-ticket-issuance-and-qr-credentials.md)),
even if commerce had no database record of an order and inventory had not allocated capacity.

We require per-principal publish and subscribe permissions to isolate service capabilities.

### Constraints

1. The wire format and event envelope ([ADR-033](ADR-033-shared-domain-event-envelope.md)) must not change.
2. Go service source code must not change. Go binaries read `NATS_URL`, and credentials must travel in that URL.
3. Durable consumers ([ADR-034](ADR-034-shared-durable-consumer-lifecycle.md)) and the existing `PLATFORM` stream
   must remain available and unbroken.
4. The long-running inventory service must not hold rights to publish to catalog subjects.
5. Configuration files must not contain committed secret values.

## Possible Solutions

- **Option 1: Do nothing (keep an unauthenticated broker).**
    - Pros: No configuration changes; zero operational migration cost.
    - Cons: Any process in the network can publish any domain event and forge entitlements.

- **Option 2: Multi-account isolation (named accounts per service).**
    - Pros: Complete tenant separation in NATS.
    - Cons: Named accounts create separate JetStream namespaces. The existing `PLATFORM` stream would become
      isolated and inaccessible across accounts without stream exports and imports. This adds configuration
      complexity and conflicts with [ADR-007](ADR-007-postgres-nats.md) durability.

- **Option 3: Single default account (`$G`) with per-user publish and subscribe authorization rules (chosen).**
    - Pros: Preserves the single `PLATFORM` stream. Implements exact publish and subscribe subject boundaries.
      Delivers credentials through `NATS_URL` without Go code modifications.
    - Cons: Requires explicit JetStream API permissions (`$JS.API.>`) for consumers and publishers.

## Decision

We adopt **Option 3**. We configure NATS with a single configuration file (`deploy/nats/nats-server.conf`)
under the default account (`$G`). We define seven user identities with passwords supplied through environment
variables.

### 1. Subject and API Permission Matrix

| User | Publish Allow | Publish Deny | Subscribe Allow | Subscribe Deny | Purpose |
|---|---|---|---|---|---|
| `platform-admin` | `>` | — | `>` | — | Admin migrations, `nats-init`, smoke runner |
| `catalog` | `platform.catalog.performance.{published,archived,closed,reopened}`, `platform.catalog.seat_map.published`, `$JS.API.STREAM.INFO.PLATFORM` | — | `_INBOX.>` | — | Catalog publications |
| `commerce` | `platform.commerce.order.completed`, `platform.commerce.order.exchanged`, `$JS.API.STREAM.INFO.PLATFORM` | — | `_INBOX.>` | — | Order completions and exchanges |
| `access` | `platform.access.ticket-issuance.failed`, `platform.access.{lifecycle-integrity,admission-conflict,admission-policy-conflict}.alarm`, `$JS.API.STREAM.INFO.PLATFORM`, and consumer APIs scoped to its OWN durables only (`CREATE`/`INFO`/`MSG.NEXT`/`ACK` for `access-ticket-issuer` and `access-slot-policy`, `INFO` for the three alarm operator durables) | — | `platform.commerce.order.completed`, `platform.commerce.order.exchanged`, `platform.catalog.performance.published`, the three `platform.access.*.alarm` subjects, `_INBOX.>` | — | Ticket issuance, policy projection, and alarms |
| `inventory` | `$JS.API.STREAM.INFO.PLATFORM`, and consumer APIs scoped to `inventory-catalog-offering` alone, plus `CONSUMER.DELETE` for the single legacy durable `inventory-performance-provisioner` | All `platform.*` | `platform.catalog.performance.{published,archived,closed,reopened}`, `_INBOX.>` | — | Long-running inventory server. Cannot publish domain events |
| `inventory-reprocess` | `platform.catalog.performance.{published,archived,closed,reopened}`, `$JS.API.STREAM.INFO.PLATFORM` | — | `_INBOX.>` | — | Operator quarantine reprocess command only |
| `payments` | — | `>` | — | `>` | Healthcheck connection only (`IsConnected`) |

### 2. The Adversary Model (ADR-021 discipline)

**Binds:**
- An unauthenticated client with network access to the NATS port.
- An authenticated service attempting to publish to subjects outside its explicit grant (for example, commerce publishing access events, or inventory publishing catalog events).

**Does NOT bind:**
- A compromised service publishing within its own authorized subjects (for example, a compromised commerce service publishing valid `order.completed` events).
- A holder of the `platform-admin` credential.
- An administrator of the NATS container or host.
- Any attacker who reads `.env` or container environment variables (`/proc/1/environ`).
- An on-path network observer (TLS is not enabled in this local stack).
- An adversary with direct SQL write access to a service database.

### 3. Envelope ID vs Authentication

[ADR-033](ADR-033-shared-domain-event-envelope.md) specifies a deterministic envelope `id` (`uuid.NewSHA1(...)`).
That identifier is strictly for deduplication and idempotency on retried publications. It provides **no cryptographic authentication**.
Only broker-enforced credentials identify the sender at publication time.

### 4. Operator Identity Separation

The `inventory-reprocess` user holds permission to publish to catalog subjects because `inventory reprocess-quarantine`
re-emits stored catalog events. The long-running `inventory` service container does NOT hold this permission.
`NATS_INVENTORY_REPROCESS_PASSWORD` is injected exclusively into the `nats` container. It is never passed to the `inventory`
container environment or the shared Compose environment anchor. Operators provide the credential when invoking
the one-shot quarantine reprocess command:

```bash
docker compose run --rm -e NATS_URL="nats://inventory-reprocess:${NATS_INVENTORY_REPROCESS_PASSWORD}@nats:4222" inventory reprocess-quarantine
```

### 5. Access Consumer Wildcard Rationale

Two grant shapes were REMOVED after adversarial review executed them, and the reasons matter more
than the diff.

**`$JS.API.STREAM.MSG.>` is administrative authority, not part of the publish ack path.** It was
granted to all four publishers on the assumption that a JetStream publisher needs it to receive its
ack. It does not: the ack returns on `_INBOX.>`, and `$JS.API.STREAM.INFO` is enough for the client
to resolve the stream. What the grant actually covers is `STREAM.MSG.GET` and `STREAM.MSG.DELETE`.
Executed with commerce's real credential, it read a `platform.catalog.performance.published`
envelope out of the stream and then **deleted** it, with the admin's re-read confirming
`no message found (10037)`. A publisher could therefore read every other service's events —
including `order.completed`, which carries `guest_order_ref`, the bearer credential for retrieving
signed ticket payloads — and erase them before consumption. Verifying that a permission is
SUFFICIENT is not the same as asking what it ALLOWS.

**Consumer APIs are scoped to each principal's own durables, never `PLATFORM.>`.** The wildcard let
one service drive another's durable: with inventory's credential, `consumer rm PLATFORM
access-slot-policy` succeeded. That deletion was nearly invisible, because access's reconnect loop
recreated the durable at once and the only evidence was its creation timestamp. A test watching for
an error would have seen a healthy system. The `CREATE.>` half is worse in principle: a principal
that can create a consumer with an arbitrary filter can read any subject, which would make the
subscribe column above decorative rather than a boundary.

The grant `$JS.API.CONSUMER.INFO.PLATFORM.*` for the `access` user is a deliberate wildcard.
Access configures three alarm consumer durable names from environment variables (`ACCESS_LIFECYCLE_ALARM_DURABLE`,
`ACCESS_ADMISSION_CONFLICT_DURABLE`, `ACCESS_POLICY_CONFLICT_DURABLE`). Access enforces `RequireAlarmRoute` at startup
and fails closed if it cannot read consumer metadata for those durables. Literal names in the configuration file would
cause service startup failure if an operator configured custom durable names. Reading consumer metadata grants no domain
publish authority.

### 6. Residual Risk and Pinned Gap

A compromised service can forge events within its own subject boundary. For example, a compromised commerce
service can publish an `order.completed` event for a slot without creating an order in its database. Access
consumes the event and issues a valid ticket.
This limitation is tracked in TKT-296 (signed event envelopes).
Test `TestNATSResidualCredentialedForgeryStillMintsTickets` in `smoke/nats_acl_test.go` explicitly pins this
limitation as present. Do not remove this test.

## Consequences

- **Positive:**
    - Unauthenticated clients cannot publish or subscribe to any subject.
    - Service compromises are contained to their respective event subjects.
    - Long-running inventory cannot forge catalog events.
    - No changes to Go application source code or event envelope wire format.
- **Negative:**
    - Seven new credentials must be managed, generated, and injected into the stack.
    - JetStream operations require explicit `$JS.API.*` permissions in user configurations.
    - Publishing within authorized subjects remains unauthenticated by consumers until TKT-296.

## References

- TKT-170 — NATS publisher ACLs
- TKT-296 — Signed event envelopes (future work)
- [ADR-007: PostgreSQL and NATS JetStream](ADR-007-postgres-nats.md)
- [ADR-012: Ticket issuance and QR credentials](ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-021: Ticket-lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-033: One domain-event envelope in the shared kernel](ADR-033-shared-domain-event-envelope.md)
- [ADR-034: A shared durable-consumer lifecycle primitive](ADR-034-shared-durable-consumer-lifecycle.md)
- `deploy/nats/nats-server.conf`
- `smoke/nats_acl_test.go`
