// The back-office UI-language preference (TKT-368, D1).
//
// One cookie, `bo_ui_language`, set by POST /admin/ui-language and read by the
// pages with Astro.cookies. It is a display preference with no security weight:
// it is never on StaffPrincipal, never in the session store, and nothing that
// decides authority reads it. middleware.ts and gate.ts do not know it exists —
// the switch endpoint is an ordinary gated route, so the origin check and the
// session check reach it the way they reach every other write.

import { BASE } from './gate';
import { parseUiLanguage, type UiLanguage } from './ui-strings';

/** Deliberately not `bo_sid`-like: a preference, not a credential. */
export const UI_LANGUAGE_COOKIE = 'bo_ui_language';

/** The whole back office, so a choice made on one page holds on every page. */
export const UI_LANGUAGE_COOKIE_PATH = BASE;

const ONE_YEAR_SECONDS = 365 * 24 * 60 * 60;

/**
 * HttpOnly because no browser script reads it (the page renders server-side);
 * SameSite=Lax like the session cookie; a year, because a language choice is not
 * a session and outlives sign-out. Secure follows the public scheme.
 */
export function uiLanguageCookieOptions(opts: { secure: boolean }) {
  return {
    path: UI_LANGUAGE_COOKIE_PATH,
    httpOnly: true,
    sameSite: 'lax' as const,
    maxAge: ONE_YEAR_SECONDS,
    secure: opts.secure,
  };
}

/** The operator's UI language; absent or unknown is English. */
export function uiLanguageFrom(cookies: { get(name: string): { value: string } | undefined }): UiLanguage {
  return parseUiLanguage(cookies.get(UI_LANGUAGE_COOKIE)?.value);
}

const FALLBACK = `${BASE}/`;

/** C0 controls and DEL. `\s` already covers tab, newline and carriage return. */
function hasControlCharacter(text: string): boolean {
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    if (code <= 0x1f || code === 0x7f) return true;
  }
  return false;
}
const PLACEHOLDER_ORIGIN = 'http://return-to.invalid';

/**
 * Where the switch sends the operator back to: the page they were on.
 *
 * `return_to` arrives as a hidden form field, so it is client-supplied and must
 * not become an open redirect. What closes the redirect is the last two lines:
 * the value is resolved as a URL, and only its PATH and query are returned, and
 * only when that path, after dot segments are removed, is the base or under it.
 * A path that starts with `/admin` cannot be read as another host, so the result
 * is always same-origin. `/admin/../x` resolves to `/x` and falls back.
 *
 * The checks before that refuse input a URL parser would REPAIR, rather than
 * returning the repaired form: a relative path (resolved from the root it would
 * be kept), a backslash (read as `/`), whitespace or a control character
 * (stripped by the parser, so `/admin/\nLocation: …` would come back as a
 * path), and a protocol-relative URL (`//host/admin/…`) whose path happens to
 * be under the base. Each falls back to the venue list instead.
 *
 * The fragment is dropped; the query is kept, because that is where the event
 * builder carries its progress.
 */
export function safeReturnTo(raw: unknown): string {
  if (typeof raw !== 'string' || !raw.startsWith('/')) return FALLBACK;
  if (/[\\\s]/.test(raw) || hasControlCharacter(raw)) return FALLBACK;
  let url: URL;
  try {
    url = new URL(raw, PLACEHOLDER_ORIGIN);
  } catch {
    return FALLBACK;
  }
  if (url.origin !== PLACEHOLDER_ORIGIN) return FALLBACK;
  if (url.pathname !== BASE && !url.pathname.startsWith(`${BASE}/`)) return FALLBACK;
  return url.pathname + url.search;
}
