// SSR page-data cache — the single cache owner of the minutes tier
// (ADR-006 caching-ownership rule; TKT-26 plan revision 2+3).
//
// The key is the full request URL, so every representation selector (route,
// id, locale) keys its own entry. Freshness is governed by the upstream
// response's max-age; the middleware derives the page's outgoing
// Cache-Control from the REMAINING freshness so total staleness never exceeds
// the tier.
//
// That last sentence used to be true because this was the only cache in the
// chain. Since TKT-206 catalog serves these reads from its own memory, so an
// entry can arrive ALREADY STALE and starting it at age zero here would stack
// two five-minute tiers into ten. Upstream `Age` is therefore seeded into the
// entry (RFC 9111) and a response already at or past its max-age is used once
// and not retained. Freshness is now measured from catalog's load, not from
// ours — which is what keeps the no-stacking claim true.

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
  /** Upstream age at fetch time; the entry is already this stale when stored. */
  upstreamAgeSeconds: number;
}

export function parseMaxAge(cacheControl: string | null): number {
  if (!cacheControl || /\bno-store\b/.test(cacheControl)) return 0;
  const match = /\bmax-age=(\d+)/.exec(cacheControl);
  return match ? Number(match[1]) : 0;
}

/**
 * Seconds the upstream says its answer has already been alive (RFC 9111).
 * A missing, malformed or negative header is treated as 0 — the conservative
 * direction is to under-report OUR freshness, never to invent some.
 */
export function parseAge(age: string | null): number {
  if (!age) return 0;
  const parsed = Number(age);
  return Number.isFinite(parsed) && parsed > 0 ? Math.floor(parsed) : 0;
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
      // Clamped at zero because `now` is a WALL clock: an NTP step backwards
      // would otherwise make elapsed time negative, pushing ageSeconds below the
      // age upstream already reported and letting the middleware advertise more
      // remaining freshness than exists. Age must never decrease.
      //
      // This does not make expiry immune to a backward step — the entry can
      // still outlive its TTL in real time by the size of the jump, which is a
      // property this cache had before TKT-206 and would need a monotonic clock
      // to close. What the clamp removes is the part this change introduced:
      // advertising freshness we know we do not have.
      const elapsedSeconds = Math.max(0, Math.floor((nowMs - entry.fetchedAtMs) / 1000));
      const ageSeconds = entry.upstreamAgeSeconds + elapsedSeconds;
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
    const upstreamAgeSeconds = parseAge(response.headers.get('age'));
    const data = (await response.json()) as T;
    // Retain only what still has life left. An answer delivered at or past its
    // max-age is served to this caller and then dropped: keeping it would hand
    // the next request a value that was already expired when it arrived.
    if (maxAgeSeconds > upstreamAgeSeconds) {
      this.#entries.set(url, { data, fetchedAtMs: nowMs, maxAgeSeconds, upstreamAgeSeconds });
    }
    return { data, ageSeconds: upstreamAgeSeconds, maxAgeSeconds };
  }
}
