import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  MAX_SESSIONS_PER_CUSTOMER,
  MAX_SESSIONS_TOTAL,
  SESSION_COOKIE_PATH,
  SESSION_TTL_MS,
  SessionCapacityError,
  createSessionStore,
  isSecureRequest,
  sessionCookieOptions,
} from '../src/lib/session';

// The capacity cases below run against a LOWERED global bound, not the production
// 20 000 (TKT-229).
//
// Why: createSession walks the whole map on every call, so filling it to N costs
// O(N²). At 20 000 each affected case did ~200M iterations, took 15-50s against
// vitest's 5s default, and failed `make check` whenever the machine was busy — a gate
// that fails on load certifies nothing.
//
// These rules do not depend on the bound's magnitude. The production value is
// pinned separately, but no test fills the map to 20 000 or measures that runtime;
// createSession remains quadratic at that size (see the follow-up on TKT-229).
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
const floodCustomer = (id: string) => ({
  customerId: id,
  email: `${id}@example.test`,
  assertion: `v1.${id}.${FUTURE}.mac`,
});
const createProductionSizedStore = () =>
  createSessionStore({ maxSessionsTotal: MAX_SESSIONS_TOTAL });

function createClockedStore(monotonicTime: number, wallTime = Date.now()) {
  const clock = { monotonicTime, wallTime };
  const store = createSessionStore({
    maxSessionsTotal: MAX_SESSIONS_TOTAL,
    monotonicClock: () => clock.monotonicTime,
    wallClock: () => clock.wallTime,
  });
  return { clock, store };
}

let sessions = createProductionSizedStore();

beforeEach(() => {
  sessions = createProductionSizedStore();
});

describe('customer sessions', () => {
  it('keeps independent stores isolated', () => {
    const other = createProductionSizedStore();
    const token = sessions.create(alice);

    expect(other.lookup(token)).toBeUndefined();
    expect(sessions.lookup(token)).toEqual(alice);
  });

  it('mints an opaque 256-bit token that resolves to the principal', () => {
    const token = sessions.create(alice);

    // base64url of 32 bytes: 43 chars, no padding, no +/ characters.
    expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    // The token must not encode who holds it — a leaked one discloses nothing.
    expect(token).not.toContain(alice.customerId);
    expect(sessions.lookup(token)).toEqual(alice);
  });

  it('does not resolve a token it never issued', () => {
    sessions.create(alice);
    expect(sessions.lookup('a'.repeat(43))).toBeUndefined();
    expect(sessions.lookup('')).toBeUndefined();
  });

  it('expires on an absolute clock and does not renew on use', () => {
    const t0 = 1_000_000;
    const timed = createClockedStore(t0);
    const token = timed.store.create(alice);

    // Used repeatedly right up to the deadline — an idle timeout would keep
    // pushing the expiry out, which is exactly what an absolute one must not do.
    timed.clock.monotonicTime = t0 + SESSION_TTL_MS - 1;
    expect(timed.store.lookup(token)).toEqual(alice);
    expect(timed.store.lookup(token)).toEqual(alice);
    timed.clock.monotonicTime++;
    expect(timed.store.lookup(token)).toBeUndefined();
  });

  it('destroys server-side, so a captured token stops working', () => {
    const token = sessions.create(alice);
    sessions.destroy(token);
    // The holder still has the string; that is the point. Expiring the browser
    // cookie only asks the browser to forget.
    expect(sessions.lookup(token)).toBeUndefined();
  });

  it('caps concurrent sessions per customer, evicting the oldest issued', () => {
    const tokens = Array.from({ length: MAX_SESSIONS_PER_CUSTOMER }, () => sessions.create(alice));
    expect(tokens.every((t) => sessions.lookup(t))).toBe(true);

    const extra = sessions.create(alice);

    expect(sessions.lookup(tokens[0]!)).toBeUndefined();
    expect(
      tokens
        .slice(1)
        .every((token) => sessions.lookup(token)?.customerId === alice.customerId),
    ).toBe(true);
    expect(sessions.lookup(extra)).toEqual(alice);
  });

  // Per principal, never global: a global cap would let one busy account evict a
  // stranger's live session, turning a safety limit into a denial of service.
  it("does not evict another customer's sessions", () => {
    const bobs = sessions.create(bob);
    for (let i = 0; i <= MAX_SESSIONS_PER_CUSTOMER; i++) sessions.create(alice);

    expect(sessions.lookup(bobs)).toEqual(bob);
  });

  // Insertion order is issuance order and no clock can move it. Sorting by
  // expiry would look equivalent under a fixed TTL and is not: a host clock
  // stepping backwards gives the NEWER session the SMALLER expiry, so the next
  // sign-in would evict the session just issued.
  it('evicts by issuance order even when the clock steps backwards', () => {
    const timed = createClockedStore(10_000_000);
    const first = timed.store.create(alice);
    const rest: string[] = [];
    for (let now = 9_000_000; now <= 9_000_003; now++) {
      timed.clock.monotonicTime = now;
      rest.push(timed.store.create(alice));
    }

    timed.clock.monotonicTime = 9_000_004;
    timed.store.create(alice);

    timed.clock.monotonicTime = 9_000_005;
    expect(timed.store.lookup(first)).toBeUndefined();
    expect(rest.every((token) => timed.store.lookup(token))).toBe(true);
  });

  it('sets the production bound to 20,000 sessions', () => {
    expect(MAX_SESSIONS_TOTAL).toBe(20_000);
  });

  it('keeps a configured bound local to its store', () => {
    const limited = createSessionStore({ maxSessionsTotal: TEST_TOTAL });
    for (let i = 0; i < TEST_TOTAL; i++) limited.create(floodCustomer(`limited-${i}`));
    expect(() => limited.create(bob)).toThrow(SessionCapacityError);

    for (let i = 0; i < TEST_TOTAL; i++) sessions.create(floodCustomer(`roomy-${i}`));
    const beyondLimitedCap = sessions.create(floodCustomer('roomy-extra'));
    expect(sessions.lookup(beyondLimitedCap)).toEqual(floodCustomer('roomy-extra'));
  });

  // Every other capacity case uses TEST_TOTAL, so a threshold hard-coded to 40
  // could pass them all. Driving create at a second limit proves the configured
  // value reaches the operation that enforces it.
  it('refuses at whatever limit is injected, not at one fixed number', () => {
    const OTHER = TEST_TOTAL + 7;
    sessions = createSessionStore({ maxSessionsTotal: OTHER });

    for (let i = 0; i < OTHER; i++) {
      sessions.create(floodCustomer(`other-${i}`));
    }

    // Full at OTHER, not at TEST_TOTAL — a cap fixed at 40 would have refused seven
    // sessions ago.
    expect(() => sessions.create(bob)).toThrow(SessionCapacityError);
  });

  // The seam's floor. Below MAX_SESSIONS_PER_CUSTOMER + 1 there is no room for a
  // stranger alongside one customer at their own cap, so the precedence test would
  // silently stop proving anything — a fixture too small to show the negative.
  it('refuses a test cap that could not discriminate', () => {
    expect(() =>
      createSessionStore({ maxSessionsTotal: MAX_SESSIONS_PER_CUSTOMER }),
    ).toThrow(RangeError);
    expect(() => createSessionStore({ maxSessionsTotal: 1.5 })).toThrow(RangeError);
    expect(() =>
      createSessionStore({ maxSessionsTotal: MAX_SESSIONS_PER_CUSTOMER + 1 }),
    ).not.toThrow();
  });

  // The per-principal cap bounds the map only if the number of principals is
  // bounded, and registration is public, so one actor can mint
  // unlimited accounts, each entitled to five sessions. The back office's cap
  // reasoning was inherited here with its premise (an operator-provisioned
  // headcount) removed.
  //
  // Deliberately many DISTINCT principals: a fixture with two customers, which is
  // what the per-principal test uses, cannot observe a global bound at all.
  it('bounds the map globally, across unlimited distinct principals', () => {
    sessions = createSessionStore({ maxSessionsTotal: TEST_TOTAL });
    let refused = 0;
    const accepted: string[] = [];
    for (let i = 0; i < TEST_TOTAL + 50; i++) {
      try {
        accepted.push(sessions.create(floodCustomer(`bounded-${i}`)));
      } catch (cause) {
        if (!(cause instanceof SessionCapacityError)) throw cause;
        refused++;
      }
    }

    expect(accepted).toHaveLength(TEST_TOTAL);
    expect(accepted.every((token) => sessions.lookup(token) !== undefined)).toBe(true);
    expect(refused).toBe(50);
  });

  // Evicting oldest-first would turn memory exhaustion into a targeted
  // availability attack: an
  // attacker who can mint principals freely (registration is public) fills the map
  // and every further sign-in displaces a real customer's live session, silently.
  //
  // Refusing instead leaves everyone already signed in untouched and makes the new
  // sign-in fail loudly. Neither is a fix; TKT-224 is. This test pins which
  // failure the code chooses.
  it('refuses a new session at capacity rather than evicting a live one', () => {
    sessions = createSessionStore({ maxSessionsTotal: TEST_TOTAL });
    const victim = sessions.create(alice);
    for (let i = 0; i < TEST_TOTAL - 1; i++) {
      sessions.create(floodCustomer(`victim-flood-${i}`));
    }

    expect(() => sessions.create(bob)).toThrow(SessionCapacityError);
    // The buyer who was already signed in is untouched.
    expect(sessions.lookup(victim)).toEqual(alice);
  });

  // The two caps interact. A customer already at their own five-session cap
  // succeeds even
  // when the map is full, because their own oldest slot is freed before the
  // global check runs.
  //
  // That is the intended precedence, not a bypass — the rule is "nobody's session
  // is taken to make room for a STRANGER", and rotating your own sixth device
  // takes nothing from anyone.
  it('lets a customer at their own cap rotate a session even when the map is full', () => {
    sessions = createSessionStore({ maxSessionsTotal: TEST_TOTAL });
    const mine = Array.from({ length: MAX_SESSIONS_PER_CUSTOMER }, () => sessions.create(alice));
    for (let i = 0; i < TEST_TOTAL - MAX_SESSIONS_PER_CUSTOMER; i++) {
      sessions.create(floodCustomer(`rotation-flood-${i}`));
    }

    // A stranger is refused...
    expect(() => sessions.create(bob)).toThrow(SessionCapacityError);
    // ...but alice rotates her own oldest, taking nothing from anyone.
    const rotated = sessions.create(alice);
    expect(sessions.lookup(rotated)).toEqual(alice);
    expect(sessions.lookup(mine[0]!)).toBeUndefined();
    expect(sessions.lookup(mine[1]!)).toEqual(alice);
  });

  // The bound must not be a permanent wedge: capacity freed by expiry or sign-out
  // has to become usable again, or one flood ends sign-in for the process's life.
  it('accepts new sessions again once capacity frees up', () => {
    sessions = createSessionStore({ maxSessionsTotal: TEST_TOTAL });
    const doomed = sessions.create(alice);
    for (let i = 0; i < TEST_TOTAL - 1; i++) {
      sessions.create(floodCustomer(`recovery-flood-${i}`));
    }
    expect(() => sessions.create(bob)).toThrow(SessionCapacityError);

    sessions.destroy(doomed);

    expect(() => sessions.create(bob)).not.toThrow();
  });

  // Date.now can move backwards after an entry should have expired. This test
  // drives the production clock defaults and separates both obligations: wall
  // clock jumps cannot expire a session, while the monotonic deadline must.
  it('expires on the default monotonic clock, which the wall clock cannot move', () => {
    const realDateNow = Date.now;
    const perf = vi.spyOn(performance, 'now');
    try {
      perf.mockReturnValue(1_000);
      const token = sessions.create(alice);

      // Wall clock leaps ten TTLs forward: irrelevant.
      Date.now = () => realDateNow() + SESSION_TTL_MS * 10;
      expect(sessions.lookup(token)).toEqual(alice);

      // Wall clock leaps ten TTLs backward: still irrelevant.
      Date.now = () => realDateNow() - SESSION_TTL_MS * 10;
      expect(sessions.lookup(token)).toEqual(alice);

      // The monotonic clock crossing the TTL is what ends it — with the wall
      // clock still rolled back, so nothing here can be attributed to Date.now.
      perf.mockReturnValue(1_000 + SESSION_TTL_MS);
      expect(sessions.lookup(token)).toBeUndefined();
    } finally {
      Date.now = realDateNow;
      perf.mockRestore();
    }
  });

  it('reclaims expired entries rather than letting the map grow', () => {
    const limit = MAX_SESSIONS_PER_CUSTOMER + 1;
    const t0 = 1_000_000;
    const clock = { now: t0 };
    sessions = createSessionStore({
      maxSessionsTotal: limit,
      monotonicClock: () => clock.now,
    });
    const expired = Array.from({ length: limit }, (_, i) =>
      sessions.create(floodCustomer(`expired-${i}`)),
    );

    // A sign-in well after those expired must collect them: the tokens nobody
    // presents again — the tab someone closed — are the common case, so
    // expiry-on-read alone would leave them for the life of the process.
    clock.now = t0 + SESSION_TTL_MS + 1;
    const fresh = sessions.create(alice);
    clock.now++;
    expect(sessions.lookup(fresh)).toEqual(alice);
    expect(
      expired.every((token) => sessions.lookup(token) === undefined),
    ).toBe(true);
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
  // Commerce mints the assertion at T1 on its clock and this
  // process starts the session at T2 > T1 on its own, so equal TTLs are not
  // enough — the session outlives the assertion by the round trip plus any skew.
  // Near the boundary that is a live session holding a token commerce refuses,
  // surfacing as a 401 at the payment button.
  it('caps the session at the assertion expiry when that is sooner', () => {
    const timed = createClockedStore(1_000, Date.now());
    const soon = Math.floor(timed.clock.wallTime / 1000) + 60; // one minute, far inside the 8h TTL
    const token = timed.store.create({ ...alice, assertion: `v1.cust-a.${soon}.mac` });

    expect(timed.store.lookup(token)).toEqual(expect.objectContaining({ customerId: 'cust-a' }));
    // A monotonic reading two minutes on — past the assertion, nowhere near the TTL.
    timed.clock.monotonicTime += 120_000;
    expect(timed.store.lookup(token)).toBeUndefined();
  });

  // An assertion that is already dead must not mint a usable session at all.
  it('refuses to hand out a session backed by an expired assertion', () => {
    const past = Math.floor(Date.now() / 1000) - 60;
    const token = sessions.create({ ...alice, assertion: `v1.cust-a.${past}.mac` });

    expect(sessions.lookup(token)).toBeUndefined();
  });

  // An unreadable assertion is commerce's problem to reject, not a reason to deny
  // someone a session here — it falls back to the plain TTL.
  it('falls back to the full TTL when the assertion is unreadable', () => {
    const token = sessions.create({ ...alice, assertion: 'not-a-token' });

    expect(sessions.lookup(token)).toEqual(expect.objectContaining({ customerId: 'cust-a' }));
  });
});

// `Number` accepts whitespace, hex, exponent notation
// and Infinity — values commerce's strconv.ParseInt could never have produced, so
// honouring them means agreeing with a token commerce never minted. They fall back
// to the plain TTL instead.
describe('the assertion expiry is parsed strictly', () => {
  it.each(['0x7fffffff', ' 99999999999', '1e12', 'Infinity', '1.5', '-1'])(
    'ignores %s and falls back to the full TTL',
    (field) => {
      const timed = createClockedStore(1_000);
      const token = timed.store.create({ ...alice, assertion: `v1.cust-a.${field}.mac` });
      expect(timed.store.lookup(token)).toEqual(expect.objectContaining({ customerId: 'cust-a' }));
      timed.clock.monotonicTime += SESSION_TTL_MS - 1000;
      expect(timed.store.lookup(token)).toBeDefined();
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
    const first = sessions.create(alice);
    const second = sessions.create(alice);

    expect(sessions.destroyAllForCustomer(alice.customerId)).toBe(2);

    expect(sessions.lookup(first)).toBeUndefined();
    expect(sessions.lookup(second)).toBeUndefined();
  });

  // The threat this closes, written as a test: the owner resets, the thief is out.
  it('signs out a stolen session when the owner resets', () => {
    const stolen = sessions.create(alice);
    expect(sessions.lookup(stolen)).toBeDefined();

    sessions.destroyAllForCustomer(alice.customerId);

    expect(sessions.lookup(stolen)).toBeUndefined();
  });

  // Scoped, never global. A reset must not sign out strangers.
  it('leaves other customers alone', () => {
    const hers = sessions.create(alice);
    const his = sessions.create(bob);

    expect(sessions.destroyAllForCustomer(alice.customerId)).toBe(1);

    expect(sessions.lookup(hers)).toBeUndefined();
    expect(sessions.lookup(his)).toEqual(bob);
  });

  // Reclaimed, not merely unresolvable: an entry left in the map is memory the caps
  // still count and a clock adjustment could resurrect.
  it('reclaims the entries rather than hiding them', () => {
    sessions = createSessionStore({
      maxSessionsTotal: MAX_SESSIONS_PER_CUSTOMER + 1,
    });
    sessions.create(alice);
    sessions.create(alice);
    const his = sessions.create(bob);

    sessions.destroyAllForCustomer(alice.customerId);

    const replacements = Array.from({ length: MAX_SESSIONS_PER_CUSTOMER }, (_, i) =>
      sessions.create(floodCustomer(`replacement-${i}`)),
    );
    expect(sessions.lookup(his)).toEqual(bob);
    expect(replacements.every((token) => sessions.lookup(token) !== undefined)).toBe(true);
    expect(() => sessions.create(floodCustomer('one-too-many'))).toThrow(SessionCapacityError);
  });

  it('is a no-op for a customer with no sessions', () => {
    const his = sessions.create(bob);

    expect(sessions.destroyAllForCustomer('cust-nobody')).toBe(0);
    expect(sessions.lookup(his)).toEqual(bob);
  });
});
