import { describe, expect, it, vi } from 'vitest';

import { PageDataCache, UpstreamError, parseMaxAge } from '../src/lib/cache';

function jsonResponse(body: unknown, cacheControl: string): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json', 'cache-control': cacheControl },
  });
}

function decodeId(value: unknown): { id: string } {
  if (typeof value !== 'object' || value === null || !('id' in value) || typeof value.id !== 'string') {
    throw new TypeError('id response is malformed');
  }
  return { id: value.id };
}

function decodeOK(value: unknown): { ok: boolean } {
  if (typeof value !== 'object' || value === null || !('ok' in value) || typeof value.ok !== 'boolean') {
    throw new TypeError('ok response is malformed');
  }
  return { ok: value.ok };
}

function decodeNested(value: unknown): { nested: { label: string } } {
  if (
    typeof value !== 'object' ||
    value === null ||
    !('nested' in value) ||
    typeof value.nested !== 'object' ||
    value.nested === null ||
    !('label' in value.nested) ||
    typeof value.nested.label !== 'string'
  ) {
    throw new TypeError('nested response is malformed');
  }
  return value as { nested: { label: string } };
}

const decodeRaw = (value: unknown): unknown => value;

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

    const first = await cache.get('http://gw/api/catalog/public/events?locale=fr', decodeRaw);
    nowMs += 200_000; // 200s later: still inside the 300s window
    const second = await cache.get('http://gw/api/catalog/public/events?locale=fr', decodeRaw);

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(first.ageSeconds).toBe(0);
    expect(second.ageSeconds).toBe(200);
    expect(second.maxAgeSeconds).toBe(300);
  });

  it('re-fetches once the upstream max-age has elapsed', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({ events: [] }, 'public, max-age=300'));
    let nowMs = 0;
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => nowMs);

    await cache.get('http://gw/x', decodeRaw);
    nowMs = 300_000; // exactly at expiry: stale
    const result = await cache.get('http://gw/x', decodeRaw);

    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(result.ageSeconds).toBe(0);
  });

  it('keys by the FULL url — /events/A never serves /events/B (plan-review finding 1)', async () => {
    const fetchSpy = vi.fn(async (url: string) =>
      jsonResponse({ id: new URL(url).pathname }, 'public, max-age=300'),
    );
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    const a = await cache.get('http://gw/public/events/A?locale=fr', decodeId);
    const b = await cache.get('http://gw/public/events/B?locale=fr', decodeId);
    const aEn = await cache.get('http://gw/public/events/A?locale=en', decodeId);

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
      Array.from({ length: 5 }, () => cache.get('http://gw/hot', decodeOK)),
    );
    release(jsonResponse({ ok: true }, 'public, max-age=300'));
    const results = await burst;

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(results.every((r) => r.data.ok)).toBe(true);
  });

  it('runs a non-idempotent projection exactly once for the miss caller and each cache-hit caller', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({ ok: true }, 'public, max-age=300'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);
    let projection = 0;
    const decode = vi.fn((value: unknown) => ({ ...decodeOK(value), projection: ++projection }));

    const miss = await cache.get('http://gw/projected', decode);
    const hit = await cache.get('http://gw/projected', decode);

    expect(miss.data.projection).toBe(1);
    expect(hit.data.projection).toBe(2);
    expect(decode).toHaveBeenCalledTimes(2);
    expect(fetchSpy).toHaveBeenCalledOnce();
  });

  it('shares raw JSON while concurrent callers choose and run their own representation once', async () => {
    let release!: (response: Response) => void;
    const upstream = new Promise<Response>((resolve) => { release = resolve; });
    const fetchSpy = vi.fn(() => upstream);
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);
    const asLabel = vi.fn((value: unknown) => `label:${decodeOK(value).ok}`);
    const asCount = vi.fn((value: unknown) => ({ count: decodeOK(value).ok ? 1 : 0 }));

    const first = cache.get('http://gw/representations', asLabel);
    const joined = cache.get('http://gw/representations', asCount);
    release(jsonResponse({ ok: true }, 'public, max-age=300'));

    await expect(first).resolves.toMatchObject({ data: 'label:true' });
    await expect(joined).resolves.toMatchObject({ data: { count: 1 } });
    expect(asLabel).toHaveBeenCalledOnce();
    expect(asCount).toHaveBeenCalledOnce();
    expect(fetchSpy).toHaveBeenCalledOnce();
  });

  it('does not let a miss decoder mutate the raw entry', async () => {
    const fetchSpy = vi.fn(async () =>
      jsonResponse({ nested: { label: 'original' } }, 'public, max-age=300'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    const miss = await cache.get('http://gw/isolated-miss', (value) => {
      const decoded = decodeNested(value);
      decoded.nested.label = 'changed by miss decoder';
      return decoded;
    });
    const hit = await cache.get('http://gw/isolated-miss', decodeNested);

    expect(miss.data.nested.label).toBe('changed by miss decoder');
    expect(hit.data.nested.label).toBe('original');
    expect(fetchSpy).toHaveBeenCalledOnce();
  });

  it('does not retain a reference returned to a cache-hit caller', async () => {
    const fetchSpy = vi.fn(async () =>
      jsonResponse({ nested: { label: 'original' } }, 'public, max-age=300'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);
    await cache.get('http://gw/isolated-hit', decodeNested);

    const firstHit = await cache.get('http://gw/isolated-hit', decodeNested);
    firstHit.data.nested.label = 'changed after return';
    const secondHit = await cache.get('http://gw/isolated-hit', decodeNested);

    expect(secondHit.data.nested.label).toBe('original');
    expect(fetchSpy).toHaveBeenCalledOnce();
  });

  it('gives joined single-flight callers independent values', async () => {
    let release!: (response: Response) => void;
    const upstream = new Promise<Response>((resolve) => { release = resolve; });
    const fetchSpy = vi.fn(() => upstream);
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    const first = cache.get('http://gw/isolated-join', decodeNested);
    const joined = cache.get('http://gw/isolated-join', decodeNested);
    release(jsonResponse({ nested: { label: 'original' } }, 'public, max-age=300'));
    const [firstResult, joinedResult] = await Promise.all([first, joined]);

    firstResult.data.nested.label = 'changed after return';
    expect(joinedResult.data.nested.label).toBe('original');
    const hit = await cache.get('http://gw/isolated-join', decodeNested);
    expect(hit.data.nested.label).toBe('original');
    expect(fetchSpy).toHaveBeenCalledOnce();
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

      const first = cache.get('http://gw/hot', decodeOK);
      const joined = cache.get('http://gw/hot', decodeOK);
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

      await expect(cache.get('http://gw/hot', decodeOK)).resolves.toMatchObject({
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

    const [a, b] = await Promise.allSettled([
      cache.get('http://gw/hot', decodeRaw),
      cache.get('http://gw/hot', decodeRaw),
    ]);
    expect(a.status).toBe('rejected');
    expect(b.status).toBe('rejected');
    expect(fetchSpy).toHaveBeenCalledTimes(1); // the failure was shared...

    await expect(cache.get('http://gw/hot', decodeOK)).resolves.toMatchObject({
      data: { ok: true },
    }); // ...but never cached
    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it('does not let one caller\'s rejecting decoder choose the cached representation', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({ ok: 'yes' }, 'public, max-age=300'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    await expect(cache.get('http://gw/hot', decodeOK)).rejects.toThrow('ok response is malformed');
    await expect(cache.get('http://gw/hot', (value) => {
      if (typeof value !== 'object' || value === null || !('ok' in value)) throw new TypeError();
      return String(value.ok);
    })).resolves.toMatchObject({
      data: 'yes',
    });
    expect(fetchSpy).toHaveBeenCalledOnce();
  });

  it('never caches no-store responses', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({}, 'no-store'));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    await cache.get('http://gw/y', decodeRaw);
    await cache.get('http://gw/y', decodeRaw);

    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it('surfaces upstream failures with their status', async () => {
    const fetchSpy = vi.fn(async () => new Response('nope', { status: 404 }));
    const cache = new PageDataCache(fetchSpy as unknown as typeof fetch, () => 0);

    await expect(cache.get('http://gw/z', decodeRaw)).rejects.toThrow(UpstreamError);
    await expect(cache.get('http://gw/z', decodeRaw)).rejects.toMatchObject({ status: 404 });
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
    const result = await cache.get('http://catalog/public/events', decodeRaw);
    expect(result.ageSeconds).toBe(120);
  });

  it('counts local elapsed time on top of upstream age', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, '200') }),
    );
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events', decodeRaw);
    now = 30_000;
    const hit = await cache.get('http://catalog/public/events', decodeRaw);
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
    await cache.get('http://catalog/public/events', decodeRaw);
    now = 20_000; // 290 + 20 = 310 > 300
    await cache.get('http://catalog/public/events', decodeRaw);
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it('does not retain a response that arrived already expired', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, '300') }),
    );
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => 0);
    await cache.get('http://catalog/public/events', decodeRaw);
    await cache.get('http://catalog/public/events', decodeRaw);
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it('treats a missing or malformed Age as zero', async () => {
    for (const age of [undefined, 'not-a-number', '-5']) {
      const fetchImpl = vi.fn(async () =>
        new Response(JSON.stringify({ ok: true }), { status: 200, headers: headers(300, age) }),
      );
      const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => 0);
      const result = await cache.get(`http://catalog/public/events?a=${age}`, decodeRaw);
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
      await cache.get(`http://catalog/public/events/${i}`, decodeRaw);
    }
    expect(cache.size).toBe(50);

    // Past every one of their max-ages, and none of those 50 URLs is ever asked for
    // again — the crawl case. The insert below is the only thing that runs.
    now = 301_000;
    await cache.get('http://catalog/public/events/fresh', decodeRaw);
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
    await cache.get('http://catalog/public/events/stale-on-arrival', decodeRaw);
    expect(cache.size).toBe(1);

    now = 11_000; // 290 + 11 = 301 > 300, but only 11s of LOCAL time have passed
    await cache.get('http://catalog/public/events/other', decodeRaw);
    expect(cache.size).toBe(1); // the stale one went, the new one stayed
  });

  it('keeps entries that are still live', async () => {
    const fetchImpl = vi.fn(async () => cacheable(300));
    let now = 0;
    const cache = new PageDataCache(fetchImpl as unknown as typeof fetch, () => now);
    await cache.get('http://catalog/public/events/a', decodeRaw);
    now = 100_000; // well inside 300s
    await cache.get('http://catalog/public/events/b', decodeRaw);
    expect(cache.size).toBe(2);
  });
});

// TKT-212: entry aging runs on a MONOTONIC clock, so a wall-clock step cannot
// change how long an entry lives. TKT-206 got the other half — age could no
// longer DECREASE — with a per-entry high-water mark, but that left the entry
// outliving its TTL in real time by the size of a backward jump, because age
// simply stopped advancing until wall time caught up.
//
// These drive the DEFAULT constructor argument and never inject `now`. That is
// load-bearing, and it is the third of the three unfalsifiable shapes
// session.test.ts:332 enumerates (TKT-220): injecting a clock ANYWHERE means the
// production default is never exercised, so swapping the default back to
// Date.now would leave the test green.
describe('entry aging is monotonic and the wall clock cannot move it', () => {
  const cacheable = (maxAge: number, age?: string) =>
    new Response(JSON.stringify({ ok: true }), {
      status: 200,
      headers:
        age === undefined
          ? { 'cache-control': `public, max-age=${maxAge}` }
          : { 'cache-control': `public, max-age=${maxAge}`, age },
    });

  it('ages on the default monotonic clock, which the wall clock cannot move OR extend', async () => {
    // Both halves in one sequence, because either alone is satisfiable by a
    // wrong implementation: asserting only "age advanced despite the backward
    // step" is also true of a cache that never expires anything, and asserting
    // only "it expired" is also true of one still reading the wall clock.
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      perf.mockReturnValue(1_000);
      Date.now = () => 10_000_000;
      const fetchImpl = vi.fn(async () => cacheable(10));
      const cache = new PageDataCache(fetchImpl as unknown as typeof fetch);
      await cache.get('http://catalog/public/events', decodeRaw);

      // Five monotonic seconds on, with the WALL clock stepped five seconds
      // BACKWARD underneath it. At HEAD the elapsed term clamps to zero and the
      // entry reports age 0 — five seconds of freshness it has already spent.
      perf.mockReturnValue(6_000);
      Date.now = () => 9_995_000;
      const hit = await cache.get('http://catalog/public/events', decodeRaw);
      expect(fetchImpl).toHaveBeenCalledTimes(1);
      expect(hit.ageSeconds).toBe(5);

      // The monotonic clock crossing max-age is what ends the entry — with the
      // wall clock STILL rolled back, so nothing here is attributable to it.
      // This is the leg that stops "never expires" from passing.
      perf.mockReturnValue(11_500);
      await cache.get('http://catalog/public/events', decodeRaw);
      expect(fetchImpl).toHaveBeenCalledTimes(2);
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
  });

  it('sweeps on the monotonic clock too, so a rolled-back wall clock cannot strand entries', async () => {
    // #sweep is the path that actually FREES memory, and get() re-fetches an
    // expired entry whether or not it was ever dropped — so this asserts
    // through `size` on a second URL. A fix that moved only get() to the
    // monotonic clock leaves the map growing on the wrong one, and no
    // assertion written against get() alone can see it.
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      // The wall clock is pinned NEAR the monotonic reading, not at an epoch
      // value. That is load-bearing: with Date.now in the trillions, a sweep on
      // the wrong clock computes a colossal elapsed and evicts anyway — the
      // right answer for the wrong reason, and the mutation survives. Pinned
      // here, a wall-clock sweep sees elapsed <= 0 and strands the entry.
      perf.mockReturnValue(1_000);
      Date.now = () => 1_000;
      const fetchImpl = vi.fn(async () => cacheable(10));
      const cache = new PageDataCache(fetchImpl as unknown as typeof fetch);
      await cache.get('http://catalog/public/events/stranded', decodeRaw);
      expect(cache.size).toBe(1);

      // 11 monotonic seconds past a 10-second max-age, wall clock rolled back.
      // The insert below is the only thing that runs the sweep.
      perf.mockReturnValue(12_000);
      Date.now = () => 500;
      await cache.get('http://catalog/public/events/other', decodeRaw);
      expect(cache.size).toBe(1);
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
  });

  it('still never reports an age below what upstream already declared', async () => {
    // TKT-206's guarantee, restated against the monotonic clock: it now falls
    // out of `upstreamAge + non-decreasing elapsed` rather than out of a
    // high-water mark. Kept because the property is the ADR-045 contract, not
    // because the deleted mechanism needs a memorial.
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      perf.mockReturnValue(1_000);
      Date.now = () => 10_000_000;
      const fetchImpl = vi.fn(async () => cacheable(300, '200'));
      const cache = new PageDataCache(fetchImpl as unknown as typeof fetch);
      const miss = await cache.get('http://catalog/public/events', decodeRaw);
      expect(miss.ageSeconds).toBe(200);

      Date.now = () => 9_940_000; // a 60-second backward wall step
      const hit = await cache.get('http://catalog/public/events', decodeRaw);
      expect(fetchImpl).toHaveBeenCalledTimes(1);
      expect(hit.ageSeconds).toBe(200);
      expect(hit.ageSeconds).toBeGreaterThanOrEqual(miss.ageSeconds);
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
  });
});
