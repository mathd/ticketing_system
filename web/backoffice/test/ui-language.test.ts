import { describe, expect, it } from 'vitest';

import { publicSchemeIsHttps } from '../src/lib/forwarded';
import {
  UI_LANGUAGE_COOKIE,
  safeReturnTo,
  uiLanguageCookieOptions,
  uiLanguageFrom,
} from '../src/lib/ui-language';
import { POST } from '../src/pages/ui-language';
import { SESSION_COOKIE } from '../src/lib/session';

// TKT-368 / D1(a). The UI-language switch is a POST to its own endpoint, which
// sets one cookie and sends the operator back to the page they were on. The page
// they were on arrives as a hidden `return_to` field — client-supplied, so it is
// confined to a same-origin path under the back office before it becomes a
// Location header. An open redirect off a staff tool is a phishing primitive.

describe('safeReturnTo', () => {
  it('keeps a back-office path and its query', () => {
    expect(safeReturnTo('/admin/events/new?event=x')).toBe('/admin/events/new?event=x');
    expect(safeReturnTo('/admin/events/new?event=1&performance=2')).toBe(
      '/admin/events/new?event=1&performance=2',
    );
    expect(safeReturnTo('/admin/')).toBe('/admin/');
    expect(safeReturnTo('/admin')).toBe('/admin');
  });

  it('drops a fragment rather than echoing it', () => {
    expect(safeReturnTo('/admin/events/new#top')).toBe('/admin/events/new');
  });

  it.each([
    ['protocol-relative', '//evil.example/x'],
    ['absolute', 'https://evil.example/'],
    ['absolute URL whose path is under the base', 'https://evil.example/admin/events/new'],
    ['protocol-relative URL whose path is under the base', '//evil.example/admin/events/new'],
    ['backslash', '/\\evil.example'],
    ['backslash later in the path', '/admin/\\\\evil.example'],
    ['userinfo lookalike', '/admin@evil.example'],
    ['dot segments out of the base', '/admin/../x'],
    ['mixed dot segments out of the base', '/admin/./../x'],
    ['prefix lookalike', '/adminx'],
    ['javascript scheme', 'javascript:alert(1)'],
    ['relative path', 'admin/events/new'],
    ['empty', ''],
    ['whitespace-led', ' /admin/'],
    ['control character', '/admin/\nLocation: https://evil.example'],
    // The URL parser strips a trailing C0 control, so without its own check this
    // would come back repaired as /admin/events/new.
    ['trailing NUL', '/admin/events/new\u0000'],
    ['trailing unit separator', '/admin/events/new\u001f'],
    ['interior DEL', '/admin/events\u007fnew'],
  ])('falls back to the venue list for a %s value', (_label, raw) => {
    expect(safeReturnTo(raw)).toBe('/admin/');
  });

  it('falls back when the field is absent or not text', () => {
    expect(safeReturnTo(null)).toBe('/admin/');
    expect(safeReturnTo(undefined)).toBe('/admin/');
    expect(safeReturnTo(new File(['/admin/'], 'x'))).toBe('/admin/');
  });
});

describe('uiLanguageFrom', () => {
  const jar = (value?: string) => ({
    get: (name: string) => (name === UI_LANGUAGE_COOKIE && value !== undefined ? { value } : undefined),
  });

  it('reads the cookie', () => {
    expect(uiLanguageFrom(jar('fr'))).toBe('fr');
    expect(uiLanguageFrom(jar('en'))).toBe('en');
  });

  it('defaults to English when the cookie is absent or unknown', () => {
    expect(uiLanguageFrom(jar())).toBe('en');
    expect(uiLanguageFrom(jar('FR'))).toBe('en');
    expect(uiLanguageFrom(jar('de'))).toBe('en');
  });

  it('is not the session cookie', () => {
    expect(UI_LANGUAGE_COOKIE).not.toBe(SESSION_COOKIE);
  });
});

describe('uiLanguageCookieOptions', () => {
  it('covers the whole back office for a year, HttpOnly and SameSite=Lax', () => {
    expect(uiLanguageCookieOptions({ secure: false })).toEqual({
      path: '/admin',
      httpOnly: true,
      sameSite: 'lax',
      maxAge: 365 * 24 * 60 * 60,
      secure: false,
    });
  });

  it('is Secure when the public origin is https', () => {
    expect(uiLanguageCookieOptions({ secure: true }).secure).toBe(true);
  });
});

describe('publicSchemeIsHttps', () => {
  const headers = (proto?: string) => new Headers(proto === undefined ? {} : { 'x-forwarded-proto': proto });

  // The gateway sends exactly "http" or "https" (SetXForwarded overwrites the
  // header from its own connection), so those two values are the contract.
  it('is true for the https the gateway reports', () => {
    expect(publicSchemeIsHttps(headers('https'))).toBe(true);
  });

  it('is false for http or an absent header', () => {
    expect(publicSchemeIsHttps(headers('http'))).toBe(false);
    expect(publicSchemeIsHttps(headers())).toBe(false);
    expect(publicSchemeIsHttps(headers(''))).toBe(false);
  });

  // NOT a proxy chain: the gateway cannot produce these values. They pin the
  // parsing login.astro already used for the session cookie (first
  // comma-separated item, trimmed, exact "https"), so extracting it into this
  // helper changed nothing.
  it('keeps login’s pre-existing parsing of values the gateway never sends', () => {
    // Headers already strips the ends of a value, so only a space BEFORE a comma
    // reaches the trim.
    expect(publicSchemeIsHttps(headers('https , http'))).toBe(true);
    expect(publicSchemeIsHttps(headers('https, http'))).toBe(true);
    expect(publicSchemeIsHttps(headers('http, https'))).toBe(false);
    expect(publicSchemeIsHttps(headers('HTTPS'))).toBe(false);
  });
});

describe('POST /admin/ui-language', () => {
  type Set = { name: string; value: string; options: unknown };

  async function call(body: BodyInit | null, headers: Record<string, string> = {}) {
    const sets: Set[] = [];
    const request = new Request('http://backoffice:8080/admin/ui-language', {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded', ...headers },
      body,
    });
    const context = {
      request,
      cookies: {
        set: (name: string, value: string, options: unknown) => sets.push({ name, value, options }),
      },
      redirect: (location: string, status: number) =>
        new Response(null, { status, headers: { location } }),
    };
    const response = await (POST as unknown as (c: typeof context) => Promise<Response>)(context);
    return { response, sets };
  }

  it('sets only the language cookie and returns to the page', async () => {
    const { response, sets } = await call(
      new URLSearchParams({ ui_language: 'fr', return_to: '/admin/events/new?event=x' }),
    );
    expect(response.status).toBe(303);
    expect(response.headers.get('location')).toBe('/admin/events/new?event=x');
    expect(sets).toEqual([
      { name: UI_LANGUAGE_COOKIE, value: 'fr', options: uiLanguageCookieOptions({ secure: false }) },
    ]);
  });

  it('marks the cookie Secure behind an https gateway', async () => {
    const { sets } = await call(new URLSearchParams({ ui_language: 'fr', return_to: '/admin/' }), {
      'x-forwarded-proto': 'https',
    });
    expect(sets[0]?.options).toEqual(uiLanguageCookieOptions({ secure: true }));
  });

  it('stores English for an unsupported value', async () => {
    const { sets } = await call(new URLSearchParams({ ui_language: 'FR', return_to: '/admin/' }));
    expect(sets.map((s) => s.value)).toEqual(['en']);
  });

  it('never redirects off the back office', async () => {
    const { response } = await call(new URLSearchParams({ ui_language: 'fr', return_to: '//evil.example/' }));
    expect(response.status).toBe(303);
    expect(response.headers.get('location')).toBe('/admin/');
  });

  it('sets nothing when the body is not a form', async () => {
    const { response, sets } = await call('{"ui_language":"fr"}', { 'content-type': 'application/json' });
    expect(response.status).toBe(303);
    expect(response.headers.get('location')).toBe('/admin/');
    expect(sets).toEqual([]);
  });
});
