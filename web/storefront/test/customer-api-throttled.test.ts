import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  authenticateCustomer,
  claimGuestOrder,
  completePasswordReset,
  registerCustomer,
  requestPasswordReset,
} from '../src/lib/customer-api';

// Commerce rate-limits the public customer surface (TKT-224, ADR-051). What is
// under test here is the one thing this layer can get wrong on its own: folding a
// 429 into a neighbouring reason.
//
// Both directions are wrong and for different reasons. Folded into a credential
// verdict it is FALSE — the buyer's password was never checked. Folded into
// `unavailable` it sends them to support for something a clock fixes. And on
// forgot-password, folded into success it becomes a lie: nothing was enqueued, so
// a buyer would wait forever on mail that does not exist.

function respondWith(status: number, body = '{}') {
  vi.stubGlobal('fetch', async () =>
    new Response(body, { status, headers: { 'content-type': 'application/json' } }),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe('a 429 from commerce is its own reason, never a neighbouring one', () => {
  it('register does not report it as taken, invalid or unavailable', async () => {
    respondWith(429);
    const result = await registerCustomer('buyer@example.test', 'correct horse battery');
    expect(result).toEqual({ ok: false, reason: 'throttled' });
  });

  it('sign-in does not report it as a credential verdict', async () => {
    respondWith(429);
    const result = await authenticateCustomer('buyer@example.test', 'correct horse battery');
    expect(result).toEqual({ ok: false, reason: 'throttled' });
  });

  it('claiming a guest order does not report it as refused', async () => {
    respondWith(429);
    const result = await claimGuestOrder('11111111-1111-1111-1111-111111111111', 'assertion');
    expect(result).toEqual({ ok: false, reason: 'throttled' });
  });

  it('completing a reset does not report the link as dead', async () => {
    respondWith(429);
    const result = await completePasswordReset('a-token', 'a new password');
    // `refused` would clear the token and send the buyer back to their mailbox
    // for a link they are already holding and which still works.
    expect(result).toEqual({ ok: false, reason: 'throttled' });
  });

  it('a throttled reset REQUEST is not reported as sent', async () => {
    respondWith(429);
    const result = await requestPasswordReset('buyer@example.test');
    expect(result.ok).toBe(false);
    expect(result.throttled).toBe(true);
  });
});

describe('the ordinary answers still map as they did', () => {
  it('202 is still an accepted reset request, with no throttle flag', async () => {
    respondWith(202);
    expect(await requestPasswordReset('buyer@example.test')).toEqual({ ok: true });
  });

  it('409 is still taken, not throttled', async () => {
    respondWith(409);
    expect(await registerCustomer('buyer@example.test', 'correct horse battery')).toEqual({
      ok: false,
      reason: 'taken',
    });
  });

  it('401 is still a credential refusal, not throttled', async () => {
    respondWith(401);
    expect(await authenticateCustomer('buyer@example.test', 'nope')).toEqual({
      ok: false,
      reason: 'invalid',
    });
  });

  it('503 is still an outage, not throttled', async () => {
    respondWith(503);
    expect(await authenticateCustomer('buyer@example.test', 'nope')).toEqual({
      ok: false,
      reason: 'unavailable',
    });
  });
});
