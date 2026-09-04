// SSR page-data cache — the single cache owner of the minutes tier
// (ADR-006 caching-ownership rule).
//
// The key is the full request URL, so every representation selector (route,
// id, locale) keys its own entry. Freshness is governed by the upstream
// response's max-age; the middleware derives the page's outgoing
// Cache-Control from the REMAINING freshness so total staleness never exceeds
// the tier.
//
// Catalog also caches these reads, so an entry can arrive already stale.
// Upstream `Age` seeds the entry's age (RFC 9111), and a response already at or
// past max-age is used once rather than retained. Freshness therefore starts at
// catalog's load instead of stacking another full tier here.
//
// Entries and single-flight loads hold raw JSON. Each caller's boundary decoder
// receives its own clone, so neither decoder mutation nor a returned reference can
// change what another page receives for the same URL.
//
// The LOCAL half of that measurement runs on a monotonic clock (TKT-212, see
// monotonicNow), so an entry's real-time lifetime is its max-age no matter what
// the system clock does. The upstream half is a wire value and keeps catalog's.

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
  rawData: unknown;
  fetchedAtMs: number;
  maxAgeSeconds: number;
  /** Upstream age at fetch time; the entry is already this stale when stored. */
  upstreamAgeSeconds: number;
}

function decodeCopy<T>(rawData: unknown, decode: (value: unknown) => T): T {
  return decode(structuredClone(rawData));
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

/**
 * The clock entry aging runs on — **monotonic, not wall-clock**.
 *
 * Same reasoning, and the same shape, as `session.ts`'s expiry clock (TKT-220):
 * `performance.now()` counts milliseconds since process start and is unaffected by
 * system clock changes in either direction, so a step cannot change how long an
 * entry lives rather than merely being defended against.
 *
 * A high-water mark over `Date.now()` is insufficient: after a backward step,
 * age stops advancing until wall time catches up and the entry outlives its TTL.
 *
 * Relative milliseconds, not epoch: never serialize it or compare it with an HTTP
 * date. Only the LOCAL elapsed term uses it — `upstreamAgeSeconds` is a value received
 * over the wire and belongs to catalog's clock (ADR-045).
 */
function monotonicNow(): number {
  return performance.now();
}

/**
 * The age this entry would report at `nowMs`, per RFC 9111 as ADR-045 applies it:
 * upstream age at fetch time plus local elapsed on the monotonic clock.
 *
 * A monotonic source makes the result non-decreasing. Both direct reads and the
 * expiry sweep use this function so they agree on when an entry becomes stale.
 */
function ageOf(entry: Entry, nowMs: number): number {
  const elapsedSeconds = Math.max(0, Math.floor((nowMs - entry.fetchedAtMs) / 1000));
  return entry.upstreamAgeSeconds + elapsedSeconds;
}

// One cache load is shared by every request waiting for the same URL. Give that
// shared operation its own deadline so no individual browser request controls it.
const PAGE_DATA_LOAD_TIMEOUT_MS = 8_000;

export class PageDataCache {
  #entries = new Map<string, Entry>();
  // Single-flight: concurrent misses for the same URL share one upstream
  // call — a hot event page expiring under an on-sale burst must not
  // stampede the catalog (ADR-004's whole point at that moment).
  #inFlight = new Map<string, Promise<CachedResult<unknown>>>();
  #fetch: typeof fetch;
  /**
   * Elapsed-time source. **Must be monotonic** — the contract, not a preference.
   *
   * Entry aging subtracts two readings of this and trusts the result not to go
   * backwards. Hand this `Date.now` and an entry can report a shrinking age and
   * re-advertise freshness it has already spent.
   *
   * It exists for tests, which drive it forward by hand. No production caller
   * passes it: `api.ts` constructs the one singleton with no arguments, on
   * purpose, so the suite exercises the default. `session.ts` uses the same
   * constructor-default pattern for its `monotonicClock` dependency.
   */
  #now: () => number;

  constructor(fetchImpl: typeof fetch = fetch, now: () => number = monotonicNow) {
    this.#fetch = fetchImpl;
    this.#now = now;
  }

  /**
   * Live entry count. Exposed because the sweep is otherwise unobservable: an expired
   * entry re-fetches through `get()` whether or not it was ever dropped, so a test
   * written against `get()` alone passes with no sweep at all.
   */
  get size(): number {
    return this.#entries.size;
  }

  /**
   * Drop every expired entry.
   *
   * Without this the map only loses an entry when that exact URL is read again, so a
   * long-lived SSR process over a large catalog grows without bound — pages read once
   * during a crawl are never revisited and never released. Swept on insert, the only
   * moment the map grows.
   *
   * O(n) per insert. That is the right trade at this size: n is bounded by the pages
   * the process has served inside one max-age window, and the alternative (an expiry
   * heap) is a data structure to maintain for a map that holds hundreds of entries.
   */
  #sweep(nowMs: number): void {
    for (const [key, entry] of this.#entries) {
      if (ageOf(entry, nowMs) >= entry.maxAgeSeconds) {
        this.#entries.delete(key);
      }
    }
  }

  /** Fetch-through cache: one upstream call per URL per max-age window. */
  async get<T>(url: string, decode: (value: unknown) => T): Promise<CachedResult<T>> {
    const nowMs = this.#now();
    const entry = this.#entries.get(url);
    if (entry) {
      // `now` is MONOTONIC (see monotonicNow), so elapsed time only ever rises
      // and the entry's real-time lifetime is exactly its max-age regardless of
      // what the system clock does in either direction. Nothing is written back
      // to the entry here: with a monotonic source there is no high-water mark
      // left to maintain (TKT-212).
      const ageSeconds = ageOf(entry, nowMs);
      if (ageSeconds < entry.maxAgeSeconds) {
        return {
          data: decodeCopy(entry.rawData, decode),
          ageSeconds,
          maxAgeSeconds: entry.maxAgeSeconds,
        };
      }
      this.#entries.delete(url);
    }
    const inFlight = this.#inFlight.get(url);
    if (inFlight) {
      const shared = await inFlight;
      return { ...shared, data: decodeCopy(shared.data, decode) };
    }
    const flight = this.#fetchThrough(url, nowMs).finally(() => this.#inFlight.delete(url));
    // Failures are shared with every coalesced caller but never cached:
    // the next request retries upstream.
    this.#inFlight.set(url, flight);
    const loaded = await flight;
    return { ...loaded, data: decodeCopy(loaded.data, decode) };
  }

  async #fetchThrough(
    url: string,
    nowMs: number,
  ): Promise<CachedResult<unknown>> {
    const controller = new AbortController();
    const deadline = setTimeout(() => controller.abort(), PAGE_DATA_LOAD_TIMEOUT_MS);
    try {
      const response = await this.#fetch(url, {
        headers: { accept: 'application/json' },
        signal: controller.signal,
      });
      if (!response.ok) {
        throw new UpstreamError(response.status, url);
      }
      const maxAgeSeconds = parseMaxAge(response.headers.get('cache-control'));
      const upstreamAgeSeconds = parseAge(response.headers.get('age'));
      // Keep body decoding inside the deadline. fetch resolves after headers, so
      // clearing the timer before json() would leave a stalled body unbounded.
      const rawData: unknown = await response.json();
      // Retain only what still has life left. An answer delivered at or past its
      // max-age is served to this caller and then dropped: keeping it would hand
      // the next request a value that was already expired when it arrived.
      if (maxAgeSeconds > upstreamAgeSeconds) {
        // Swept with a FRESH reading, not the `nowMs` captured before the upstream call:
        // that call is the slow part, and sweeping against a clock from before it would
        // spare entries that expired while it was in flight.
        this.#sweep(this.#now());
        this.#entries.set(url, {
          rawData,
          fetchedAtMs: nowMs,
          maxAgeSeconds,
          upstreamAgeSeconds,
        });
      }
      return { data: rawData, ageSeconds: upstreamAgeSeconds, maxAgeSeconds };
    } finally {
      clearTimeout(deadline);
    }
  }
}
