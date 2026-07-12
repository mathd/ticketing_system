# ADR-006: Astro 7 as the storefront shell (React islands + SPA checkout)

Date: 2026-07-12

## Status

**Accepted** (2026-07-12, by spike TKT-39 — see `docs/spikes/TKT-39-astro7-storefront-shell.md`). This **amends ADR-001** (which stands: React remains the UI library; this decision only changes the storefront's shell framework). Back office and scanner are unaffected — they stay React SPAs.

**API-name correction (from the spike):** this ADR was drafted against an announcement naming `src/fetch.ts` (per-route request pipeline) and `Astro.cache` (route caching). Those names do **not** exist in the shipped `astro@7.0.7`. The real equivalents, used by the spike and required going forward, are **`src/middleware.ts`** (`defineMiddleware` / `onRequest`, mutating the returned `Response`'s headers) for per-route-class `Cache-Control`, SSR via **`@astrojs/node`** for cacheable pages, and the real **`i18n.routing`** config for locale routing. Read every `src/fetch.ts` / `Astro.cache` reference below as those real APIs.

## Context

The storefront has two halves with opposite shapes. The **browse half** (event lists, event/festival pages) is content-heavy, SEO-relevant, read-mostly, and takes the brunt of on-sale read spikes — exactly what ADR-004's cache-first read path serves. The **buy half** (queue → hold countdown → seat picking → checkout) is a stateful app spanning pages. The owner has also made SEO, GEO and agent-readability a must (PRD «Discovery & agents», TKT-40) — server-rendered, structured-data-rich HTML serves all three, which weighs in Astro's favor for the browse half. Astro 7 (released 2026-06-22) matches that half unusually well: server-rendered cacheable pages (SSR via `@astrojs/node`, whose `Cache-Control` a CDN/reverse proxy caches — verified in the spike) with islands of interactivity, a **`src/middleware.ts` request-pipeline entrypoint** (`defineMiddleware`/`onRequest`) for per-route-class `Cache-Control` policy, first-class `i18n.routing` locale routing (TKT-36), and React components as islands (TKT-35 seat picker stays React). *(The drafting names `Astro.cache` and `src/fetch.ts` do not exist in shipped `astro@7.0.7`; the real APIs just named are used throughout — see the Status correction.)* It is also three weeks old: stricter Rust compiler, Vite 8/Rolldown bundler swap, Sätteri Markdown pipeline — a simultaneous early-adopter tax on several layers — and it adds a second frontend paradigm for a solo/AI team, against the same integration-tax logic that produced ADR-002's coarse service cut.

The open question the spike must answer: do the browse-half gains survive contact with the buy-half seams?

## Possible Solutions

- **Option 1 — Astro 7 storefront shell, React islands, SPA checkout island/subtree:**
    - Pros: ADR-004 made into a framework (page-layer TTLs per route class); minimal JS on the pages that take on-sale read spikes; built-in FR/EN locale routing; agent-friendly dev tooling.
    - Cons: three-week-old major release (compiler strictness, Rolldown ecosystem lag, Sätteri plugin porting); MPA state seams across the hold-countdown/checkout hand-off; a second frontend paradigm to maintain; CDN cache adapters still experimental; overlaps TKT-31 — needs an explicit page-layer vs API-layer caching ownership rule.
- **Option 2 — React SPA storefront (ADR-001 as written):**
    - Pros: one paradigm across all three frontends; the buy half is in its natural habitat; no early-adopter tax.
    - Cons: browse pages ship an SPA runtime to serve mostly-static content; ADR-004 caching lives entirely in the API/CDN layer; SEO/SSR needs its own machinery anyway if added later.
- **Option 3 — Astro for marketing/SEO pages only, SPA for everything transactional:**
    - Pros: smallest Astro surface.
    - Cons: event detail pages — the highest-traffic cacheable pages — end up on the SPA side, forfeiting the main benefit.

## Decision

**Option 1 is chosen — Astro 7 storefront shell, React islands, SPA checkout.** The timeboxed spike
(TKT-39) evidenced all five acceptance criteria below: page HTML at the minutes tier with availability as a
seconds-tier React island (headers captured through real nginx); hold-countdown state surviving MPA
navigation into a React checkout SPA with every failure state handled and the seam boundary documented;
FR/EN localized routing with locale-preserving links; a caching-ownership rule that makes stale-page/
fresh-API mismatch impossible by construction (availability is structurally absent from cached HTML); and a
v7 early-adopter tax with **zero** genuine Astro/architecture blockers. Full report + evidence:
`docs/spikes/TKT-39-astro7-storefront-shell.md`, `spike/EVIDENCE.md`.

**Scope of this acceptance:** the *shell hypothesis* is proven, **not** integration. The prototype ran
behind a mock API (the Go catalog service has no domain routes yet). Real gateway wiring, generated API
contracts, hold identity/security, seat-selection state (TKT-35), the SSR deploy adapter under on-sale
load, and CDN `s-maxage` honoring remain unproven — carried as the risk register for the first real
storefront slice (spike report, "Untested integration costs").

Acceptance criteria (all met — evidenced by the spike):

1. Event list + event detail render in Astro 7 with correct ADR-004 tiers — page HTML cached at the minutes tier, availability as a React island polling the seconds tier — with `Cache-Control` set per route class via `src/middleware.ts`. *Evidenced: headers captured through real nginx, and actual HTML cache reuse shown (`X-Cache-Status: MISS`→`HIT`), not merely header emission.*
2. The hold-countdown state survives MPA navigation into a React checkout surface without fragile seams (documented honestly, including what breaks). *Evidenced: countdown continued across the nav; all failure states rendered; the checkout re-validates the hold server-side rather than trusting the client deadline.*
3. FR/EN locale routing works for localized event content (TKT-36 shape).
4. A written **caching ownership rule** resolving the TKT-31 overlap: which layer (page vs API) owns each ADR-004 tier, such that stale-page/fresh-API mismatches are impossible by construction. *The rule requires **single-source discipline**: any value embedded in cacheable HTML must have no independently-cached API copy (the spike found and fixed a price double-source). Availability is safe by structural absence from HTML; embedded values like price are safe by being fetched once, page-side, with no separately-cached endpoint.*
5. The v7 early-adopter tax observed during the spike (compiler strictness, plugin gaps, bugs) is judged tolerable for this testbed.

If any of 1–4 fails, Option 2 stands and this ADR is marked Rejected with the evidence.

## Consequences

- **Positive (if accepted):** the on-sale read path serves mostly cached HTML + one small island; ADR-004 discipline gets framework enforcement; storefront i18n comes largely free.
- **Negative (if accepted):** two frontend paradigms (Astro storefront, React SPAs elsewhere); v7.0.x churn absorbed early; every storefront story must state which caching layer owns its reads.

## References

- [ADR-001](./ADR-001-go-typescript-stack.md) · [ADR-004](./ADR-004-cache-first-read-path.md) · TKT-39 (spike) · TKT-31, TKT-35, TKT-36
- [Astro 7.0 announcement](https://astro.build/blog/astro-7/) · [Upgrade to Astro v7](https://docs.astro.build/en/guides/upgrade-to/v7/)
- Real APIs used (verified `astro@7.0.7`): [`src/middleware.ts` / `defineMiddleware`](https://docs.astro.build/en/guides/middleware/) · [`i18n` routing](https://docs.astro.build/en/guides/internationalization/) · [`@astrojs/node` SSR](https://docs.astro.build/en/guides/integrations-guide/node/). The `Astro.cache` / `src/fetch.ts` names in earlier drafts are not shipped APIs.
