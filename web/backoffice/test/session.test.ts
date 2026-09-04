import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  SESSION_COOKIE,
  MAX_SESSIONS_PER_STAFF,
  SESSION_COOKIE_PATH,
  SESSION_TTL_MS,
  createSessionStore,
  sessionCookieOptions,
} from '../src/lib/session';

// The assertion's expiry is far in the future so the TTL clamp (TKT-245) is not
// what these pre-existing session tests are measuring: they are about the 8h
// session lifetime, the cap and the sweep. The clamp has its own tests below.
const FAR_FUTURE_ASSERTION =
  'v1.11111111-1111-1111-1111-111111111111.22222222-2222-2222-2222-222222222222.99999999999.mac';
let sessions = createSessionStore();

const principal = {
  staffId: 'staff-1',
  organizerId: 'org-1',
  role: 'admin',
  organizerAssertion: FAR_FUTURE_ASSERTION,
} as const;

beforeEach(() => {
  sessions = createSessionStore();
});

describe('session lifecycle', () => {
  it('keeps independent stores isolated', () => {
    const other = createSessionStore();
    const token = sessions.create(principal);

    expect(other.lookup(token)).toBeUndefined();
    expect(sessions.lookup(token)).toEqual(principal);
  });

  it('issues a token that resolves back to the principal', () => {
    const token = sessions.create(principal);
    expect(sessions.lookup(token)).toEqual(principal);
  });

  it('issues a distinct, high-entropy token each time', () => {
    const tokens = new Set(Array.from({ length: 50 }, () => sessions.create(principal)));
    expect(tokens.size).toBe(50);
    for (const token of tokens) {
      // 256 bits, base64url — enough that guessing is not an attack path.
      expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    }
  });

  it('does not resolve a token it never issued', () => {
    expect(sessions.lookup('not-a-real-token')).toBeUndefined();
    expect(sessions.lookup('')).toBeUndefined();
  });

  // COS-3: sign-out invalidates SERVER-side. Clearing the browser's cookie is not
  // invalidation — an attacker who captured the cookie beforehand still holds it.
  it('stops resolving a destroyed token', () => {
    const token = sessions.create(principal);
    sessions.destroy(token);
    expect(sessions.lookup(token)).toBeUndefined();
  });

  it('destroying an unknown token is a no-op, not a throw', () => {
    expect(() => sessions.destroy('never-issued')).not.toThrow();
  });

  // The expiry is absolute, not idle: an eight-hour-old session ends whether or
  // not it was in use. A sliding window would let one stolen cookie live forever.
  it('stops resolving a token past its absolute lifetime', () => {
    const issuedAt = 1_000_000;
    const token = sessions.create(principal, issuedAt);
    expect(sessions.lookup(token, issuedAt + SESSION_TTL_MS - 1)).toEqual(principal);
    expect(sessions.lookup(token, issuedAt + SESSION_TTL_MS)).toBeUndefined();
  });

  it('does not sweep away sessions that are still live', () => {
    const issuedAt = 1_000_000;
    const live = sessions.create(principal, issuedAt);
    const next = sessions.create(principal, issuedAt + 1);
    expect(sessions.lookup(live, issuedAt + 2)).toEqual(principal);
    expect(sessions.lookup(next, issuedAt + 2)).toEqual(principal);
  });

  it('forgets an expired entry rather than holding it forever', () => {
    const issuedAt = 1_000_000;
    const token = sessions.create(principal, issuedAt);
    sessions.lookup(token, issuedAt + SESSION_TTL_MS); // the read that observes expiry
    // Even rewinding the clock cannot bring it back: expiry is a deletion.
    expect(sessions.lookup(token, issuedAt)).toBeUndefined();
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
      sessions.create(principal),
    );

    // The survivors are the newest; the oldest were evicted to make room.
    const live = tokens.filter((t) => sessions.lookup(t) !== undefined);
    expect(live).toEqual(tokens.slice(-MAX_SESSIONS_PER_STAFF));
  });

  it('caps per principal, so one busy account cannot evict a colleague', () => {
    const colleague = {
      staffId: 'staff-2',
      organizerId: 'org-1',
      role: 'box_office',
      organizerAssertion: FAR_FUTURE_ASSERTION,
    } as const;
    const theirs = sessions.create(colleague);

    const mine = Array.from({ length: MAX_SESSIONS_PER_STAFF * 3 }, () =>
      sessions.create(principal),
    );

    // A global cap would have thrown this away; a per-principal one must not.
    expect(sessions.lookup(theirs)).toEqual(colleague);
    expect(
      mine.slice(-MAX_SESSIONS_PER_STAFF).every((token) => sessions.lookup(token)),
    ).toBe(true);
  });

  // ai-review pass 3, T1. The previous implementation evicted the smallest
  // `expiresAt`, on the reasoning that a fixed TTL makes expiry a proxy for issue
  // time. It is not: Date.now() is wall-clock and not monotonic, so if the host
  // clock steps backwards (NTP correction, a manual change) the NEWER session
  // carries the SMALLER expiry and the next sign-in evicts the session just
  // issued, while genuinely older ones survive.
  //
  // The earlier cap test could not catch this — with an implicit clock its
  // timestamps and the Map's insertion order agreed, so both orderings gave the
  // same answer. This one drives `now` backwards explicitly, which is the only
  // input shape that can tell them apart.
  it('evicts the earliest-ISSUED session even when the clock runs backwards', () => {
    const t0 = 5_000_000;
    const oldest = sessions.create(principal, t0);
    const older = sessions.create(principal, t0 + 1000);
    const mid = sessions.create(principal, t0 + 2000);
    const newer = sessions.create(principal, t0 + 3000);

    // The clock jumps back. This session is issued LAST but expires FIRST.
    const newestButExpiringSoonest = sessions.create(principal, t0 - 60_000);

    // A sixth sign-in must retire `oldest` — not the one just issued.
    const sixth = sessions.create(principal, t0 + 4000);

    expect(sessions.lookup(oldest, t0 + 4000)).toBeUndefined();
    for (const [name, token] of Object.entries({ older, mid, newer, newestButExpiringSoonest, sixth })) {
      expect(sessions.lookup(token, t0 + 4000), `${name} must have survived`).toBeDefined();
    }
  });
});

// TKT-245: the session never outlives the organizer assertion it carries.
//
// Equal TTLs are not enough on their own, which is the whole reason this exists.
// Catalog mints at T1 on ITS clock; this process stamps the session at T2, after
// the round trip. Without the clamp a session created at T2 with an 8h assertion
// outlives its credential by the round trip plus any clock skew — surfacing near
// the boundary as a 401 from catalog on a session this process still believes is
// live, which is the least diagnosable failure available to a staff member.
describe('session lifetime is clamped to the assertion (TKT-245)', () => {
  /** An assertion in catalog's format, expiring `secondsFromNow` after `now`. */
  const assertionExpiringAt = (nowMs: number, secondsFromNow: number) =>
    `v1.11111111-1111-1111-1111-111111111111.22222222-2222-2222-2222-222222222222.${
      Math.floor(nowMs / 1000) + secondsFromNow
    }.mac`;

  // TKT-302. The two tests below are the ones that fail if the monotonic clock is
  // ever reverted, or if the two clocks are merged back into one parameter.
  //
  // Neither is covered by the test that follows them: that one passes `now`
  // EXPLICITLY, so it drives both clocks with the same value and stays green
  // under a merge. These drive the DEFAULTS, which is where the bug lives.
  it('expires on the default monotonic clock, which the wall clock cannot move', () => {
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      perf.mockReturnValue(1_000);
      const token = sessions.create(principal);

      // Wall clock leaps ten TTLs forward: irrelevant to an in-process TTL.
      Date.now = () => realDateNow() + SESSION_TTL_MS * 10;
      expect(sessions.lookup(token)).toEqual(principal);

      // Wall clock leaps ten TTLs BACKWARD — the resurrection case. An expired
      // entry that was never presented must not come back to life.
      Date.now = () => realDateNow() - SESSION_TTL_MS * 10;
      expect(sessions.lookup(token)).toEqual(principal);

      // The monotonic clock crossing the TTL is what ends it, with the wall
      // clock still rolled back so nothing here is attributable to Date.now.
      perf.mockReturnValue(1_000 + SESSION_TTL_MS);
      expect(sessions.lookup(token)).toBeUndefined();
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
  });

  it('clamps to the assertion expiry using the WALL clock, not the monotonic one', () => {
    // The merge trap (TKT-302 plan-final): assertionLifetimeMs subtracts from a
    // Unix timestamp catalog minted. Feed it a monotonic reading and
    // `expiry * 1000 - performance.now()` is astronomically large, Math.min
    // never binds, and the session outlives its assertion.
    //
    // performance.now() is small (ms since process start) while Date.now() is
    // ~1.8e12, so a merge is observable: the clamp silently stops working.
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      const wall = 1_800_000_000_000;
      Date.now = () => wall;
      perf.mockReturnValue(5_000);

      // One hour of assertion left, far short of the 8h session TTL.
      const shortLived = { ...principal, organizerAssertion: assertionExpiringAt(wall, 60 * 60) };
      const token = sessions.create(shortLived);

      // Alive just before the assertion dies, on the MONOTONIC clock.
      perf.mockReturnValue(5_000 + 59 * 60 * 1000);
      expect(sessions.lookup(token)).toBeDefined();

      // Gone once it has — which only happens if the clamp saw the wall clock.
      perf.mockReturnValue(5_000 + 61 * 60 * 1000);
      expect(sessions.lookup(token)).toBeUndefined();

      // Proof the assertion ended it rather than the session TTL.
      expect(61 * 60 * 1000).toBeLessThan(SESSION_TTL_MS);
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
  });

  it('ends the session when the assertion dies first, not eight hours later', () => {
    const now = 1_800_000_000_000;
    // One hour of assertion left — far short of the 8h session TTL.
    const shortLived = {
      ...principal,
      organizerAssertion: assertionExpiringAt(now, 60 * 60),
    };

    const token = sessions.create(shortLived, now, now);

    // Alive just before the assertion expires...
    expect(sessions.lookup(token, now + 59 * 60 * 1000)).toBeDefined();
    // ...and gone once it has, rather than surviving to the session's own 8h.
    expect(sessions.lookup(token, now + 61 * 60 * 1000)).toBeUndefined();
    // Without the clamp this would still be live: proof the assertion is what
    // ended it, not SESSION_TTL_MS.
    expect(now + 61 * 60 * 1000).toBeLessThan(now + SESSION_TTL_MS);
  });

  it('does not EXTEND the session when the assertion outlives it', () => {
    const now = 1_800_000_000_000;
    const longLived = {
      ...principal,
      organizerAssertion: assertionExpiringAt(now, 48 * 60 * 60),
    };

    const token = sessions.create(longLived, now, now);

    // The session's own 8h still governs: the clamp is a minimum, not a lease.
    expect(sessions.lookup(token, now + SESSION_TTL_MS - 1000)).toBeDefined();
    expect(sessions.lookup(token, now + SESSION_TTL_MS + 1000)).toBeUndefined();
  });

  // Whether a token is well-formed is catalog's verdict to give. This process
  // pre-judging it would turn a format change into a silent sign-out loop here
  // rather than a clear refusal there.
  it('falls back to the session TTL when the assertion cannot be parsed', () => {
    const now = 1_800_000_000_000;
    for (const bad of ['', 'not-a-token', 'v1.only.three.parts', 'v1.a.b.not-a-number.mac']) {
      sessions = createSessionStore();
      const token = sessions.create({ ...principal, organizerAssertion: bad }, now, now);
      expect(sessions.lookup(token, now + SESSION_TTL_MS - 1000), `bad=${bad}`).toBeDefined();
      expect(sessions.lookup(token, now + SESSION_TTL_MS + 1000), `bad=${bad}`).toBeUndefined();
    }
  });

  // An already-dead assertion must not produce a session with a NEGATIVE
  // lifetime that some future arithmetic reads as "very old" or, worse, wraps.
  it('creates an already-expired session when the assertion is already dead', () => {
    const now = 1_800_000_000_000;
    const dead = { ...principal, organizerAssertion: assertionExpiringAt(now, -60) };
    const token = sessions.create(dead, now, now);
    expect(sessions.lookup(token, now)).toBeUndefined();
  });
});
