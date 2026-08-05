// Cache-Control per route class (ADR-004 / ADR-006, via the real Astro 7
// API: src/middleware.ts + defineMiddleware — not the fictional src/fetch.ts).
//
// Route classes in this story:
//   /:locale/events, /:locale/events/:id, /:locale/festivals/:id
//     -> minutes tier, page-layer owned.
//     The outgoing TTL is the REMAINING freshness of the SSR page-data cache
//     entry the page rendered from (locals.pageData), so page + data caching
//     can never stack beyond the tier. X-Page-Data-Age exposes the data age
//     for the smoke suite's no-refetch assertion (AC3).
//   everything else (healthz, redirects, errors) -> no-store.
import { defineMiddleware } from 'astro:middleware';

import { pageCacheControl } from './lib/page-tier';

// The three cached catalog reads, and only those. TKT-208 added the festival
// route: its DATA was already cached for the minutes tier while its HTML fell
// through to no-store, which ADR-004's TKT-128 amendment recorded as a gap and
// asked a later ticket to close.
//
// This is coverage of an existing tier, not a new one. The TTL below stays the
// REMAINING freshness of the entry the page rendered from, so a festival page
// cannot add a second five-minute lifetime on top of catalog's own cache — which
// is what would reopen the ~600s stacking TKT-206 closed. Do not replace it with
// a literal.
//
// Deliberately NOT covered: the buyer ticket page (order state is ADR-004's
// never tier), healthz, redirects, and anything deeper.

export const onRequest = defineMiddleware(async (context, next) => {
  const response = await next();
  // The decision lives in lib/page-tier so it can be tested without Astro's
  // virtual modules; this stays a thin adapter over it (TKT-208).
  const { cacheControl, pageDataAge } = pageCacheControl(
    context.url.pathname,
    response.status,
    context.locals.pageData,
  );
  response.headers.set('Cache-Control', cacheControl);
  if (pageDataAge !== undefined) {
    // X-Page-Data-Age exposes the data age for the smoke suite's no-refetch
    // assertion (AC3).
    response.headers.set('X-Page-Data-Age', String(pageDataAge));
  }
  return response;
});
