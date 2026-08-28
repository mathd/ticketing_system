# ADR-066: Scanners pull a voided-ticket feed, and the staleness ceiling is the scanner's to enforce

Date: 2026-08-22

## Status

Accepted (TKT-162; decision taken under `config.gates: autonomous`, so gates 2–4 were the agent's —
the plan-review critique, the self-made decisions and any overridden objections are on the ticket).

The product decisions below — fail closed past a generous ceiling, with a recorded operator override
— were taken by the **owner** in session on 2026-08-22, not by the agent.

Closes the capability [ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) §4 named and left open.
Extends [ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md) into territory it
deliberately did not cover. Inherits [ADR-019](./ADR-019-catalog-read-path-scoping.md)'s scoped-read
rule and [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s adversary language.

## Context

A refunded ticket is refused at a **live** gate (TKT-157) and an offline admission of one is now
**visible** after the fact through an admission-conflict alarm (TKT-269). Neither stops it. ADR-038
§4 said so precisely:

> **a refunded ticket is refused by a LIVE gate. An offline scanner will still admit it, and the sync
> will faithfully record that it did.**
>
> The missing capability is revocation propagation to offline scanners, which is a scanner/offline
> distribution feature (ADR-025's territory), not something this path can fix by denying a fact.

It is **not** in fact ADR-025's territory, and that mattered to this design. ADR-025 is entirely
about **recording after the fact** — occurrence identity, reconciliation outcomes, what the trail may
say about an admission that already happened. It contains no provision for pushing state *toward* a
gate. So this is new decision territory rather than an implementation of an existing decision.

Two facts from the code shaped everything below:

1. **`lifecycle_events` carries no organizer.** Its columns are `id`, `ticket_id`, `event_type`,
   `occurred_at` and nothing else, and no migration 0001–0009 adds one. Organizer lives on `tickets`,
   which had **no index on it**. Every existing voided-ticket query is a `ticket_id` point lookup,
   which is why the gap had never bitten.
2. **`scanner_devices.last_seen_at` cannot serve a freshness bound.** Its own migration comment says
   *"Advisory only… Never read by any decision"*, and it records when the server last **heard from** a
   device — not when the device last **successfully pulled**. Those are different facts and only the
   scanner holds the second.

## Decision

### 1. Access exposes an organizer-scoped feed of voided tickets

`GET /scans/voided-tickets`, authenticated by `ScannerDeviceToken`, returning the ticket ids the
calling device's organizer has voided.

### 2. Voided means refunded **or** exchanged

Both, from the first version, decided before the contract existed rather than widened later.

The live gate already treats them as one class (`ticketCommerciallyVoid`), and the exchanged case is
the **sharper** one offline: an exchanged ticket's replacement is live somewhere else, so admitting
the original admits the exchange **twice** on the same night. Shipping refunded-only would have meant
an additive contract change, a second index migration, and re-testing the scanner against a feed
whose meaning moved underneath it.

### 3. The organizer is unsubmittable, not validated

The operation declares no organizer parameter. The organizer comes from the context slot the device
authentication func fills and from nowhere else.

This is the difference between *"the client may not lie about this"* and *"there is nothing here to
lie about"*. A mutation that taught the handler to read an `organizer_id` query parameter passed
every test in the suite at the time — and OpenAPI query validation does **not** reject undeclared
parameters, so nothing external refuses one. What makes the value unsubmittable is that the handler
never looks for it. The test that pins this asserts the *same request with and without* a foreign
`organizer_id` returns the same feed.

### 4. Keyset pagination, and the cursor is bound to its organizer

`(occurred_at, event_id)`, newest first, `limit+1` to detect more, `next_cursor` **required and
nullable**, page limit 100 — matching this service's existing `ReconcileRequest` bound rather than
inventing a second number for one API. The first page binds a far-future sentinel rather than
switching to a second SQL string, because two statements mean the plan proof covers one of them.

`occurred_at` alone is not unique: a single refund of a multi-ticket order appends every `refunded`
event inside one transaction, sharing an instant exactly. The event id breaks the tie.

The cursor carries the organizer it was **issued for**, and a foreign one is a `400`. A cursor is
only a position, so it cannot read another organizer's rows — the query filters regardless. What an
unbound cursor can do is **suppress**: copied or forged, it makes the holder's own next page skip
rows and come back short, with no error anywhere. For a revocation feed that is the dangerous
direction, because a silently incomplete view is exactly the state this whole line of work exists to
prevent. Refusing is loud; a short page is not.

### 4b. A page walk is NOT a snapshot, and that limit is accepted

Two adversarial passes went into this clause and it ended up weaker than the first
two versions claimed. Recording the path, because the wrong versions were
plausible and the right one is a limitation rather than a mechanism.

**The claim.** A walk down the feed terminates, and does not lose rows that
existed when it began. A void created *during* a walk is excluded from that walk
and appears in the next one. That is all.

**What is NOT claimed: consistency.** `occurred_at` defaults to Postgres `now()`,
which is **transaction start time**. A voiding transaction that began before the
first page and commits after it can therefore carry a timestamp *below* the
walk's current position — a place the descending walk has already passed and will
never revisit. That row is absent from the walk while `next_cursor: null` still
reports it finished. Confirmed by executing it, not by reading the code.

**A timestamp bound cannot fix this, and one was tried.** An explicit ceiling —
the newest event the first page saw, carried through the walk — was added in
response to the first review pass, and it was **dead code**: on a strictly
descending keyset walk the cursor is always at or below any such ceiling, so the
keyset predicate is strictly stronger and the ceiling can never change a result.
The test written to prove it stayed green with the predicate deleted, which is how
it was caught. It was removed rather than kept beside an unfalsifiable test. A
real fix needs **commit ordering**, not a timestamp: a sequence or transaction-id
column on `lifecycle_events`, which is an append-only, hash-chained, signed table
(ADR-021), so that is a schema decision of its own and not this ticket's.

**The accepted mitigation** is the scanner's next pull. The window is bounded by
how long a voiding transaction stays open — short, and unrelated to how long a
scanner is offline — and the scanner polls on a **clock**, not on a cursor, so
anything missed by one walk is picked up by the next. Combined with §5's staleness
ceiling, a device that cannot pull at all fails closed rather than trusting a list
it knows is old. The guarantee is therefore *eventually complete across pulls*,
never *complete within one walk*, and §1's "incremental" should be read that way.

**The cursor is MAC-signed** (`ACCESS_FEED_CURSOR_KEY`, its own key, per the
one-key-one-claim rule in `qrlink.go`). Base64 is an encoding, not a protection:
without a MAC an enrolled device can hand-craft a position — an old one, or one
past the end — and receive an empty page with `next_cursor: null`. It cannot reach
another organizer's rows, since the query filters on the token's organizer
regardless, so **the forger and the victim are the same party**. That is weaker
than it first sounds and is recorded as such; it still matters, because a device
putting *itself* into a falsely-complete state and never learning is precisely
this feed's failure mode.

### 5. Fail closed past a generous staleness ceiling — enforced by the SCANNER

**Owner decision.** A scanner whose revocation view is older than the ceiling refuses to admit,
rather than admitting on a stale list.

The precedent cuts both ways and the distinction is deliberate. ADR-038 §4 took the *opposite* call
for a broken integrity chain — admit once, because "denying a real customer at a live turnstile is
the worse failure" — and then the same call as this one for a refunded holder, because "a refunded
holder is not a real customer". Fail-open on a stale list would re-open precisely the hole this feed
closes.

The ceiling is **generous** — hours, not minutes — because refunds cluster days ahead of the
performance, so a pull at start-of-shift closes most of the gap and the ceiling is a backstop rather
than a hot path. The exact value is TKT-271's to fix.

**It is enforced client-side, and that is forced rather than chosen** (see Context 2). The server
cannot know when a device last successfully pulled. Any server-side "freshness" check would be
comparing a value the server produced with itself, which is the precondition-that-cannot-fail shape.

### 6. An operator may override past the ceiling, and the override is recorded

**Owner decision.** Fail-closed with no local mitigation means a connectivity outage closes the door
entirely; an override that lands in the audit trail keeps fail-closed as the *default* without making
connectivity a single point of failure at a turnstile.

The override is TKT-271's to build, including what records it. Note that adding a **value** to
`event_type` changes no signed bytes — the canonical form treats it as a free string — so the cheap
option is available if it turns out to be the right one.

### 7. The scoped read is index-backed, and that is proved twice

Migration 0010 adds `tickets (organizer_id, id)`. Under ADR-019 the index is part of the change, not
a later optimisation: without it the feed returns exactly the right rows **having read every
organizer's tickets to find them**, and no assertion about the returned rows can tell the difference.

Two tests, and only the second can catch it: a **poison row** (another organizer's voided ticket is
absent) and an **EXPLAIN under `force_generic_plan`** against the shipped statement as a `const`.
Widening the tenant predicate to `($1 IS NULL OR t.organizer_id = $1)` leaves all four result tests
green and turns the plan into a `Seq Scan on tickets` — executed, not argued.

The relation order is `tickets → lifecycle_events`. The tempting alternative — an index on
`lifecycle_events (occurred_at DESC, id DESC) WHERE event_type IN (…)` — optimises the *global*
voided stream and would walk other organizers' voided rows before discarding them: right answer,
wrong scan.

## Consequences

- **A walk is not a consistent read, and the feed's completeness is across pulls
  rather than within one** — see §4b. The `next_cursor: null` that ends a walk
  means "this walk is done", not "you now hold every revocation".
- **"Bounded" is bounded two different ways, and only one is a guarantee.** The *response* is bounded
  by the page limit. The *scan* is bounded by the authenticated organizer's voided set, not by the
  page size — an organizer with a very large voided history pays a sort proportional to it. Closing
  that would need a denormalised feed or an organizer column on `lifecycle_events`. Revisit when an
  organizer's voided set is large enough that the sort dominates; not before.
- **This ticket closes no gate behaviour on its own.** A feed nothing consumes stops no admission.
  TKT-271 is where the hole actually closes.
- **Polling is a new traffic profile, and access is deliberately not rate limited (TKT-272).**
  `/scans` is one request per physical admission; a feed invites one every N seconds per device.
  The decision is **no limiter** — not "not yet", and this supersedes the provisional acceptance
  this bullet previously recorded. **Targeted device revocation is the control**:
  `access revoke-scanner <device-id>` is idempotent, permanent, and the authentication query
  filters `revoked_at IS NULL`, so a revoked device stops at the door on its next request.
  Three reasons, in the order they decide it. (1) `shared/go/ratelimit` exists for the
  *credential-free* surfaces — commerce's customer operations (ADR-051) and catalog's back-office
  login (ADR-055/ADR-042); this route is device-authenticated, so it is not one of them. (2) That
  package's own doc names what a Limiter does not bound — a distributed caller, a caller who waits
  out the window, and anything at all after a restart — and revocation is unbounded by none of
  those. (3) The per-request cost is a bounded, organizer-scoped, indexed keyset read with a hard
  page cap of 100, so it does not carry the write amplification that made a Postgres-backed limiter
  the wrong shape for the credential-free surfaces — the counters `ratelimit` deliberately keeps out
  of Postgres. (The device `last_seen_at` touch on the authentication path is a separate, smaller
  cost; see the bullet on it below.)
- **Name the adversary for the polling decision (ADR-021).** An **enrolled** device may poll this
  feed as fast as it likes, without bound, until an operator revokes it. Nothing throttles it, and
  no sentence in this repo should say this route is rate limited. What makes that residual
  survivable is that the abusive device is *identifiable*: every authenticated request to the feed
  emits one `abuse.request` record carrying the device's UUID, which is the input
  `access revoke-scanner` takes. Telemetry here is **visibility, not containment** — it makes the
  control usable by an operator, and refuses nothing on its own. The device UUID travels in logs;
  it is deliberately kept off metric labels (unbounded cardinality) and off span attributes (a
  sampler must not decide whether an operator can see an abusive device, and spans leave the
  process for a collector this repo does not control). The token itself never appears in any of
  the three (ADR-012 § TKT-202).
- **The record is emitted at AUTHENTICATION, not in the handler, and the difference is not
  cosmetic.** Authentication runs *before* parameter validation, and it both reads the device row
  and writes `last_seen_at`. So a request whose `limit` fails the contract's schema — the cheapest
  abusive request there is — already costs a SELECT and an UPDATE by the time the validator refuses
  it. An emit in the handler would never see that request, leaving the highest-frequency, lowest-cost
  abuse invisible to the telemetry whose entire job is to name the device to revoke. Emitting from
  the authentication path, scoped to the resolved `listVoidedTickets` operation, counts every
  authenticated poll of this feed whatever refuses it and wherever. (Found by adversarial review and
  confirmed by execution, after a first implementation emitted from the handler.)
- **The per-request cost above is the cost of a SERVED poll, and a refused one is not free.** Every
  authenticated request to any scan route performs the device lookup and a best-effort
  `UPDATE scanner_devices SET last_seen_at`, before any parameter is validated. That write is small,
  unindexed on the updated column and confined to one row per device, but it is a write, and a
  determined enrolled poller does cause row churn on its own row. It is bounded by the same control
  as everything else here: revoke the device. Stated so the "no write amplification" reasoning above
  is read as being about the feed's *query* path, which is what it describes.
- **Name the adversary (ADR-021).** This constrains a caller who does not hold an enrolled device's
  token, and one enrolled device from reading another organizer's revocations. It constrains **nobody
  with write access to the access database**, who can enrol their own device — the same trust
  boundary every other table in this service sits inside. It is also not a confidentiality guarantee
  about the *contents*: a revocation list tells its holder which of an organizer's tickets have been
  voided, which is why it is organizer-scoped and device-authenticated rather than merely obscure.
- **The feed cannot help a scanner that never pulls.** ADR-025's "a gate that never syncs" caveat
  applies in the other direction too: a device that has never reached the network has no revocation
  state, and the ceiling is what turns that from a silent hole into a refusal.
