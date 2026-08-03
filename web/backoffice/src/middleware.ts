// The back-office gate (TKT-190 / US-B1). Wiring only — both rules live in
// src/lib/gate.ts as plain functions so they are unit-testable without Astro.
//
// Order matters and is deliberate:
//   1. the origin check runs for EVERY unsafe method, including the login and
//      logout posts. Exempting login would leave login-CSRF open, and exempting
//      logout would let any site sign a staff member out.
//   2. the session check runs for every path that is not explicitly anonymous,
//      including unknown ones — so an anonymous caller cannot tell a real admin
//      page from a path that does not exist.

import { defineMiddleware } from 'astro:middleware';

import { LOGIN_PATH, isAnonymousPath, isUnsafeMethod, originIsTrusted } from './lib/gate';
import { SESSION_COOKIE, lookupSession } from './lib/session';

export const onRequest = defineMiddleware(async (context, next) => {
  const { request, url, cookies } = context;

  if (isUnsafeMethod(request.method) && !originIsTrusted(request)) {
    // Generic and terse: naming the expected origin would hand an attacker the
    // one string they need.
    return new Response('forbidden\n', {
      status: 403,
      headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
    });
  }

  if (!isAnonymousPath(url.pathname)) {
    const principal = lookupSession(cookies.get(SESSION_COOKIE)?.value ?? '');
    if (!principal) {
      // 302 to the login page rather than a 401 body: the caller is a browser
      // following a link, and it should land somewhere it can act. No `next`
      // parameter — an open redirect off the sign-in page is a well-worn
      // phishing primitive, and the venue list is a fine place to land.
      return context.redirect(LOGIN_PATH, 302);
    }
    // Everything downstream reads the organizer from here rather than from a
    // hard-coded constant.
    context.locals.staff = principal;
  }

  const response = await next();
  // Authenticated HTML is per-staff-member and must never reach a shared cache;
  // healthz is exempt from the gate but harmless to mark. The login page is
  // marked too: it is where a credential is typed.
  response.headers.set('Cache-Control', 'no-store');
  return response;
});
