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
  /**
   * The highest age this entry has ever reported. `now` is a WALL clock, so a
   * backward step shrinks the elapsed term; without a high-water mark the same
   * live entry would report a smaller age than a previous call already
   * established, and the middleware would hand the page back freshness it had
   * already spent.
   */
  reportedAgeSeconds: number;
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
 * The age this entry would report at `nowMs`, per RFC 9111 as TKT-206 applies it:
 * upstream age at fetch time plus local elapsed, floored by the high-water mark so a
 * backward wall-clock step cannot hand back freshness already spent.
 *
 * Pure on purpose. `get()` writes the high-water mark because it REPORTS an age; the
 * sweep only asks, so it must not. Extracted so expiry has exactly one definition —
 * a sweep that decided expiry differently from `get()` would drop live entries or
 * keep dead ones, and either way the two would drift apart silently.
 */
function ageOf(entry: Entry, nowMs: number): number {
  const elapsedSeconds = Math.max(0, Math.floor((nowMs - entry.fetchedAtMs) / 1000));
  return Math.max(entry.reportedAgeSeconds, entry.upstreamAgeSeconds + elapsedSeconds);
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
      // `now` is a WALL clock and can step backwards, so age is computed as a
      // HIGH-WATER MARK rather than a subtraction. Clamping the elapsed term at
      // zero is not enough on its own: an entry sitting at age 300 whose clock
      // steps back 50 seconds still yields 250 — a decrease, and 50 seconds of
      // advertised freshness the previous call had already spent.
      //
      // Monotonic by construction, and it makes expiry no later than before: age
      // only ever rises, so an entry cannot regain life it has used.
      //
      // What this does NOT close: after a backward step the age stops advancing
      // until wall time catches up, so an entry can outlive its TTL in real time
      // by the size of the jump. That predates TKT-206 — this cache measured
      // wall time before the ticket touched it — and closing it needs a
      // monotonic clock source, which changes the injected-clock contract this
      // cache shares with its tests and the middleware.
      const ageSeconds = ageOf(entry, nowMs);
      entry.reportedAgeSeconds = ageSeconds;
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
      // Swept with a FRESH reading, not the `nowMs` captured before the upstream call:
      // that call is the slow part, and sweeping against a clock from before it would
      // spare entries that expired while it was in flight.
      this.#sweep(this.#now());
      this.#entries.set(url, {
        data,
        fetchedAtMs: nowMs,
        maxAgeSeconds,
        upstreamAgeSeconds,
        reportedAgeSeconds: upstreamAgeSeconds,
      });
    }
    return { data, ageSeconds: upstreamAgeSeconds, maxAgeSeconds };
  }
}
