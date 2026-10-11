# ADR-072: NATS JetStream publisher access control lists

Date: 2026-09-03

## Status

Accepted (TKT-170)

Amended by **TKT-326** (2026-10-11, §Amendment below) — the operator identity of §4 is removed.
Catalog is the only service principal that can publish on `platform.catalog.*`, and the stack has
six principals.

Amended by **TKT-327** (2026-10-11, §Amendment below) — access and inventory hold no CREATE grant
for their plural-filter durables, so the body-controlled filter of §5 and §6(a) is closed for both.
Cross-service READS are **not** closed: §6(c) records a second read path, through the shared
`_INBOX.>` namespace, executed during TKT-327.

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
    - Pros: Preserves the single `PLATFORM` stream. Implements exact PUBLISH subject boundaries.
      Delivers credentials through `NATS_URL` without Go code modifications.
    - Cons: Requires explicit JetStream API permissions for CONSUMERS (pure publishers need none —
      see §6). And it does **not** deliver an exact SUBSCRIBE boundary against a compromised
      principal. As first accepted: two durables declare plural `FilterSubjects`, whose create
      subject carries no filter token, so their filter was body-controlled and could not be
      constrained by subject permission. TKT-327 closed that path (§Amendment). The shared
      `_INBOX.>` namespace is a second path, still open (§6(c)).

## Decision

We adopt **Option 3**. We configure NATS with a single configuration file (`deploy/nats/nats-server.conf`)
under the default account (`$G`). We define six user identities with passwords supplied through environment
variables (seven until TKT-326 removed the operator identity of §4).

### 1. Subject and API Permission Matrix

| User | Publish Allow | Publish Deny | Subscribe Allow | Subscribe Deny | Purpose |
|---|---|---|---|---|---|
| `platform-admin` | `>` | — | `>` | — | Admin migrations, `nats-init`, smoke runner |
| `catalog` | `platform.catalog.performance.{published,archived,closed,reopened}`, `platform.catalog.seat_map.published` | — | `_INBOX.>` | — | Catalog publications |
| `commerce` | `platform.commerce.order.completed`, `platform.commerce.order.exchanged` | — | `_INBOX.>` | — | Order completions and exchanges |
| `access` | `platform.access.ticket-issuance.failed`, `platform.access.{lifecycle-integrity,admission-conflict,admission-policy-conflict}.alarm`, `$JS.API.STREAM.INFO.PLATFORM`, and consumer APIs scoped to its OWN durables for every MUTATING or CONSUMING call (`MSG.NEXT`/`ACK` on both; `CREATE` only for `access-slot-policy`, pinned to its one filter; none for `access-ticket-issuer`, which `nats-init` pre-provisions — TKT-327), with read-only `CONSUMER.INFO.PLATFORM.*` | — | `platform.commerce.order.completed`, `platform.commerce.order.exchanged`, `platform.catalog.performance.published`, the three `platform.access.*.alarm` subjects, `_INBOX.>` | — | Ticket issuance, policy projection, and alarms |
| `inventory` | `$JS.API.STREAM.INFO.PLATFORM`, `CONSUMER.INFO`/`MSG.NEXT`/`ACK` scoped to `inventory-catalog-offering` (no `CREATE`: `nats-init` pre-provisions it — TKT-327), plus `CONSUMER.DELETE` for the single legacy durable `inventory-performance-provisioner` | All `platform.*` | `platform.catalog.performance.{published,archived,closed,reopened}`, `_INBOX.>` | — | Long-running inventory server. Cannot publish domain events |
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

### 4. Operator Identity Separation (removed by TKT-326)

TKT-170 defined a separate operator identity with publish rights on catalog's four performance
subjects, because `inventory reprocess-quarantine` republished stored catalog events onto them. Its
password was delivered only to the `nats` container, never to the long-running `inventory`
container. TKT-326 replaced the republication with in-process reprocessing and removed the identity.
See §Amendment (TKT-326).

### 5. Access Consumer Wildcard Rationale (read-only INFO)

Two grant shapes were REMOVED after adversarial review executed them, and the reasons matter more
than the diff.

**`$JS.API.STREAM.MSG.>` is administrative authority, not part of the publish ack path.** It was
granted to all four publishers on the assumption that a JetStream publisher needs it to receive its
ack. It does not: the ack returns on `_INBOX.>`. A pure publisher in fact needs **no** `$JS.API` grant
at all, verified by publishing with the domain subject as its only publish permission, so
`$JS.API.STREAM.INFO.PLATFORM` was removed from catalog, commerce and the operator identity of §4 too.
Only `access` and `inventory` keep it, because they open the stream explicitly to bind consumers. What the grant actually covers is `STREAM.MSG.GET` and `STREAM.MSG.DELETE`.
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

**A residual that scoping did not close — closed by TKT-327 for this path only.** A durable
declared with PLURAL `FilterSubjects` carries no filter token in its create subject, so its filter
arrives in the request body and NATS cannot constrain it by subject permission. Two durables were
affected — `access-ticket-issuer` and `inventory-catalog-offering`. Demonstrated in TKT-170: a
principal holding only inventory's grants re-created its OWN durable filtered on
`platform.commerce.order.completed` and read the payload back. TKT-327 removed the services' CREATE
grants for both durables: `nats-init` pre-provisions them under `platform-admin`, and each service
binds with INFO and verifies the config before it consumes
([ADR-034](ADR-034-shared-durable-consumer-lifecycle.md) §Amendment: pre-provisioned plural-filter
durables). The subscribe column above is still **not** a boundary against a compromised principal,
for a different reason: §6(c). `access-slot-policy` is **not** affected, and the reason is worth stating because it was wrong once:
it declares a SINGLE `FilterSubject`, so its create subject carries the filter as a suffix token —
but that only helps if the UNSUFFIXED create subject is not also granted. It was, in the first fix
round, which left the same body-controlled path open on a third durable (ai-review pass 3). Only the
suffixed grant remains, verified by watching the real client create the durable with the suffixed
grant as its only CREATE permission.

The grant `$JS.API.CONSUMER.INFO.PLATFORM.*` for the `access` user is a deliberate wildcard.
Access configures three alarm consumer durable names from environment variables (`ACCESS_LIFECYCLE_ALARM_DURABLE`,
`ACCESS_ADMISSION_CONFLICT_DURABLE`, `ACCESS_POLICY_CONFLICT_DURABLE`). Access enforces `RequireAlarmRoute` at startup
and fails closed if it cannot read consumer metadata for those durables. Literal names in the configuration file would
cause service startup failure if an operator configured custom durable names. Reading consumer metadata grants no domain
publish authority.

### 6. Residual Risk and Pinned Gap

**Three residuals. (a) is closed by TKT-327; (b) and (c) are open and accepted.**

**(a) Cross-service READS through a body-controlled durable filter — closed by TKT-327.**
Demonstrated in TKT-170: a principal holding exactly inventory's grants re-created its own durable
(`inventory-catalog-offering`) with a filter of `platform.commerce.order.completed` and read the
payload back. `access-ticket-issuer` had the same shape. The cause is a NATS property rather than a
configuration mistake — a durable declared with plural `FilterSubjects` has no filter token in its
create subject, so the filter is body-controlled. TKT-327 removed both services' CREATE authority
over these durables (§Amendment).

What is claimed now, and against whom (ADR-021): **a holder of the access or the inventory service
credential cannot change the filters of its plural-filter durable through the JetStream consumer
create APIs** — not the unsuffixed subject (plural filter in the body), not the suffixed subject
(single filter as a token), and not the legacy `DURABLE.CREATE` subject.
`TestNATSServiceCannotWidenItsOwnPluralDurable` executes each request with the real credential and
asserts a publish Permissions Violation, no reply and unchanged filters. Restoring each removed
grant, one at a time, was executed on an isolated broker with this ACL file and turned exactly its
own case red. What is **not** claimed:

- Nothing about `platform-admin` or the NATS host administrator (§2). Either can change a durable at
  will, and the services verify their durable only at startup, not continuously.
- Nothing about other read paths. §6(c) is one, executed, and still open.
- Nothing about `access-slot-policy` beyond §5: its CREATE grant stays, pinned to its one filter.

**Therefore the subscribe column of the matrix above is still a boundary for an honest client
only** — because of (c), not (a).

**(b) Forged events within a principal's own publish boundary — TKT-296.**

A holder of commerce's NATS credentials alone can publish `order.completed` for an order Commerce does not
know. Access reads the order's seat data from Commerce, so that event mints no ticket. With a fresh event ID,
it fails issuance at the seat read. If it reuses an already consumed event ID, the consumed-event pre-check
skips it without a failure record.
This narrows the gap. The following cases are inferred from code, not executed by any test. The Commerce
seat query has no order-status predicate (`services/commerce/internal/store/order_seats.go:31`), and
Access compares only order ID, organizer, slot, ticket type, and quantity
(`services/access/internal/consumer/seats.go:26`). A forger could name a declined or otherwise unpaid
order that still has a reservation row and get tickets for seats whose hold may already be released. A
forger could also keep those checked fields from a real order and choose a different `buyer_id` or
`guest_order_ref`. The smoke test executes only the completed-order, original-buyer case. A compromised
Commerce service can answer the read for any forged event. Before TKT-327, a holder of the inventory
credential could read `order.completed` payloads from JetStream through a widened durable (§6(a)). That
path is closed, but §6(c) still exposes the same payloads, and the access principal reads them by design;
each payload exposes an order line. The seat read is not a security control and does not authenticate
the event. TKT-296 tracks the residual: signed event
envelopes close forgery by a holder of stolen NATS credentials who does not have the signing key. A
compromised service or signer remains outside that guarantee.

Test `TestNATSResidualCredentialedForgeryStillMintsTickets` in `smoke/nats_acl_test.go` pins both the
narrowed case and the still-open case. Do not remove this test.

**(c) Cross-service READS through the shared inbox namespace — open, found in TKT-327, tracked as
TKT-519.** Every
service principal subscribes to `_INBOX.>`, and no service sets a custom inbox prefix, so all of them
share nats.go's default `_INBOX.` namespace. A JetStream pull delivers each message to the puller's
reply inbox. Any principal can therefore subscribe to `_INBOX.>` and receive copies of another
principal's deliveries. **Executed, not argued:** on an isolated broker with this ACL file and the
TKT-327 grants, the inventory credential, watching `_INBOX.>`, read `access-ticket-issuer`'s delivery
of an `order.completed` carrying `guest_order_ref`. It does not need any CREATE grant, so TKT-327
does not close it. The likely fix is a per-principal inbox prefix (each service's `nats.Connect`
with a custom inbox prefix, and each principal's subscribe grant narrowed to that prefix); it
touches every service's connection code, so it is its own ticket: **TKT-519** ("Any service
credential can read every other service's JetStream pull deliveries through `_INBOX.>`"). Until
TKT-519 lands, a compromised service principal can read every event any other service consumes.
`TestNATSInboxWildcardStillExposesOtherPrincipalsDeliveries` pins this gap as PRESENT. When TKT-519
lands, that test goes red because nothing is seen; TKT-519 then updates this paragraph and flips the
test to assert a refusal.

## Amendment (2026-10-11, TKT-326) — catalog is the only service that publishes on its prefix

**The cross-prefix exception of §4 no longer exists.** `inventory reprocess-quarantine` used to
republish quarantined catalog events onto `platform.catalog.performance.*`, under the dedicated
`inventory-reprocess` identity. It now runs the inventory consumer's handler in-process on the
stored envelope, against inventory's own database ([ADR-017](ADR-017-domain-event-schema-evolution.md)
§5b recovery paragraph). It opens no NATS connection.

TKT-326 therefore removed the identity and all of its plumbing: the user block in
`deploy/nats/nats-server.conf`, its password entry in `compose.yaml`, its draw in
`scripts/env-bootstrap.sh` and `scripts/stack-env.sh`, its exports in `scripts/smoke.sh`, and
`TestNATSOperatorCredentialIsConfinedToTheBroker`, whose property no longer exists. The stack has
six principals.

What is true now:

- Among service principals, only `catalog` can publish on `platform.catalog.*`. `platform-admin`
  (`publish: [">"]`) stays an explicit exception, and it is the principal for the manual re-drive
  of a parked catalog event ([ADR-077](ADR-077-moot-tombstones-and-bounded-parking.md),
  `docs/development.md`).
- The `inventory` principal is unchanged. It holds no `platform.*` publish. It does publish on
  JetStream API and ack subjects (`$JS.API.*`, `$JS.ACK.*`), because a JetStream API call and an ack
  are publishes. "Inventory holds no publish permission at all" is therefore not true, and it is
  not claimed.
- Constraint 4 is now met because no inventory principal has a publish grant on catalog subjects,
  not because an operator identity is kept apart from the server.
- Constraint 2 ("Go service source code must not change") bound TKT-170 only. TKT-326 changed
  inventory's Go code to remove the republication.
- The adversary model of §2 is unchanged. TKT-326 removes a credential that an operator had to
  deliver correctly at each invocation. It does not change what a holder of `platform-admin`, or
  an administrator of the NATS host, can do. Residual (a), TKT-327, is untouched by TKT-326 (see
  the TKT-327 amendment below).

## Amendment (2026-10-11, TKT-327) — no service can create its plural-filter durable

**What changed.** `deploy/nats/nats-server.conf` no longer grants
`$JS.API.CONSUMER.CREATE.PLATFORM.access-ticket-issuer`, `...access-ticket-issuer.>`,
`...inventory-catalog-offering` or `...inventory-catalog-offering.>`. `nats-init` creates both
durables under `platform-admin` from `deploy/nats/consumers/`, and each service binds to its durable
and refuses to start if it is missing or differs from the service's config
([ADR-034](ADR-034-shared-durable-consumer-lifecycle.md) §Amendment: pre-provisioned plural-filter
durables). Inventory now holds no CREATE grant at all. Access holds exactly one,
`$JS.API.CONSUMER.CREATE.PLATFORM.access-slot-policy.platform.catalog.performance.published`, which
§5 explains.

**What is true now** is §6(a)'s narrowed claim, and only that. §6(c) is the read path that remains.
Constraint 2 ("Go service source code must not change") bound TKT-170 only: TKT-327 changed both
services' consumer startup.

**Operational note.** The ACL is read when `nats` starts. On an existing stack, the removed grants
stay in force until `docker compose restart nats` (`docs/development.md`). `make down` is not the
way to apply it: it runs `down -v` and deletes every volume.

## Consequences

- **Positive:**
    - Unauthenticated clients cannot publish or subscribe to any subject.
    - A service compromise is contained to its own **publish** subjects: a compromised principal
      cannot publish another service's events. It is **not** contained on the read side — see
      §6(c), which is demonstrated rather than theoretical. Since TKT-327 a compromised access or
      inventory principal cannot widen its own plural-filter durable (§6(a)).
    - Long-running inventory cannot forge catalog events.
    - No changes to Go application source code or event envelope wire format.
- **Negative:**
    - Six credentials must be managed, generated, and injected into the stack (seven until TKT-326).
    - JetStream operations require explicit `$JS.API.*` permissions in user configurations.
    - Publishing within authorized subjects remains unauthenticated by consumers until TKT-296.

## References

- TKT-170 — NATS publisher ACLs
- TKT-326 — In-process quarantine reprocess; the operator identity is removed
- TKT-327 — Pre-provisioned plural-filter durables; the services' CREATE grants for them are removed
- TKT-296 — Signed event envelopes (future work)
- [ADR-007: PostgreSQL and NATS JetStream](ADR-007-postgres-nats.md)
- [ADR-012: Ticket issuance and QR credentials](ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-021: Ticket-lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-033: One domain-event envelope in the shared kernel](ADR-033-shared-domain-event-envelope.md)
- [ADR-034: A shared durable-consumer lifecycle primitive](ADR-034-shared-durable-consumer-lifecycle.md)
- `deploy/nats/nats-server.conf`
- `smoke/nats_acl_test.go`
