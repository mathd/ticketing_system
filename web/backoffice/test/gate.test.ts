import { describe, expect, it } from 'vitest';

import { LOGIN_PATH, gateRequest, isAnonymousPath, originIsTrusted } from '../src/lib/gate';

describe('anonymous paths (COS-1)', () => {
  // The exemption list is the whole attack surface of the gate: anything on it is
  // reachable without a session, so it is enumerated rather than pattern-matched.
  it('exempts exactly the login page, the health probe and the static assets', () => {
    expect(isAnonymousPath('/admin/login')).toBe(true);
    expect(isAnonymousPath('/admin/healthz')).toBe(true);
    expect(isAnonymousPath('/admin/_astro/index.abc123.css')).toBe(true);
  });

  it('gates the venue list, its subpaths and the base itself', () => {
    for (const path of ['/admin', '/admin/', '/admin/venues/abc', '/admin/venues']) {
      expect(isAnonymousPath(path)).toBe(false);
    }
  });

  // An unknown admin path must be gated too. If unknown paths 404'd anonymously
  // while real ones redirected, the difference would map the admin surface for
  // anyone who cared to look.
  it('gates unknown admin paths, so anonymous callers cannot map the surface', () => {
    expect(isAnonymousPath('/admin/nope')).toBe(false);
    expect(isAnonymousPath('/admin/orders/secret')).toBe(false);
  });

  // The health probe is exempt because Compose hits it directly on the container,
  // pre-gateway: gating it makes the container unhealthy, which makes the
  // gateway's `depends_on: service_healthy` never satisfy and the whole stack
  // fails to start. But the exemption must be the exact path, not a prefix.
  it('does not let a lookalike ride the healthz exemption', () => {
    expect(isAnonymousPath('/admin/healthz/../venues')).toBe(false);
    expect(isAnonymousPath('/admin/healthzz')).toBe(false);
    expect(isAnonymousPath('/admin/healthz/anything')).toBe(false);
  });

  it('does not let a lookalike ride the login exemption', () => {
    expect(isAnonymousPath('/admin/login/../venues')).toBe(false);
    expect(isAnonymousPath('/admin/logins')).toBe(false);
  });

  it('agrees with the path it redirects to', () => {
    expect(isAnonymousPath(LOGIN_PATH)).toBe(true);
  });
});

describe('proxy-aware origin check (COS-7)', () => {
  const req = (headers: Record<string, string>) =>
    new Request('http://backoffice:8080/admin/login', { method: 'POST', headers });

  // The back office is served exclusively behind the gateway, which sets
  // X-Forwarded-* from the real inbound request (Go's SetXForwarded overwrites
  // rather than appends for Host and Proto, so a client cannot forge them
  // *through* the gateway). Astro's own checkOrigin compares Origin against the
  // container's Host, which never matches through a proxy — that is why it is
  // disabled and this stands in its place.
  it('accepts a submission whose Origin matches the forwarded public origin', () => {
    expect(
      originIsTrusted(
        req({
          origin: 'http://localhost:8080',
          'x-forwarded-proto': 'http',
          'x-forwarded-host': 'localhost:8080',
        }),
      ),
    ).toBe(true);
  });

  it('refuses a submission from another site', () => {
    expect(
      originIsTrusted(
        req({
          origin: 'http://evil.example',
          'x-forwarded-proto': 'http',
          'x-forwarded-host': 'localhost:8080',
        }),
      ),
    ).toBe(false);
  });

  it('refuses a submission with no Origin at all', () => {
    expect(
      originIsTrusted(req({ 'x-forwarded-proto': 'http', 'x-forwarded-host': 'localhost:8080' })),
    ).toBe(false);
  });

  // Straight at the container, bypassing the gateway: no forwarded headers, so
  // there is no public origin to compare against. Fail closed.
  it('refuses when the forwarded headers are absent', () => {
    expect(originIsTrusted(req({ origin: 'http://backoffice:8080' }))).toBe(false);
  });

  it('refuses when only one of the two forwarded headers is present', () => {
    expect(
      originIsTrusted(req({ origin: 'http://localhost:8080', 'x-forwarded-host': 'localhost:8080' })),
    ).toBe(false);
    expect(
      originIsTrusted(req({ origin: 'http://localhost:8080', 'x-forwarded-proto': 'http' })),
    ).toBe(false);
  });

  // Comma-separated forwarded values are a proxy chain; the first entry is the
  // original client-facing hop. Comparing against the raw joined string would
  // never match and would 403 every write behind two proxies.
  it('reads the first hop of a proxy chain', () => {
    expect(
      originIsTrusted(
        req({
          origin: 'https://tickets.example',
          'x-forwarded-proto': 'https, http',
          'x-forwarded-host': 'tickets.example, gateway:8080',
        }),
      ),
    ).toBe(true);
  });

  it('is scheme-sensitive: an https page is not the same origin as an http one', () => {
    expect(
      originIsTrusted(
        req({
          origin: 'http://tickets.example',
          'x-forwarded-proto': 'https',
          'x-forwarded-host': 'tickets.example',
        }),
      ),
    ).toBe(false);
  });
});

describe('ordering: refused before any credential is read (ai-review S2, T2)', () => {
  // Pass 2 established that no wire-level test can see this ordering: a
  // middleware that ran the handler first, created a session, then replaced the
  // response with a 403 emits identical bytes, because the discarded response
  // takes its Set-Cookie with it — while an orphaned server-side session lives on.
  //
  // Pass 3 (T2) then pointed out that testing the origin CHECK alone does not pin
  // it either: that proves one helper behaves, not that the caller invokes it
  // first. So these drive `gateRequest`, which owns the only calls to `next` and
  // `lookup`, and instrument BOTH — a refused request must reach neither.
  const trusted = {
    origin: 'http://localhost:8080',
    'x-forwarded-proto': 'http',
    'x-forwarded-host': 'localhost:8080',
  };

  function harness(request: Request, pathname: string) {
    const calls = { next: 0, lookup: 0, authenticated: 0, redirect: 0 };
    const run = () =>
      gateRequest({
        request,
        pathname,
        sessionToken: 'a-valid-looking-token',
        lookup: (t) => {
          calls.lookup++;
          return t ? ({ staffId: 's1', organizerId: 'o1' } as const) : undefined;
        },
        onAuthenticated: () => {
          calls.authenticated++;
        },
        redirectToLogin: () => {
          calls.redirect++;
          return new Response(null, { status: 302 });
        },
        next: async () => {
          calls.next++;
          return new Response('downstream ran', { status: 200 });
        },
      });
    return { calls, run };
  }

  const unsafe = (headers: Record<string, string>, path = '/admin/login') =>
    new Request(`http://backoffice:8080${path}`, { method: 'POST', headers });

  it('reaches neither the session lookup nor anything downstream, cross-site', async () => {
    const { calls, run } = harness(unsafe({ ...trusted, origin: 'http://evil.example' }), '/admin/login');
    const res = await run();
    expect(res.status).toBe(403);
    expect(calls).toEqual({ next: 0, lookup: 0, authenticated: 0, redirect: 0 });
  });

  it('reaches neither, when Origin is absent entirely', async () => {
    const { calls, run } = harness(
      unsafe({ 'x-forwarded-proto': 'http', 'x-forwarded-host': 'localhost:8080' }),
    );
    const res = await run();
    expect(res.status).toBe(403);
    expect(calls).toEqual({ next: 0, lookup: 0, authenticated: 0, redirect: 0 });
  });

  // The login page is anonymous, so nothing else would stop this one — the
  // origin check is the ONLY thing standing between a forged cross-site POST and
  // the credential handler.
  it('refuses an unsafe request to an ANONYMOUS path too', async () => {
    for (const path of ['/admin/login', '/admin/healthz']) {
      const { calls, run } = harness(unsafe({ origin: 'http://evil.example' }, path), path);
      expect((await run()).status).toBe(403);
      expect(calls.next).toBe(0);
    }
  });

  it('lets a same-origin submission through to the handler', async () => {
    const { calls, run } = harness(unsafe(trusted), '/admin/login');
    const res = await run();
    expect(res.status).toBe(200);
    expect(calls.next).toBe(1);
  });

  // Reads are not state changes, and the Compose health probe arrives with no
  // forwarded headers at all — refusing safe methods would take the container
  // unhealthy and the whole stack down with it.
  it('lets a safe method through whatever its origin', async () => {
    const req = new Request('http://backoffice:8080/admin/healthz', {
      method: 'GET',
      headers: { origin: 'http://evil.example' },
    });
    const { calls, run } = harness(req, '/admin/healthz');
    await run();
    expect(calls.next).toBe(1);
  });

  it('redirects an unauthenticated caller instead of running the page', async () => {
    const req = new Request('http://backoffice:8080/admin/', { method: 'GET' });
    const calls = { next: 0, redirect: 0 };
    const res = await gateRequest({
      request: req,
      pathname: '/admin/',
      sessionToken: '',
      lookup: () => undefined, // no session
      onAuthenticated: () => {},
      redirectToLogin: () => {
        calls.redirect++;
        return new Response(null, { status: 302 });
      },
      next: async () => {
        calls.next++;
        return new Response('the venue list', { status: 200 });
      },
    });
    expect(res.status).toBe(302);
    expect(calls).toEqual({ next: 0, redirect: 1 });
  });

  it('refuses without a cacheable header', async () => {
    const { run } = harness(unsafe({ origin: 'http://evil.example' }));
    expect((await run()).headers.get('cache-control')).toBe('no-store');
  });
});
