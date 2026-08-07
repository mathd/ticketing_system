import { describe, expect, it, vi } from 'vitest';

import {
  forbiddenResponse,
  gateRequest,
  isUnsafeMethod,
  originIsTrusted,
  requiresSession,
  signInPath,
} from '../src/lib/gate';

const PUBLIC_ORIGIN = 'https://tickets.example.test';

function proxied(
  pathname: string,
  { method = 'GET', origin = PUBLIC_ORIGIN, proto = 'https', host = 'tickets.example.test' } = {},
): Request {
  const headers = new Headers();
  if (origin) headers.set('origin', origin);
  if (proto) headers.set('x-forwarded-proto', proto);
  if (host) headers.set('x-forwarded-host', host);
  // The URL is the CONTAINER's, deliberately: Astro's Node adapter builds it
  // from the container Host, which is why Astro's own checkOrigin cannot work
  // behind the gateway (TKT-105).
  return new Request(`http://storefront:8080${pathname}`, { method, headers });
}

function deps(request: Request, pathname: string, principal: unknown = undefined) {
  const lookup = vi.fn(() => principal);
  const next = vi.fn(async () => new Response('page', { status: 200 }));
  const onAuthenticated = vi.fn();
  const redirectToSignIn = vi.fn(
    (path: string) => new Response(null, { status: 302, headers: { location: path } }),
  );
  return {
    calls: { lookup, next, onAuthenticated, redirectToSignIn },
    gate: {
      request,
      pathname,
      sessionToken: 'a-token',
      lookup,
      onAuthenticated,
      redirectToSignIn,
      next,
    },
  };
}

describe('which paths need a session', () => {
  // The storefront is guest-first: the default is OPEN and only the account
  // subtree is gated. A regression here is a redirect-to-login appearing on a
  // page anyone must be able to buy from.
  it.each([
    '/en/events',
    '/fr/events',
    '/en/events/abc-123',
    '/fr/festivals/def-456',
    '/en/tickets/00000000-0000-0000-0000-000000000001',
    '/healthz',
    '/',
  ])('leaves %s reachable signed out', (path) => {
    expect(requiresSession(path)).toBe(false);
  });

  it.each(['/en/account', '/en/account/', '/fr/account', '/en/account/anything'])(
    'gates %s',
    (path) => {
      expect(requiresSession(path)).toBe(true);
    },
  );

  // A buyer cannot sign in through a page that requires a session — and cannot
  // RECOVER through one either (TKT-226). The recovery pages' entire audience is
  // people who are locked out, so gating them would redirect a locked-out buyer to
  // the form they are locked out of.
  it.each([
    '/en/account/sign-in',
    '/en/account/register',
    '/fr/account/sign-in/',
    '/fr/account/register',
    '/en/account/forgot-password',
    '/en/account/reset-password',
    '/fr/account/forgot-password/',
    '/fr/account/reset-password/',
  ])('leaves %s anonymous', (path) => {
    expect(requiresSession(path)).toBe(false);
  });

  // The anonymous list is a prefix-free allowlist, not a substring match: a path that
  // merely STARTS with an anonymous page's name is still gated.
  it.each(['/en/account/reset-password/extra', '/en/account/forgot-password-x'])(
    'still gates %s',
    (path) => {
      expect(requiresSession(path)).toBe(true);
    },
  );

  it('sends a signed-out visitor to sign-in in their own locale', () => {
    expect(signInPath('/fr/account')).toBe('/fr/account/sign-in');
    expect(signInPath('/en/account/whatever')).toBe('/en/account/sign-in');
    // Unknown locale falls back rather than reflecting the path segment into a
    // redirect target.
    expect(signInPath('/zz/account')).toBe('/en/account/sign-in');
  });
});

describe('the proxy-aware origin check', () => {
  it('accepts a same-origin submission through the gateway', () => {
    expect(originIsTrusted(proxied('/en/account/sign-in', { method: 'POST' }))).toBe(true);
  });

  it('refuses a cross-origin submission', () => {
    expect(
      originIsTrusted(proxied('/en/account/sign-in', { method: 'POST', origin: 'https://evil.test' })),
    ).toBe(false);
  });

  // Every current browser sends Origin on POST; a request without one is a
  // non-browser caller, and the storefront has no non-browser writers.
  it('refuses a submission with no Origin at all', () => {
    expect(originIsTrusted(proxied('/en/account/sign-in', { method: 'POST', origin: '' }))).toBe(
      false,
    );
  });

  it('refuses when the forwarded headers are missing', () => {
    expect(originIsTrusted(proxied('/en/account/sign-in', { method: 'POST', proto: '' }))).toBe(false);
    expect(originIsTrusted(proxied('/en/account/sign-in', { method: 'POST', host: '' }))).toBe(false);
  });

  // X-Forwarded-* accumulate left to right, so the first entry is the hop the
  // browser addressed. Comparing against the joined string would 403 every write
  // behind two proxies.
  it('reads the first hop of a chained forwarded header', () => {
    const request = proxied('/en/account/sign-in', {
      method: 'POST',
      proto: 'https, http',
      host: 'tickets.example.test, storefront:8080',
    });
    expect(originIsTrusted(request)).toBe(true);
  });

  it('classifies methods that can change state', () => {
    expect(isUnsafeMethod('POST')).toBe(true);
    expect(isUnsafeMethod('delete')).toBe(true);
    expect(isUnsafeMethod('GET')).toBe(false);
    expect(isUnsafeMethod('head')).toBe(false);
  });

  it('names nothing in the refusal', () => {
    const response = forbiddenResponse();
    expect(response.status).toBe(403);
    expect(response.headers.get('cache-control')).toBe('no-store');
  });
});

describe('gate composition', () => {
  // The claim is not "a cross-origin POST gets a 403" — it is "refused BEFORE any
  // credential is read". Nothing observable from outside distinguishes those: a
  // gate that ran the handler first, let it mint a session, and then replaced the
  // response emits identical bytes while an orphaned session survives. So the
  // test instruments both seams.
  it('refuses a cross-origin submission without reaching lookup or next', async () => {
    const { calls, gate } = deps(
      proxied('/en/account/sign-in', { method: 'POST', origin: 'https://evil.test' }),
      '/en/account/sign-in',
    );

    const response = await gateRequest(gate);

    expect(response.status).toBe(403);
    expect(calls.next).not.toHaveBeenCalled();
    expect(calls.lookup).not.toHaveBeenCalled();
  });

  // Exempting sign-in leaves login-CSRF open; exempting sign-out lets any site
  // sign a buyer out. Both are anonymous PATHS and still origin-checked.
  it.each(['/en/account/sign-in', '/en/account/register', '/en/account/sign-out'])(
    'origin-checks %s even though it is anonymous',
    async (path) => {
      const { calls, gate } = deps(
        proxied(path, { method: 'POST', origin: 'https://evil.test' }),
        path,
      );

      const response = await gateRequest(gate);

      expect(response.status).toBe(403);
      expect(calls.next).not.toHaveBeenCalled();
    },
  );

  it('lets a signed-out visitor through to every public page', async () => {
    for (const path of ['/en/events', '/fr/events/abc', '/en/tickets/ref-1']) {
      const { calls, gate } = deps(proxied(path), path);

      const response = await gateRequest(gate);

      expect(response.status).toBe(200);
      expect(calls.next).toHaveBeenCalled();
      expect(calls.redirectToSignIn).not.toHaveBeenCalled();
      // Guest-first: a public page must not even consult the session.
      expect(calls.lookup).not.toHaveBeenCalled();
    }
  });

  it('redirects a signed-out visitor away from an account page', async () => {
    const { calls, gate } = deps(proxied('/fr/account'), '/fr/account');

    const response = await gateRequest(gate);

    expect(response.status).toBe(302);
    expect(response.headers.get('location')).toBe('/fr/account/sign-in');
    expect(calls.next).not.toHaveBeenCalled();
  });

  it('hands the principal downstream on an authenticated account page', async () => {
    const principal = { customerId: 'cust-a', email: 'alice@example.test' };
    const { calls, gate } = deps(proxied('/en/account'), '/en/account', principal);

    const response = await gateRequest(gate);

    expect(response.status).toBe(200);
    expect(calls.onAuthenticated).toHaveBeenCalledWith(principal);
    expect(calls.next).toHaveBeenCalled();
  });

  it('allows a same-origin POST through', async () => {
    const { calls, gate } = deps(
      proxied('/en/account/sign-in', { method: 'POST' }),
      '/en/account/sign-in',
    );

    const response = await gateRequest(gate);

    expect(response.status).toBe(200);
    expect(calls.next).toHaveBeenCalled();
  });
});
