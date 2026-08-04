// Back-office staff sessions (TKT-190 / US-B1). See ADR-042.
//
// Sessions live in this process and are NOT persisted. A restart signs everyone
// out, and a second replica would not share them — both true, both documented,
// and both acceptable for a single-replica Compose staff tool. The alternative
// costs a table, a migration and a cleanup job to buy durability that nobody has
// asked for; the day someone does, this module is replaced without moving the
// enforcement point (docs/development.md § Back-office sign-in).
//
// The token is opaque: it says nothing about who holds it, so a leaked one
// discloses nothing on its own and cannot be forged from a principal.

import { randomBytes } from 'node:crypto';

export interface StaffPrincipal {
  staffId: string;
  organizerId: string;
}

/** Deliberately unremarkable: a cookie named `admin_token` advertises its worth. */
export const SESSION_COOKIE = 'bo_sid';

/**
 * Absolute lifetime, not idle timeout. An idle window renews on every request,
 * so a stolen cookie stays good for as long as the thief keeps using it; an
 * absolute one ends the session on the clock regardless. Eight hours is one
 * working shift.
 */
export const SESSION_TTL_MS = 8 * 60 * 60 * 1000;

interface Entry {
  principal: StaffPrincipal;
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

export function createSession(principal: StaffPrincipal, now = Date.now()): string {
  // Reclaim on create (ai-review F4). Expiry-on-read alone only collects a token
  // that is presented again — and the entries that never come back are precisely
  // the ones whose owner closed the tab, i.e. the common case. Left alone the map
  // grows monotonically for the process's whole life. Sweeping here bounds it by
  // the number of LIVE sessions instead, and the cost is trivial: it runs once
  // per sign-in, behind a bcrypt comparison that costs far more, over a map whose
  // size is the staff headcount.
  for (const [token, entry] of sessions) {
    if (now >= entry.expiresAt) sessions.delete(token);
  }
  // 32 bytes = 256 bits. base64url so it survives a cookie value untouched.
  const token = randomBytes(32).toString('base64url');
  sessions.set(token, { principal, expiresAt: now + SESSION_TTL_MS });
  return token;
}

export function lookupSession(token: string, now = Date.now()): StaffPrincipal | undefined {
  if (!token) return undefined;
  const entry = sessions.get(token);
  if (!entry) return undefined;
  if (now >= entry.expiresAt) {
    // Delete on read rather than leave it: the map is the only thing holding the
    // principal, and an expired entry that lingers is a session that a clock
    // adjustment could resurrect.
    sessions.delete(token);
    return undefined;
  }
  return entry.principal;
}

/**
 * Server-side invalidation — the half that matters. Expiring the browser's
 * cookie only asks the browser to forget; anyone who captured the value still
 * holds it, and replay is exactly what COS-3 is about.
 */
export function destroySession(token: string): void {
  sessions.delete(token);
}

/**
 * The cookie is scoped to the back office, NOT to the whole origin (ai-review
 * F3). The gateway serves the storefront at `/`, the scanner at `/scanner/` and
 * the service APIs at `/api/*` — all the SAME origin. A `Path=/` cookie is
 * therefore attached by the browser to every storefront page view, every scanner
 * request and every API call, so any access log, error report or diagnostic echo
 * in those unrelated surfaces captures a live, reusable back-office credential.
 * HttpOnly does not help: it stops scripts reading the cookie, not servers
 * receiving it.
 *
 * Must match the deletion path exactly, or the browser keeps the old cookie
 * beside the deletion and hands it straight back.
 */
export const SESSION_COOKIE_PATH = '/admin';

export interface SessionCookieOptions {
  httpOnly: true;
  sameSite: 'lax';
  path: typeof SESSION_COOKIE_PATH;
  maxAge: number;
  secure: boolean;
}

export function sessionCookieOptions({ secure }: { secure: boolean }): SessionCookieOptions {
  return {
    // No script needs to read this, and XSS in a staff tool that could read it
    // would turn one bug into full session theft.
    httpOnly: true,
    // Lax, not Strict: Strict withholds the cookie on top-level navigations INTO
    // the app (a bookmark, a link pasted in a ticket), so every arrival would
    // look signed-out. Lax still withholds it from cross-site POSTs, which is
    // the CSRF-relevant half — see gate.ts for the server-side control that does
    // not depend on the browser honouring this at all.
    sameSite: 'lax',
    path: SESSION_COOKIE_PATH,
    maxAge: SESSION_TTL_MS / 1000,
    // Only over TLS. Setting it unconditionally would make the cookie
    // undeliverable on the plain-HTTP local stack, i.e. break sign-in entirely.
    secure,
  };
}
