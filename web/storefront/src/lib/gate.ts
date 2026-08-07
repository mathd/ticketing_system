// Request policy for the storefront (TKT-220 / US-A1): which submissions the
// server accepts, and which paths need a customer session.
//
// Kept separate from middleware.ts and free of Astro imports so both rules are
// unit-testable as plain functions — `astro:middleware` is a virtual module that
// only exists inside an Astro build, so a rule left in the middleware file is a
// rule nothing can test. The storefront already splits page-tier.ts out for the
// same reason.
//
// The storefront is GUEST-FIRST. Almost nothing here is gated: only the account
// area needs a session, and every existing route stays reachable signed-out.

import { LOCALES } from './locales';

/** `/en/account`, `/fr/account/…` — and nothing else. */
const ACCOUNT_PATH = new RegExp(`^/(?:${LOCALES.join('|')})/account(?:/|$)`);

/**
 * The account pages a signed-out buyer must be able to reach.
 *
 * The two recovery pages (TKT-226) belong here for a reason worth stating: their
 * entire audience is people who CANNOT sign in. Gating them would redirect a
 * locked-out buyer to the sign-in form they are locked out of — the trap this
 * feature exists to remove, rebuilt one layer up.
 */
const ANONYMOUS_ACCOUNT_PATH = new RegExp(
  `^/(?:${LOCALES.join('|')})/account/(?:register|sign-in|forgot-password|reset-password)/?$`,
);

/**
 * Does this path require a customer session?
 *
 * Note the asymmetry with the back office, and it is deliberate: there, an
 * unclassified path fails CLOSED because every admin route is sensitive. Here the
 * default is OPEN, because the storefront's whole job is to sell tickets to
 * anonymous visitors. Only the account subtree is gated, and it is matched
 * positively — an account page added tomorrow is covered by the prefix, while a
 * typo'd path is a 404 rather than an accidentally-public account page.
 */
export function requiresSession(pathname: string): boolean {
  return ACCOUNT_PATH.test(pathname) && !ANONYMOUS_ACCOUNT_PATH.test(pathname);
}

/** Where a signed-out visitor is sent when they ask for an account page. */
export function signInPath(pathname: string): string {
  const locale = pathname.split('/')[1];
  return `/${LOCALES.includes(locale as (typeof LOCALES)[number]) ? locale : 'en'}/account/sign-in`;
}

/**
 * The server-side CSRF control.
 *
 * Astro's own `security.checkOrigin` is disabled (astro.config.mjs) and cannot
 * simply be left on: it compares the browser's `Origin` against the app's own
 * `Host`, which behind the gateway is the container name and never matches — it
 * 403s every POST (TKT-105). The storefront had no form until this ticket, so
 * nothing had ever exercised that. This stands in its place and compares against
 * the *public* origin the gateway reports instead.
 *
 * Go's `httputil.ReverseProxy.SetXForwarded()` sets X-Forwarded-Host and
 * X-Forwarded-Proto from the real inbound request, overwriting anything the
 * client sent, so these cannot be forged **through the gateway**. They can be
 * forged by anything already inside the Compose network — this control binds a
 * browser to one origin, it does not constrain an attacker with network access to
 * the container (ADR-021: name the adversary).
 *
 * Missing Origin is refused rather than allowed. Every current browser sends it
 * on POST, and the storefront has no non-browser writers.
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
export interface GateDeps {
  request: Request;
  pathname: string;
  /** The raw session cookie value, or '' when absent. */
  sessionToken: string;
  /** Resolves a token to a principal. MUST NOT be reached by a refused request. */
  lookup: (token: string) => unknown | undefined;
  /** Called with the principal once an authenticated request is allowed through. */
  onAuthenticated: (principal: unknown) => void;
  redirectToSignIn: (path: string) => Response;
  /** Everything downstream: routing, page rendering, the form handlers. */
  next: () => Promise<Response>;
}

/**
 * The whole gate, composed — and composed HERE rather than in middleware.ts,
 * because the security property IS the composition.
 *
 * The claim is not "a cross-origin submission gets a 403", it is "a cross-origin
 * submission is refused **before any credential is read**". Those differ, and
 * nothing observable from outside distinguishes them: a middleware that ran the
 * handler first, let it verify the password and mint a session, and only then
 * replaced the response with a 403 emits identical bytes — the discarded response
 * takes its Set-Cookie with it — while an orphaned server-side session survives.
 *
 * So the ordering lives in one Astro-free function owning the ONLY calls to
 * `next` and `lookup`, and the tests instrument both: a refused request reaches
 * neither.
 *
 * Order:
 *   1. origin, for every unsafe method — INCLUDING register, sign-in and
 *      sign-out. Exempting sign-in leaves login-CSRF open; exempting sign-out
 *      lets any site sign a buyer out.
 *   2. session, for account pages that are not the two anonymous ones.
 */
export async function gateRequest(deps: GateDeps): Promise<Response> {
  if (isUnsafeMethod(deps.request.method) && !originIsTrusted(deps.request)) {
    return forbiddenResponse();
  }
  if (requiresSession(deps.pathname)) {
    const principal = deps.lookup(deps.sessionToken);
    if (!principal) {
      // 302 rather than a 401 body: the caller is a browser following a link and
      // should land somewhere it can act. No `next` parameter — an open redirect
      // off a sign-in page is a well-worn phishing primitive.
      return deps.redirectToSignIn(signInPath(deps.pathname));
    }
    deps.onAuthenticated(principal);
  }
  return deps.next();
}
