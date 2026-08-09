// Storefront customer sessions (TKT-220 / US-A1). See ADR-049.
//
// Sessions live in this process and are NOT persisted. A restart signs everyone
// out, and a second replica would not share them — both true, both documented,
// and both acceptable for a single-replica Compose stack. The alternative costs a
// table, a migration and an expiry sweep to buy durability nobody has asked for;
// the day someone does, this module is replaced without moving the enforcement
// point.
//
// This deliberately mirrors web/backoffice/src/lib/session.ts, whose design was
// argued in ADR-042 and hardened by three adversarial review passes. The one
// place it departs is the cookie PATH — see SESSION_COOKIE_PATH.
//
// The token is opaque: it says nothing about who holds it, so a leaked one
// discloses nothing on its own and cannot be forged from a principal.

import { randomBytes } from 'node:crypto';

export interface CustomerPrincipal {
  customerId: string;
  /** The address in the spelling the buyer registered with, for display only. */
  email: string;
  /**
   * Commerce's signed proof that this buyer authenticated (TKT-221). Presented on
   * the checkout this process proxies, in `X-Customer-Assertion`.
   *
   * A BEARER CREDENTIAL, and it stays on this side of the wire: never in the
   * cookie (which is why the cookie value is an opaque token and not this), never
   * in a prop a page renders, never in a log. The browser must not be able to
   * obtain it — an XSS that could read it would be able to attribute purchases to
   * this customer for the rest of the session.
   *
   * Its expiry is deliberately the same as this session's, so the two cannot
   * disagree and strand a buyer at the payment button (ADR-049 § TKT-221).
   */
  assertion: string;
}

/** Deliberately unremarkable: a cookie named `customer_token` advertises its worth. */
export const SESSION_COOKIE = 'sf_sid';

/**
 * Absolute lifetime, not idle timeout. An idle window renews on every request,
 * so a stolen cookie stays good for as long as the thief keeps using it; an
 * absolute one ends the session on the clock regardless.
 *
 * Eight hours is inherited from the back office, where it was argued as "one
 * working shift". That argument does not transplant to a buyer, and no
 * customer-specific one has been made (ADR-049 records this). It is one constant;
 * a real complaint moves it.
 */
export const SESSION_TTL_MS = 8 * 60 * 60 * 1000;

/**
 * How many concurrent sessions ONE customer may hold. Signing in on a sixth
 * device ends the first.
 *
 * Sweeping expired entries is not a bound on its own: every sign-in mints a new
 * token while the old ones live out their full TTL, so one valid credential could
 * accumulate entries without limit — and the sweep degrades with them. ADR-042
 * records that a review pass caught exactly this claim in the back office.
 *
 * Per principal, never global. A global-only cap would let one busy account evict
 * a different customer's live session, turning a safety limit into a denial of
 * service against strangers.
 *
 * This cap alone does NOT bound the map — see MAX_SESSIONS_TOTAL.
 */
export const MAX_SESSIONS_PER_CUSTOMER = 5;

/**
 * How many sessions this process holds in total, across every customer.
 *
 * The per-principal cap bounds the map only if the number of principals is
 * bounded, and here it is not: **registration is public and unauthenticated**, so
 * one actor can mint unlimited distinct accounts and therefore unlimited distinct
 * principals, each entitled to five sessions that live out the full TTL. That is
 * the back office's reasoning failing to transplant — there, principals are
 * provisioned by an operator, so headcount is the bound. It was inherited here
 * with its premise removed (ai-review, TKT-220 [high]).
 *
 * **At capacity this REFUSES a new session; it does not evict a live one.** The
 * first version of this cap evicted oldest-issued-first, and pass 2 of the review
 * was right that it turned memory exhaustion into a targeted availability attack:
 * an attacker who can mint principals freely fills the map and then every further
 * sign-in displaces the *oldest live session*, which is a real customer's.
 *
 * Both behaviours are bad under the flood, so the choice is which failure to
 * prefer, and it is not close for a ticketing storefront:
 *
 *  - Evicting silently signs out buyers who are already signed in — including
 *    mid-purchase — for something a stranger did, with no error anywhere.
 *  - Refusing leaves every existing session untouched and makes NEW sign-ins fail
 *    loudly with "temporarily unavailable", which is diagnosable and recoverable.
 *
 * Neither is a fix. The cause is unauthenticated unlimited registration, and the
 * fix for that is rate limiting — **TKT-224**. This bound exists only to stop the
 * SSR process being killed by memory growth, and hitting it is a symptom to
 * escalate, not a state to tune around.
 *
 * 20 000 tokens is ~2 MB of map at this entry size — far above any real
 * concurrent-buyer count for a single-replica stack, and far below anything that
 * threatens the process.
 */
export const MAX_SESSIONS_TOTAL = 20_000;

/**
 * Thrown by createSession when the process is at MAX_SESSIONS_TOTAL.
 *
 * A distinct type, not a boolean or a null: the sign-in and registration pages
 * must render this as an OUTAGE and never as a credential verdict. Telling a
 * buyer their password is wrong because a stranger filled a Map is both false and
 * unactionable.
 */
export class SessionCapacityError extends Error {
  constructor() {
    super('session capacity reached');
    this.name = 'SessionCapacityError';
  }
}

interface Entry {
  principal: CustomerPrincipal;
  expiresAt: number;
}

const sessions = new Map<string, Entry>();

/**
 * The clock sessions expire on — **monotonic, not wall-clock**.
 *
 * `Date.now()` is not monotonic. If the host clock advances past an entry's
 * expiry with nothing reading it, then steps BACKWARDS — an NTP correction, a VM
 * snapshot resume, an operator setting the date — a `Date.now()` comparison
 * starts resolving the dead token again. Nothing collected it in the meantime,
 * because expiry-on-read only collects a token that is presented (ai-review,
 * TKT-220 [medium]).
 *
 * `performance.now()` counts milliseconds since process start and is unaffected
 * by system clock changes in either direction, so the resurrection is not
 * expressible rather than merely defended against.
 *
 * A first attempt at this fix clamped `Date.now()` to a non-decreasing floor.
 * That does not work, and the mutation check is what showed it: the floor only
 * rises when something calls in at the higher time — and every such call
 * (`createSession`'s sweep, `lookupSession`'s delete-on-read) has already removed
 * the entry. In the one scenario that matters, where nothing calls in at all, the
 * floor never learns the higher time and the clamp is a no-op.
 *
 * Sessions do not survive a restart, so a clock that resets to 0 with the process
 * costs nothing: there is nothing left to compare against.
 */
function monotonicNow(): number {
  return performance.now();
}

/**
 * The global bound actually enforced, which is MAX_SESSIONS_TOTAL everywhere except
 * under the capacity tests. See setMaxSessionsTotalForTest.
 */
let maxSessionsTotal: number = MAX_SESSIONS_TOTAL;

/** Test-only: sessions are module state, so suites must not leak into each other. */
export function resetSessionsForTest(): void {
  sessions.clear();
  // Restores the production bound too, so a test that lowered it cannot leak into
  // the next one. The suite's beforeEach already calls this, which makes the reset
  // structural rather than something each test has to remember (TKT-229).
  maxSessionsTotal = MAX_SESSIONS_TOTAL;
}

/**
 * Test-only: run the capacity rules against a small bound.
 *
 * createSession walks the WHOLE map on every call — it sweeps expired entries and
 * collects the caller's own tokens in one pass — so filling the map to a cap of N
 * costs O(N²). At the production 20 000 that is ~200M iterations per test, and the
 * four capacity cases each did it: they took 15-50s against vitest's 5s default and
 * failed the gate whenever the machine was busy (TKT-229).
 *
 * Lowering the bound removes the quadratic term while leaving every RULE intact —
 * the tests prove refusal-not-eviction, the global bound across distinct principals,
 * own-cap rotation and recovery, none of which depend on the bound's magnitude.
 *
 * Guarded by NAME, not by a runtime environment check, matching the two ForTest
 * exports above: nothing in src/lib/ branches on NODE_ENV, and resetSessionsForTest
 * is already a more dangerous export than this one — it wipes every live session.
 * A bundler-dependent branch in a security path would buy nothing.
 *
 * The floor is real: MAX_SESSIONS_PER_CUSTOMER + 1. Five slots are needed to reach
 * one customer's own cap and one more for a stranger, or the test that pins the
 * precedence between the two caps has no stranger to be refused and silently stops
 * proving anything.
 */
export function setMaxSessionsTotalForTest(limit: number): void {
  if (!Number.isInteger(limit) || limit < MAX_SESSIONS_PER_CUSTOMER + 1) {
    throw new RangeError(
      `session cap for tests must be an integer >= ${MAX_SESSIONS_PER_CUSTOMER + 1}, got ${limit}`,
    );
  }
  maxSessionsTotal = limit;
}

/** Test-only: proves a sweep actually reclaimed entries rather than hiding them. */
export function sessionCountForTest(): number {
  return sessions.size;
}

/**
 * Whether a map of `size` is full. **The single capacity decision** — createSession
 * calls exactly this, and so do the tests.
 *
 * One function rather than a value tests observe alongside the one createSession
 * reads (ai-review pass 2 [high], TKT-229). A parallel accessor proves nothing: with
 * one, a capacity check hard-coded to 40 — or an initializer set to 40 — kept the
 * accessor honest and passed the whole suite, verified by making both edits. The
 * only assertion that cannot be fooled is one that calls the decision the
 * production path calls.
 *
 * Exported for tests, but not test-only in the way the ForTest helpers are: it is
 * the real rule, and it is why it carries no ForTest suffix.
 */
export function isAtSessionCapacity(size: number): boolean {
  return size >= maxSessionsTotal;
}

// `now` is a MONOTONIC reading (see monotonicNow), not a wall-clock timestamp.
// Callers never pass it; tests do, to drive expiry deterministically.
/**
 * The expiry commerce stamped into an assertion, in milliseconds from `now`.
 *
 * The token is `v1.<customer id>.<unix expiry>.<mac>` and this reads the expiry
 * WITHOUT verifying the signature — which is safe because of what it is used for:
 * shortening this process's own session. The worst an attacker can do by lying
 * here is give themselves a shorter session. Nothing is authorized on it, ever.
 *
 * Why it is needed at all: commerce mints the assertion at T1 on its clock and
 * this process starts the session at T2 > T1 on its own, so an 8h session paired
 * with an 8h assertion outlives it by the round trip plus any clock skew. Near the
 * boundary that produces exactly the failure the equal TTLs exist to prevent — a
 * live session holding an assertion commerce refuses, surfacing as a 401 at the
 * payment button (ai-review [medium]).
 *
 * This is a BEST-EFFORT UPPER BOUND, not synchronization, and the difference
 * matters (ai-review pass 2). The subtraction mixes this host's wall clock with
 * commerce's, so a storefront clock running behind commerce's still leaves a
 * window where the session outlives the assertion. **Commerce remains
 * authoritative for expiry**; what this removes is the systematic gap (the round
 * trip), not skew. The residual surfaces as a 401, which the island now handles
 * honestly rather than as payment uncertainty.
 *
 * Returns undefined when the token is unreadable, which falls back to the plain
 * TTL rather than refusing: an unparseable assertion is commerce's problem to
 * reject, not a reason to deny someone a session.
 */
function assertionLifetimeMs(assertion: string, wallClockNow = Date.now()): number | undefined {
  const parts = assertion.split('.');
  // Four fields, exactly — commerce's format is `v1.<id>.<expiry>.<mac>`. A token
  // with a stray dot is one commerce will reject anyway, and reading position 2 out
  // of it lands on a fragment (`v1.a.1.5.mac` yields "1", i.e. 1970).
  if (parts.length !== 4) return undefined;
  const field = parts[2];
  // Strict, matching the issuer's grammar exactly. `Number` accepts whitespace,
  // hex, exponent notation and Infinity — none of which commerce's strconv.ParseInt
  // would have produced, so accepting them here means agreeing with a token
  // commerce never minted (ai-review pass 2 [medium]).
  if (!field || !/^\d+$/.test(field)) return undefined;
  return Number(field) * 1000 - wallClockNow;
}

export function createSession(principal: CustomerPrincipal, now = monotonicNow()): string {
  // One pass over the map does both jobs, because both need the same walk:
  // reclaim expired entries (expiry-on-read alone only collects a token that is
  // presented again, and the ones that never come back — the tab someone closed —
  // are the common case), and collect this principal's live tokens for the cap.
  //
  // Deleting from a Map while iterating it is well-defined in JS.
  //
  // `mine` comes out in Map INSERTION order, which is issuance order, and that is
  // the ordering the cap uses. Sorting by `expiresAt` would look equivalent under
  // a fixed TTL and is not: Date.now() is wall-clock and not monotonic, so a host
  // clock stepping backwards between two sign-ins gives the NEWER session the
  // SMALLER expiry, and the next sign-in would evict the one just issued.
  const mine: string[] = [];
  for (const [token, entry] of sessions) {
    if (now >= entry.expiresAt) {
      sessions.delete(token);
    } else if (entry.principal.customerId === principal.customerId) {
      mine.push(token);
    }
  }

  for (let i = 0; i <= mine.length - MAX_SESSIONS_PER_CUSTOMER; i++) {
    sessions.delete(mine[i]!);
  }

  // Then the global bound. Checked AFTER the sweep and the per-principal
  // eviction, so it only ever fires when the map is full of genuinely LIVE
  // sessions belonging to distinct principals — the flood case — and never on
  // entries the two cheaper rules were about to reclaim anyway.
  //
  // Refuses rather than evicting: see MAX_SESSIONS_TOTAL. The caller's own
  // sign-in fails; nobody already signed in is disturbed.
  //
  // **Precedence, stated because the ordering has an observable consequence**
  // (ai-review pass 3): a customer who is already at their own five-session cap
  // succeeds even when the map is full, because the eviction above freed their
  // own oldest slot before this check runs. That is deliberate and it is the
  // right way round — the rule this cap enforces is "nobody's session is taken to
  // make room for a STRANGER", and rotating your own sixth device takes nothing
  // from anyone. Reversing the order would refuse a returning customer during a
  // flood they have no part in, while freeing nothing.
  if (isAtSessionCapacity(sessions.size)) {
    throw new SessionCapacityError();
  }

  // 32 bytes = 256 bits. base64url so it survives a cookie value untouched.
  const token = randomBytes(32).toString('base64url');
  // The session never outlives the assertion it carries. Equal TTLs are not
  // enough on their own — see assertionLifetimeMs.
  const assertionMs = assertionLifetimeMs(principal.assertion);
  const lifetime =
    assertionMs === undefined ? SESSION_TTL_MS : Math.min(SESSION_TTL_MS, assertionMs);
  sessions.set(token, { principal, expiresAt: now + lifetime });
  return token;
}

export function lookupSession(token: string, now = monotonicNow()): CustomerPrincipal | undefined {
  if (!token) return undefined;
  const entry = sessions.get(token);
  if (!entry) return undefined;
  if (now >= entry.expiresAt) {
    // Delete on read rather than leave it: the map is the only thing holding the
    // principal, and an expired entry that lingers is a session a clock
    // adjustment could resurrect.
    sessions.delete(token);
    return undefined;
  }
  return entry.principal;
}

/**
 * Server-side invalidation — the half that matters. Expiring the browser's
 * cookie only asks the browser to forget; anyone who captured the value still
 * holds it.
 */
export function destroySession(token: string): void {
  sessions.delete(token);
}

/**
 * Every session belonging to one customer, ended (TKT-226).
 *
 * This is what a password reset calls, and the threat it closes is the one that
 * makes "reset your password" mean anything: an attacker holding a stolen live
 * session is signed out when the real owner recovers the account. Changing the
 * credential alone would not do it — the session map never re-checks a password.
 *
 * Scoped to ONE customer, never global. A reset must not sign out strangers.
 *
 * Returns how many were destroyed, for the caller's log and because a function
 * whose only evidence is absence is a function a test cannot distinguish from a
 * no-op.
 *
 * WHAT THIS CANNOT DO, said here rather than implied (ADR-050 names it too): it
 * ends sessions in THIS process. Sessions are in-process by ADR-049 §4, so a
 * second replica keeps its own, and a reset completed by calling commerce
 * directly — the operation is public contract — never reaches this function at
 * all. The storefront route is the path that closes the gap; it is not closed by
 * commerce.
 */
export function destroyAllSessionsForCustomer(customerId: string): number {
  let destroyed = 0;
  for (const [token, entry] of sessions) {
    if (entry.principal.customerId === customerId) {
      sessions.delete(token);
      destroyed++;
    }
  }
  return destroyed;
}

/**
 * The cookie is scoped to the whole origin, and that is a DEPARTURE from the back
 * office's `/admin`-scoped cookie. ADR-049 argues it; the short version:
 *
 * The storefront is the gateway's catch-all (`/`) and its pages are locale-first
 * (`/en/…`, `/fr/…`). A cookie scoped to an account subtree is not sent on the
 * pages that need it — the checkout flow lives on `/[locale]/events/[eventId]`
 * (TKT-221) and the claim form on `/[locale]/tickets/[orderRef]` (TKT-223) — and
 * a per-locale path would drop the session on every language switch.
 *
 * The cost is real and is not softened: the browser attaches this token to
 * same-origin requests to `/api/*` (including the seat- and hold-picker's
 * client-side fetches), `/admin/` and `/scanner/`. What makes it acceptable today
 * rather than in principle: no service or gateway log records it —
 * shared/go/obs/requestlog.go logs method, path, status and duration only. That
 * is a property of today's logging, not a guarantee, and docs/development.md
 * carries the standing constraint that request logging must never log the Cookie
 * header.
 *
 * Must match the deletion path exactly, or the browser keeps the old cookie
 * beside the deletion and hands it straight back.
 */
export const SESSION_COOKIE_PATH = '/';

export interface SessionCookieOptions {
  httpOnly: true;
  sameSite: 'lax';
  path: typeof SESSION_COOKIE_PATH;
  maxAge: number;
  secure: boolean;
}

export function sessionCookieOptions({ secure }: { secure: boolean }): SessionCookieOptions {
  return {
    // No script needs to read this, and an XSS that could read it would turn one
    // bug into full session theft.
    httpOnly: true,
    // Lax, not Strict: Strict withholds the cookie on top-level navigations INTO
    // the site (a bookmark, a link in an email), so every arrival would look
    // signed-out. Lax still withholds it from cross-site POSTs, which is the
    // CSRF-relevant half — see gate.ts for the server-side control that does not
    // depend on the browser honouring this at all.
    sameSite: 'lax',
    path: SESSION_COOKIE_PATH,
    maxAge: SESSION_TTL_MS / 1000,
    // Only over TLS. Setting it unconditionally would make the cookie
    // undeliverable on the plain-HTTP local stack, i.e. break sign-in entirely.
    secure,
  };
}

/** Secure iff the PUBLIC origin is https — the gateway reports the real scheme. */
export function isSecureRequest(request: Request): boolean {
  return (request.headers.get('x-forwarded-proto') ?? '').split(',')[0]?.trim() === 'https';
}
