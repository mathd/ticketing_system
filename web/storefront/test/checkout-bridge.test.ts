import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { POST } from '../src/pages/checkout';
import { SESSION_COOKIE, createSession, destroySession, resetSessionsForTest } from '../src/lib/session';

// The checkout bridge (TKT-221). What it must do is narrow and what it must NOT do
// is the interesting half: it adds one header and changes nothing else, because
// "guest checkout is unchanged" has to mean the bytes.

// A realistic assertion expiry. createSession caps the session at whatever the
// assertion says, so a fixture with a stale one produces a session that is dead on
// arrival — correct behaviour, useless fixture.
//
// Deliberately FURTHER out than SESSION_TTL_MS, so the cap is not the binding
// constraint here and the TTL tests measure the TTL. The cap's own behaviour is
// tested separately, where it is the point.
const FUTURE = Math.floor(Date.now() / 1000) + 24 * 60 * 60;

const alice = { customerId: 'cust-a', email: 'alice@example.test', assertion: `v1.cust-a.${FUTURE}.mac` };

interface Captured {
  headers: Headers;
  body: string;
}

let captured: Captured | undefined;
let upstream: Response;

beforeEach(() => {
  resetSessionsForTest();
  captured = undefined;
  upstream = new Response('{"order_id":"o-1","guest_order_ref":"g-1","status":"completed"}', {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
  vi.stubGlobal('fetch', async (_url: string, init: RequestInit) => {
    captured = { headers: new Headers(init.headers), body: String(init.body) };
    return upstream;
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function post(cookie?: string, smuggled?: string) {
  // The Cookie header is set on the REQUEST as a real browser would send it, not
  // only in the `cookies` accessor. Without it this fixture cannot observe the
  // header being forwarded at all — adding 'cookie' to the allowlist left the
  // suite green until this was fixed.
  const headers: Record<string, string> = {
    'content-type': 'application/json',
    'idempotency-key': 'key-1',
  };
  if (cookie) headers.cookie = `${SESSION_COOKIE}=${cookie}`;
  if (smuggled) headers['x-customer-assertion'] = smuggled;
  const request = new Request('http://storefront:8080/checkout', {
    method: 'POST',
    headers,
    body: '{"reservation_id":"r-1","name":"A","email":"a@example.test","payment_token":"fake-ok"}',
  });
  return POST({
    request,
    cookies: { get: (name: string) => (name === SESSION_COOKIE && cookie ? { value: cookie } : undefined) },
  } as never);
}

describe('the checkout bridge', () => {
  it('forwards a signed-out checkout with no assertion and no cookie', async () => {
    const response = await post();

    expect(captured!.headers.get('X-Customer-Assertion')).toBeNull();
    // The cookie must never leave this process — commerce has no use for it and
    // it is a live credential.
    expect(captured!.headers.get('cookie')).toBeNull();
    expect(captured!.headers.get('idempotency-key')).toBe('key-1');
    expect(response.status).toBe(200);
  });

  it('attaches the assertion for a live session, and never the cookie', async () => {
    const token = createSession(alice);

    await post(token);

    expect(captured!.headers.get('X-Customer-Assertion')).toBe(alice.assertion);
    expect(captured!.headers.get('cookie')).toBeNull();
    // The token itself must not travel either.
    expect(JSON.stringify([...captured!.headers])).not.toContain(token);
  });

  // An expired or unknown session is a GUEST checkout, not a refusal. Refusing
  // would break a working checkout for the sin of having once been signed in, and
  // the buyer would have no idea why — while the guest path still delivers their
  // tickets by order reference.
  it('falls back to a guest checkout when the session is gone', async () => {
    const response = await post('a-token-this-process-never-issued');

    expect(captured!.headers.get('X-Customer-Assertion')).toBeNull();
    expect(response.status).toBe(200);
  });

  // ai-review [medium]: the bridge decides WHO the caller is, so a caller must not
  // be able to decide it for themselves by sending the header. Without an inbound
  // assertion in the fixture, adding 'x-customer-assertion' to the forwarding
  // allowlist would leave this suite green — and any internet caller could
  // attribute a purchase to any customer, which is the exact forgery the whole
  // assertion mechanism exists to prevent.
  it('never forwards a caller-supplied assertion when signed out', async () => {
    await post(undefined, 'v1.victim.99999999999.forged');

    expect(captured!.headers.get('X-Customer-Assertion')).toBeNull();
  });

  it("ignores a caller-supplied assertion and sends the SESSION's", async () => {
    const token = createSession(alice);

    await post(token, 'v1.victim.99999999999.forged');

    expect(captured!.headers.get('X-Customer-Assertion')).toBe(alice.assertion);
  });

  // A genuinely dead session, not merely an unknown token: the session was issued
  // by this process and then destroyed, which is what sign-out and eviction do.
  it('falls back to guest for a session that existed and is now gone', async () => {
    const token = createSession(alice);
    destroySession(token);

    const response = await post(token);

    expect(captured!.headers.get('X-Customer-Assertion')).toBeNull();
    expect(response.status).toBe(200);
  });

  // ai-review [medium]: a transport failure or a hung gateway must not reach the
  // island as HTML it tries to parse as JSON, and must not read as payment
  // uncertainty — nothing was submitted, so a retry is safe.
  it('answers a declared 503 when the upstream hop fails', async () => {
    vi.stubGlobal('fetch', async () => {
      throw new Error('connect ECONNREFUSED');
    });

    const response = await post();

    expect(response.status).toBe(503);
    expect(response.headers.get('content-type')).toContain('application/json');
    expect(await response.json()).toHaveProperty('error');
  });

  it('forwards the request body verbatim', async () => {
    await post();

    expect(JSON.parse(captured!.body)).toEqual({
      reservation_id: 'r-1',
      name: 'A',
      email: 'a@example.test',
      payment_token: 'fake-ok',
    });
  });

  // The island already understands every status checkout can answer with.
  // Re-interpreting them here would put a second opinion about payment outcomes
  // in a process that knows nothing about payments.
  it.each([
    [202, '{"order_id":"o-1","status":"release_pending"}'],
    [402, '{"order_id":"o-1","status":"declined"}'],
    [409, '{"error":"checkout conflicts with an existing request"}'],
    [503, '{"error":"temporarily unavailable"}'],
  ])('passes status %i and its body through unchanged', async (status, body) => {
    upstream = new Response(body, { status, headers: { 'content-type': 'application/json' } });

    const response = await post();

    expect(response.status).toBe(status);
    expect(await response.text()).toBe(body);
  });

  it('never lets an order outcome be cached', async () => {
    const response = await post();
    expect(response.headers.get('cache-control')).toBe('no-store');
  });
});
