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
 * Eviction is oldest-issued-first. Under the flood this exists for, that means
 * real customers get signed out — which is a bad day, and strictly better than
 * the SSR process dying and taking the whole storefront with it. The signature of
 * hitting it is customers being signed out for no reason, and the actual fix for
 * the cause is rate limiting (TKT-224), not a larger number here.
 *
 * 20 000 tokens is ~2 MB of map at this entry size — far above any real
 * concurrent-buyer count for a single-replica stack, and far below anything that
 * threatens the process.
 */
export const MAX_SESSIONS_TOTAL = 20_000;

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

/** Test-only: sessions are module state, so suites must not leak into each other. */
export function resetSessionsForTest(): void {
  sessions.clear();
}

/** Test-only: proves a sweep actually reclaimed entries rather than hiding them. */
export function sessionCountForTest(): number {
  return sessions.size;
}

// `now` is a MONOTONIC reading (see monotonicNow), not a wall-clock timestamp.
// Callers never pass it; tests do, to drive expiry deterministically.
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

  // Then the global bound. Runs AFTER the sweep and the per-principal eviction so
  // it only ever fires on genuinely live sessions belonging to distinct
  // principals — the flood case — and never on entries the two cheaper rules were
  // about to reclaim anyway. Oldest issued goes first; Map iteration is insertion
  // order, which no clock can move.
  if (sessions.size >= MAX_SESSIONS_TOTAL) {
    for (const token of sessions.keys()) {
      if (sessions.size < MAX_SESSIONS_TOTAL) break;
      sessions.delete(token);
    }
  }

  // 32 bytes = 256 bits. base64url so it survives a cookie value untouched.
  const token = randomBytes(32).toString('base64url');
  sessions.set(token, { principal, expiresAt: now + SESSION_TTL_MS });
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
