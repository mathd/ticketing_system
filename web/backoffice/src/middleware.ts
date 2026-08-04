// The back-office gate (TKT-190 / US-B1). Wiring only — every rule lives in
// src/lib/gate.ts and src/lib/session.ts as plain functions, so all of them are
// unit-testable without Astro (middleware.ts imports `astro:middleware`, which
// does not resolve under vitest).
//
// Order matters and is deliberate:
//   1. `guardUnsafeRequest` runs for EVERY unsafe method, including the login and
//      logout posts — exempting login would leave login-CSRF open, and exempting
//      logout would let any site sign a staff member out. It owns the ONLY call
//      to `next`, which is what makes "refused before any credential is read"
//      testable rather than merely intended (gate.ts, ai-review pass 2 S2).
//   2. the session check runs for every path that is not explicitly anonymous,
//      including unknown ones — so an anonymous caller cannot tell a real admin
//      page from a path that does not exist.

import { defineMiddleware } from 'astro:middleware';

import { LOGIN_PATH, guardUnsafeRequest, isAnonymousPath } from './lib/gate';
import { SESSION_COOKIE, lookupSession } from './lib/session';

export const onRequest = defineMiddleware(async (context, next) => {
  const { request, url, cookies } = context;

  return guardUnsafeRequest(request, async () => {
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
    // Authenticated HTML is per-staff-member and must never reach a shared cache.
    // The login page is marked too: it is where a credential is typed.
    response.headers.set('Cache-Control', 'no-store');
    return response;
  });
});
