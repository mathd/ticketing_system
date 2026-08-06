// The claim bridge (TKT-223 / US-A4).
//
// Same shape and the same reason as `/checkout` (TKT-221): the session cookie is
// httpOnly, so nothing in the browser can prove who is claiming, and commerce has
// no way to resolve a storefront session. Something server-side has to stand
// between them.
//
// Not under `/api/`, and not locale-scoped — `/checkout` established both, and two
// bridges with two shapes is a convention nobody can state. The locale rides as a
// form field, because the redirect needs it.
import type { APIRoute } from 'astro';

import { claimGuestOrder } from '../lib/customer-api';
import { LOCALES, type Locale } from '../lib/locales';
import { SESSION_COOKIE, lookupSession } from '../lib/session';

export const POST: APIRoute = async ({ request, cookies, redirect }) => {
  const form = await request.formData();
  const raw = String(form.get('locale') ?? '');
  const locale: Locale = LOCALES.includes(raw as Locale) ? (raw as Locale) : 'en';
  const ref = String(form.get('guest_order_ref') ?? '');

  const principal = lookupSession(cookies.get(SESSION_COOKIE)?.value ?? '');
  if (!principal) {
    // Signed out — or a session that has since expired. Send them to sign in
    // rather than refusing: the claim is the whole reason they are here, and the
    // ticket page they came from is public and still works.
    return redirect(`/${locale}/account/sign-in`, 303);
  }
  if (!ref) return redirect(`/${locale}/account`, 303);

  const result = await claimGuestOrder(ref, principal.assertion);

  // Outcome as a query flag on the page the buyer came from, and the flag is a
  // fixed vocabulary rather than a message: anything reflected from a response
  // into a URL is a place to be careful, and there is nothing here that needs to
  // be.
  if (result.ok) return redirect(`/${locale}/account`, 303);
  return redirect(`/${locale}/tickets/${encodeURIComponent(ref)}?claim=${result.reason}`, 303);
};
