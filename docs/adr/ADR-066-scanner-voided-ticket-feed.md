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

- **"Bounded" is bounded two different ways, and only one is a guarantee.** The *response* is bounded
  by the page limit. The *scan* is bounded by the authenticated organizer's voided set, not by the
  page size — an organizer with a very large voided history pays a sort proportional to it. Closing
  that would need a denormalised feed or an organizer column on `lifecycle_events`. Revisit when an
  organizer's voided set is large enough that the sort dominates; not before.
- **This ticket closes no gate behaviour on its own.** A feed nothing consumes stops no admission.
  TKT-271 is where the hole actually closes.
- **Polling is a new traffic profile, and access has no rate limiting.** `/scans` is one request per
  physical admission; a feed invites one every N seconds per device. `shared/go/ratelimit` is
  in-process and per-replica, so it would be a weak answer. **Accepted for now, stated rather than
  unnoticed**, and tracked as a follow-up.
- **Name the adversary (ADR-021).** This constrains a caller who does not hold an enrolled device's
  token, and one enrolled device from reading another organizer's revocations. It constrains **nobody
  with write access to the access database**, who can enrol their own device — the same trust
  boundary every other table in this service sits inside. It is also not a confidentiality guarantee
  about the *contents*: a revocation list tells its holder which of an organizer's tickets have been
  voided, which is why it is organizer-scoped and device-authenticated rather than merely obscure.
- **The feed cannot help a scanner that never pulls.** ADR-025's "a gate that never syncs" caveat
  applies in the other direction too: a device that has never reached the network has no revocation
  state, and the ceiling is what turns that from a silent hole into a refusal.
