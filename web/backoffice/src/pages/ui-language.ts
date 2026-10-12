// The UI-language switch (TKT-368, D1(a)). POST only.
//
// An ordinary gated route (ROUTE_MATRIX: every staff role, never anonymous), so
// the gate's origin check and session check run before this does, exactly as for
// every other write. It sets one cookie and answers 303 to the page the operator
// was on. It touches nothing else: not the session, not the principal, and not
// any business form — the switch is its own form, never nested in another.
//
// A GET would be simpler to link, and is not used: a state change on GET is
// triggerable by any <img> on any site, and the origin check guards only unsafe
// methods.

import type { APIRoute } from 'astro';

import { publicSchemeIsHttps } from '../lib/forwarded';
import { UI_LANGUAGE_COOKIE, safeReturnTo, uiLanguageCookieOptions } from '../lib/ui-language';
import { parseUiLanguage } from '../lib/ui-strings';

export const POST: APIRoute = async ({ request, cookies, redirect }) => {
  let form: FormData;
  try {
    form = await request.formData();
  } catch {
    // Not a form: set nothing, and land somewhere the operator can act.
    return redirect(safeReturnTo(null), 303);
  }
  cookies.set(
    UI_LANGUAGE_COOKIE,
    parseUiLanguage(form.get('ui_language')),
    uiLanguageCookieOptions({ secure: publicSchemeIsHttps(request.headers) }),
  );
  return redirect(safeReturnTo(form.get('return_to')), 303);
};
