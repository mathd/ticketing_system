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

1. **Every public read endpoint declares a TTL tiered by data volatility.** Responses carry explicit `Cache-Control`/`s-maxage` (CDN-ready even though v1 is local) and are cacheable by construction: no session-varying content on public reads, buyer-specific data on separate endpoints. Indicative tiers — venue/seat-map geometry: hours; event lists & event detail: minutes; price display: ~1 min; remaining capacity/availability level: seconds; hold/order/scan state: **never cached**.
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

**The TTL declarations are a contract, not evidence of a cache.** The tiers are committed in each
service's OpenAPI document, which [ADR-009](./ADR-009-contract-first-apis.md) makes the authoritative
source and the gate diffs on every run — so they are a live, reviewed artifact, not incidental
headers. They are exactly the contract a future cache owner must honor. They are not proof that such
an owner is deployed, and emitting `Cache-Control` must never be read as one.

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
while its HTML response is `no-store`. So of the three cached reads, two propagate a positive tier to
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

**Ownership of the gap: [TKT-31](../product/prd-v1.md) — read-path caching & hot-event serving**, which
already owns the shared/gateway tier, the service-side in-memory structures, invalidation, the
incident kill-switch, staleness tests, and the on-sale read-load evidence this ADR's Consequences
anticipated. This amendment routes the gap there rather than leaving it implicit; it opens no new work
of its own.

## References

- [brief](../product/brief.md) · [PRD](../product/prd-v1.md) (TKT-31) · [ADR-002](./ADR-002-services-from-day-one.md)
- Amendment (TKT-128) evidence — [ADR-006](./ADR-006-astro-storefront-shell.md) (minutes-tier ownership),
  [ADR-009](./ADR-009-contract-first-apis.md) (the TTLs as contract);
  `web/storefront/src/lib/cache.ts` (`PageDataCache`), `web/storefront/src/lib/api.ts` (`pageRead`),
  `web/storefront/src/middleware.ts` (`EVENT_PAGE`), `gateway/cmd/gateway/main.go` (`apiProxy`),
  `web/backoffice/src/lib/api.ts` (`getVenues`, `listVenueSeatMaps`), `compose.yaml`
