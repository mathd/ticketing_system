import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { POST } from '../src/pages/claim';
import { SESSION_COOKIE, createSession, resetSessionsForTest } from '../src/lib/session';

// The claim bridge (TKT-223). Same shape and the same reason as /checkout: the
// session cookie is httpOnly, so nothing in the browser can prove who is
// claiming, and commerce cannot resolve a storefront session.

const FUTURE = Math.floor(Date.now() / 1000) + 24 * 60 * 60;
const alice = { customerId: 'cust-a', email: 'alice@example.test', assertion: `v1.cust-a.${FUTURE}.mac` };

let captured: { headers: Headers; body: string } | undefined;
let upstream: Response;

beforeEach(() => {
  resetSessionsForTest();
  captured = undefined;
  upstream = new Response('{"order_id":"o-1","guest_order_ref":"g-1","customer_id":"cust-a"}', {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
  vi.stubGlobal('fetch', async (_url: string, init: RequestInit) => {
    captured = { headers: new Headers(init.headers), body: String(init.body) };
    return upstream;
  });
});

afterEach(() => vi.unstubAllGlobals());

function post(cookie: string | undefined, fields: Record<string, string>) {
  const form = new URLSearchParams(fields);
  const headers: Record<string, string> = { 'content-type': 'application/x-www-form-urlencoded' };
  if (cookie) headers.cookie = `${SESSION_COOKIE}=${cookie}`;
  const request = new Request('http://storefront:8080/claim', {
    method: 'POST',
    headers,
    body: form.toString(),
  });
  return POST({
    request,
    cookies: { get: (name: string) => (name === SESSION_COOKIE && cookie ? { value: cookie } : undefined) },
    redirect: (location: string, status: number) =>
      new Response(null, { status, headers: { location } }),
  } as never);
}

describe('the claim bridge', () => {
  it('sends the session assertion and never the cookie', async () => {
    const token = createSession(alice);

    await post(token, { guest_order_ref: 'g-1', locale: 'en' });

    expect(captured!.headers.get('X-Customer-Assertion')).toBe(alice.assertion);
    expect(captured!.headers.get('cookie')).toBeNull();
    expect(JSON.parse(captured!.body)).toEqual({ guest_order_ref: 'g-1' });
  });

  // The body must not be able to name a customer — the bridge builds it, so the
  // form cannot smuggle one through.
  it('ignores any customer the form tries to name', async () => {
    const token = createSession(alice);

    await post(token, { guest_order_ref: 'g-1', locale: 'en', customer_id: 'somebody-else' });

    expect(JSON.parse(captured!.body)).toEqual({ guest_order_ref: 'g-1' });
  });

  it('lands the buyer in their wallet on success', async () => {
    const token = createSession(alice);

    const response = await post(token, { guest_order_ref: 'g-1', locale: 'fr' });

    expect(response.status).toBe(303);
    expect(response.headers.get('location')).toBe('/fr/account');
  });

  // Signed out, or a session that has since expired: sending them to sign in
  // beats refusing, because the claim is the whole reason they are here and the
  // public ticket page they came from still works.
  it.each([undefined, 'a-token-this-process-never-issued'])(
    'sends an unauthenticated claimer to sign in (cookie: %s)',
    async (cookie) => {
      const response = await post(cookie, { guest_order_ref: 'g-1', locale: 'en' });

      expect(response.status).toBe(303);
      expect(response.headers.get('location')).toBe('/en/account/sign-in');
      expect(captured).toBeUndefined();
    },
  );

  // Commerce answers every refused case identically, so this layer must not
  // invent a distinction it was not given.
  it.each([
    [404, 'refused'],
    [400, 'refused'],
    [503, 'unavailable'],
  ])('returns the buyer to the ticket page with %i -> %s', async (status, reason) => {
    upstream = new Response('{"error":"not found"}', { status });
    const token = createSession(alice);

    const response = await post(token, { guest_order_ref: 'g-1', locale: 'en' });

    expect(response.status).toBe(303);
    expect(response.headers.get('location')).toBe(`/en/tickets/g-1?claim=${reason}`);
  });

  // An unknown locale must not be reflected into a redirect target.
  it('falls back to a known locale rather than reflecting the form value', async () => {
    const token = createSession(alice);

    const response = await post(token, { guest_order_ref: 'g-1', locale: '../../evil' });

    expect(response.headers.get('location')).toBe('/en/account');
  });
});
