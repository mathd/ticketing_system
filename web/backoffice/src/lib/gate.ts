// Request policy for the back-office gate (TKT-190 / US-B1): which paths may be
// reached without a session, and which submissions the server will accept.
//
// Kept separate from middleware.ts and free of Astro imports so both rules are
// unit-testable as plain functions — the middleware is only the wiring.

/** Astro's `base`. Every path this app serves is under it, healthz included. */
export const BASE = '/admin';

export const LOGIN_PATH = `${BASE}/login`;
export const LOGOUT_PATH = `${BASE}/logout`;
export const HEALTHZ_PATH = `${BASE}/healthz`;

/**
 * The exemption list IS the anonymous attack surface, so it is enumerated
 * exactly rather than matched by prefix — `/admin/healthz/../venues` and
 * `/admin/healthzz` must not ride it.
 *
 * `healthz` is exempt because Compose probes it **directly on the container**,
 * before the gateway. Gating it makes the container unhealthy, which makes the
 * gateway's `depends_on: { backoffice: service_healthy }` never satisfy, and the
 * entire stack fails to start — a failure that looks nothing like an auth bug.
 *
 * `_astro/` is the build's hashed static assets, needed to render the login page
 * itself. It is a prefix by necessity (the filenames are content-hashed), and it
 * serves only files the build emitted.
 */
export function isAnonymousPath(pathname: string): boolean {
  const path = normalize(pathname);
  return path === LOGIN_PATH || path === HEALTHZ_PATH || path.startsWith(`${BASE}/_astro/`);
}

/** Trailing slashes are cosmetic; `/admin/` and `/admin` are the same page. */
function normalize(pathname: string): string {
  if (pathname.length > 1 && pathname.endsWith('/')) return pathname.slice(0, -1);
  return pathname;
}

/**
 * The server-side CSRF control.
 *
 * Astro's own `security.checkOrigin` is disabled (astro.config.mjs) and cannot
 * simply be re-enabled: it compares the browser's `Origin` against the app's own
 * `Host`, which behind the gateway is the container name and never matches — it
 * 403s every POST (TKT-105). This stands in its place and compares against the
 * *public* origin the gateway reports instead.
 *
 * Go's `httputil.ReverseProxy.SetXForwarded()` sets X-Forwarded-Host and
 * X-Forwarded-Proto from the real inbound request, overwriting anything the
 * client sent, so these cannot be forged **through the gateway**. They can be
 * forged by anything already inside the Compose network — this control binds a
 * browser to one origin, it does not constrain an attacker who already has
 * network access to the container. ADR-042 names that boundary.
 *
 * Missing Origin is refused rather than allowed. Every current browser sends it
 * on POST; a request without one is a non-browser caller, and the back office
 * has no non-browser writers.
 */
export function originIsTrusted(request: Request): boolean {
  const origin = request.headers.get('origin');
  const proto = firstHop(request.headers.get('x-forwarded-proto'));
  const host = firstHop(request.headers.get('x-forwarded-host'));
  if (!origin || !proto || !host) return false;
  return origin === `${proto}://${host}`;
}

/**
 * X-Forwarded-* accumulate left-to-right through a proxy chain, so the first
 * entry is the hop the browser actually addressed. Comparing against the whole
 * joined string would never match and would 403 every write behind two proxies.
 */
function firstHop(value: string | null): string | undefined {
  const first = value?.split(',')[0]?.trim();
  return first || undefined;
}

/** Methods that can change state, and so must carry a trusted origin. */
export function isUnsafeMethod(method: string): boolean {
  return !['GET', 'HEAD', 'OPTIONS'].includes(method.toUpperCase());
}
