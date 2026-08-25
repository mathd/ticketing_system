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

// Any fixed instant. Its value is irrelevant; that it never advances is the point.
const FROZEN_NOW_MS = 1_780_000_000_000;
// Relative ms since process start, not epoch — the monotonic clock's units.
const FROZEN_MONOTONIC_MS = 5_000;

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
const perf = (id: string, amount: number, venue: string) => ({
  id,
  starts_at: '2026-09-01T18:00:00Z',
  timezone: 'UTC',
  // Distinct venue per performance: it is the marker that proves each repeated
  // child was actually emitted, on every page that renders performances.
  venue_name: venue,
  venue: { id: `v-${id}`, name: venue },
  from_price: { amount, currency: 'EUR' },
  ticket_types: [{ id: `t-${id}`, name: 'GA', price: { amount, currency: 'EUR' } }],
});

const performances = [perf('p1', 1000, 'Alpha'), perf('p2', 2000, 'Bravo'), perf('p3', 3000, 'Charlie')];

// One payload serving every page shape. Non-trivial on purpose: an empty payload
// renders no children, so a future per-child fetch would have nothing to fetch
// FOR and the budget would pass while the regression shipped.
const payload = {
  // Event DETAIL shape.
  id: 'e1',
  organizer_id: 'org-1',
  name: 'One',
  description: 'desc',
  performances,
  series: [{ id: 's1', name: 'Series', performance_ids: ['p1'] }],
  // Festival DETAIL shape.
  festival: { id: 'f1', name: 'Fest', shared_capacity: 100 },
  days: performances,
  // Event LIST shape — three events, each with performances, so a per-child
  // fetch would have children to fetch for.
  events: [
    { id: 'e1', organizer_id: 'org-1', name: 'One', description: '', series: [], performances },
    { id: 'e2', organizer_id: 'org-1', name: 'Two', description: '', series: [], performances },
    { id: 'e3', organizer_id: 'org-1', name: 'Three', description: '', series: [], performances },
  ],
};


async function renderPage(path: string, params: Record<string, string>) {
  // The REAL page module, rendered through Astro's container. The first version
  // of this test called the api.ts wrappers directly and asserted their call
  // counts — which proves a wrapper reaches pageRead once and proves nothing
  // about the page. An SSR fetch added to page frontmatter or to a child .astro
  // component would never have run, and the budget would have stayed green while
  // the regression shipped. Found in ai-review, having been explicitly warned
  // against in this ticket's own plan-review.
  const { experimental_AstroContainer } = await import('astro/container');
  const { loadRenderers } = await import('astro:container');
  const { getContainerRenderer } = await import('@astrojs/react');
  // The React renderer is required, not incidental: the event detail page
  // instantiates a client island (HoldPicker), and a page whose islands cannot
  // be rendered is a page this test would only half measure — precisely the
  // half where a stray fetch would hide.
  const renderers = await loadRenderers([getContainerRenderer()]);
  const container = await experimental_AstroContainer.create({ renderers });
  const mod = await import(/* @vite-ignore */ path);
  return container.renderToResponse(mod.default, { params, routeType: 'page' });
}

describe('storefront SSR call budget (ADR-004 rule 3)', () => {
  // The clock is FROZEN for every case in this file, and the order of these two
  // lines is load-bearing (TKT-218).
  //
  // PageDataCache decides freshness from a MONOTONIC clock since TKT-212, so
  // performance.now() is the reading that has to be frozen here; freezing only
  // Date.now would leave this file green while freezing nothing the cache reads,
  // which is the shape TKT-218's own first attempt hit. Date.now stays frozen
  // too: it is cheap, and it keeps anything else in the render path still.
  //
  // An entry serves for max-age seconds of REAL time. A busy machine that loses
  // more than max-age between two reads of one URL therefore makes the cache
  // miss and re-fetch,
  // and the budget assertion counts that as a second upstream call — a page-call
  // violation reported for a machine-load reason. That is the fail-open this
  // ticket closes: the assertion could not tell "the page made a second call"
  // from "the process was starved". Reproduced before fixing: two reads of one
  // URL spend 1 call on a still clock and 2 across a 301s stall.
  //
  // Freezing does NOT make the budget inert. A genuine second fetch reads a
  // different URL (or a URL the cache cannot serve) and still counts — verified
  // in both directions, not assumed.
  //
  // WHY THE SPY MUST COME FIRST: cache.ts takes the clock as a DEFAULT PARAMETER
  // (`now: () => number = monotonicNow`) and stores the resolved reference, so
  // the singleton in api.ts binds whatever that resolves to when the module is
  // first imported. renderPage imports it dynamically, so a spy installed here
  // reaches it — but a spy installed after the first import silently does
  // nothing, and the symptom is a test that PASSES while proving nothing. The
  // first attempt at this fix failed exactly that way.
  beforeEach(() => {
    vi.resetModules();
    vi.spyOn(performance, 'now').mockReturnValue(FROZEN_MONOTONIC_MS);
    vi.spyOn(Date, 'now').mockReturnValue(FROZEN_NOW_MS);
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it.each([
    // Each page renders a different repeated child, so the marker is per page.
    // The point is the same everywhere: prove the tree was actually emitted,
    // three times over, not that a number came back.
    ['event list', '../src/pages/[locale]/events/index.astro', { locale: 'en' }, ['One', 'Two', 'Three']],
    ['event detail', '../src/pages/[locale]/events/[eventId].astro', { locale: 'en', eventId: 'e1' }, ['Alpha', 'Bravo', 'Charlie']],
    ['festival detail', '../src/pages/[locale]/festivals/[festivalId].astro', { locale: 'en', festivalId: 'f1' }, ['Alpha', 'Bravo', 'Charlie']],
  ])('%s renders on exactly one upstream call', async (_name, page, params, children) => {
    const calls = stubFetch(() => new Response(JSON.stringify(payload), { status: 200, headers: minutesTier }));
    const res = await renderPage(page, params as Record<string, string>);
    const html = await res.text();

    // The render must have SUCCEEDED and reached its children. Without these two
    // the count alone is a weak oracle: a page that read once and then
    // short-circuited to a 503 or a 404 spends exactly one call too, and would
    // have passed while rendering nothing (ai-review).
    expect(res.status, 'the page must render, not short-circuit').toBe(200);
    for (const child of children as string[]) {
      expect(html.includes(child), `repeated child ${child} must be rendered`).toBe(true);
    }

    // One call for the whole render — frontmatter AND every child component.
    expect(calls.map((c) => c.url)).toHaveLength(1);
    expect(calls[0].url.startsWith(`${GATEWAY}/api/catalog/`)).toBe(true);
    // 30s, not the 5s default (TKT-218, found while gating TKT-216). These cases
    // render real .astro modules through Astro's vite plugin, and that transform
    // alone takes ~10s on a busy machine — so the default turned load into a
    // failure of the BUDGET, which is not what this test is about.
    //
    // The timeout addressed the two cases that TIMED OUT. The third did not: it
    // counted TWO upstream calls where the budget allows one, so the assertion
    // could still fail OPEN. TKT-218 answered which it was — the second call is
    // GENUINE, not a stub artefact: PageDataCache expires entries against real
    // elapsed time, so a process starved for longer than max-age really does
    // re-fetch. (That clock is monotonic since TKT-212 rather than wall — which
    // changes nothing here: both advance under load, which is the starvation
    // this describes.)
    // The frozen clock in beforeEach is what closes it; see the comment there for
    // why the spy must precede the page import.
  }, 30_000);

  it('a repeat render inside the tier spends zero — not per-request either', async () => {
    const calls = stubFetch(() => new Response(JSON.stringify(payload), { status: 200, headers: minutesTier }));
    await renderPage('../src/pages/[locale]/events/index.astro', { locale: 'en' });
    await renderPage('../src/pages/[locale]/events/index.astro', { locale: 'en' });
    await renderPage('../src/pages/[locale]/events/index.astro', { locale: 'en' });
    expect(calls).toHaveLength(1);
  });

  it('the locale route reads no service at all', async () => {
    const calls = stubFetch(() => new Response('{}', { status: 200, headers: minutesTier }));
    await renderPage('../src/pages/[locale]/index.astro', { locale: 'en' });
    expect(calls).toHaveLength(0);
  });
});

describe('the page-data freshness the middleware depends on', () => {
  beforeEach(() => vi.resetModules());
  afterEach(() => vi.unstubAllGlobals());

  it('is recorded by the read, so the page layer can publish REMAINING freshness', async () => {
    stubFetch(() => new Response(JSON.stringify(payload), { status: 200, headers: minutesTier }));
    vi.resetModules();
    const api = await import('../src/lib/api');
    const astro = { locals: {} } as { locals: { pageData?: { ageSeconds: number; maxAgeSeconds: number } } };
    await api.getPublicEvents(astro as never, 'en');
    // Without this the middleware falls through to no-store and the page layer
    // silently stops participating in the tier (ADR-006).
    expect(astro.locals.pageData).toEqual({ ageSeconds: 0, maxAgeSeconds: 300 });
  });
});
