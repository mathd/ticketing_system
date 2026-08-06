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
 * How many concurrent sessions one customer may hold. Signing in on a sixth
 * device ends the first.
 *
 * This is what actually BOUNDS the session map. Sweeping expired entries is not a
 * bound: every sign-in mints a new token while the old ones live out their full
 * TTL, so one valid credential could accumulate entries without limit — and the
 * sweep degrades with them. ADR-042 records that a review pass caught exactly
 * this claim in the back office.
 *
 * Per principal, never global. A global cap would let one busy account evict a
 * different customer's live session, turning a safety limit into a denial of
 * service against strangers.
 */
export const MAX_SESSIONS_PER_CUSTOMER = 5;

interface Entry {
  principal: CustomerPrincipal;
  expiresAt: number;
}

const sessions = new Map<string, Entry>();

/** Test-only: sessions are module state, so suites must not leak into each other. */
export function resetSessionsForTest(): void {
  sessions.clear();
}

/** Test-only: proves a sweep actually reclaimed entries rather than hiding them. */
export function sessionCountForTest(): number {
  return sessions.size;
}

export function createSession(principal: CustomerPrincipal, now = Date.now()): string {
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

  // 32 bytes = 256 bits. base64url so it survives a cookie value untouched.
  const token = randomBytes(32).toString('base64url');
  sessions.set(token, { principal, expiresAt: now + SESSION_TTL_MS });
  return token;
}

export function lookupSession(token: string, now = Date.now()): CustomerPrincipal | undefined {
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
