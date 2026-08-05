// Which storefront routes participate in ADR-006's minutes-tier page ownership,
// and what TTL they publish (TKT-208).
//
// Extracted from middleware.ts so the decision is testable: `astro:middleware`
// is a virtual module that only exists inside an Astro build, so a test of the
// middleware file itself cannot import it. The middleware is now a thin adapter
// over this function, which is where the actual decision lives.

/** Freshness of the SSR page-data entry the page rendered from. */
export interface PageDataFreshness {
  ageSeconds: number;
  maxAgeSeconds: number;
}

// The three cached catalog reads, and only those. TKT-208 added the festival
// route: its DATA was already cached for the minutes tier while its HTML fell
// through to no-store, which ADR-004's TKT-128 amendment recorded as a gap and
// asked a later ticket to close.
//
// Deliberately NOT covered: the buyer ticket page — order and ticket state is
// ADR-004's never tier (ADR-002/ADR-012) — plus healthz, redirects, collection
// paths and anything deeper.
const CACHED_PAGE = /^\/[a-z]{2}\/(events(\/[^/]+)?|festivals\/[^/]+)\/?$/;

/**
 * pageCacheControl decides a page response's Cache-Control.
 *
 * The positive tier is the REMAINING freshness of the entry the page rendered
 * from, never a literal. That is what stops the page layer adding a second
 * five-minute lifetime on top of catalog's own cache — the ~600-second stacking
 * TKT-206 closed. Anything else is no-store: an unsuccessful render, or one that
 * established no freshness, has nothing to promise.
 */
export function pageCacheControl(
  pathname: string,
  status: number,
  pageData: PageDataFreshness | undefined,
): { cacheControl: string; pageDataAge?: number } {
  if (status !== 200 || !pageData || !CACHED_PAGE.test(pathname)) {
    return { cacheControl: 'no-store' };
  }
  const remaining = Math.max(0, pageData.maxAgeSeconds - pageData.ageSeconds);
  return {
    cacheControl: `public, max-age=${remaining}, s-maxage=${remaining}`,
    pageDataAge: pageData.ageSeconds,
  };
}
