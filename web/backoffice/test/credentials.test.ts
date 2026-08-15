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
  const ok = {
    CATALOG_STAFF_WRITE_TOKEN: 'catalog-value',
    COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value',
    INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value',
  };

  it('accepts three different credentials', () => {
    expect(() => assertCredentialSeparation(ok)).not.toThrow();
  });

  // Every pair, so no single collapse can hide behind another pair being distinct.
  it.each([
    ['catalog/commerce', { ...ok, CATALOG_STAFF_WRITE_TOKEN: 'same', COMMERCE_STAFF_WRITE_TOKEN: 'same' }],
    ['catalog/inventory', { ...ok, CATALOG_STAFF_WRITE_TOKEN: 'same', INVENTORY_STAFF_WRITE_TOKEN: 'same' }],
    ['commerce/inventory', { ...ok, COMMERCE_STAFF_WRITE_TOKEN: 'same', INVENTORY_STAFF_WRITE_TOKEN: 'same' }],
    ['all three', {
      CATALOG_STAFF_WRITE_TOKEN: 'same',
      COMMERCE_STAFF_WRITE_TOKEN: 'same',
      INVENTORY_STAFF_WRITE_TOKEN: 'same',
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
  ])('does not echo the credential: %s', (_name, build) => {
    const secret = 'a-real-looking-credential-value';
    try {
      assertCredentialSeparation(build(secret));
      expect.unreachable('should have thrown');
    } catch (e) {
      expect(String(e)).not.toContain(secret);
    }
  });

  it.each([
    ['catalog', { COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value', INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value' }, /CATALOG_STAFF_WRITE_TOKEN/],
    ['commerce', { CATALOG_STAFF_WRITE_TOKEN: 'catalog-value', INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value' }, /COMMERCE_STAFF_WRITE_TOKEN/],
    ['inventory', { CATALOG_STAFF_WRITE_TOKEN: 'catalog-value', COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value' }, /INVENTORY_STAFF_WRITE_TOKEN/],
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
  const start = fileURLToPath(new URL('../start.mjs', import.meta.url));
  const run = (env: Record<string, string>) =>
    spawnSync(process.execPath, [start], {
      env: { ...process.env, ...env },
      encoding: 'utf8',
      timeout: 20_000,
    });

  it('exits non-zero when two credentials are identical', () => {
    const got = run({
      CATALOG_STAFF_WRITE_TOKEN: 'same',
      COMMERCE_STAFF_WRITE_TOKEN: 'same',
      INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value',
    });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/refusing to start/);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain('same');
  });

  // The third credential joins the entrypoint check, not only the client (TKT-244):
  // a value read lazily by a module under dist/ would be checked after the server is
  // already listening, which is the failure ai-review pass 2 caught for the first two.
  it('exits non-zero when the inventory credential collapses onto another', () => {
    const got = run({
      CATALOG_STAFF_WRITE_TOKEN: 'catalog-value',
      COMMERCE_STAFF_WRITE_TOKEN: 'shared',
      INVENTORY_STAFF_WRITE_TOKEN: 'shared',
    });
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(/must not equal/);
    expect(got.stderr).not.toContain('shared');
  });

  it.each([
    ['commerce', { CATALOG_STAFF_WRITE_TOKEN: 'catalog-value', COMMERCE_STAFF_WRITE_TOKEN: '', INVENTORY_STAFF_WRITE_TOKEN: 'inventory-value' }, /COMMERCE_STAFF_WRITE_TOKEN/],
    ['inventory', { CATALOG_STAFF_WRITE_TOKEN: 'catalog-value', COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value', INVENTORY_STAFF_WRITE_TOKEN: '' }, /INVENTORY_STAFF_WRITE_TOKEN/],
  ])('exits non-zero when the %s credential is missing', (_name, env, want) => {
    const got = run(env);
    expect(got.status).toBe(1);
    expect(got.stderr).toMatch(want);
  });
});
