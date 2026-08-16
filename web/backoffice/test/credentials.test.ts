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
// TKT-244 extends the rule to a THIRD credential (inventory, ADR-057). Pairwise
// rather than "all distinct" as a set: with three values there are three pairs, and
// a check that only compared the newest against one of the others would let the
// other collapse silently.
describe('the back office refuses collapsed credentials at startup', () => {
  // Every fixture is >= 32 bytes because TKT-252 gave these credentials a length
  // floor (ADR-059). They were lengthened rather than replaced: each still says
  // which service it belongs to, so a failure still reads.
  const ok = {
    CATALOG_STAFF_WRITE_TOKEN: 'catalog-value-0f3d1c9a8b7e6f5d4c3b',
    COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value-0f3d1c9a8b7e6f5d4c3b',
    INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value-0f3d1c9a8b7e6f5d4c3b',
  };

  // A collapsed value must still clear the length floor, or the test would be
  // refused for its LENGTH and never reach the pairwise comparison it exists for.
  const SAME = 'same-collapsed-value-0f3d1c9a8b7e6f';

  it('accepts three different credentials', () => {
    expect(() => assertCredentialSeparation(ok)).not.toThrow();
  });

  // TKT-252 / ADR-059. The Go services refuse a short credential at startup, so a
  // back office holding one cannot write — it just finds out per-request, as an
  // opaque 401, instead of at boot.
  //
  // Asserted from BOTH sides of the boundary: one byte under must fail and exactly
  // 32 must pass. A test that only tried a very short value would pass just as
  // happily against a floor of 4.
  it.each(['CATALOG_STAFF_WRITE_TOKEN', 'COMMERCE_STAFF_WRITE_TOKEN', 'INVENTORY_STAFF_WRITE_TOKEN'])(
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
  it.each([
    ['catalog/commerce', { ...ok, CATALOG_STAFF_WRITE_TOKEN: SAME, COMMERCE_STAFF_WRITE_TOKEN: SAME }],
    ['catalog/inventory', { ...ok, CATALOG_STAFF_WRITE_TOKEN: SAME, INVENTORY_STAFF_WRITE_TOKEN: SAME }],
    ['commerce/inventory', { ...ok, COMMERCE_STAFF_WRITE_TOKEN: SAME, INVENTORY_STAFF_WRITE_TOKEN: SAME }],
    ['all three', {
      CATALOG_STAFF_WRITE_TOKEN: SAME,
      COMMERCE_STAFF_WRITE_TOKEN: SAME,
      INVENTORY_STAFF_WRITE_TOKEN: SAME,
    }],
  ])('refuses identical credentials: %s', (_name, env) => {
    expect(() => assertCredentialSeparation(env)).toThrow(/must not equal/);
  });

  // The error must not echo the value it is complaining about — this runs at
  // startup and its message lands in container logs.
  it.each([
    ['catalog/commerce', (s: string) => ({ ...ok, CATALOG_STAFF_WRITE_TOKEN: s, COMMERCE_STAFF_WRITE_TOKEN: s })],
    ['catalog/inventory', (s: string) => ({ ...ok, CATALOG_STAFF_WRITE_TOKEN: s, INVENTORY_STAFF_WRITE_TOKEN: s })],
    ['commerce/inventory', (s: string) => ({ ...ok, COMMERCE_STAFF_WRITE_TOKEN: s, INVENTORY_STAFF_WRITE_TOKEN: s })],
  // Same capture-then-assert shape as the length case below, and for the same
  // reason: this test previously put expect.unreachable() inside the try and its
  // assertions inside the catch, which meant it stayed green with the equality
  // check deleted — the catch swallowed the assertion error and `not.toContain`
  // passed against that. Pre-existing (TKT-194), found while fixing the identical
  // flaw this ticket introduced, and fixed here because it is the same three lines.
  ])('does not echo the credential: %s', (_name, build) => {
    const secret = 'a-real-looking-credential-value-0f3d1c9a';
    let caught: unknown;
    try {
      assertCredentialSeparation(build(secret));
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    const message = String(caught);
    expect(message).toMatch(/must not equal/);
    expect(message).not.toContain(secret);
  });

  it.each([
    ['catalog', { COMMERCE_STAFF_WRITE_TOKEN: ok.COMMERCE_STAFF_WRITE_TOKEN, INVENTORY_STAFF_WRITE_TOKEN: ok.INVENTORY_STAFF_WRITE_TOKEN }, /CATALOG_STAFF_WRITE_TOKEN/],
    ['commerce', { CATALOG_STAFF_WRITE_TOKEN: ok.CATALOG_STAFF_WRITE_TOKEN, INVENTORY_STAFF_WRITE_TOKEN: ok.INVENTORY_STAFF_WRITE_TOKEN }, /COMMERCE_STAFF_WRITE_TOKEN/],
    ['inventory', { CATALOG_STAFF_WRITE_TOKEN: ok.CATALOG_STAFF_WRITE_TOKEN, COMMERCE_STAFF_WRITE_TOKEN: ok.COMMERCE_STAFF_WRITE_TOKEN }, /INVENTORY_STAFF_WRITE_TOKEN/],
  ])('refuses a missing %s credential, naming it', (_name, env, want) => {
    expect(() => assertCredentialSeparation(env)).toThrow(want);
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
  const SAME = 'same-collapsed-value-0f3d1c9a8b7e6f';

  const start = fileURLToPath(new URL('../start.mjs', import.meta.url));
  const run = (env: Record<string, string>) =>
    spawnSync(process.execPath, [start], {
      env: { ...process.env, ...env },
      encoding: 'utf8',
      timeout: 20_000,
    });

  it('exits non-zero when two credentials are identical', () => {
    const got = run({
      CATALOG_STAFF_WRITE_TOKEN: SAME,
      COMMERCE_STAFF_WRITE_TOKEN: SAME,
      INVENTORY_STAFF_WRITE_TOKEN: INVENTORY,
    });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/refusing to start/);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain(SAME);
  });

  // The third credential joins the entrypoint check, not only the client (TKT-244):
  // a value read lazily by a module under dist/ would be checked after the server is
  // already listening, which is the failure ai-review pass 2 caught for the first two.
  it('exits non-zero when the inventory credential collapses onto another', () => {
    const got = run({
      CATALOG_STAFF_WRITE_TOKEN: CATALOG,
      COMMERCE_STAFF_WRITE_TOKEN: SAME,
      INVENTORY_STAFF_WRITE_TOKEN: SAME,
    });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain(SAME);
  });

  it.each([
    ['commerce', { CATALOG_STAFF_WRITE_TOKEN: CATALOG, COMMERCE_STAFF_WRITE_TOKEN: '', INVENTORY_STAFF_WRITE_TOKEN: INVENTORY }, /COMMERCE_STAFF_WRITE_TOKEN/],
    ['inventory', { CATALOG_STAFF_WRITE_TOKEN: CATALOG, COMMERCE_STAFF_WRITE_TOKEN: COMMERCE, INVENTORY_STAFF_WRITE_TOKEN: '' }, /INVENTORY_STAFF_WRITE_TOKEN/],
  ])('exits non-zero when the %s credential is missing', (_name, env, want) => {
    const got = run(env);
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(want);
  });
});
