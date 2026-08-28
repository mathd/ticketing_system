import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { assertCredentialSeparation } from '../credentials.mjs';

// TKT-194, ai-review pass 1. The back office is the ONLY process that holds both
// staff credentials, so it is the only one that can compare them: catalog checks
// its own against INTERNAL_SERVICE_TOKEN and commerce checks its own, and
// neither is ever given the other's. Set both to one value and every service
// starts happily while an internet-facing process holds a single bearer token
// that both authors catalog content and moves money.
// TKT-244 extends the rule to a THIRD credential (inventory, ADR-057) and TKT-203 to
// a FOURTH (access, ADR-068). Pairwise rather than "all distinct" as a set: the pair
// count grows quadratically — four values is six pairs — and a check that only
// compared the newest against one of the others would let the other collapse silently.
//
// The pair cases below are DERIVED from the credential list rather than written out,
// because a hand-written list is exactly what the rule above warns about: the pair
// someone forgets to add is the one that collapses in silence. Adding a fifth
// credential to `ok` extends the coverage automatically.
describe('the back office refuses collapsed credentials at startup', () => {
  // Every fixture is >= 32 bytes because TKT-252 gave these credentials a length
  // floor (ADR-059). They were lengthened rather than replaced: each still says
  // which service it belongs to, so a failure still reads.
  const ok = {
    CATALOG_STAFF_WRITE_TOKEN: 'catalog-value-0f3d1c9a8b7e6f5d4c3b',
    COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value-0f3d1c9a8b7e6f5d4c3b',
    INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value-0f3d1c9a8b7e6f5d4c3b',
    ACCESS_STAFF_WRITE_TOKEN: 'access-value-0f3d1c9a8b7e6f5d4c3b',
  };

  const NAMES = Object.keys(ok);
  // Every unordered pair of credential names.
  const PAIRS = NAMES.flatMap((a, i) => NAMES.slice(i + 1).map((b) => [a, b] as const));

  // A collapsed value must still clear the length floor, or the test would be
  // refused for its LENGTH and never reach the pairwise comparison it exists for.
  const SAME = 'same-collapsed-value-0f3d1c9a8b7e6f';

  it('accepts four different credentials', () => {
    expect(() => assertCredentialSeparation(ok)).not.toThrow();
  });

  // TKT-252 / ADR-059. The Go services refuse a short credential at startup, so a
  // back office holding one cannot write — it just finds out per-request, as an
  // opaque 401, instead of at boot.
  //
  // Asserted from BOTH sides of the boundary: one byte under must fail and exactly
  // 32 must pass. A test that only tried a very short value would pass just as
  // happily against a floor of 4.
  it.each(NAMES)(
    'refuses a %s below the shared floor, and accepts one exactly at it',
    (name) => {
      const under = 'x'.repeat(31);
      expect(() => assertCredentialSeparation({ ...ok, [name]: under })).toThrow(
        new RegExp(`${name}.*at least 32 bytes`),
      );
      const atFloor = `${name.toLowerCase()}-`.padEnd(32, 'z').slice(0, 32);
      expect(atFloor).toHaveLength(32);
      expect(() => assertCredentialSeparation({ ...ok, [name]: atFloor })).not.toThrow();
    },
  );

  // The floor counts BYTES, matching Go's len(). '.length' would count UTF-16 code
  // units, so 16 'é' (32 bytes, 16 units) would be refused here while the Go
  // service accepts it — the two processes disagreeing about one string.
  it('counts bytes, not UTF-16 code units', () => {
    const thirtyTwoBytes = 'é'.repeat(16);
    expect(thirtyTwoBytes).toHaveLength(16);
    expect(Buffer.byteLength(thirtyTwoBytes, 'utf8')).toBe(32);
    expect(() =>
      assertCredentialSeparation({ ...ok, CATALOG_STAFF_WRITE_TOKEN: thirtyTwoBytes }),
    ).not.toThrow();
  });

  // The refusal must not disclose the value or its length: a credential that is
  // already too short does not also need its search space narrowed in a log.
  //
  // The thrown error is captured and asserted on OUTSIDE the catch, deliberately.
  // The obvious shape — expect.unreachable() inside the try, assertions inside the
  // catch — cannot fail: expect.unreachable throws, the catch swallows it as though
  // it were the production error, and two `not.toContain` assertions against an
  // assertion-error message pass trivially. Written that way this test stayed green
  // with the floor deleted (caught by ai-review pass 2). Capture, then assert an
  // Error was actually caught, then inspect it.
  it('does not echo a short credential or its length', () => {
    const short = 'sekrit-but-far-too-short';
    let caught: unknown;
    try {
      assertCredentialSeparation({ ...ok, COMMERCE_STAFF_WRITE_TOKEN: short });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    const message = String(caught);
    expect(message).toMatch(/COMMERCE_STAFF_WRITE_TOKEN.*at least 32 bytes/);
    expect(message).not.toContain(short);
    expect(message).not.toContain(String(short.length));
  });

  // Every pair, so no single collapse can hide behind another pair being distinct.
  it.each(PAIRS)('refuses identical credentials: %s/%s', (a, b) => {
    expect(() => assertCredentialSeparation({ ...ok, [a]: SAME, [b]: SAME })).toThrow(
      /must not equal/,
    );
  });

  it('refuses every credential collapsed onto one value', () => {
    const all = Object.fromEntries(NAMES.map((n) => [n, SAME]));
    expect(() => assertCredentialSeparation(all)).toThrow(/must not equal/);
  });

  // The error must not echo the value it is complaining about — this runs at
  // startup and its message lands in container logs.
  // Same capture-then-assert shape as the length case below, and for the same
  // reason: this test previously put expect.unreachable() inside the try and its
  // assertions inside the catch, which meant it stayed green with the equality
  // check deleted — the catch swallowed the assertion error and `not.toContain`
  // passed against that. Pre-existing (TKT-194), found while fixing the identical
  // flaw this ticket introduced, and fixed here because it is the same three lines.
  it.each(PAIRS)('does not echo the credential: %s/%s', (a, b) => {
    const secret = 'a-real-looking-credential-value-0f3d1c9a';
    let caught: unknown;
    try {
      assertCredentialSeparation({ ...ok, [a]: secret, [b]: secret });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    const message = String(caught);
    expect(message).toMatch(/must not equal/);
    expect(message).not.toContain(secret);
  });

  // Each credential, omitted in turn — derived, so a new one is covered on arrival.
  it.each(NAMES)('refuses a missing %s credential, naming it', (name) => {
    const without = { ...ok };
    delete (without as Record<string, string>)[name];
    expect(() => assertCredentialSeparation(without)).toThrow(new RegExp(name));
  });
});

// The rule above is only worth anything if the PROCESS applies it. Astro's
// standalone build defers middleware to the first request, so a module-scope
// call there cannot fail startup — start.mjs is the entrypoint that can, and
// this runs the real file (ai-review pass 2).
//
// The refusal path exits before importing dist/, so this does not need a build.
describe('the entrypoint refuses to start', () => {
  // >= 32 bytes throughout (TKT-252): these cases assert the COLLAPSE and MISSING
  // diagnostics, and a short fixture would be refused for its length first — the
  // test would still be green while proving something else entirely.
  const CATALOG = 'catalog-value-0f3d1c9a8b7e6f5d4c3b';
  const COMMERCE = 'commerce-value-0f3d1c9a8b7e6f5d4c3b';
  const INVENTORY = 'inventory-value-0f3d1c9a8b7e6f5d4c3b';
  const ACCESS = 'access-value-0f3d1c9a8b7e6f5d4c3b';
  const SAME = 'same-collapsed-value-0f3d1c9a8b7e6f';
  // The credentials NOT under test in a given case, so each case sets only what it
  // is about. A case that left one unset would be refused for the omission and never
  // reach the collapse it exists to prove.
  const complete = {
    CATALOG_STAFF_WRITE_TOKEN: CATALOG,
    COMMERCE_STAFF_WRITE_TOKEN: COMMERCE,
    INVENTORY_STAFF_WRITE_TOKEN: INVENTORY,
    ACCESS_STAFF_WRITE_TOKEN: ACCESS,
  };

  const start = fileURLToPath(new URL('../start.mjs', import.meta.url));
  const run = (env: Record<string, string>) =>
    spawnSync(process.execPath, [start], {
      env: { ...process.env, ...env },
      encoding: 'utf8',
      timeout: 20_000,
    });

  it('exits non-zero when two credentials are identical', () => {
    const got = run({ ...complete, CATALOG_STAFF_WRITE_TOKEN: SAME, COMMERCE_STAFF_WRITE_TOKEN: SAME });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/refusing to start/);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain(SAME);
  });

  // The third credential joins the entrypoint check, not only the client (TKT-244):
  // a value read lazily by a module under dist/ would be checked after the server is
  // already listening, which is the failure ai-review pass 2 caught for the first two.
  it('exits non-zero when the inventory credential collapses onto another', () => {
    const got = run({ ...complete, COMMERCE_STAFF_WRITE_TOKEN: SAME, INVENTORY_STAFF_WRITE_TOKEN: SAME });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain(SAME);
  });

  // TKT-203: the fourth credential joins the entrypoint check too, for the same reason
  // the third did — a value checked only by a module under dist/ would be checked after
  // the server is already listening.
  it('exits non-zero when the access credential collapses onto another', () => {
    const got = run({ ...complete, CATALOG_STAFF_WRITE_TOKEN: SAME, ACCESS_STAFF_WRITE_TOKEN: SAME });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain(SAME);
  });

  it.each(Object.keys(complete))('exits non-zero when %s is missing', (name) => {
    const got = run({ ...complete, [name]: '' });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(new RegExp(name));
  });
});
