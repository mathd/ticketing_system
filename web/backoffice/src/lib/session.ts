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

import type { StaffRole } from './authorization';

export interface StaffPrincipal {
  staffId: string;
  organizerId: string;
  /**
   * Snapshotted at sign-in (TKT-197). There is no role-change surface today, so
   * nothing can go stale; when one is added it MUST invalidate or refresh live
   * sessions, or a demoted staff member keeps their old role until logout,
   * eviction, restart, or the eight-hour expiry. ADR-042 records this.
   */
  role: StaffRole;
  /**
   * Catalog's signed statement of which organizer this staff member acts for
   * (TKT-245, ADR-058). Forwarded on every catalog write; catalog takes the
   * organizer from here and no longer accepts one in a request body.
   *
   * A CREDENTIAL, and it lives only here: server-side, in this process, never
   * rendered into a page and never handed to browser JavaScript. `organizerId`
   * above stays for reads that are scoped by an explicit parameter, and is the
   * same value this token names — but it is not authority, and nothing may send
   * it as one.
   */
  organizerAssertion: string;
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

/**
 * How many concurrent sessions one staff member may hold. Signing in on a
 * fourth device ends the oldest — normal for a shift-based staff tool, and it
 * is what actually BOUNDS the session map.
 *
 * Without it the map is bounded by nothing (ai-review pass 2, S1): every
 * sign-in mints a new token and the old ones stay live for the full eight
 * hours, so one valid credential can accumulate entries without limit, and the
 * sweep below degrades with them. With it, `sessions.size` is at most
 * `staff headcount x MAX_SESSIONS_PER_STAFF`.
 */
export const MAX_SESSIONS_PER_STAFF = 5;

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
  // One pass over the map does both jobs, because both need the same walk.
  //
  //  - Reclaim expired entries (ai-review pass 1, F4). Expiry-on-read alone only
  //    collects a token that is presented again, and the ones that never come
  //    back — the tab someone closed — are the common case, so the map would
  //    otherwise grow for the life of the process.
  //  - Collect this principal's live tokens for the cap below (ai-review pass 2,
  //    S1). The cap is what makes this walk's cost bounded. An earlier version of
  //    this comment claimed the map was "the staff headcount" and that was simply
  //    wrong: every sign-in mints a new token and old ones live out their full
  //    eight hours, so one credential could inflate `sessions.size` without
  //    limit — and this very loop is what degraded with it.
  //
  // Deleting from a Map while iterating it is well-defined in JS: entries already
  // visited are gone, and no entry is skipped.
  //
  // `mine` comes out in Map INSERTION order, which is issuance order, and that is
  // the ordering the cap uses. An earlier version sorted by `expiresAt` on the
  // reasoning that a fixed TTL makes expiry a proxy for issue time — it is not
  // (ai-review pass 3, T1). `Date.now()` is wall-clock and not monotonic: let the
  // host clock step backwards between two sign-ins and the NEWER session carries
  // the SMALLER expiry, so the next sign-in evicts the session just issued while
  // older ones survive. Insertion order cannot be moved by a clock at all, and no
  // token is ever re-inserted (each is fresh), so it stays true.
  const mine: string[] = [];
  for (const [token, entry] of sessions) {
    if (now >= entry.expiresAt) {
      sessions.delete(token);
    } else if (entry.principal.staffId === principal.staffId) {
      mine.push(token);
    }
  }

  // Make room for the new one: drop this principal's oldest sessions until it is
  // under the cap. Per principal, never global — a global cap would let one busy
  // account evict another staff member's live session, turning a safety limit
  // into a denial of service against colleagues.
  for (let i = 0; i <= mine.length - MAX_SESSIONS_PER_STAFF; i++) {
    sessions.delete(mine[i]!);
  }

  // 32 bytes = 256 bits. base64url so it survives a cookie value untouched.
  const token = randomBytes(32).toString('base64url');
  // The session never outlives the assertion it carries (TKT-245). Equal TTLs are
  // not enough on their own — see assertionLifetimeMs.
  const assertionMs = assertionLifetimeMs(principal.organizerAssertion, now);
  const lifetime = assertionMs === undefined ? SESSION_TTL_MS : Math.min(SESSION_TTL_MS, assertionMs);
  sessions.set(token, { principal, expiresAt: now + lifetime });
  return token;
}

/**
 * The expiry catalog stamped into an assertion, in milliseconds from `now`.
 *
 * Why it is needed at all, when both TTLs are eight hours: catalog mints the
 * assertion at T1 on ITS clock and this process stamps the session at T2, after
 * the round trip. A session created at T2 with an 8h assertion outlives it by the
 * round trip plus any clock skew. Near the boundary that surfaces as a 401 from
 * catalog on a session this process still considers live — a staff member told
 * their write failed, with a session that looks fine, and nothing to point at.
 *
 * Clamping closes the window. Catalog remains the authority on whether an
 * assertion is valid; this only ensures the session cannot promise more than the
 * credential it holds.
 *
 * An unparseable or absent assertion falls back to the session TTL rather than
 * refusing: whether a token is well-formed is catalog's verdict to give, not this
 * process's to pre-empt. (Same shape, same reasoning, as the storefront's
 * customer assertion — web/storefront/src/lib/session.ts.)
 */
function assertionLifetimeMs(assertion: string, wallClockNow: number): number | undefined {
  const parts = assertion.split('.');
  // v1.<staff>.<organizer>.<unix expiry>.<mac>
  if (parts.length !== 5) return undefined;
  const expiry = Number(parts[3]);
  if (!Number.isFinite(expiry)) return undefined;
  const remaining = expiry * 1000 - wallClockNow;
  return remaining > 0 ? remaining : 0;
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
