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

/**
 * The clock entry aging runs on — **monotonic, not wall-clock**.
 *
 * Same reasoning, and the same shape, as `session.ts`'s expiry clock (TKT-220):
 * `performance.now()` counts milliseconds since process start and is unaffected by
 * system clock changes in either direction, so a step cannot change how long an
 * entry lives rather than merely being defended against.
 *
 * TKT-206 defended instead, with a per-entry high-water mark over `Date.now()`. That
 * closed age DECREASING but not the residual this replaces: after a backward step the
 * age stopped ADVANCING until wall time caught up, so the entry outlived its TTL in
 * real time by the size of the jump. TKT-220 had already learned by mutation that a
 * non-decreasing floor over a wall clock is the wrong tool for this exact defect.
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
 * No high-water mark. With a monotonic source `upstreamAgeSeconds + elapsed` is
 * non-decreasing by construction, so flooring it by the highest age already reported
 * could never change the result — there is no input for which the two differ, which
 * is the test AGENTS.md sets for deleting a mechanism rather than keeping it beside
 * the one that works.
 *
 * Pure on purpose, and now pure in both directions: `get()` no longer writes anything
 * back through it. Extracted so expiry has exactly one definition — a sweep that
 * decided expiry differently from `get()` would drop live entries or keep dead ones,
 * and either way the two would drift apart silently.
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
   * backwards; there is no floor underneath it any more, because a floor over a
   * wall clock is what TKT-206 tried and TKT-212 replaced. Hand this `Date.now`
   * and an entry can report a shrinking age and re-advertise freshness it has
   * already spent.
   *
   * It exists for tests, which drive it forward by hand. No production caller
   * passes it: `api.ts` constructs the one singleton with no arguments, on
   * purpose, so the default is the thing the suite exercises (TKT-220 — inject a
   * clock anywhere in production and swapping the default back to a wall clock
   * leaves every test green). Same shape, and the same reasoning, as
   * `session.ts`'s `now = monotonicNow()`.
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
  async get<T>(url: string): Promise<CachedResult<T>> {
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
      const data = (await response.json()) as T;
      // Retain only what still has life left. An answer delivered at or past its
      // max-age is served to this caller and then dropped: keeping it would hand
      // the next request a value that was already expired when it arrived.
      if (maxAgeSeconds > upstreamAgeSeconds) {
        // Swept with a FRESH reading, not the `nowMs` captured before the upstream call:
        // that call is the slow part, and sweeping against a clock from before it would
        // spare entries that expired while it was in flight.
        this.#sweep(this.#now());
        this.#entries.set(url, {
          data,
          fetchedAtMs: nowMs,
          maxAgeSeconds,
          upstreamAgeSeconds,
        });
      }
      return { data, ageSeconds: upstreamAgeSeconds, maxAgeSeconds };
    } finally {
      clearTimeout(deadline);
    }
  }
}
