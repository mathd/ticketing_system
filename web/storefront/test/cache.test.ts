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

  it('sweeps expired entries on insert so the map cannot grow unbounded', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({ ok: true }, 'public, max-age=300'));
    let nowMs = 0;
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => nowMs);

    // A crawl over pages that are each read exactly once.
    for (let i = 0; i < 50; i++) {
      await cache.get(`http://gw/api/catalog/public/events/${i}`);
    }
    expect(cache.size).toBe(50);

    nowMs = 300_000; // every entry above is now expired
    await cache.get('http://gw/api/catalog/public/events/fresh');

    // Without the sweep this would be 51: the 50 stale entries are never
    // revisited, so nothing would ever release them.
    expect(cache.size).toBe(1);
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
