// @vitest-environment node
import { describe, expect, it } from 'vitest';

import { pageCacheControl } from '../src/lib/page-tier';

// TKT-208: which routes participate in ADR-006's minutes-tier page ownership.
//
// The festival route was excluded before this ticket: its DATA was cached for
// the minutes tier while its HTML was no-store — the gap ADR-004's TKT-128
// amendment recorded. Closing it is coverage of an existing tier, and the TTL
// must stay the REMAINING freshness, never a literal, or the page layer adds a
// second five-minute lifetime on top of catalog's cache and reopens the ~600s
// stacking TKT-206 closed.

function run(pathname: string, pageData?: { ageSeconds: number; maxAgeSeconds: number }, status = 200) {
  return pageCacheControl(pathname, status, pageData);
}

describe('page-layer cache tier', () => {
  it('covers all three cached catalog routes, festivals included', () => {
    for (const path of ['/en/events', '/en/events/e1', '/en/festivals/f1', '/fr/festivals/f1']) {
      const res = run(path, { ageSeconds: 60, maxAgeSeconds: 300 });
      expect(res.cacheControl, path).toBe('public, max-age=240, s-maxage=240');
      expect(res.pageDataAge, path).toBe(60);
    }
  });

  it('publishes REMAINING freshness, not the full tier', () => {
    // The assertion that keeps TKT-206's ~300s chain intact: a page rendered
    // from a nearly-expired entry must not restart the clock.
    expect(run('/en/festivals/f1', { ageSeconds: 299, maxAgeSeconds: 300 }).cacheControl).toBe('public, max-age=1, s-maxage=1');
  });

  it('never publishes a negative TTL', () => {
    expect(run('/en/festivals/f1', { ageSeconds: 400, maxAgeSeconds: 300 }).cacheControl).toBe('public, max-age=0, s-maxage=0');
  });

  it('leaves capability-bearing and operational routes no-store', () => {
    // Order and ticket state is ADR-004's never tier (ADR-002/ADR-012). A
    // regex that grew to cover them would be a real exposure, not a nicety.
    for (const path of ['/en/tickets/ORDER-1', '/en', '/en/events/e1/extra', '/healthz', '/en/festivals']) {
      expect(run(path, { ageSeconds: 0, maxAgeSeconds: 300 }).cacheControl, path).toBe('no-store');
    }
  });

  it('is no-store when the render did not establish freshness, or did not succeed', () => {
    expect(run('/en/festivals/f1', undefined).cacheControl).toBe('no-store');
    expect(run('/en/festivals/f1', { ageSeconds: 0, maxAgeSeconds: 300 }, 503).cacheControl).toBe('no-store');
  });

  // TKT-220. Account HTML is per-customer and can never be cached.
  //
  // These are asserted with page data present AND status 200 on purpose: that is
  // the ONLY input combination that can produce a positive tier, so it is the
  // only one where a widened CACHED_PAGE would be visible. Asserting no-store on
  // a 404 or on a render that established no freshness proves nothing — those are
  // no-store for a different reason entirely.
  //
  // It holds today because the account paths do not match CACHED_PAGE. That regex
  // is edited by other tickets (TKT-208 widened it once, TKT-209 is open against
  // the same area), which is why this is pinned rather than left to inspection.
  it('never caches an account page, even with fresh page data behind a 200', () => {
    for (const path of [
      '/en/account',
      '/fr/account',
      '/en/account/sign-in',
      '/fr/account/register',
      '/en/account/sign-out',
    ]) {
      const res = run(path, { ageSeconds: 0, maxAgeSeconds: 300 }, 200);
      expect(res.cacheControl, path).toBe('no-store');
      expect(res.pageDataAge, path).toBeUndefined();
    }
  });
});
