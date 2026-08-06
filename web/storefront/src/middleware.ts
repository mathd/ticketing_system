// Storefront middleware — Astro wiring, and nothing else.
//
// Two jobs, in this order:
//   1. The gate (TKT-220): the proxy-aware origin check on every unsafe method,
//      then the session requirement for the account area.
//   2. Cache-Control per route class (ADR-004 / ADR-006).
//
// Both decisions live in src/lib (gate.ts, page-tier.ts) rather than here, and
// that is deliberate: `astro:middleware` is a virtual module that only exists
// inside an Astro build, so it does not resolve under vitest — a rule left in
// this file is a rule nothing can test. The gate's security property is an
// ORDERING ("refused before any credential is read"), which is a property of the
// composition, so the composition lives in gate.ts too.
//
// Any rule added here instead of in lib/ is a rule nothing can test.
//
// Route classes for the cache half:
//   /:locale/events, /:locale/events/:id, /:locale/festivals/:id
//     -> minutes tier, page-layer owned. The outgoing TTL is the REMAINING
//     freshness of the SSR page-data cache entry the page rendered from
//     (locals.pageData), so page + data caching can never stack beyond the tier.
//   everything else (the account area, healthz, redirects, errors) -> no-store.
import { defineMiddleware } from 'astro:middleware';

import { gateRequest } from './lib/gate';
import { pageCacheControl } from './lib/page-tier';
import { SESSION_COOKIE, lookupSession, type CustomerPrincipal } from './lib/session';

export const onRequest = defineMiddleware(async (context, next) => {
  const response = await gateRequest({
    request: context.request,
    pathname: context.url.pathname,
    sessionToken: context.cookies.get(SESSION_COOKIE)?.value ?? '',
    lookup: (token) => lookupSession(token),
    onAuthenticated: (principal) => {
      context.locals.customer = principal as CustomerPrincipal;
    },
    redirectToSignIn: (path) => context.redirect(path, 302),
    next,
  });

  // Applied to EVERY response the gate produces, including its refusals and its
  // redirect — not only rendered pages. A cached "302 to sign-in" would be served
  // to a signed-in buyer by any shared cache that stored it, and the refusals are
  // not cacheable either.
  //
  // The account area is never in page-tier's cached set, so it falls through to
  // no-store here; test/middleware-page-tier.test.ts pins that with page data
  // present, which is the only input that could ever produce a positive tier.
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
