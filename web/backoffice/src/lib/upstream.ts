// Talking to a service through the gateway, and checking what came back.
//
// Shared by the four service clients (catalog.ts, commerce.ts, access.ts, inventory.ts).
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

/** A dispatched write may have committed, but its result could not be verified. */
export class AmbiguousMutationError extends Error {
  constructor(service: string, options?: ErrorOptions) {
    super(`${service} may have accepted the write, but its outcome could not be verified.`, options);
    this.name = 'AmbiguousMutationError';
  }
}

/**
 * Hand a write to fetch, then classify failures whose commit state is unknowable.
 *
 * Callers build credentials, headers, URLs, and bodies before calling this
 * function. Those errors are definite local failures and pass through unchanged.
 * Once fetch returns a promise, a rejection can follow a sent request, and a 5xx
 * does not say whether the service committed before failing. Both are ambiguous.
 */
export async function fetchMutation(
  service: string,
  input: RequestInfo | URL,
  init: RequestInit,
): Promise<Response> {
  // Keep this call outside the catch below. A synchronous fetch failure occurs
  // before fetch takes ownership of the request and remains a definite local
  // failure, while a later promise rejection has an unknown commit outcome.
  const pending = fetch(input, init);

  let response: Response;
  try {
    response = await pending;
  } catch (cause) {
    throw new AmbiguousMutationError(service, { cause });
  }
  if (response.status >= 500 && response.status <= 599) {
    throw new AmbiguousMutationError(service, {
      cause: new Error(`${service} answered ${response.status}`),
    });
  }
  return response;
}

/** Decode a successful mutation without turning an unreadable 2xx into a refusal. */
export async function decodeMutationResponse<T>(
  response: Response,
  service: string,
  decode: (value: unknown) => T,
): Promise<T> {
  try {
    return decode(await response.json());
  } catch (cause) {
    throw new AmbiguousMutationError(service, { cause });
  }
}

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

/** The common primitives below validate JSON values; service-specific decoders assemble them. */
export function responseObject(value: unknown, name: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`response ${name} is not an object`);
  }
  return value as Record<string, unknown>;
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function uuid(v: unknown, field: string): string {
  const value = required(v, field);
  if (!UUID.test(value)) throw new Error(`response ${field} is not a UUID`);
  return value;
}

const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})[tT](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[zZ]|[+-](\d{2}):(\d{2}))$/;

export function dateTime(v: unknown, field: string): string {
  const value = required(v, field);
  const match = RFC3339.exec(value);
  if (!match) throw new Error(`response ${field} is not an RFC 3339 date-time`);

  const [, yearText, monthText, dayText, hourText, minuteText, secondText, offsetHourText, offsetMinuteText] = match;
  const year = Number(yearText);
  const month = Number(monthText);
  const day = Number(dayText);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (
    month < 1 || month > 12 ||
    day < 1 || day > days[month - 1] ||
    Number(hourText) > 23 || Number(minuteText) > 59 || Number(secondText) > 59 ||
    Number(offsetHourText ?? 0) > 23 || Number(offsetMinuteText ?? 0) > 59
  ) {
    throw new Error(`response ${field} is not an RFC 3339 date-time`);
  }
  return value;
}

export function responseArray(v: unknown, field: string): unknown[] {
  if (!Array.isArray(v)) throw new Error(`response ${field} is not an array`);
  return v;
}

export function currency(v: unknown, field: string): string {
  const value = required(v, field);
  if (!/^[A-Z]{3}$/.test(value)) throw new Error(`response ${field} is not an ISO currency code`);
  return value;
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
 * isSafeInteger, not isInteger. Several contracts declare int64 values, while
 * JSON.parse silently rounds numbers past 2^53. Accepting a rounded count or
 * amount would make the console state a different value than the service sent.
 */
export function wholeNumber(v: unknown, field: string): number {
  if (typeof v !== 'number' || !Number.isSafeInteger(v)) {
    throw new Error(`response ${field} is not a whole number this console can represent exactly`);
  }
  return v;
}

export function nonNegativeWholeNumber(v: unknown, field: string): number {
  const value = wholeNumber(v, field);
  if (value < 0) throw new Error(`response ${field} is negative`);
  return value;
}

export function positiveWholeNumber(v: unknown, field: string): number {
  const value = wholeNumber(v, field);
  if (value < 1) throw new Error(`response ${field} is not positive`);
  return value;
}

const MAX_INT32 = 2_147_483_647;

/** A positive OpenAPI int32. */
export function positiveInt32(v: unknown, field: string): number {
  const value = positiveWholeNumber(v, field);
  if (value > MAX_INT32) throw new Error(`response ${field} is above the int32 maximum`);
  return value;
}

/** A required string with the contract's Unicode code-point maximum. */
export function boundedString(v: unknown, field: string, maximum: number): string {
  const value = required(v, field);
  if ([...value].length > maximum) {
    throw new Error(`response ${field} is longer than ${maximum} characters`);
  }
  return value;
}

export function boolean(v: unknown, field: string): boolean {
  if (typeof v !== 'boolean') throw new Error(`response ${field} is not a boolean`);
  return v;
}
