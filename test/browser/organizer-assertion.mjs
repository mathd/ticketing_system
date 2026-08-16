// Browser-submit verification — the organizer assertion (TKT-245, ADR-058).
//
// AGENTS.md: "A web-UI ticket isn't verified until a browser has *submitted* its
// forms." This ticket changed what travels on every back-office catalog write —
// a field left the request body and a signed header replaced it — so the whole
// SSR path is what has to be exercised: sign-in mints the assertion, the session
// holds it server-side, and the write forwards it. `make check` renders these
// pages and never submits one, so a write that 401s because the assertion never
// reached catalog looks identical to a working one there.
//
// What can ONLY be checked from a browser, and why each is here:
//
//   * A WRITE STILL SUCCEEDS END TO END. The assertion is minted by catalog,
//     parked in an in-process session, and forwarded by a different request than
//     the one that obtained it. Every unit test on either side mocks the other;
//     this is the only place the two halves meet.
//   * THE ASSERTION IS NEVER IN THE PAGE. It is a bearer credential. A unit test
//     asserting "the client does not render it" tests the client; only a real
//     page can show what actually reached the browser, including via a hidden
//     input, a data- attribute, or an inlined island prop.
//   * THE TENANT IS BOUND TO THE SESSION, NOT THE FORM. A submit carrying an
//     extra organizer_id must not move the write. This is the "unsubmittable,
//     not validated" claim, executed rather than argued (AGENTS.md: a security
//     claim is a hypothesis until it is run).
//   * SIGNING OUT ENDS THE ABILITY TO WRITE. The assertion lives in the session;
//     destroying the session must take the write capability with it.
//
// The assertions read the DATABASE for what the write left behind, not the
// rendered page alone — and read it exactly. A browser spec cannot see the
// request the SSR handler makes to catalog (it happens server-side), so counting
// it from Playwright would observe zero.
//
// Run via `make browser` (or ./scripts/browser.sh), which owns the stack and
// sets BASE, POSTGRES_CONTAINER and CATALOG_CONTAINER.

import { execFileSync } from 'node:child_process';
import { chromium } from 'playwright-core';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const CATALOG = process.env.CATALOG_CONTAINER;
const POSTGRES = process.env.POSTGRES_CONTAINER;
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset — run through ./scripts/browser.sh');
if (!POSTGRES) throw new Error('POSTGRES_CONTAINER is unset — run through ./scripts/browser.sh');

const SEEDED_ORGANIZER = '00000000-0000-0000-0000-000000000001';

const results = [];
let failed = false;

function check(name, ok, detail = '') {
  results.push(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failed = true;
}

function provisionAdmin(identifier, password) {
  execFileSync(
    'docker',
    [
      'exec', '-i', CATALOG, '/app', 'provision-staff',
      '--organizer-id', SEEDED_ORGANIZER,
      '--identifier', identifier,
      '--role', 'admin',
    ],
    { input: password, encoding: 'utf8' },
  );
}

// Read the row the write left behind. The rendered page is the client's account
// of what happened; this is the database's.
function catalogQuery(sql) {
  return execFileSync(
    'docker',
    ['exec', '-i', POSTGRES, 'psql', '-U', 'postgres', '-d', 'catalog', '-tAqc', sql],
    { encoding: 'utf8' },
  ).trim();
}

const stamp = Date.now();
const identifier = `assertion-${stamp}@example.test`;
const password = 'correct horse battery staple';
const code = `assert-${stamp}`;

provisionAdmin(identifier, password);

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();

  // --- 1. Sign in. This is the request that MINTS the assertion; the session
  // cookie is set by the server, and the assertion is parked behind it.
  await page.goto('/admin/login', { waitUntil: 'domcontentloaded' });
  await page.fill('#identifier', identifier);
  await page.fill('#password', password);
  await Promise.all([page.waitForURL('**/admin**'), page.click('button[type=submit]')]);
  check('sign-in succeeds and mints a session', page.url().includes('/admin'));

  // --- 2. The credential must not have come with it. The assertion is a bearer
  // token: anything that reaches the browser can be read by a script, an
  // extension, or anyone looking over a shoulder at a screenshot.
  await page.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
  const html = await page.content();
  check(
    'the assertion never reaches the browser',
    !/\bv1\.[0-9a-f-]{36}\.[0-9a-f-]{36}\.\d+\./.test(html),
    'a bearer credential in the DOM is readable by any script on the page',
  );
  check(
    'no element carries an organizer id either',
    !html.includes(SEEDED_ORGANIZER),
    'the tenant comes from the session; a page that names it invites a forged submit',
  );

  // --- 3. A WRITE SUCCEEDS END TO END. Sign-in minted the assertion on one
  // request; this submit forwards it on another. If the session did not carry it,
  // or the client did not send it, catalog answers 401 and no row appears.
  await page.fill('#code', code);
  await page.fill('#display_name', 'Assertion box office');
  await page.selectOption('#kind', 'pos');
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.click('form:has(input[value="create"]) button[type=submit]'),
  ]);
  const row = page.locator(`tr[data-channel-code="${code}"]`);
  check('a submitted create survives the whole SSR path', (await row.count()) === 1);

  // Read it back EXACTLY: the row exists AND belongs to the signed-in staff
  // member's organizer. "A row appeared" would also be true if the write had
  // landed in another tenant.
  const owner = catalogQuery(
    `SELECT organizer_id FROM channels WHERE code = '${code}'`,
  );
  check(
    'the row belongs to the signed-in staff member\'s organizer',
    owner === SEEDED_ORGANIZER,
    `stored organizer_id = ${owner || '(no row)'}`,
  );

  // --- 4. THE CLAIM, EXECUTED. A submit that carries an extra organizer_id must
  // not move the write. The field is gone from the schema, so catalog refuses the
  // body outright rather than ignoring the extra key — but what matters here is
  // the OUTCOME: no row is created for the organizer the form named.
  const victim = '00000000-0000-0000-0000-0000000000ff';
  const forgedCode = `forged-${stamp}`;
  await page.evaluate(
    ({ forgedCode, victim }) => {
      const form = document.querySelector('form:has(input[value="create"])');
      form.querySelector('#code').value = forgedCode;
      form.querySelector('#display_name').value = 'Forged tenant';
      const injected = document.createElement('input');
      injected.type = 'hidden';
      injected.name = 'organizer_id';
      injected.value = victim;
      form.appendChild(injected);
    },
    { forgedCode, victim },
  );
  await page.click('form:has(input[value="create"]) button[type=submit]');
  await page.waitForLoadState('domcontentloaded');

  const forgedOwner = catalogQuery(
    `SELECT coalesce((SELECT organizer_id::text FROM channels WHERE code = '${forgedCode}'), 'none')`,
  );
  check(
    'a form-supplied organizer_id cannot place a row in another tenant',
    forgedOwner === 'none' || forgedOwner === SEEDED_ORGANIZER,
    `a channel named "${forgedCode}" landed under ${forgedOwner}`,
  );
  const victimRows = catalogQuery(
    `SELECT count(*) FROM channels WHERE organizer_id = '${victim}'`,
  );
  check(
    'the named victim organizer gained nothing',
    victimRows === '0',
    `${victimRows} row(s) exist for the organizer the form named`,
  );

  // --- 5. SIGNING OUT takes the write capability with it. The assertion lives in
  // the session; a destroyed session must not leave a usable credential behind.
  await page.goto('/admin/', { waitUntil: 'domcontentloaded' });
  const signOut = page.locator('form[action*="logout"] button[type=submit], button[data-action="sign-out"]');
  if ((await signOut.count()) > 0) {
    await Promise.all([page.waitForLoadState('domcontentloaded'), signOut.first().click()]);
    await page.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
    check(
      'after signing out the channels page is no longer reachable',
      !page.url().endsWith('/admin/channels'),
      `landed on ${page.url()}`,
    );
  } else {
    // Not a silent skip: an absent control is reported, so this does not read as
    // a passing assertion.
    check('sign-out control found on the back office', false, 'no sign-out form or button located');
  }
} finally {
  await browser.close();
}

console.log(results.join('\n'));
if (failed) {
  console.error('\norganizer-assertion browser spec FAILED');
  process.exit(1);
}
console.log('\norganizer-assertion browser spec passed');
