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

  it('aborts a stalled shared load, removes it, and retries upstream', async () => {
    vi.useFakeTimers();
    try {
      let attempt = 0;
      const signals: AbortSignal[] = [];
      const fetchImpl = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
        attempt += 1;
        if (init?.signal) signals.push(init.signal);
        if (attempt > 1) {
          return Promise.resolve(jsonResponse({ ok: true }, 'public, max-age=300'));
        }
        return new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => reject(init.signal?.reason), { once: true });
        });
      });
      const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => 0);

      const first = cache.get<{ ok: boolean }>('http://gw/hot');
      const joined = cache.get<{ ok: boolean }>('http://gw/hot');
      let settled = false;
      const failures = Promise.allSettled([first, joined]).then((results) => {
        settled = true;
        return results;
      });

      expect(fetchImpl).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(8_000);
      expect(settled).toBe(true);
      expect(await failures).toEqual([
        expect.objectContaining({ status: 'rejected' }),
        expect.objectContaining({ status: 'rejected' }),
      ]);
      expect(signals[0]?.aborted).toBe(true);

      await expect(cache.get<{ ok: boolean }>('http://gw/hot')).resolves.toMatchObject({
        data: { ok: true },
      });
      expect(fetchImpl).toHaveBeenCalledTimes(2);

      await vi.advanceTimersByTimeAsync(8_000);
      expect(signals[1]?.aborted).toBe(false);
    } finally {
      vi.useRealTimers();
    }
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

// R12 (2026-07-25 architecture review). The map only ever lost an entry when that
// exact URL was read again, so an SSR process crawling a large catalog grew without
// bound: a page read once is never revisited and was never released.
//
// These assert through `size`, not `get()`. An expired entry re-fetches either way,
// so a test written against `get()` alone passes with no sweep at all — it cannot
// reach the state that would fail.
describe('expired entries are swept', () => {
  const cacheable = (maxAge: number, age?: string) =>
    new Response(JSON.stringify({ ok: true }), {
      status: 200,
      headers:
        age === undefined
          ? { 'cache-control': `public, max-age=${maxAge}` }
          : { 'cache-control': `public, max-age=${maxAge}`, age },
    });

  it('drops entries no one will ever read again, on insert', async () => {
    const fetchImpl = vi.fn(async () => cacheable(300));
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);

    for (let i = 0; i < 50; i += 1) {
      await cache.get(`http://catalog/public/events/${i}`);
    }
    expect(cache.size).toBe(50);

    // Past every one of their max-ages, and none of those 50 URLs is ever asked for
    // again — the crawl case. The insert below is the only thing that runs.
    now = 301_000;
    await cache.get('http://catalog/public/events/fresh');
    expect(cache.size).toBe(1);
  });

  it('sweeps on the COMBINED age, so an entry that arrived stale is not kept longer', async () => {
    // 290 seconds old on arrival against a 300-second max-age: it expires 10 seconds
    // from now, not 300. The pre-TKT-206 predicate (now - fetchedAt >= maxAge) would
    // hold this for another 290 seconds — sweeping past exactly the entries the sweep
    // exists to drop.
    const fetchImpl = vi.fn(async () => cacheable(300, '290'));
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events/stale-on-arrival');
    expect(cache.size).toBe(1);

    now = 11_000; // 290 + 11 = 301 > 300, but only 11s of LOCAL time have passed
    await cache.get('http://catalog/public/events/other');
    expect(cache.size).toBe(1); // the stale one went, the new one stayed
  });

  it('keeps entries that are still live', async () => {
    const fetchImpl = vi.fn(async () => cacheable(300));
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events/a');
    now = 100_000; // well inside 300s
    await cache.get('http://catalog/public/events/b');
    expect(cache.size).toBe(2);
  });
});

// TKT-206 ai-review: `now` is a wall clock, so it can step backwards. Age must
// never decrease below what upstream already reported, or the middleware
// advertises remaining freshness that does not exist — which is precisely the
// stacking guarantee this ticket added Age to keep.
describe('age is monotonic under a backward clock', () => {
  it('does not decrease after age has already advanced', async () => {
    // The case the first version of this test missed. Clamping elapsed time at
    // zero only covers a jump to before fetchedAtMs; an entry that has already
    // aged forward can still shrink, handing the page back freshness it spent.
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'cache-control': 'public, max-age=300', age: '100' },
      }),
    );
    let now = 1_000_000;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events');

    now = 1_100_000; // +100s → age 200
    const advanced = await cache.get('http://catalog/public/events');
    expect(advanced.ageSeconds).toBe(200);

    now = 1_050_000; // 50s back, still AFTER fetchedAtMs
    const afterStep = await cache.get('http://catalog/public/events');
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(afterStep.ageSeconds).toBeGreaterThanOrEqual(advanced.ageSeconds);
    expect(afterStep.ageSeconds).toBe(200);
  });

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
