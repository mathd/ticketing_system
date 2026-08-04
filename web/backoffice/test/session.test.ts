import { beforeEach, describe, expect, it } from 'vitest';

import {
  SESSION_COOKIE,
  MAX_SESSIONS_PER_STAFF,
  SESSION_COOKIE_PATH,
  SESSION_TTL_MS,
  createSession,
  destroySession,
  lookupSession,
  resetSessionsForTest,
  sessionCookieOptions,
  sessionCountForTest,
} from '../src/lib/session';

const principal = { staffId: 'staff-1', organizerId: 'org-1' };

beforeEach(() => {
  resetSessionsForTest();
});

describe('session lifecycle', () => {
  it('issues a token that resolves back to the principal', () => {
    const token = createSession(principal);
    expect(lookupSession(token)).toEqual(principal);
  });

  it('issues a distinct, high-entropy token each time', () => {
    const tokens = new Set(Array.from({ length: 50 }, () => createSession(principal)));
    expect(tokens.size).toBe(50);
    for (const token of tokens) {
      // 256 bits, base64url — enough that guessing is not an attack path.
      expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    }
  });

  it('does not resolve a token it never issued', () => {
    expect(lookupSession('not-a-real-token')).toBeUndefined();
    expect(lookupSession('')).toBeUndefined();
  });

  // COS-3: sign-out invalidates SERVER-side. Clearing the browser's cookie is not
  // invalidation — an attacker who captured the cookie beforehand still holds it.
  it('stops resolving a destroyed token', () => {
    const token = createSession(principal);
    destroySession(token);
    expect(lookupSession(token)).toBeUndefined();
  });

  it('destroying an unknown token is a no-op, not a throw', () => {
    expect(() => destroySession('never-issued')).not.toThrow();
  });

  // The expiry is absolute, not idle: an eight-hour-old session ends whether or
  // not it was in use. A sliding window would let one stolen cookie live forever.
  it('stops resolving a token past its absolute lifetime', () => {
    const issuedAt = 1_000_000;
    const token = createSession(principal, issuedAt);
    expect(lookupSession(token, issuedAt + SESSION_TTL_MS - 1)).toEqual(principal);
    expect(lookupSession(token, issuedAt + SESSION_TTL_MS)).toBeUndefined();
  });

  // ai-review F4: expiry-on-read only collects a token that is presented again,
  // and the entries that never come back — the tab someone just closed — are the
  // common case. Without a sweep the map grows for the process's whole life.
  it('reclaims expired entries that were never presented again', () => {
    const issuedAt = 1_000_000;
    for (let i = 0; i < 5; i++) createSession(principal, issuedAt);
    expect(sessionCountForTest()).toBe(5);

    // One sign-in after they all expire: the abandoned five are gone, not merely
    // unreadable.
    createSession(principal, issuedAt + SESSION_TTL_MS);
    expect(sessionCountForTest()).toBe(1);
  });

  it('does not sweep away sessions that are still live', () => {
    const issuedAt = 1_000_000;
    const live = createSession(principal, issuedAt);
    createSession(principal, issuedAt + 1);
    expect(sessionCountForTest()).toBe(2);
    expect(lookupSession(live, issuedAt + 2)).toEqual(principal);
  });

  it('forgets an expired entry rather than holding it forever', () => {
    const issuedAt = 1_000_000;
    const token = createSession(principal, issuedAt);
    lookupSession(token, issuedAt + SESSION_TTL_MS); // the read that observes expiry
    // Even rewinding the clock cannot bring it back: expiry is a deletion.
    expect(lookupSession(token, issuedAt)).toBeUndefined();
  });
});

describe('session cookie attributes (COS-6)', () => {
  it('is HttpOnly, SameSite=Lax, back-office-scoped and lifetime-bounded', () => {
    const opts = sessionCookieOptions({ secure: false });
    expect(opts.httpOnly).toBe(true);
    // Lax, not Strict: Strict would drop the cookie on a top-level navigation
    // INTO the back office (a bookmark, a link from a ticket), so every arrival
    // would look signed-out. Lax still withholds it from cross-site POSTs, which
    // is the CSRF-relevant half.
    expect(opts.sameSite).toBe('lax');
    // NOT '/' (ai-review F3). The gateway serves the storefront at '/', the
    // scanner at '/scanner/' and the APIs at '/api/*' on the SAME origin, so an
    // origin-wide cookie is handed to all of them on every request — and any log
    // or diagnostic echo there captures a reusable back-office credential.
    expect(opts.path).toBe('/admin');
    expect(opts.path).toBe(SESSION_COOKIE_PATH);
    expect(opts.maxAge).toBe(SESSION_TTL_MS / 1000);
    expect(SESSION_TTL_MS).toBeLessThanOrEqual(24 * 60 * 60 * 1000);
  });

  it('sets Secure only when the public origin is https', () => {
    expect(sessionCookieOptions({ secure: false }).secure).toBe(false);
    expect(sessionCookieOptions({ secure: true }).secure).toBe(true);
  });

  it('names a cookie that does not disclose what it is worth', () => {
    expect(SESSION_COOKIE).not.toMatch(/token|auth|admin/i);
  });
});

describe('the session map is bounded (ai-review pass 2, S1)', () => {
  // Without a per-principal cap the map has no ceiling: every sign-in mints a new
  // token and the old ones live out their full TTL, so ONE valid credential can
  // inflate it without limit — and the sweep inside createSession is what degrades
  // with it. The cap is what makes "bounded by headcount" a true statement instead
  // of a comment.
  it('keeps at most MAX_SESSIONS_PER_STAFF live sessions for one staff member', () => {
    const tokens = Array.from({ length: MAX_SESSIONS_PER_STAFF + 7 }, () =>
      createSession(principal),
    );
    expect(sessionCountForTest()).toBe(MAX_SESSIONS_PER_STAFF);

    // The survivors are the newest; the oldest were evicted to make room.
    const live = tokens.filter((t) => lookupSession(t) !== undefined);
    expect(live).toEqual(tokens.slice(-MAX_SESSIONS_PER_STAFF));
  });

  it('caps per principal, so one busy account cannot evict a colleague', () => {
    const colleague = { staffId: 'staff-2', organizerId: 'org-1' };
    const theirs = createSession(colleague);

    for (let i = 0; i < MAX_SESSIONS_PER_STAFF * 3; i++) createSession(principal);

    // A global cap would have thrown this away; a per-principal one must not.
    expect(lookupSession(theirs)).toEqual(colleague);
    expect(sessionCountForTest()).toBe(MAX_SESSIONS_PER_STAFF + 1);
  });
});
