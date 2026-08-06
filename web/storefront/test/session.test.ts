import { beforeEach, describe, expect, it } from 'vitest';

import {
  MAX_SESSIONS_PER_CUSTOMER,
  SESSION_COOKIE_PATH,
  SESSION_TTL_MS,
  createSession,
  destroySession,
  isSecureRequest,
  lookupSession,
  resetSessionsForTest,
  sessionCookieOptions,
  sessionCountForTest,
} from '../src/lib/session';

const alice = { customerId: 'cust-a', email: 'Alice@Example.TEST' };
const bob = { customerId: 'cust-b', email: 'bob@example.test' };

beforeEach(() => {
  resetSessionsForTest();
});

describe('customer sessions', () => {
  it('mints an opaque 256-bit token that resolves to the principal', () => {
    const token = createSession(alice);

    // base64url of 32 bytes: 43 chars, no padding, no +/ characters.
    expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    // The token must not encode who holds it — a leaked one discloses nothing.
    expect(token).not.toContain(alice.customerId);
    expect(lookupSession(token)).toEqual(alice);
  });

  it('does not resolve a token it never issued', () => {
    createSession(alice);
    expect(lookupSession('a'.repeat(43))).toBeUndefined();
    expect(lookupSession('')).toBeUndefined();
  });

  it('expires on an absolute clock and does not renew on use', () => {
    const t0 = 1_000_000;
    const token = createSession(alice, t0);

    // Used repeatedly right up to the deadline — an idle timeout would keep
    // pushing the expiry out, which is exactly what an absolute one must not do.
    expect(lookupSession(token, t0 + SESSION_TTL_MS - 1)).toEqual(alice);
    expect(lookupSession(token, t0 + SESSION_TTL_MS - 1)).toEqual(alice);
    expect(lookupSession(token, t0 + SESSION_TTL_MS)).toBeUndefined();
  });

  it('destroys server-side, so a captured token stops working', () => {
    const token = createSession(alice);
    destroySession(token);
    // The holder still has the string; that is the point. Expiring the browser
    // cookie only asks the browser to forget.
    expect(lookupSession(token)).toBeUndefined();
  });

  it('caps concurrent sessions per customer, evicting the oldest issued', () => {
    const tokens = Array.from({ length: MAX_SESSIONS_PER_CUSTOMER }, () => createSession(alice));
    expect(tokens.every((t) => lookupSession(t))).toBe(true);

    const extra = createSession(alice);

    expect(lookupSession(tokens[0]!)).toBeUndefined();
    expect(lookupSession(tokens[1]!)).toEqual(alice);
    expect(lookupSession(extra)).toEqual(alice);
    expect(sessionCountForTest()).toBe(MAX_SESSIONS_PER_CUSTOMER);
  });

  // Per principal, never global: a global cap would let one busy account evict a
  // stranger's live session, turning a safety limit into a denial of service.
  it("does not evict another customer's sessions", () => {
    const bobs = createSession(bob);
    for (let i = 0; i <= MAX_SESSIONS_PER_CUSTOMER; i++) createSession(alice);

    expect(lookupSession(bobs)).toEqual(bob);
  });

  // Insertion order is issuance order and no clock can move it. Sorting by
  // expiry would look equivalent under a fixed TTL and is not: a host clock
  // stepping backwards gives the NEWER session the SMALLER expiry, so the next
  // sign-in would evict the session just issued.
  it('evicts by issuance order even when the clock steps backwards', () => {
    const first = createSession(alice, 10_000_000);
    const rest = [
      createSession(alice, 9_000_000),
      createSession(alice, 9_000_001),
      createSession(alice, 9_000_002),
      createSession(alice, 9_000_003),
    ];

    createSession(alice, 9_000_004);

    expect(lookupSession(first, 9_000_005)).toBeUndefined();
    expect(rest.every((t) => lookupSession(t, 9_000_005))).toBe(true);
  });

  it('reclaims expired entries rather than letting the map grow', () => {
    const t0 = 1_000_000;
    for (let i = 0; i < 3; i++) createSession(bob, t0);
    expect(sessionCountForTest()).toBe(3);

    // A sign-in well after those expired must collect them: the tokens nobody
    // presents again — the tab someone closed — are the common case, so
    // expiry-on-read alone would leave them for the life of the process.
    createSession(alice, t0 + SESSION_TTL_MS + 1);
    expect(sessionCountForTest()).toBe(1);
  });
});

describe('the session cookie', () => {
  it('is HttpOnly, Lax, and scoped to the whole storefront origin', () => {
    const options = sessionCookieOptions({ secure: true });

    expect(options.httpOnly).toBe(true);
    expect(options.sameSite).toBe('lax');
    expect(options.maxAge).toBe(SESSION_TTL_MS / 1000);
    // `/` and not an account subtree: the checkout flow (TKT-221) and the guest
    // ticket page (TKT-223) are locale-first routes outside any /account prefix,
    // and a per-locale path would drop the session on a language switch.
    // ADR-049 records what this costs.
    expect(options.path).toBe('/');
    expect(SESSION_COOKIE_PATH).toBe('/');
  });

  // Setting Secure unconditionally would make the cookie undeliverable on the
  // plain-HTTP local stack, i.e. break sign-in outright.
  it('is Secure iff the public origin is https', () => {
    const https = new Request('http://storefront:8080/en/account', {
      headers: { 'x-forwarded-proto': 'https, http' },
    });
    const http = new Request('http://storefront:8080/en/account', {
      headers: { 'x-forwarded-proto': 'http' },
    });
    const none = new Request('http://storefront:8080/en/account');

    expect(isSecureRequest(https)).toBe(true);
    expect(isSecureRequest(http)).toBe(false);
    expect(isSecureRequest(none)).toBe(false);
    expect(sessionCookieOptions({ secure: isSecureRequest(https) }).secure).toBe(true);
  });
});
