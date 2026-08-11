# ADR-055: The on-sale write path is rate limited per source, and that is not a queue

Date: 2026-08-10

## Status

Accepted

Discharges ai-review S7. Extends ADR-051's mechanism to a second surface; amends nothing in it.
Names two controls this repo does **not** have and states what each needs first.

## Context

The 2026-08-10 security review, finding S7:

> The only limiter in the platform guards commerce's customer-credential surface. `POST /holds`,
> `POST /reservations`, `POST /orders`, and `POST /scans` are unlimited. For a system whose domain
> includes high-contention on-sales, an unauthenticated caller scripting the gateway directly can
> churn holds at line rate (each reserving stock for `HOLD_TTL`=10m) — the canonical
> inventory-hoarding / denial-of-sale bot pattern.

The finding is correct and the attack is the one that matters for this domain. A bot that
reserves faster than buyers can does not need to complete a single purchase to take an
allocation off sale for ten minutes at a time.

The review also allowed that this may be a deliberate testbed gap and asked for an ADR if so.
It is not entirely deliberate, and it is not entirely closable with a token bucket. This ADR
does both halves: the bucket that is now installed, and the honest statement of what it leaves
open.

## Decision

### 1. A per-source limiter on `POST /reservations` and `POST /orders`

Commerce grows a third bucket, beside the per-subject and per-source buckets ADR-051 installed
for the customer-credential surface. It is a **separate budget**, not a shared one: a buyer
signing in and a buyer reserving are independent activities, and a shared bucket would let
on-sale traffic lock people out of signing in — the mistake ADR-051 §Subject scopes already had
to correct once between credential and recovery.

Both routes are wrapped as a `chi` group rather than annotated individually, for the reason
`registerRoutes` already documents: a limiter attached route-by-route is a list someone has to
remember to extend, and the next write added there would silently be the unlimited one.

The budget is sized for **aggregate** traffic, and the reason is structural rather than a
tuning choice. The storefront calls commerce server-side through the gateway, and the gateway
replaces `X-Forwarded-For` with its own peer — the storefront container. So every buyer using
the forms shares **one** source key. This budget is therefore a ceiling on the whole site's
checkout traffic, and what it really bounds is a caller scripting the gateway **directly**, who
does get their own key. That is exactly the caller S7 describes.

### 2. No limiter on inventory's `POST /holds` — it would throttle commerce, not the bot

The review names `POST /holds`. A per-source limiter there is not merely weak, it is actively
harmful, and the reason is worth stating so nobody "finishes the job" later:

**Commerce is that route's dominant caller.** `reserve` and the exchange path both POST to
inventory's public `/holds` over the compose network. A source-keyed bucket there would key
every real checkout in the system to the commerce container's address and start refusing
holds — a self-inflicted denial of sale, at the exact moment the limiter was added to prevent
one.

Inventory cannot separate the two callers today, because commerce presents no credential on
that route. Bounding `/holds` therefore requires giving commerce→inventory calls an identity
first — which is the same work item as S8's per-caller internal credentials. Until then, the
control for hold volume is the commerce-side limiter in §1, which sits in front of the only
path that reaches `/holds` with a buyer behind it.

### 3. No per-buyer hold-concurrency cap — there is no buyer to key it on

The review's second half asks for "bounded hold concurrency per buyer". That cannot be built as
stated, and the reason is a property of this system rather than an oversight:

**At reserve time there is no buyer.** Checkout is guest-first. `reserve` mints
`uuid.NewSHA1(NameSpaceOID, "buyer:"+reservationID)` — a synthetic identity derived from the
reservation, which is itself derived from the idempotency key the caller chose. So every
reservation has a *distinct* buyer id by construction, and a per-buyer cap would bound every
attacker to a cap of one reservation each, forever, which is to say it would bound nothing.

The signed-in case has a real identity (the customer assertion, ADR-049/TKT-221), but it only
arrives at `POST /orders` and is optional there by design. Capping only signed-in buyers would
put the cap on exactly the population that is not attacking.

What per-buyer concurrency actually needs first is a **session identity issued before the
reserve** — the thing a waiting room hands out. That is the same prerequisite as §4.

### 4. What this is NOT

Read `shared/go/ratelimit`'s package doc before quoting any of this as an on-sale control. A
token bucket:

- bounds **one scripted client against one replica**, and every distributed caller gets its own
  budget by definition;
- empties completely on restart;
- collapses to one key for all storefront traffic, per §1.

The real defence for a high-contention on-sale is a **waiting room**: a queue that admits a
bounded number of sessions, issues each an identity, and makes reservation concurrency a
property of that identity rather than of an address. This repo has not built one, and this ADR
does not pretend the bucket is a substitute. It raises the cost of the naive bot and leaves the
determined one to the queue.

`POST /scans` is deliberately absent from §1. The control there is a credential, not a rate —
see the scanner device enrolment work (ai-review S1); an authenticated gate that scans quickly
is a busy door, not an attack.

## Consequences

- A caller scripting the gateway directly gets a bounded number of reservations and checkouts
  per window, per address, and a `429` with `Retry-After` when it is spent. Declared on both
  operations in commerce's contract, so ADR-028's response validator holds the handlers to it.
- Storefront traffic shares one budget. If a real on-sale ever approaches it, the symptom is
  buyers seeing `429` on the reserve step — escalate by building §4, not by raising the number
  until the symptom stops.
- Two named gaps remain open and are now written down rather than implied: `/holds` (§2, blocked
  on per-caller internal identity) and per-buyer concurrency (§3, blocked on a pre-reserve
  session identity).
