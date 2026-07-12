# ADR-004: Cache-first read path with volatility-tiered TTLs

Date: 2026-07-12

## Status

Accepted

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

## References

- [brief](../product/brief.md) · [PRD](../product/prd-v1.md) (TKT-31) · [ADR-002](./ADR-002-services-from-day-one.md)
