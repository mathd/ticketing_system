// SSR page-data cache — the single cache owner of the minutes tier
// (ADR-006 caching-ownership rule; TKT-26 plan revision 2+3).
//
// The key is the full request URL, so every representation selector (route,
// id, locale) keys its own entry. Freshness is governed by the upstream
// response's max-age; the middleware derives the page's outgoing
// Cache-Control from the REMAINING freshness so total staleness never
// exceeds the tier (no cache stacking).

export class UpstreamError extends Error {
  constructor(
    public readonly status: number,
    url: string,
  ) {
    super(`upstream ${status} for ${url}`);
  }
}

export interface CachedResult<T> {
  data: T;
  /** Seconds since the underlying upstream fetch. 0 on a fresh fetch. */
  ageSeconds: number;
  /** The upstream max-age governing this entry. */
  maxAgeSeconds: number;
}

interface Entry {
  data: unknown;
  fetchedAtMs: number;
  maxAgeSeconds: number;
}

export function parseMaxAge(cacheControl: string | null): number {
  if (!cacheControl || /\bno-store\b/.test(cacheControl)) return 0;
  const match = /\bmax-age=(\d+)/.exec(cacheControl);
  return match ? Number(match[1]) : 0;
}

export class PageDataCache {
  #entries = new Map<string, Entry>();
  // Single-flight: concurrent misses for the same URL share one upstream
  // call — a hot event page expiring under an on-sale burst must not
  // stampede the catalog (ADR-004's whole point at that moment).
  #inFlight = new Map<string, Promise<CachedResult<unknown>>>();
  #fetch: typeof fetch;
  #now: () => number;

  constructor(fetchImpl: typeof fetch = fetch, now: () => number = Date.now) {
    this.#fetch = fetchImpl;
    this.#now = now;
  }

  /** Fetch-through cache: one upstream call per URL per max-age window. */
  async get<T>(url: string): Promise<CachedResult<T>> {
    const nowMs = this.#now();
    const entry = this.#entries.get(url);
    if (entry) {
      const ageSeconds = Math.floor((nowMs - entry.fetchedAtMs) / 1000);
      if (ageSeconds < entry.maxAgeSeconds) {
        return { data: entry.data as T, ageSeconds, maxAgeSeconds: entry.maxAgeSeconds };
      }
      this.#entries.delete(url);
    }
    const inFlight = this.#inFlight.get(url);
    if (inFlight) {
      return inFlight as Promise<CachedResult<T>>;
    }
    const flight = this.#fetchThrough<T>(url, nowMs).finally(() => this.#inFlight.delete(url));
    // Failures are shared with every coalesced caller but never cached:
    // the next request retries upstream.
    this.#inFlight.set(url, flight);
    return flight;
  }

  async #fetchThrough<T>(url: string, nowMs: number): Promise<CachedResult<T>> {
    const response = await this.#fetch(url, { headers: { accept: 'application/json' } });
    if (!response.ok) {
      throw new UpstreamError(response.status, url);
    }
    const maxAgeSeconds = parseMaxAge(response.headers.get('cache-control'));
    const data = (await response.json()) as T;
    if (maxAgeSeconds > 0) {
      this.#entries.set(url, { data, fetchedAtMs: nowMs, maxAgeSeconds });
    }
    return { data, ageSeconds: 0, maxAgeSeconds };
  }
}
