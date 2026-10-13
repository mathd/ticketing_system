import { afterEach, describe, expect, it, vi } from 'vitest';

import { requestPasswordReset } from '../src/lib/customer-api';

// TKT-366: the reset request carries the locale of the page it was made on, so commerce
// can mail the message in that language. What this layer can get wrong on its own is the
// WIRE: dropping the field, renaming it, or sending something other than the value the
// page passed. The page's own forwarding of its route locale is proven by the browser
// spec (test/browser/reset-password.mjs), which reads the message the request produced.

function captureBodies(): unknown[] {
  const bodies: unknown[] = [];
  vi.stubGlobal('fetch', async (_url: string, init?: RequestInit) => {
    bodies.push(JSON.parse(String(init?.body)));
    return new Response('{"status":"accepted"}', {
      status: 202,
      headers: { 'content-type': 'application/json' },
    });
  });
  return bodies;
}

afterEach(() => vi.unstubAllGlobals());

describe('a reset request names the language of its page', () => {
  it.each(['en', 'fr'] as const)('sends locale %s beside the address, and nothing else', async (locale) => {
    const bodies = captureBodies();
    expect(await requestPasswordReset('buyer@example.test', locale)).toEqual({ ok: true });
    expect(bodies).toEqual([{ email: 'buyer@example.test', locale }]);
  });
});
