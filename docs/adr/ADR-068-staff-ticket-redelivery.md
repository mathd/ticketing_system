# ADR-068: Staff re-delivery of a completed order's tickets

Date: 2026-08-28

## Status

Accepted (TKT-203; decision taken under the autonomous gates of that run, recorded on the ticket).
Amends nothing. **ADR-043 draws the line for where a guard lives, and this does not move it** — the
guard stays an inline check on access's `/internal/` surface. **ADR-057 decided the same-shaped
credential question for inventory, and its reasoning transfers exactly** — the difference from
ADR-053, which answered the other way, is set out below.

## Context

A customer whose delivery mail was lost or spam-filed could not be helped: **no resend existed
anywhere.** Delivery is a side effect of consuming `order.completed` —
`Consumer.deliver` (`services/access/internal/consumer/consumer.go:403-425`) reads the buyer's
address from commerce, sends the guest link, and calls `MarkDelivered`. Nothing re-triggers it.

Three mechanisms on that path exist specifically to make delivery happen **once**, and each is an
obstacle a resend has to answer rather than reuse:

1. `PendingDeliveries` filters on `NOT EXISTS (… event_type='delivered')`
   (`internal/store/postgres.go:141`), so the tickets a resend exists to serve are exactly the ones
   it excludes. Replaying the event delivers nothing.
2. `DeliveryID` (`postgres.go:157`) derives **one message id per ticket for all time** —
   `uuid.NewSHA1(ticketID + ":delivery")` — inserted `ON CONFLICT(ticket_id) DO NOTHING` into a
   table whose `ticket_id` is the **PRIMARY KEY**.
3. `lifecycle_events` carries a partial unique index making `delivered` **singleton per ticket**, so
   the database refuses a second one.

The product question was settled by the owner on 2026-08-26 and is not reopened here: **the
destination is always the address already on file.** An operator-typed address turns the back office
into a mail relay emitting ticket capabilities to arbitrary recipients — a fraud surface that would
need an audited operator identity on the action, which does not exist and does not reach access.

## Decision

**Access owns a staff-triggered redelivery, on `POST /internal/orders/{id}/redeliveries`, guarded by
a new `ACCESS_STAFF_WRITE_TOKEN` accepted on that one operation and nothing else.**

### 1. The destination is unsubmittable, not validated

There is **no recipient field** on the request, in the contract, in the client, or in the form.
Access resolves the address per ticket from commerce's existing internal delivery-email read, at
send time. A validated recipient field is still a recipient field: if *"can the client influence
this?"* answers *"yes, but only if it lies in a way I now check for"*, it is not fixed. The response
carries a **count**, never the address and never a capability link.

**Accepted limitation, stated rather than left to be discovered:** a customer who has *changed*
address is not served by this. Their path is a buyer-contact correction, which does not exist
either. That case is refused explicitly rather than solved unsafely.

### 2. A new credential, on ADR-057's premise

The back office holds **nothing** for access today. Reusing its catalog or commerce credential would
grant a new capability class across a service boundary and turn one of those compromises into the
power to re-emit ticket capabilities. That is ADR-057's reasoning exactly, and it is why **ADR-053's
opposite answer does not transfer**: there, the catalog token already held create and update power
over the very channels it was then allowed to list, so the allowance added amplification but no new
capability class.

**Additional, not replacing.** `X-Internal-Token` still works — every service-to-service caller
presents it. Access refuses to start when the two are equal, or when the new one is absent or below
`runtimecfg.CredentialMinBytes` (ADR-059). The back office refuses to boot when any two of the
**four** credentials it now holds are equal.

**The grant is a route table, not a check inside the handler.** `staffWriteOperations`
(`internal/api/redeliveries.go`) names the operations the staff credential opens, and
`staffOrInternal` consults it. This is the one structural departure from inventory's shape, and it
was forced by evidence: with the check inlined, granting the credential a second route **killed no
test**, because `refundTickets` carries its own inline guard and went on refusing with the same 404
for its own reason. That is ADR-053's recorded failure — *"the guard stopped being narrow while
every route-level test stayed green"* — reproduced by mutation during the build. A property about a
**set** ("this credential opens these and no others") cannot be enforced from inside one member of
it. A route added later is therefore closed to the staff credential by default.

### 3. `redelivered` is a new, REPEATABLE lifecycle event type

`delivered` is left alone: it keeps its singleton index entry and keeps meaning *"the automatic
delivery on issuance happened once"*. A resend is a different act by a different actor, and
collapsing the two would destroy the only record of which is which.

`redelivered` stays **out** of the singleton partial index, alongside the admission types. Its retry
idempotency comes from the event id being derived per `(organizer, key, ticket)` — ADR-025 §D3's
rule that repeatable types are protected from retry duplication by the event id, not by an index.
Migration `0011` widens the CHECK **before** dropping the old one, as 0007 and 0008 did, and its
`Down` is a hard refusal like every access migration over the trail.

Adding a **value** to `event_type` changes no signed canonical bytes and needs no canonical-version
migration — the claim `0008` states and this migration inherits. Adding a canonical **field** would.
The `lifecycle.Event` type carries identifiers only, and a test refuses any field named for PII or
the guest reference (ADR-003 §D3) — which is also what keeps the trail free of the recipient.

### 4. Resend attempts get their own table

`redelivery_attempts` sits **beside** `delivery_attempts` rather than relaxing it. Turning that
table's primary key into a non-unique foreign key would change the meaning of every existing row to
serve a feature representable next to it. Each resend mints a message id distinct from the
original's, because a transport that deduplicates on message id would otherwise drop the resend as a
replay of the first send — the precise failure being fixed.

### 5. Completion is decided locally, and no commerce credential is taken

Access does **not** ask commerce whether the order completed. Its tickets exist only as a
consequence of consuming `order.completed`: `store.Issue` has one non-test caller
(`consumer.go:367`), reached from one site (`processCompleted`), and the exchange path moves tickets
rather than issuing them. So *"this order has ticket rows here"* is access's own evidence, held
locally and needing no credential.

The alternative — commerce's `GET /internal/orders/{id}` — is guarded by
`COMMERCE_STAFF_WRITE_TOKEN`, which access does not hold. Taking it would have been the very
cross-service grant §2 refuses, arriving by the back door as a plumbing detail.

**This is an invariant the feature now depends on, not an incidental fact.** A future path that
issues tickets for a non-completed order makes it silently wrong.

An order with no tickets answers **503**, not 404: issuance is asynchronous, so a resend can outrun
it, and access cannot distinguish *never completed* from *completed, issuance still in flight*. The
refund route reached the same conclusion first.

### 6. The bound is durable and per order

**Five distinct redelivery requests per order per rolling 24 hours**, counted in PostgreSQL against
`redelivery_requests`. Replays of an existing key are exempt — an operator retrying an ambiguous
request must not be refused by a quota that request already passed.

Not `shared/go/ratelimit`: that package is in-process and per-replica, and its own doc says so. A
durable row count survives a restart and holds across replicas, which is what "per order" requires.

Five rather than one because the support interaction this serves is *resend → "still nothing" →
check the address → resend* inside one phone call, and a bound of one refuses the second attempt and
offers the operator nothing but "wait a day".

**Name what this does not bound.** It bounds distinct requests per **order**. It does not bound
redeliveries across different orders, a caller who waits out the window, a holder of a stolen staff
credential doing either, or anyone with write access to the access database. It is a blast-radius
bound on capability re-emission per order, not an anti-abuse control.

## What this costs

**Name the adversary (ADR-021).** Access authenticates the calling **process**, not the staff member
behind it. This credential limits the blast radius of a compromised or buggy back office to one
access operation. It is **not** tamper-evidence: anyone with the access database can write these
tables directly, and no credential changes that.

What the grant actually adds, stated plainly:

- A holder can **cause a resend of any order's tickets, for a caller-supplied organizer**, up to the
  bound. The organizer is the **scope, not the authority** — access has no organizer identity it can
  verify independently of the request, exactly as for catalog (ADR-053) and inventory (ADR-057), and
  for the same reason. A stolen credential is not confined to one tenant. That gap is not new here
  and this ADR does not claim to close it.
- **Every resend re-emits the guest retrieval capability, and nothing revokes the earlier one**
  (ADR-012). The set of holders widens permanently, per send. **Accepted as-is**, because the
  alternative — invalidating the prior link — is a capability-lifecycle change that would alter the
  behaviour of already-issued links, and it deserves its own decision rather than arriving as a side
  effect of a support feature. The bound in §6 is what keeps the widening finite per order.

**There is no mail sender in this platform.** The only `Mailer` implementation hashes the recipient
and the link, logs them, and returns nil (`consumer.go:116-124`); ADR-050 records that migrating
access onto the real mail port is deferred work. So the strongest true claim is **"the transport
accepted the message"**, and no code, test, document or PO note here says the customer received an
email.

**A crash window is left open, deliberately.** Between the transport accepting and the trail
recording, a crash leaves a sent mail with no lifecycle event. Recovery is a retry under the same
idempotency key, which re-derives the same message id — so a transport that deduplicates on message
id will not send twice. That is a requirement **on** the transport, written down rather than claimed
as closed.

## Consequences

- **Positive:**
    - A support agent can serve the lost-or-spam-filed case, which is the common one, from the order
      console they already use.
    - The trail records **each** delivery. It no longer says delivered-once about a ticket delivered
      four times.
    - The staff-credential grant is enforced by a table one edit wide, and the router walk in
      `staff_credential_test.go` fails on both an uncovered route and a stale entry.
- **Negative:**
    - A fifth credential class in the system (ADR-042 staff sessions, ADR-043 internal tokens,
      ADR-049 customer sessions, ADR-056 partner credentials, ADR-057/this staff-write tokens). Each
      is justified separately; the count itself is a cost.
    - One more secret to generate, distribute and rotate, and a fourth value the back office must
      compare at startup — six pairs now, which is why that comparison and its test are derived from
      a list rather than written out.
    - Capability widening is accepted rather than solved (§ What this costs).
- **Revisit when:** a real mail sender lands (ADR-050) and "delivered" can mean something stronger;
  an organizer identity access can verify exists; or a second consumer needs this operation — two
  callers on a route-specific allowance is the point at which the allowance should become a
  mechanism.

## References

- TKT-203 (this ticket), TKT-194 (the order console's refund), TKT-201 (the staff order read)
- [ADR-012](./ADR-012-ticket-issuance-and-qr-credentials.md) — the guest retrieval capability, and its § TKT-202 redaction amendment
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — the append path, and name the adversary
- [ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md) — §D1/§D3, repeatable event types
- [ADR-043](./ADR-043-where-a-service-auth-guard-lives.md) — contract operation vs internal route
- [ADR-053](./ADR-053-channel-operator-read-credential.md) — the same question, answered the other way, and why
- [ADR-057](./ADR-057-inventory-staff-write-credential.md) — the controlling precedent
- [ADR-042](./ADR-042-staff-identity-and-backoffice-sessions.md), [ADR-049](./ADR-049-customer-identity-and-storefront-sessions.md), [ADR-056](./ADR-056-partner-credential-identity.md) — the other identity classes
- [ADR-050](./ADR-050-transactional-mail-and-password-recovery.md) — the mail port, and the absent sender
- [ADR-059](./ADR-059-credential-length-floor.md) — the credential floor
- [ADR-002](./ADR-002-services-from-day-one.md) — the gateway edge-denies `/internal/`
