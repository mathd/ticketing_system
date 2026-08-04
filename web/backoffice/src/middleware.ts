// The back-office gate (TKT-190 / US-B1) — Astro wiring, and nothing else.
//
// The gate's rules AND their composition live in src/lib/gate.ts. That is
// deliberate: the security property this ticket claims is an ORDERING ("refused
// before any credential is read"), which is a property of how the steps are
// composed, not of any one step. Leaving the composition here would leave it
// untestable, because middleware.ts imports `astro:middleware` and does not
// resolve under vitest — and pass 2 of the adversarial review showed that no
// wire-level test can observe the ordering either.
//
// So this file must stay a pure adapter: translate Astro's context into
// GateDeps, hand over, set the cache header. Any rule added here instead of in
// gate.ts is a rule nothing can test.

import { defineMiddleware } from 'astro:middleware';

import { LOGIN_PATH, gateRequest } from './lib/gate';
import { SESSION_COOKIE, lookupSession } from './lib/session';

export const onRequest = defineMiddleware(async (context, next) => {
  const response = await gateRequest({
    request: context.request,
    pathname: context.url.pathname,
    sessionToken: context.cookies.get(SESSION_COOKIE)?.value ?? '',
    lookup: (token) => lookupSession(token),
    onAuthenticated: (principal) => {
      // Everything downstream reads the organizer from here rather than from a
      // hard-coded constant.
      context.locals.staff = principal;
    },
    // 302 rather than a 401 body: the caller is a browser following a link and
    // should land somewhere it can act. No `next` parameter — an open redirect
    // off a sign-in page is a well-worn phishing primitive, and the venue list
    // is a fine place to land.
    redirectToLogin: () => context.redirect(LOGIN_PATH, 302),
    next,
  });

  // Set on EVERY response the gate produces, including the refusals and the
  // redirect — not only rendered pages. Authenticated HTML is per-staff-member,
  // the login page is where a credential is typed, and a cached "302 to login"
  // would be served to a signed-in staff member by any shared cache that stored
  // it.
  response.headers.set('Cache-Control', 'no-store');
  return response;
});
