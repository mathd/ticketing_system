import { describe, expect, it, vi } from 'vitest';

import { PageDataCache, UpstreamError, parseMaxAge } from '../src/lib/cache';

function jsonResponse(body: unknown, cacheControl: string): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json', 'cache-control': cacheControl },
  });
}

describe('parseMaxAge', () => {
  it('reads max-age and treats no-store/absent as uncacheable', () => {
    expect(parseMaxAge('public, max-age=300, s-maxage=300')).toBe(300);
    expect(parseMaxAge('no-store')).toBe(0);
    expect(parseMaxAge(null)).toBe(0);
  });
});

describe('PageDataCache', () => {
  it('makes exactly one upstream call per URL within the TTL (AC3)', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({ events: [] }, 'public, max-age=300'));
    let nowMs = 1_000_000;
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => nowMs);

    const first = await cache.get('http://gw/api/catalog/public/events?locale=fr');
    nowMs += 200_000; // 200s later: still inside the 300s window
    const second = await cache.get('http://gw/api/catalog/public/events?locale=fr');

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(first.ageSeconds).toBe(0);
    expect(second.ageSeconds).toBe(200);
    expect(second.maxAgeSeconds).toBe(300);
  });

  it('re-fetches once the upstream max-age has elapsed', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({ events: [] }, 'public, max-age=300'));
    let nowMs = 0;
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => nowMs);

    await cache.get('http://gw/x');
    nowMs = 300_000; // exactly at expiry: stale
    const result = await cache.get('http://gw/x');

    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(result.ageSeconds).toBe(0);
  });

  it('keys by the FULL url — /events/A never serves /events/B (plan-review finding 1)', async () => {
    const fetchSpy = vi.fn(async (url: string) =>
      jsonResponse({ id: new URL(url).pathname }, 'public, max-age=300'),
    );
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    const a = await cache.get<{ id: string }>('http://gw/public/events/A?locale=fr');
    const b = await cache.get<{ id: string }>('http://gw/public/events/B?locale=fr');
    const aEn = await cache.get<{ id: string }>('http://gw/public/events/A?locale=en');

    expect(fetchSpy).toHaveBeenCalledTimes(3);
    expect(a.data.id).toBe('/public/events/A');
    expect(b.data.id).toBe('/public/events/B');
    expect(aEn.data.id).toBe('/public/events/A');
  });

  it('coalesces concurrent misses into one upstream call (no stampede)', async () => {
    let release!: (r: Response) => void;
    const gate = new Promise<Response>((resolve) => {
      release = resolve;
    });
    const fetchSpy = vi.fn(() => gate);
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    const burst = Promise.all(
      Array.from({ length: 5 }, () => cache.get<{ ok: boolean }>('http://gw/hot')),
    );
    release(jsonResponse({ ok: true }, 'public, max-age=300'));
    const results = await burst;

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(results.every((r) => r.data.ok)).toBe(true);
  });

  it('shares a coalesced failure but retries upstream on the next call', async () => {
    const fetchSpy = vi
      .fn()
      .mockResolvedValueOnce(new Response('boom', { status: 502 }))
      .mockResolvedValueOnce(jsonResponse({ ok: true }, 'public, max-age=300'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    const [a, b] = await Promise.allSettled([cache.get('http://gw/hot'), cache.get('http://gw/hot')]);
    expect(a.status).toBe('rejected');
    expect(b.status).toBe('rejected');
    expect(fetchSpy).toHaveBeenCalledTimes(1); // the failure was shared...

    await expect(cache.get<{ ok: boolean }>('http://gw/hot')).resolves.toMatchObject({
      data: { ok: true },
    }); // ...but never cached
    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it('never caches no-store responses', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({}, 'no-store'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    await cache.get('http://gw/y');
    await cache.get('http://gw/y');

    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it('surfaces upstream failures with their status', async () => {
    const fetchSpy = vi.fn(async () => new Response('nope', { status: 404 }));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    await expect(cache.get('http://gw/z')).rejects.toThrow(UpstreamError);
    await expect(cache.get('http://gw/z')).rejects.toMatchObject({ status: 404 });
  });
});

// TKT-206: catalog now serves these reads from its own memory, so a response can
// arrive ALREADY stale. Starting such an entry at age zero here would stack two
// five-minute tiers into ten minutes of buyer-visible staleness — the exact
// thing the epic's COS bounds. These pin the propagation.
describe('upstream Age propagation', () => {
  const headers = (maxAge: number, age?: string) =>
    new Headers(
      age === undefined
        ? { 'cache-control': `public, max-age=${maxAge}` }
        : { 'cache-control': `public, max-age=${maxAge}`, age },
    );

  it('reports upstream age on a miss rather than zero', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, '120') }),
    );
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => 0);
    const result = await cache.get('http://catalog/public/events');
    expect(result.ageSeconds).toBe(120);
  });

  it('counts local elapsed time on top of upstream age', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, '200') }),
    );
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events');
    now = 30_000;
    const hit = await cache.get('http://catalog/public/events');
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    // 200 upstream + 30 local. Without propagation this would report 30, and the
    // middleware would grant the page 270 seconds it does not have.
    expect(hit.ageSeconds).toBe(230);
  });

  it('expires an entry using the combined age, not the local one', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, '290') }),
    );
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events');
    now = 20_000; // 290 + 20 = 310 > 300
    await cache.get('http://catalog/public/events');
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it('does not retain a response that arrived already expired', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, '300') }),
    );
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => 0);
    await cache.get('http://catalog/public/events');
    await cache.get('http://catalog/public/events');
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it('treats a missing or malformed Age as zero', async () => {
    for (const age of [undefined, 'not-a-number', '-5']) {
      const fetchImpl = vi.fn(async () =>
        new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, age) }),
      );
      const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => 0);
      const result = await cache.get(`http://catalog/public/events?a=${age}`);
      expect(result.ageSeconds).toBe(0);
    }
  });
});

// TKT-206 ai-review: `now` is a wall clock, so it can step backwards. Age must
// never decrease below what upstream already reported, or the middleware
// advertises remaining freshness that does not exist — which is precisely the
// stacking guarantee this ticket added Age to keep.
describe('age is monotonic under a backward clock', () => {
  it('never reports less than the upstream age after the clock steps back', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'cache-control': 'public, max-age=300', age: '200' },
      }),
    );
    let now = 1_000_000;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    const miss = await cache.get('http://catalog/public/events');
    expect(miss.ageSeconds).toBe(200);

    now = 940_000; // a 60-second backward step
    const hit = await cache.get('http://catalog/public/events');
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    // Without the clamp this would report 140 and the page would claim 160
    // seconds of freshness it does not have.
    expect(hit.ageSeconds).toBe(200);
    expect(hit.ageSeconds).toBeGreaterThanOrEqual(miss.ageSeconds);
  });
});
