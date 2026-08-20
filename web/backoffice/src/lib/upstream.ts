// Talking to a service through the gateway, and checking what came back.
//
// Shared by the three service clients (catalog.ts, commerce.ts, access.ts).
// These validators are the reason a 200 with the wrong shape is not a read: each
// one exists because a specific wrong claim reached a support agent's screen in
// review, and the messages name the field so a contract skew is diagnosable.

export const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

export const UPSTREAM_DEADLINE_MS = 5_000;

/**
 * Bound one complete service operation, including response-body decoding.
 *
 * A helper that stopped the timer when `fetch` returned would cover headers but
 * leave a stalled JSON body unbounded. Callers therefore put the whole operation
 * inside this function and pass its signal to `fetch`.
 */
export async function withUpstreamDeadline<T>(
  operation: (signal: AbortSignal) => Promise<T>,
): Promise<T> {
  const controller = new AbortController();
  const deadline = setTimeout(() => controller.abort(), UPSTREAM_DEADLINE_MS);
  try {
    return await operation(controller.signal);
  } finally {
    clearTimeout(deadline);
  }
}

/** A read that can be absent or broken, and must never confuse the two. */
export type Read<T> = { ok: true; value: T } | { ok: false; kind: 'not-found' | 'unavailable' };

/**
 * A read result, or WHY there isn't one. The distinction is the whole point: a
 * support agent told "no such order" when commerce is merely down will tell the
 * customer their order does not exist. 404 is the only absence; everything else
 * — including a 400, which means this client and the contract disagree — is a
 * failure to answer.
 */
export async function readJson<T>(
  url: string,
  project: (body: unknown) => T,
): Promise<Read<T>> {
  try {
    return await withUpstreamDeadline(async (signal) => {
      const res = await fetch(url, { signal });
      if (res.status === 404) return { ok: false, kind: 'not-found' };
      if (!res.ok) return { ok: false, kind: 'unavailable' };
      return { ok: true, value: project(await res.json()) };
    });
  } catch {
    return { ok: false, kind: 'unavailable' };
  }
}

/**
 * A required string, or a thrown projection failure that `readJson` turns into
 * `unavailable`.
 *
 * A `200` whose body is missing the field is NOT a successful read: without
 * this, commerce answering `{}` would render "Commerce reports this order as
 * **undefined**" — a claim about an order, sourced from nothing. An upstream
 * that answers 200 with the wrong shape has failed to answer.
 */
export function required(v: unknown, field: string): string {
  if (typeof v !== 'string' || v === '') throw new Error(`response is missing ${field}`);
  return v;
}

/**
 * The identifier a response carries must be the one we asked about.
 *
 * The console labels each half with the identifier the OPERATOR typed, so a
 * misrouted, stale or proxy-cached 200 would otherwise let it present order B's
 * status under order A's heading — the exact misreading the page's caveat
 * exists to prevent, arriving through the back door. Compared
 * case-insensitively: both sides are UUIDs, and a case difference is a
 * formatting choice, not a different order.
 */
export function sameIdentity(got: string, asked: string, field: string): void {
  if (got.toLowerCase() !== asked.toLowerCase()) {
    throw new Error(`response ${field} is not the one requested`);
  }
}

/**
 * An integer this console can render exactly.
 *
 * isSafeInteger, not isInteger. Commerce declares these int64, and JSON.parse
 * silently rounds anything past 2^53 — 9007199254740993 minor units arrives as
 * ...992, still passes an isInteger check, and would be rendered to an operator
 * as commerce's exact refund amount (ai-review pass 1). The money that moved is
 * server-derived and unaffected; the CLAIM on screen would be wrong, and a money
 * figure that is quietly wrong is worse than one this console admits it cannot
 * show.
 */
export function wholeNumber(v: unknown, field: string): number {
  if (typeof v !== 'number' || !Number.isSafeInteger(v)) {
    throw new Error(`response ${field} is not a whole number this console can represent exactly`);
  }
  return v;
}

export function boolean(v: unknown, field: string): boolean {
  if (typeof v !== 'boolean') throw new Error(`response ${field} is not a boolean`);
  return v;
}
