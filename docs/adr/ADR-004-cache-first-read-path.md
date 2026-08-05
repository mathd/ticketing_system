# ADR-004: Cache-first read path with volatility-tiered TTLs

Date: 2026-07-12

## Status

Accepted

*Extended by [ADR-019](./ADR-019-catalog-read-path-scoping.md): this decision stands; it fixes what a
read costs when the cache **hits**. ADR-019 covers the **miss** path for catalog reads scoped to a
subset — the filter needs an index behind it, and proving so takes a plan assertion, not an
output assertion.*

**Amended by TKT-128 (2026-07-28)** — *coverage only*, under that run's owner-waived gates. The three
rules stay accepted and unweakened; the amendment records which of them are **implemented today** and
which are still only declared. See § Amendment (TKT-128).

**Amended by TKT-107 (2026-07-29)** — *rule 1's seat-map tier only*, under that run's owner-waived
gates. Splits the seat-map row of rule 1's tier table on publication status: a seat-map read earns
the hours tier only when its payload is entirely published. No other tier changes and the three
rules stay unweakened. See § Amendment (TKT-107).

## Context

High-contention on-sales are a core v1 requirement (brief). Correct atomic claims (ADR-002's inventory hot path) solve oversell, but an on-sale's load is overwhelmingly **reads** — event lists, event pages, price displays, remaining-capacity checks — and an uncached read path melts the database long before the write path is stressed. The owner directed (2026-07-12): endpoints should be cacheable wherever possible with TTLs matched to data volatility, hot events should be served from in-memory structures to shed database reads, and the web frontends should minimize API calls and refresh on TTL-appropriate cadences.

## Possible Solutions

- **Option 1 — Scale reads with the database (read replicas, no cache design):**
    - Pros: no cache-invalidation complexity.
    - Cons: every buyer poll still hits a database; replica lag gives worse staleness than a designed TTL, without the control. Doesn't meet the directive.
- **Option 2 — Cache as an afterthought (add headers when slow):**
    - Pros: no upfront cost.
    - Cons: endpoints designed personal/uncacheable (session-varying responses, chatty per-widget calls) can't be made cacheable later without API redesign — the same retrofit trap as NF525 (ADR-003).
- **Option 3 — Cache-first read path, volatility-tiered TTLs (chosen):**
    - Pros: read scale is a property of the API design; staleness is an explicit, per-data-class decision.
    - Cons: standing discipline on every endpoint; hot-event in-memory state adds an invalidation/refresh mechanism to own.

## Decision

We design the read path cache-first, on three rules:

1. **Every public read endpoint declares a TTL tiered by data volatility.** Responses carry explicit `Cache-Control`/`s-maxage` (CDN-ready even though v1 is local) and are cacheable by construction: no session-varying content on public reads, buyer-specific data on separate endpoints. Indicative tiers — venue geometry: hours; **published** seat-map geometry: hours, **draft-bearing, mixed or empty** seat-map responses: never cached (TKT-107 amendment); event lists & event detail: minutes; price display: ~1 min; remaining capacity/availability level: seconds; hold/order/scan state: **never cached**.
2. **Hot events are served from memory.** Services keep in-memory read structures (availability counters, event snapshots) for designated hot events — refreshed/invalidated from the write path, so buyer-facing reads during an on-sale do not touch the database. The write path (claims, ADR-002) is never served from cache.
3. **Frontends are call-frugal.** Storefront pages consume few, aggregated endpoints (one call per page view, not per widget); each response's TTL drives the client refresh cadence (e.g. availability re-polled every few seconds, event detail not re-fetched at all). No polling faster than the endpoint's TTL.

Correctness stays at checkout: a stale "available" display is acceptable and resolved by the atomic claim; a stale "sold out" must decay within its tier's TTL.

## Consequences

- **Positive:**
    - Read load during on-sales scales with cache/memory, not database; TTLs make staleness a reviewed, per-endpoint decision with a written rationale.
    - The API shape is CDN-compatible from day one.
- **Negative:**
    - Every read endpoint review now includes "what's the TTL and why" — a standing tax (added to quality gates).
    - In-memory hot-event structures introduce cache-invalidation bugs as a real failure class; needs staleness tests and a kill-switch to bypass caches during incidents.

## Amendment (2026-07-28, TKT-128) — declared coverage vs deployed coverage

This ADR read as though a cache tier were deployed. It is not, and the gap is wide enough that a
reader could reasonably plan against a cache that does not exist. **Nothing below weakens the
decision** — the three rules remain the target and remain binding on new endpoints. What is recorded
is the distance between them and the running system as of this date.

**The TTL declarations are a contract, not evidence of a cache — but only where they are declared.**
Where a tier is committed in a service's OpenAPI document, [ADR-009](./ADR-009-contract-first-apis.md)
makes that document authoritative and the gate diffs it on every run, so the TTL is a live, reviewed
artifact rather than an incidental header, and it is exactly the contract a future cache owner must
honor. It is still not proof that such an owner is deployed, and emitting `Cache-Control` must never
be read as one.

**Only catalog has made that commitment.** *(True as of TKT-128; superseded by TKT-110 — see the
callout at the end of this amendment. Inventory now declares its tier too.)* Catalog's contract declares `Cache-Control` on its public
reads (minutes tier for events/seasons/festivals, hours tier for venues and seat maps) and its handlers
emit it through named constants. **Inventory's public availability read is the exception that matters**:
`GET /slots/{id}/availability` emits `public, max-age=5, s-maxage=5` — rule 1's seconds tier — from the
handler alone, and `services/inventory/api/openapi.yaml` declares no `Cache-Control` on that response.
The header is real and undeclared, so the contract gate cannot review it or detect drift in it, and a
future cache owner reading the contract would not find the tier at all. That is this ADR's own defect
in the opposite direction — a real behaviour with no declaration, rather than a declaration with no
behaviour — and closing it is a source change, tracked as TKT-137. Commerce, payments and access
declare no tiers and emit none on their domain reads; their `no-store` default stands. (Four services
do set `public, max-age=300` on their `/openapi.yaml` **spec document** endpoint. That is a static-asset
TTL, not an ADR-004 data tier; do not read it as tier participation.)

**What honors a tier today — one cache, three reads, two of them end-to-end.**
The single implementation of rule 1's consumer side is the storefront's SSR page-data cache
(`web/storefront/src/lib/cache.ts`, `PageDataCache`), held as one module-level instance in the Astro
SSR process (`web/storefront/src/lib/api.ts`). It is process-local, URL-keyed, fetch-through, governed
by the upstream response's `max-age`, and it coalesces concurrent misses for the same URL through a
single upstream call — so an expiring hot-event page cannot stampede the catalog. Exactly three reads
pass through it (`pageRead`): the public event list, public event detail, and public festival detail.

One qualification, because the difference matters to anyone reasoning about total staleness:
`web/storefront/src/middleware.ts` derives the page's outgoing `Cache-Control` from the **remaining**
freshness of the entry it rendered from, so page-layer and data-layer staleness cannot stack — but its
route test (`EVENT_PAGE`) covers the two `events` routes only. The **festival** page's data is cached
while its HTML response is `no-store`.

> **Page-layer festival gap CLOSED by TKT-208 (2026-08-05).** `/:locale/festivals/:festivalId` now
> participates in [ADR-006](./ADR-006-astro-storefront-shell.md)'s minutes-tier page ownership
> alongside the two `events` routes, so all three cached reads propagate a positive tier to the page
> response and none stops at the data layer.
>
> **This is coverage, not a new cache and not a new TTL.** The page still performs its existing
> single aggregated `getPublicFestival` read, and the middleware publishes only the **remaining**
> freshness carried in `locals.pageData` — `max(0, maxAgeSeconds - ageSeconds)`. It therefore does not
> reset catalog's `Age`, does not add another five-minute lifetime, and does not reopen the ~300-second
> end-to-end bound [ADR-045](./ADR-045-catalog-public-read-cache.md) established. Replacing that
> expression with a literal would reopen it, which is why `page-tier.ts` says so and a test pins it.
>
> The route stays `no-store` for a non-200 response or when the render established no freshness.
> Buyer order and ticket routes stay outside this page class entirely: that state is rule 1's **never**
> tier ([ADR-002](./ADR-002-services-from-day-one.md), [ADR-012](./ADR-012-ticket-issuance-and-qr-credentials.md)),
> and a test asserts they cannot drift into it. So of the three cached reads, two propagate a positive tier to
the page response and one stops at the data layer. Ownership of the minutes tier is
[ADR-006](./ADR-006-astro-storefront-shell.md)'s caching-ownership rule, and this is its whole extent.

**What honors nothing.** The gateway is a bare `httputil.ReverseProxy` per route
(`gateway/cmd/gateway/main.go`, `apiProxy`) — it forwards responses and caches none of them. The back
office reads through plain `fetch` (`web/backoffice/src/lib/api.ts`) and discards the hours-tier
`Cache-Control` its venue and seat-map reads receive; that is the clearest instance of a declared tier
with no consumer. Service-to-service HTTP clients neither cache nor read TTLs. The Compose topology
(`compose.yaml`) contains no CDN, reverse-proxy cache, or shared cache of any kind. **The scanner is
not in this list as a gap:** it honors no cache, but its scan and reconcile calls are transactional
`POST`s, which rule 1 already places in the never-cached tier — correct behaviour, not missing coverage.

**Rule 2 is accepted and unimplemented.** No service keeps in-memory hot-event snapshots or
availability counters. Catalog's public reads and inventory's availability read both go to their
Postgres store and then set `Cache-Control` on the way out. Rule 2 is not obsolete, optional, or
superseded by the SSR cache — it is simply not built.

**Ownership — two gaps, two owners.** The **deployment** gap is
[TKT-31](../product/prd-v1.md)'s — read-path caching & hot-event serving, which already owns the
shared/gateway tier, the service-side in-memory structures (rule 2), invalidation, the incident
kill-switch, staleness tests, and the on-sale read-load evidence this ADR's Consequences anticipated.
The **contract** gap is TKT-137's: declaring inventory's seconds-tier availability header in its
OpenAPI document, which is a source change and so could not be made here. This amendment routes both
rather than leaving either implicit.

> **Contract gap CLOSED by TKT-110 (2026-07-29).** TKT-137 was absorbed into TKT-110 and the
> declaration shipped: `services/inventory/api/openapi.yaml` now declares `Cache-Control` on
> `getAvailability`'s `200` via `components.headers.AvailabilityCacheControl` — `required: true`,
> with `public, max-age=5, s-maxage=5` as its single allowed value — and the handler emits it
> through the named constant `CacheControlPublicAvailability`
> (`services/inventory/internal/api/server.go`). So **inventory is no longer the exception**: two
> services now commit a tier in their contract, and the sentence above stating that only catalog
> has made that commitment is true as of TKT-128 but superseded here. Because ADR-028's response
> validator fails closed on a declared header, the constant and the declaration can only move
> together — a mismatch is a 500, pinned stack-free by
> `TestAvailabilityCacheTierIsContractEnforced` and end-to-end by
> `smoke/inventory_contention_test.go`. **The deployment gap is untouched and remains TKT-31's:**
> declaring a tier still is not evidence of a cache, and no shared cache is deployed. Rule 2
> remains accepted and unimplemented.

## Amendment (2026-07-29, TKT-107) — the seat-map tier splits on publication status

Rule 1's tier table said *"venue/seat-map geometry: hours"* without qualification, and catalog's three
public seat-map reads emitted the hours tier unconditionally. That was written before publishing
existed. Once it shipped (TKT-103), the same tier covered two data classes with opposite volatility:
an immutable published version and a **mutable draft** an author is actively editing. This amendment
splits them. **Nothing below weakens the three rules** — it narrows one row of rule 1's indicative
table and records why.

**The rule, as shipped.** A seat-map response carries the hours tier
(`public, max-age=3600, s-maxage=3600`) **only when it is non-empty and every seat map in it is
`published`**; otherwise `no-store`. One function decides for all three reads
(`cacheControlForSeatMaps`, `services/catalog/internal/api/server.go`), applied by
`getPublicSeatMapGeometry`, `listVenueSeatMaps` and `listSeatMapVersions`.

- **A draft is mutable, so an hour of shared-cache lifetime makes an authoring write look lost.**
  That is the defect, and it is a *staleness* defect on an authoring surface, not a load problem.
- **A published version is immutable**, which is what makes the hours branch correct rather than
  merely inherited: by [ADR-029](./ADR-029-seat-identity-pinning-contract.md) an edit *inserts a new
  published version* and leaves its predecessor untouched, so a cached published payload cannot go
  stale by editing — only by being superseded, and the successor has a different id.
- **A list takes its least-cacheable member's tier.** One HTTP response carries one `Cache-Control`,
  so a single draft row makes the whole response `no-store`. Conservative and unavoidable.
- **An empty list fails closed.** Not because emptiness is volatile in itself, but because no
  published row is present to justify caching — and caching "this venue has no seat maps" for an hour
  would hide the venue's first map. The guard exists for `listVenueSeatMaps`, the only one of the
  three that can return zero rows; `listSeatMapVersions` returns 404 instead.
- **Any other status fails closed.** Migration `0009_seat_maps.sql` constrains the column to
  `draft | published | archived`; only the literal `published` earns the tier, so `archived` and any
  future status get `no-store` without another code change.

**`no-store` chosen over a short private tier.** These responses carry no validator — the handlers set
`Cache-Control` and nothing else, no `ETag` or `Last-Modified` — so `private, max-age=0` costs the same
full round trip and buys nothing, and any positive `public` TTL keeps the exact defect. The only
consumer today reads through plain uncached `fetch` (`web/backoffice/src/lib/api.ts`), so nothing loses
a cache it was using.

**Declared, not just emitted.** The TKT-128 amendment established that an emitted-but-undeclared header
is a defect in its own right, so the tier is committed in the contract:
`services/catalog/api/openapi.yaml` gains a `SeatMapCacheControl` response-header component whose
schema is an `enum` of exactly the two permitted values, referenced by all three 200 responses. This is
**enforced at runtime, not only by the gate**: catalog wraps every response in
[ADR-028](./ADR-028-response-drift-fail-closed.md)'s validator (`contract.ResponseValidator`), which
validates response *headers* and turns a third value into a 500 with the drifted payload withheld. A
future ticket that adds a seat-map tier without extending the enum gets 500s rather than a wrong
header — the correct fail-closed direction, and a real constraint on whoever comes next.

**Two limits, stated plainly, because the ticket that produced this amendment was framed as an exposure
fix and it is not one.**

1. **This is not access control.** `no-store` forbids *storing* a response; it does nothing about
   *retrieving* one. A reader who knows a draft map's UUID still gets the draft, exactly as before —
   these routes live under `/public/` and are unauthenticated. Naming the adversary, per
   [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s rule: this change closes the
   **shared-cache** vector (a CDN or proxy retaining and re-serving unpublished geometry to third
   parties) and closes nothing against a direct reader. Organizer-scoping the by-id reads is the other
   half of TKT-107 and remains **deferred pending admin auth**, which does not exist in this codebase.
2. **An all-published list still carries hour-long membership staleness.** The tier is decided from the
   rows a response *contains*; a seat map created a minute after the response was cached is invisible
   until the TTL expires. That is the same "authoring write looks lost" failure this amendment fixes
   for draft *content*, left open for list *membership*. It is not closed here: doing so would demote
   the hours tier for the only reads that use it, on a judgement no second model reviewed. A future
   cache deployment must invalidate the venue and family URLs on authoring writes, or the list tier
   must be demoted. Tracked as TKT-141.

**Still true, unchanged by this amendment:** no CDN or shared cache exists anywhere in the stack, so
this change has **no observable runtime effect today** beyond the emitted header. That is the point —
ADR-004 chose Option 3 precisely to avoid Option 2's retrofit trap, and a tier is cheap to correct
before a cache honors it and expensive after.

## References

- [brief](../product/brief.md) · [PRD](../product/prd-v1.md) (TKT-31) · [ADR-002](./ADR-002-services-from-day-one.md)
- Amendment (TKT-128) evidence — [ADR-006](./ADR-006-astro-storefront-shell.md) (minutes-tier ownership),
  [ADR-009](./ADR-009-contract-first-apis.md) (the TTLs as contract);
  `web/storefront/src/lib/cache.ts` (`PageDataCache`), `web/storefront/src/lib/api.ts` (`pageRead`),
  `web/storefront/src/middleware.ts` (`EVENT_PAGE`), `gateway/cmd/gateway/main.go` (`apiProxy`),
  `web/backoffice/src/lib/api.ts` (`getVenues`, `listVenueSeatMaps`), `compose.yaml`;
  `services/catalog/api/openapi.yaml` (the declared tiers) vs
  `services/inventory/internal/api/server.go` (`availability`) and `services/inventory/api/openapi.yaml`
  (`getAvailability`) — the emitted-but-undeclared seconds tier
- Amendment (TKT-107) evidence — `services/catalog/internal/api/server.go` (`cacheControlForSeatMaps`
  and its three call sites), `services/catalog/api/openapi.yaml`
  (`components/headers/SeatMapCacheControl`), `services/catalog/internal/store/migrations/0009_seat_maps.sql`
  (the `draft | published | archived` CHECK), `shared/go/contract/http.go` (the header-validating
  fail-closed wrap), `services/catalog/internal/api/seatmap_test.go`
  (`TestSeatMapReadCacheTierByStatus`); [ADR-029](./ADR-029-seat-identity-pinning-contract.md)
  (published-version immutability), [ADR-028](./ADR-028-response-drift-fail-closed.md),
  [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) (name the adversary)
