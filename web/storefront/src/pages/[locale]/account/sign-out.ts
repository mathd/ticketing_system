// Customer sign-out (TKT-220 / US-A1).
//
// POST only. A GET sign-out is triggered by any <img src> on any page on the
// internet, and the middleware's origin check does not run on safe methods — so
// exporting GET here would hand every site a way to sign a buyer out.
//
// Two halves, and the server-side one is what matters: destroySession removes the
// entry, so a token someone captured stops working. Expiring the browser cookie
// only asks the browser to forget.
import type { APIRoute } from 'astro';

import { LOCALES, type Locale } from '../../../lib/locales';
import { SESSION_COOKIE, SESSION_COOKIE_PATH, sessionStore } from '../../../lib/session';

export const POST: APIRoute = ({ params, cookies, redirect }) => {
  const locale = LOCALES.includes(params.locale as Locale) ? (params.locale as Locale) : 'en';

  const token = cookies.get(SESSION_COOKIE)?.value;
  if (token) sessionStore.destroy(token);

  // The delete MUST name the same path the cookie was set with, or the browser
  // keeps the old cookie beside the deletion and hands it straight back on the
  // next request. One constant, used by both.
  cookies.delete(SESSION_COOKIE, { path: SESSION_COOKIE_PATH });

  return redirect(`/${locale}/events`, 303);
};
