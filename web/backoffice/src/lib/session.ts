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
 * Every sign-in mints a fresh token, so this cap bounds both the map and the
 * creation-time sweep by staff headcount.
 */
export const MAX_SESSIONS_PER_STAFF = 5;

interface Entry {
  principal: StaffPrincipal;
  expiresAt: number;
}

/**
 * The clock used to measure in-process session lifetimes.
 *
 * `Date.now()` is wall-clock and can step BACKWARDS — NTP correction, a VM
 * migration, an operator setting the clock. When it does, a session stamped
 * before the step has an `expiresAt` in what is now the future, so a token that
 * should have expired is honoured again. Delete-on-read does not help: the entry
 * is only removed when someone presents it, and the dangerous case is precisely
 * the token that was never presented while it was expired.
 *
 * `performance.now()` cannot go backwards. Sessions do not survive a restart, so
 * a clock that resets with the process costs nothing — there is nothing left to
 * compare against.
 *
 * NOT used for assertion expiry: see assertionLifetimeMs, which subtracts from a
 * Unix timestamp catalog minted and therefore needs the wall clock.
 */
function monotonicNow(): number {
  return performance.now();
}

/**
 * Two clocks are deliberate here.
 *
 * `now` is a MONOTONIC reading and measures this process's own TTL.
 * `wallClockNow` is a Unix timestamp and exists ONLY for assertionLifetimeMs,
 * which computes `expiry * 1000 - wallClockNow` against a value catalog stamped.
 * Pass a monotonic reading there and the subtraction compares milliseconds since
 * process start against the Unix epoch: `remaining` becomes astronomically large,
 * the Math.min clamp below never binds, and the session can outlive the assertion
 * it carries.
 */
export interface StaffSessionStore {
  create(principal: StaffPrincipal, now?: number, wallClockNow?: number): string;
  lookup(token: string, now?: number): StaffPrincipal | undefined;
  destroy(token: string): void;
}

/**
 * Creates an independent session store. Production owns one process-wide instance;
 * unit tests create their own instances so state cannot leak between cases.
 */
export function createSessionStore(): StaffSessionStore {
  const sessions = new Map<string, Entry>();

  function createSession(
    principal: StaffPrincipal,
    now = monotonicNow(),
    wallClockNow = Date.now(),
  ): string {
    // One pass reclaims expired entries and collects this principal's live tokens.
    // Expiry-on-read cannot collect abandoned tokens, while the per-staff cap
    // bounds both the map and this sweep.
    //
    // Deleting from a Map while iterating it is well-defined in JS: entries already
    // visited are gone, and no entry is skipped.
    //
    // Map iteration preserves insertion order, which is issuance order because
    // every token is fresh and is never reinserted. Eviction must use that order:
    // expiry timestamps can reorder when the wall clock moves backwards.
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
    const assertionMs = assertionLifetimeMs(principal.organizerAssertion, wallClockNow);
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
   * An unparseable assertion can only arrive through a hand-built principal or a
   * future construction path: authenticateStaff rejects that shape before creating
   * a production session. Falling back here keeps this store from becoming a second
   * assertion parser while catalog remains the authority on signatures and expiry.
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

  function lookupSession(token: string, now = monotonicNow()): StaffPrincipal | undefined {
    if (!token) return undefined;
    const entry = sessions.get(token);
    if (!entry) return undefined;
    if (now >= entry.expiresAt) {
      // Delete on read rather than leave it: the map is the only thing holding the
      // principal, so dropping it here bounds how long an expired entry occupies
      // memory between sweeps.
      //
      // This only reclaims a presented token. Monotonic expiry prevents an
      // unpresented token from becoming live again after a wall-clock rewind.
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
  function destroySession(token: string): void {
    sessions.delete(token);
  }

  return {
    create: createSession,
    lookup: lookupSession,
    destroy: destroySession,
  };
}

/** The session store owned by this back-office process. */
export const sessionStore = createSessionStore();

/**
 * The cookie is scoped to the back office, not to the whole origin. The gateway
 * serves the storefront at `/`, the scanner at `/scanner/` and
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
