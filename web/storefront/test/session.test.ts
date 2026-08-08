import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  MAX_SESSIONS_PER_CUSTOMER,
  MAX_SESSIONS_TOTAL,
  SESSION_COOKIE_PATH,
  SESSION_TTL_MS,
  SessionCapacityError,
  createSession,
  destroyAllSessionsForCustomer,
  destroySession,
  isSecureRequest,
  lookupSession,
  resetSessionsForTest,
  sessionCookieOptions,
  sessionCountForTest,
  setMaxSessionsTotalForTest,
} from '../src/lib/session';

// The capacity cases below run against a LOWERED global bound, not the production
// 20 000 (TKT-229).
//
// Why: createSession walks the whole map on every call, so filling it to N costs
// O(N²). At 20 000 each of these four cases did ~200M iterations, took 15-50s against
// vitest's 5s default, and failed `make check` whenever the machine was busy — a gate
// that fails on load certifies nothing.
//
// What this does NOT cost: every rule these tests pin — refusal rather than eviction,
// a bound across distinct principals, own-cap rotation, recovery after a slot frees —
// is a property of the RULE, not of the bound's magnitude. What is no longer covered
// is the production NUMBER: nothing here proves 20 000 specifically, and nothing here
// measures createSession's cost at that size (it is still quadratic in production —
// see the follow-up on TKT-229).
//
// 40, not the floor of 6: enough headroom that a fixture needing "the cap minus a
// handful" still has room, cheap enough to be instant.
const TEST_TOTAL = 40;

// A realistic assertion expiry. createSession caps the session at whatever the
// assertion says, so a fixture with a stale one produces a session that is dead on
// arrival — correct behaviour, useless fixture.
//
// Deliberately FURTHER out than SESSION_TTL_MS, so the cap is not the binding
// constraint here and the TTL tests measure the TTL. The cap's own behaviour is
// tested separately, where it is the point.
const FUTURE = Math.floor(Date.now() / 1000) + 24 * 60 * 60;

const alice = { customerId: 'cust-a', email: 'Alice@Example.TEST', assertion: `v1.cust-a.${FUTURE}.mac` };
const bob = { customerId: 'cust-b', email: 'bob@example.test', assertion: `v1.cust-b.${FUTURE}.mac` };

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

  // ai-review [high]: the per-principal cap bounds the map only if the number of
  // principals is bounded, and registration is PUBLIC — so one actor mints
  // unlimited accounts, each entitled to five sessions. The back office's cap
  // reasoning was inherited here with its premise (an operator-provisioned
  // headcount) removed.
  //
  // Deliberately many DISTINCT principals: a fixture with two customers, which is
  // what the per-principal test uses, cannot observe a global bound at all.
  // Guards the seam itself (TKT-229). The four cases below run at TEST_TOTAL, so
  // nothing else here would notice if the production default were changed — or if a
  // future edit made the module start at the test value. This is the one assertion
  // that still concerns the real number.
  it('enforces the production bound by default, and refuses a test cap that cannot discriminate', () => {
    expect(MAX_SESSIONS_TOTAL).toBe(20_000);
    // Below MAX_SESSIONS_PER_CUSTOMER + 1 there is no room for a stranger alongside
    // one customer at their own cap, so the precedence test would silently stop
    // proving anything. The seam refuses rather than allowing a fixture too small.
    expect(() => setMaxSessionsTotalForTest(MAX_SESSIONS_PER_CUSTOMER)).toThrow(RangeError);
    expect(() => setMaxSessionsTotalForTest(1.5)).toThrow(RangeError);
    expect(() => setMaxSessionsTotalForTest(MAX_SESSIONS_PER_CUSTOMER + 1)).not.toThrow();
  });

  it('bounds the map globally, across unlimited distinct principals', () => {
    setMaxSessionsTotalForTest(TEST_TOTAL);
    let refused = 0;
    for (let i = 0; i < TEST_TOTAL + 50; i++) {
      try {
        createSession({ customerId: `flood-${i}`, email: `flood-${i}@example.test`, assertion: `v1.flood-${i}.${FUTURE}.mac` });
      } catch (cause) {
        if (!(cause instanceof SessionCapacityError)) throw cause;
        refused++;
      }
    }

    expect(sessionCountForTest()).toBeLessThanOrEqual(TEST_TOTAL);
    expect(refused).toBeGreaterThan(0);
  });

  // ai-review pass 2 [high]: the first version of this cap EVICTED oldest-first,
  // which turned memory exhaustion into a targeted availability attack — an
  // attacker who can mint principals freely (registration is public) fills the map
  // and every further sign-in displaces a real customer's live session, silently.
  //
  // Refusing instead leaves everyone already signed in untouched and makes the new
  // sign-in fail loudly. Neither is a fix; TKT-224 is. This test pins which
  // failure the code chooses, because it is the kind of thing a later "cleanup"
  // reverses without noticing.
  it('refuses a new session at capacity rather than evicting a live one', () => {
    setMaxSessionsTotalForTest(TEST_TOTAL);
    const victim = createSession(alice);
    for (let i = 0; i < TEST_TOTAL - 1; i++) {
      createSession({ customerId: `flood-${i}`, email: `flood-${i}@example.test`, assertion: `v1.flood-${i}.${FUTURE}.mac` });
    }

    expect(() => createSession(bob)).toThrow(SessionCapacityError);
    // The buyer who was already signed in is untouched.
    expect(lookupSession(victim)).toEqual(alice);
    expect(sessionCountForTest()).toBe(TEST_TOTAL);
  });

  // ai-review pass 3 [medium]: the two caps interact, and the interaction is
  // observable. A customer already at their OWN five-session cap succeeds even
  // when the map is full, because their own oldest slot is freed before the
  // global check runs.
  //
  // That is the intended precedence, not a bypass — the rule is "nobody's session
  // is taken to make room for a STRANGER", and rotating your own sixth device
  // takes nothing from anyone. Pinned because the previous test used a victim
  // with ONE session and could not observe this case at all.
  it('lets a customer at their own cap rotate a session even when the map is full', () => {
    setMaxSessionsTotalForTest(TEST_TOTAL);
    const mine = Array.from({ length: MAX_SESSIONS_PER_CUSTOMER }, () => createSession(alice));
    for (let i = 0; i < TEST_TOTAL - MAX_SESSIONS_PER_CUSTOMER; i++) {
      createSession({ customerId: `flood-${i}`, email: `flood-${i}@example.test`, assertion: `v1.flood-${i}.${FUTURE}.mac` });
    }
    expect(sessionCountForTest()).toBe(TEST_TOTAL);

    // A stranger is refused...
    expect(() => createSession(bob)).toThrow(SessionCapacityError);
    // ...but alice rotates her own oldest, taking nothing from anyone.
    const rotated = createSession(alice);
    expect(lookupSession(rotated)).toEqual(alice);
    expect(lookupSession(mine[0]!)).toBeUndefined();
    expect(lookupSession(mine[1]!)).toEqual(alice);
    expect(sessionCountForTest()).toBe(TEST_TOTAL);
  });

  // The bound must not be a permanent wedge: capacity freed by expiry or sign-out
  // has to become usable again, or one flood ends sign-in for the process's life.
  it('accepts new sessions again once capacity frees up', () => {
    setMaxSessionsTotalForTest(TEST_TOTAL);
    const doomed = createSession(alice);
    for (let i = 0; i < TEST_TOTAL - 1; i++) {
      createSession({ customerId: `flood-${i}`, email: `flood-${i}@example.test`, assertion: `v1.flood-${i}.${FUTURE}.mac` });
    }
    expect(() => createSession(bob)).toThrow(SessionCapacityError);

    destroySession(doomed);

    expect(() => createSession(bob)).not.toThrow();
  });

  // ai-review [medium]: Date.now() is not monotonic. If the wall clock passes an
  // entry's expiry with nothing reading it, then steps BACKWARDS — NTP, a VM
  // snapshot resume, an operator setting the date — a Date.now() comparison
  // starts resolving the dead token again.
  //
  // Asserting that through the injected `now` parameter proves nothing: a lookup
  // at the expired time DELETES the entry, so the second call would return
  // undefined whatever the clock did. That version of this test passed against a
  // fix that did not work, and the mutation check is what exposed it.
  //
  // What actually has to hold is that expiry does not consult the wall clock AT
  // ALL — while still expiring on the monotonic one.
  //
  // Asserting only "survives a Date.now() jump" is not enough either, and pass 2
  // of the review caught that: an implementation that never expires anything also
  // survives it. So this asserts BOTH halves in one sequence — the wall clock is
  // moved ten TTLs in each direction and cannot kill the session, and the
  // monotonic clock crossing the TTL still does.
  // Third attempt at this test, and the two before it could not fail:
  //
  //  1. injecting `now` on both sides proved nothing, because the lookup at the
  //     expired time DELETES the entry;
  //  2. stubbing Date.now and asserting survival also passes against an
  //     implementation that never expires anything (ai-review pass 3);
  //  3. and injecting `now` ANYWHERE means the production clock default is never
  //     exercised, so swapping monotonicNow back to Date.now would leave the test
  //     green (also pass 3).
  //
  // So this drives the DEFAULT arguments only, controls `performance.now` — the
  // clock the code is supposed to be using — and moves `Date.now` in both
  // directions underneath. It fails if expiry stops happening, and it fails if
  // the default clock goes back to the wall clock.
  it('expires on the default monotonic clock, which the wall clock cannot move', () => {
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      perf.mockReturnValue(1_000);
      const token = createSession(alice);

      // Wall clock leaps ten TTLs forward: irrelevant.
      Date.now = () => realDateNow() + SESSION_TTL_MS * 10;
      expect(lookupSession(token)).toEqual(alice);

      // Wall clock leaps ten TTLs backward: still irrelevant.
      Date.now = () => realDateNow() - SESSION_TTL_MS * 10;
      expect(lookupSession(token)).toEqual(alice);

      // The monotonic clock crossing the TTL is what ends it — with the wall
      // clock still rolled back, so nothing here can be attributed to Date.now.
      perf.mockReturnValue(1_000 + SESSION_TTL_MS);
      expect(lookupSession(token)).toBeUndefined();
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
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

describe('the session never outlives its assertion', () => {
  // ai-review [medium]: commerce mints the assertion at T1 on its clock and this
  // process starts the session at T2 > T1 on its own, so equal TTLs are not
  // enough — the session outlives the assertion by the round trip plus any skew.
  // Near the boundary that is a live session holding a token commerce refuses,
  // surfacing as a 401 at the payment button.
  it('caps the session at the assertion expiry when that is sooner', () => {
    const soon = Math.floor(Date.now() / 1000) + 60; // one minute, far inside the 8h TTL
    const token = createSession({ ...alice, assertion: `v1.cust-a.${soon}.mac` });

    expect(lookupSession(token)).toEqual(expect.objectContaining({ customerId: 'cust-a' }));
    // A monotonic reading two minutes on — past the assertion, nowhere near the TTL.
    expect(lookupSession(token, performance.now() + 120_000)).toBeUndefined();
  });

  // An assertion that is already dead must not mint a usable session at all.
  it('refuses to hand out a session backed by an expired assertion', () => {
    const past = Math.floor(Date.now() / 1000) - 60;
    const token = createSession({ ...alice, assertion: `v1.cust-a.${past}.mac` });

    expect(lookupSession(token)).toBeUndefined();
  });

  // An unreadable assertion is commerce's problem to reject, not a reason to deny
  // someone a session here — it falls back to the plain TTL.
  it('falls back to the full TTL when the assertion is unreadable', () => {
    const token = createSession({ ...alice, assertion: 'not-a-token' });

    expect(lookupSession(token)).toEqual(expect.objectContaining({ customerId: 'cust-a' }));
  });
});

// ai-review pass 2 [medium]: `Number` accepts whitespace, hex, exponent notation
// and Infinity — values commerce's strconv.ParseInt could never have produced, so
// honouring them means agreeing with a token commerce never minted. They fall back
// to the plain TTL instead.
describe('the assertion expiry is parsed strictly', () => {
  it.each(['0x7fffffff', ' 99999999999', '1e12', 'Infinity', '1.5', '-1'])(
    'ignores %s and falls back to the full TTL',
    (field) => {
      const token = createSession({ ...alice, assertion: `v1.cust-a.${field}.mac` });
      expect(lookupSession(token)).toEqual(expect.objectContaining({ customerId: 'cust-a' }));
      expect(lookupSession(token, performance.now() + SESSION_TTL_MS - 1000)).toBeDefined();
    },
  );
});

// Password recovery invalidates the customer's sessions (TKT-226).
//
// This is the half that makes a reset mean anything. Changing the credential does not
// touch this map — it never re-checks a password — so an attacker holding a stolen
// live session keeps it until something ends it explicitly.
describe('destroying every session for one customer', () => {
  it('ends all of that customer’s sessions and reports how many', () => {
    const first = createSession(alice);
    const second = createSession(alice);

    expect(destroyAllSessionsForCustomer(alice.customerId)).toBe(2);

    expect(lookupSession(first)).toBeUndefined();
    expect(lookupSession(second)).toBeUndefined();
  });

  // The threat this closes, written as a test: the owner resets, the thief is out.
  it('signs out a stolen session when the owner resets', () => {
    const stolen = createSession(alice);
    expect(lookupSession(stolen)).toBeDefined();

    destroyAllSessionsForCustomer(alice.customerId);

    expect(lookupSession(stolen)).toBeUndefined();
  });

  // Scoped, never global. A reset must not sign out strangers.
  it('leaves other customers alone', () => {
    const hers = createSession(alice);
    const his = createSession(bob);

    expect(destroyAllSessionsForCustomer(alice.customerId)).toBe(1);

    expect(lookupSession(hers)).toBeUndefined();
    expect(lookupSession(his)).toEqual(bob);
  });

  // Reclaimed, not merely unresolvable: an entry left in the map is memory the caps
  // still count and a clock adjustment could resurrect.
  it('reclaims the entries rather than hiding them', () => {
    createSession(alice);
    createSession(alice);
    createSession(bob);

    destroyAllSessionsForCustomer(alice.customerId);

    expect(sessionCountForTest()).toBe(1);
  });

  it('is a no-op for a customer with no sessions', () => {
    createSession(bob);

    expect(destroyAllSessionsForCustomer('cust-nobody')).toBe(0);
    expect(sessionCountForTest()).toBe(1);
  });
});
