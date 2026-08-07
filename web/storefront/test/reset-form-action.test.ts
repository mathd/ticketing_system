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
// This cannot prove what the browser actually posts. That needs a real submission, and it
// is on the manual browser checklist. What it CAN do is fail the moment someone
// "simplifies" this form back to the pattern the sibling pages use.
const source = readFileSync(
  fileURLToPath(new URL('../src/pages/[locale]/account/reset-password.astro', import.meta.url)),
  'utf8',
);

describe('the reset form does not post to a URL carrying the token', () => {
  it('has exactly one form, and its action is not empty', () => {
    const forms = source.match(/<form\b[^>]*>/g) ?? [];
    expect(forms).toHaveLength(1);

    // `action=""` and a missing action both resolve to the document URL.
    expect(forms[0]).not.toMatch(/action=""/);
    expect(forms[0]).toMatch(/action=\{/);
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
