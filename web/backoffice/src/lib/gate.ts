// Request policy for the back-office gate (TKT-190 / US-B1): which paths may be
// reached without a session, and which submissions the server will accept.
//
// Kept separate from middleware.ts and free of Astro imports so both rules are
// unit-testable as plain functions — the middleware is only the wiring.

import { canAccessRoute, isAnonymousRoute } from './authorization';

/** Astro's `base`. Every path this app serves is under it, healthz included. */
export const BASE = '/admin';

export const LOGIN_PATH = `${BASE}/login`;
export const LOGOUT_PATH = `${BASE}/logout`;
export const HEALTHZ_PATH = `${BASE}/healthz`;

/**
 * Is this path reachable without a session?
 *
 * Delegates to the route matrix, which is the single declaration of the
 * unauthenticated attack surface (ai-review F2). This used to be a separate
 * hand-written predicate, and the two had already drifted apart on bare
 * `/admin/_astro`. Kept as a named export because it reads better at the call
 * site and because TKT-190's tests exercise it as a second view of the same
 * rule — not as a second rule.
 *
 * Which routes are anonymous, and why, is documented on ROUTE_MATRIX itself.
 */
export function isAnonymousPath(pathname: string): boolean {
  return isAnonymousRoute(pathname);
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

/**
 * The refusal for an untrusted origin. Generic and terse: naming the expected
 * origin would hand an attacker the one string they need.
 */
export function forbiddenResponse(): Response {
  return new Response('forbidden\n', {
    status: 403,
    headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
  });
}

/** What the gate needs from its host. Astro supplies these; a test supplies fakes. */
export interface GateDeps<P extends { role: string }> {
  request: Request;
  pathname: string;
  /** The raw session cookie value, or '' when absent. */
  sessionToken: string;
  /** Resolves a token to a principal. MUST NOT be reached by a refused request. */
  lookup: (token: string) => P | undefined;
  /** Called with the principal once the request is allowed through. */
  onAuthenticated: (principal: P) => void;
  redirectToLogin: () => Response;
  /** Everything downstream: routing, page rendering, the login handler. */
  next: () => Promise<Response>;
}

/**
 * The whole gate, composed — and composed HERE rather than in middleware.ts,
 * because the security property IS the composition (ai-review pass 2 S2, pass 3
 * T2).
 *
 * The claim TKT-190 makes is not "a cross-origin submission gets a 403", it is
 * "a cross-origin submission is refused **before any credential is read**". Those
 * differ, and nothing observable from outside distinguishes them: a middleware
 * that ran the handler first, let it verify the password and create a session,
 * and only then replaced the response with a 403 emits the identical bytes —
 * the discarded response takes its Set-Cookie with it — while an orphaned
 * server-side session survives.
 *
 * Testing the origin check alone does not pin that either; it proves one helper
 * behaves, not that the caller invokes it first. So the ordering lives in one
 * Astro-free function that owns the ONLY calls to `next` and `lookup`, and the
 * tests instrument both: a refused request must reach neither.
 *
 * Order:
 *   1. origin, for every unsafe method — including login and logout. Exempting
 *      login leaves login-CSRF open; exempting logout lets any site sign a staff
 *      member out.
 *   2. session, for every path that is not explicitly anonymous, including
 *      unknown ones, so an anonymous caller cannot tell a real admin page from a
 *      path that does not exist.
 */
export async function gateRequest<P extends { role: string }>(deps: GateDeps<P>): Promise<Response> {
  if (isUnsafeMethod(deps.request.method) && !originIsTrusted(deps.request)) {
    return forbiddenResponse();
  }
  if (!isAnonymousPath(deps.pathname)) {
    const principal = deps.lookup(deps.sessionToken);
    if (!principal) {
      // Authentication BEFORE authorization, and the order is observable: an
      // anonymous caller gets 302-to-login, never 403. Reversing these would
      // tell an anonymous caller which routes exist and which roles they need
      // (COS-7; TKT-190's redirect test pins it).
      return deps.redirectToLogin();
    }
    // TKT-197. Fail closed on everything: an unclassified route, an
    // unrecognised role, and a role not on the route's list all refuse. A route
    // nobody classified must not be a route everybody can reach.
    if (!canAccessRoute(deps.pathname, principal.role)) {
      // The SAME generic refusal as an untrusted origin — it names neither the
      // required role nor whether the route exists, so a signed-in box-office
      // member cannot map the admin surface by probing it.
      return forbiddenResponse();
    }
    deps.onAuthenticated(principal);
  }
  return deps.next();
}
