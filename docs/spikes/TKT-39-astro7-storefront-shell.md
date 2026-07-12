# Spike report — TKT-39: Astro 7 storefront shell

**Date:** 2026-07-12 · **Type:** timeboxed throwaway spike · **Decides:** ADR-006 (Astro 7 shell vs React SPA)
**Outcome:** ✅ **ACCEPT — Option 1 (Astro 7 storefront shell, React islands, SPA checkout).** ADR-006 → Accepted.

> Acceptance means **the shell hypothesis is supported**, not that integration is proven. The prototype
> was a throwaway behind a mock API; §"Untested integration costs" lists what a real adoption still owes.

## What was built (throwaway, `spike/`, gitignored)

Three real processes, wired so the mock is a **separate HTTP origin behind a real gateway** — the explicit
guard against a false accept from a friendly in-process mock (plan-review's lead finding):

- **`spike/mock-api/`** — Node HTTP server (`:9101`) emitting ADR-004-tiered `Cache-Control`, rev/timestamp
  markers, integer-minor-unit prices, hold create/expire, and fault injection (`?delay`, `?fail`, changing ETag).
- **`spike/gateway/`** — an nginx config (mirrors the repo's storefront nginx). Captured through **real
  nginx 1.27** (`:9103`, Docker via `host.docker.internal`); a dependency-free Node proxy (`:9102`) is the
  no-Docker fallback.
- **`spike/astro7/`** — Astro 7.0.7 SSR (`@astrojs/node`), `src/middleware.ts` for per-route-class headers,
  `i18n` routing, a React availability island, a hold/countdown island, and a `/[locale]/checkout` route
  hosting a React SPA root.

Stack installed clean: astro 7.0.7 · @astrojs/react 6.0.1 · @astrojs/node 11.0.2 · vite 8.1.4 · react 19.2.7.

## Acceptance criteria — verdicts (evidence in `spike/EVIDENCE.md`)

| # | Criterion | Verdict |
|---|---|---|
| 1 | List + detail in Astro 7, page HTML minutes-tier, availability island seconds-tier, `Cache-Control` per route class | ✅ Pass — headers captured through real nginx; availability provably absent from HTML |
| 2 | Hold-countdown survives MPA nav into a React checkout surface; seams documented incl. what breaks | ✅ Pass — countdown continued 115s→111s across the nav; all 5 failure states verified; boundary documented honestly |
| 3 | FR/EN locale routing on localized content (TKT-36 shape) | ✅ Pass — localized content + locale-preserving links + checkout; invalid locale → 404 (noted) |
| 4 | Written caching-ownership rule making stale-page/fresh-API mismatch impossible by construction | ✅ Pass — see below; demonstrated structurally |
| 5 | v7 early-adopter tax judged tolerable | ✅ Tolerable — **zero** genuine Astro/architecture blockers; only ADR-naming + environment items |

Because 1–4 hold and 5 is tolerable, the decision rule in ADR-006 selects **Accept**.

## Caching-ownership rule (AC4) — resolves the TKT-31 overlap

The rule that makes a stale-page / fresh-API mismatch **impossible by construction**, per ADR-004 tier:

1. **The page layer owns everything at the minutes tier or slower** — event list HTML, event detail HTML,
   and **price display** (price is SSR-embedded into the page and therefore inherits the page's minutes
   TTL; it is treated as minutes-tier data, never seconds-tier). Cached HTML may contain only data whose
   tier is ≥ the HTML's own TTL.
2. **The API layer owns everything faster than the page tier** — availability (seconds) and any
   sub-minute-volatility value. These are **never** rendered into cacheable HTML; they exist only behind
   client-fetched endpoints called by React islands, on a poll cadence no faster than the endpoint TTL.
3. **Transactional state (holds, orders, scans) is `no-store` on both layers** — the checkout page and the
   hold/order endpoints alike.

**Why mismatch is impossible, not merely unlikely:** a stale page and a fresh API can only *disagree* about
a value if both carry it. Rule 2 forbids any faster-than-page value from appearing in the HTML, so the only
values in cached HTML are ones whose own tier permits that staleness. Availability — the classic
"page says 5 left, API says sold out" bug — is structurally absent from the HTML (verified: grepping the
served detail HTML for an availability number returns nothing). The island is the *single writer* of
availability to the DOM.

**The standing rule for every storefront story (ADR-006 consequence made concrete):** each read a story
introduces must declare its tier and therefore its owning layer. Slower-than-page → may be SSR-embedded.
Faster-than-page → must be an island-fetched endpoint. This is the framework-enforced form of ADR-004.

## Early-adopter tax log (AC5)

Schema: issue · workaround · elapsed · upstream status · production consequence · tolerable. Full table in
`spike/astro7/NOTES.md`. Summary: **no genuine Astro 7 / architecture blocker.** The recorded items split into:

- **ADR-naming (not a defect):** ADR-006's `src/fetch.ts` / `Astro.cache` don't exist in real `astro@7.0.7`;
  the real APIs are `src/middleware.ts` + `defineMiddleware` + `Response.headers`, and the `i18n.routing`
  config. Consequence: ADR-006 must describe shipped APIs — done in this pass.
- **Environment (not Astro):** Docker daemon/host-nginx absent in one shell (Node-proxy fallback + real
  nginx via Docker on another); Astro telemetry wanted a writable config home (`ASTRO_TELEMETRY_DISABLED=1`);
  the Codex sandbox blocks TCP listeners (runtime validation done here instead).

Separating framework blockers from environment/setup failures was an explicit guard against a **false
reject** — none of the friction above is a reason to reject Astro.

## Untested integration costs (what Accept does NOT prove)

The prototype ran behind a mock. A real adoption (US-002 / TKT-26 onward) still owes, and these are the
honest risks carried forward:

- **Real gateway wiring** — the storefront behind the actual Go gateway (ADR-002), not a demo nginx; cross-origin/cookie behavior, header passthrough, `s-maxage` honored by the real CDN/edge (CDN cache adapters are still experimental per ADR-006).
- **Real catalog/inventory contracts** — the Go catalog service has **no domain routes yet** (verified: it's a skeleton). The event/price/availability endpoints were mocked; the generated API contract and its error/latency behavior are unproven against Astro's SSR fetch path.
- **Real hold identity & security** — the opaque hold id + `sessionStorage` seam is sound in shape, but real hold tokens, auth, CSRF, and multi-tab/queue (TKT-20) semantics are untested.
- **Seat selection (TKT-35)** — a far richer buy-half state than a countdown; the MPA seam discipline must extend to it or a shared client store is needed.
- **SSR deploy mode** — `@astrojs/node` standalone here; the production runtime/adapter and its cold-start/scaling under on-sale load are unproven.
- **Second paradigm cost** — the ongoing maintenance tax of Astro-storefront + React-SPA-elsewhere (ADR-006's standing negative) is a real, unmeasured cost.

## Recommendation

Flip ADR-006 to **Accepted**, unblock TKT-26, and require each storefront story to state its caching-layer
ownership per the rule above. Treat the "Untested integration costs" as the risk register for the first
real storefront slice — the shell is proven; the integration is the next thing to earn.
