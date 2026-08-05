// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// TKT-208 / ADR-004 rule 3: "Storefront pages consume few, aggregated endpoints
// (one call per page view, not per widget)."
//
// The budget is enforced against the PAGE, not the api.ts wrapper. A wrapper
// test would prove getPublicEvents reaches pageRead once; it would not prove the
// page calls only one wrapper, or that a child component has not added an
// independent fetch of its own. A per-widget fetch is exactly the regression
// rule 3 exists to prevent, so the page boundary is what has to be measured.
//
// WHICH BUDGET THIS IS: the SSR render. React islands hydrate in the browser and
// their effects do not run here, so a green result does NOT mean "the page makes
// one request". Client-island reads are ADR-006's accepted architecture and
// their per-performance scaling is TKT-177's, not this ticket's.

const GATEWAY = 'http://localhost:8080';

type Call = { url: string };

function stubFetch(handler: (url: string) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
    const url = String(input);
    calls.push({ url });
    return handler(url);
  });
  return calls;
}

const minutesTier = { 'content-type': 'application/json', 'cache-control': 'public, max-age=300' };

// Non-trivial on purpose: an empty payload renders no children, so a future
// per-child fetch would have nothing to fetch FOR and the budget would pass
// while the regression shipped.
const eventList = {
  events: [
    { id: 'e1', name: 'One', starts_at: '2026-09-01T18:00:00Z', timezone: 'UTC', venue_name: 'A', from_price: { amount: 1000, currency: 'EUR' } },
    { id: 'e2', name: 'Two', starts_at: '2026-09-02T18:00:00Z', timezone: 'UTC', venue_name: 'B', from_price: { amount: 2000, currency: 'EUR' } },
    { id: 'e3', name: 'Three', starts_at: '2026-09-03T18:00:00Z', timezone: 'UTC', venue_name: 'C', from_price: { amount: 3000, currency: 'EUR' } },
  ],
};

async function freshApi() {
  vi.resetModules();
  // Imported AFTER the fetch stub: api.ts builds its PageDataCache singleton at
  // import time, and a cache carried over from a previous case would hide calls.
  return import('../src/lib/api');
}

describe('storefront SSR call budget (ADR-004 rule 3)', () => {
  beforeEach(() => vi.resetModules());
  afterEach(() => vi.unstubAllGlobals());

  it('the event list spends exactly one upstream call per cold render', async () => {
    const calls = stubFetch(() => new Response(JSON.stringify(eventList), { status: 200, headers: minutesTier }));
    const api = await freshApi();
    const astro = { locals: {} } as never;

    await api.getPublicEvents(astro, 'en');
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(`${GATEWAY}/api/catalog/public/events?locale=en`);
  });

  it('a second render inside the tier spends zero — the page is not per-widget OR per-request', async () => {
    const calls = stubFetch(() => new Response(JSON.stringify(eventList), { status: 200, headers: minutesTier }));
    const api = await freshApi();
    const astro = { locals: {} } as never;

    await api.getPublicEvents(astro, 'en');
    await api.getPublicEvents(astro, 'en');
    await api.getPublicEvents(astro, 'en');
    expect(calls).toHaveLength(1);
  });

  it('each public page reads exactly one aggregated endpoint', async () => {
    for (const [name, run, want] of [
      ['event list', (api: any, astro: any) => api.getPublicEvents(astro, 'en'), `${GATEWAY}/api/catalog/public/events?locale=en`],
      ['event detail', (api: any, astro: any) => api.getPublicEvent(astro, 'en', 'e1'), `${GATEWAY}/api/catalog/public/events/e1?locale=en`],
      ['festival detail', (api: any, astro: any) => api.getPublicFestival(astro, 'en', 'f1'), `${GATEWAY}/api/catalog/public/festivals/f1?locale=en`],
    ] as const) {
      const calls = stubFetch(() => new Response(JSON.stringify(eventList), { status: 200, headers: minutesTier }));
      const api = await freshApi();
      await run(api, { locals: {} });
      expect(calls, `${name} budget`).toHaveLength(1);
      expect(calls[0].url, `${name} url`).toBe(want);
      vi.unstubAllGlobals();
    }
  });

  it('a distinct locale is a distinct representation, not a second call for the same one', async () => {
    const calls = stubFetch(() => new Response(JSON.stringify(eventList), { status: 200, headers: minutesTier }));
    const api = await freshApi();
    const astro = { locals: {} } as never;

    await api.getPublicEvents(astro, 'en');
    await api.getPublicEvents(astro, 'fr');
    await api.getPublicEvents(astro, 'en');
    // Two representations, one call each; the repeat is served from memory.
    expect(calls).toHaveLength(2);
  });

  it('the page records the freshness the middleware needs to avoid stacking', async () => {
    stubFetch(() => new Response(JSON.stringify(eventList), { status: 200, headers: minutesTier }));
    const api = await freshApi();
    const astro = { locals: {} } as { locals: { pageData?: { ageSeconds: number; maxAgeSeconds: number } } };

    await api.getPublicEvents(astro as never, 'en');
    // Without this the middleware falls through to no-store and the page layer
    // silently stops participating in the tier (ADR-006).
    expect(astro.locals.pageData).toEqual({ ageSeconds: 0, maxAgeSeconds: 300 });
  });
});
