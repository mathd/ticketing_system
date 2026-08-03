// Sign-out (TKT-190 / US-B1 COS-3).
//
// POST only. A GET logout is triggerable by any <img src> on any page, which
// makes signing a staff member out a one-line prank; and the middleware's origin
// check only guards unsafe methods, so a GET would sidestep it too.

import type { APIRoute } from 'astro';

import { LOGIN_PATH } from '../lib/gate';
import { SESSION_COOKIE, destroySession } from '../lib/session';

export const POST: APIRoute = ({ cookies, redirect }) => {
  const token = cookies.get(SESSION_COOKIE)?.value;
  if (token) {
    // Server-side first. Expiring the browser's copy only asks the browser to
    // forget; anyone who captured the value still holds it, and replaying it is
    // exactly what COS-3 requires to fail.
    destroySession(token);
  }
  // Path must match how it was set, or the browser keeps the old cookie
  // alongside the deletion and sends it right back.
  cookies.delete(SESSION_COOKIE, { path: '/' });
  return redirect(LOGIN_PATH, 303);
};
