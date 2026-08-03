import { describe, expect, it } from 'vitest';

import { LOGIN_PATH, isAnonymousPath, originIsTrusted } from '../src/lib/gate';

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
