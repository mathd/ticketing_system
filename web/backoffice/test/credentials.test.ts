import { describe, expect, it } from 'vitest';

import { assertCredentialSeparation } from '../src/lib/credentials';

// TKT-194, ai-review pass 1. The back office is the ONLY process that holds both
// staff credentials, so it is the only one that can compare them: catalog checks
// its own against INTERNAL_SERVICE_TOKEN and commerce checks its own, and
// neither is ever given the other's. Set both to one value and every service
// starts happily while an internet-facing process holds a single bearer token
// that both authors catalog content and moves money.
describe('the back office refuses collapsed credentials at startup', () => {
  const ok = { CATALOG_STAFF_WRITE_TOKEN: 'catalog-value', COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value' };

  it('accepts two different credentials', () => {
    expect(() => assertCredentialSeparation(ok)).not.toThrow();
  });

  it('refuses two identical credentials', () => {
    expect(() =>
      assertCredentialSeparation({ CATALOG_STAFF_WRITE_TOKEN: 'same', COMMERCE_STAFF_WRITE_TOKEN: 'same' }),
    ).toThrow(/must not equal/);
  });

  // The error must not echo the value it is complaining about — this runs at
  // startup and its message lands in container logs.
  it('does not echo the credential', () => {
    const secret = 'a-real-looking-credential-value';
    try {
      assertCredentialSeparation({ CATALOG_STAFF_WRITE_TOKEN: secret, COMMERCE_STAFF_WRITE_TOKEN: secret });
      expect.unreachable('should have thrown');
    } catch (e) {
      expect(String(e)).not.toContain(secret);
    }
  });

  it.each([
    ['catalog', { COMMERCE_STAFF_WRITE_TOKEN: 'commerce-value' }, /CATALOG_STAFF_WRITE_TOKEN/],
    ['commerce', { CATALOG_STAFF_WRITE_TOKEN: 'catalog-value' }, /COMMERCE_STAFF_WRITE_TOKEN/],
  ])('refuses a missing %s credential, naming it', (_name, env, want) => {
    expect(() => assertCredentialSeparation(env)).toThrow(want);
  });
});
