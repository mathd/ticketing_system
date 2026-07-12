// Cache-Control per route class (ADR-004 / ADR-006, via the real Astro 7
// API: src/middleware.ts + defineMiddleware — not the fictional src/fetch.ts).
//
// Route classes in this story:
//   /:locale/events, /:locale/events/:id  -> minutes tier, page-layer owned.
//     The outgoing TTL is the REMAINING freshness of the SSR page-data cache
//     entry the page rendered from (locals.pageData), so page + data caching
//     can never stack beyond the tier. X-Page-Data-Age exposes the data age
//     for the smoke suite's no-refetch assertion (AC3).
//   everything else (healthz, redirects, errors) -> no-store.
import { defineMiddleware } from 'astro:middleware';

const EVENT_PAGE = /^\/[a-z]{2}\/events(\/[^/]+)?\/?$/;

export const onRequest = defineMiddleware(async (context, next) => {
  const response = await next();
  const { pageData } = context.locals;
  if (response.status === 200 && EVENT_PAGE.test(context.url.pathname) && pageData) {
    const remaining = Math.max(0, pageData.maxAgeSeconds - pageData.ageSeconds);
    response.headers.set('Cache-Control', `public, max-age=${remaining}, s-maxage=${remaining}`);
    response.headers.set('X-Page-Data-Age', String(pageData.ageSeconds));
  } else {
    response.headers.set('Cache-Control', 'no-store');
  }
  return response;
});
