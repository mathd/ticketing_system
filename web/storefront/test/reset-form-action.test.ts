import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

// A source scan, in the same spirit as the repo's other invariant guards (catalog's
// staff-credential rule, commerce's attribution allowlist): the property is a property of
// the SOURCE, and no unit test can reach it because `.astro` files are not modules vitest
// can import.
//
// Why it exists (ai-review [high]): the reset page is reached by a GET whose URL carries
// the token in its query. `action=""` — which every other form in this app uses, correctly
// — resolves to the DOCUMENT's URL, so the POST would carry the token in a request URL
// too. Putting it in the hidden body field does not remove it from the URL.
//
// WHAT THIS CANNOT DO, said plainly because a green source scan reads like a proof: it
// does not render the page, does not submit anything, and therefore establishes nothing
// about the URL a browser actually posts to or about the trusted-origin path. Only a real
// submission does that, and it is on the manual browser checklist for this ticket
// (AGENTS.md: a web-UI change is not verified until a browser has submitted its forms).
// What this CAN do is fail the moment someone "simplifies" this form back to the
// `action=""` pattern its sibling pages correctly use.
const source = readFileSync(
  fileURLToPath(new URL('../src/pages/[locale]/account/reset-password.astro', import.meta.url)),
  'utf8',
);

describe('the reset form does not post to a URL carrying the token', () => {
  // Asserts the form uses THIS constant, not merely that some expression is present and
  // the constant exists somewhere (ai-review pass 2 [medium]). The looser pair of checks
  // this replaces would have passed on `action={somethingElse}` with the safe constant
  // sitting unused three lines above.
  it('has exactly one form, and it posts to formAction', () => {
    const forms = source.match(/<form\b[^>]*>/g) ?? [];
    expect(forms).toHaveLength(1);
    expect(forms[0]).toMatch(/\baction=\{formAction\}/);
  });

  it('builds that action from the locale with no query string', () => {
    expect(source).toContain('const formAction = `/${locale}/account/reset-password`;');
    // A query on the action would defeat the whole point.
    expect(source).not.toMatch(/const formAction = [^;]*\?/);
  });

  // The GET's own URL is where the token legitimately lives, and these two headers are
  // what bound how far that one request's URL travels. Deleting either is silent.
  it('keeps the headers that bound where the GET URL can travel', () => {
    expect(source).toContain("Astro.response.headers.set('Referrer-Policy', 'no-referrer')");
    expect(source).toContain("Astro.response.headers.set('Cache-Control', 'no-store')");
  });
});
