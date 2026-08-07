// Browser-submit verification — password recovery, end to end (TKT-226).
//
// AGENTS.md: "A web-UI ticket isn't verified until a browser has *submitted* its forms."
// `make check` renders storefront pages and never submits one, so the whole class of "the
// SSR layer rejects or mangles the write before the handler runs" — the proxy-aware origin
// check, relative form actions, redirects, cookie paths, cache headers — is invisible to
// it. This drives real Chrome against the real stack through the real gateway.
//
// Two things here can ONLY be checked by a browser, and both are review findings:
//
//   * The POST's URL must not carry the token. `action=""` resolves to the DOCUMENT's URL
//     including its query, and the source-scan guard in web/storefront/test cannot see
//     what a browser actually posts to. This intercepts the request and reads its URL.
//   * A reset must evict a live session. That needs two browser contexts: one signed in
//     and left alone, one performing the reset.
//
// Run via `make browser` (or ./scripts/browser.sh), which owns the stack and sets BASE
// and POSTGRES_CONTAINER. When a second spec lands here, lift check()/psql() into a
// shared module — one copy is cheaper than the abstraction, two is not.

import { execFileSync } from 'node:child_process';
import { chromium } from 'playwright-core';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset — run through ./scripts/browser.sh');

const results = [];
let failed = false;

function check(name, ok, detail = '') {
  results.push(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failed = true;
}

// The fake sender never delivers, so the mailbox is a table. Same query an operator runs
// (docs/development.md § Password recovery). Through psql in the running container rather
// than a node postgres driver: scalar results only, so an email body's newlines can never
// be parsed as rows.
function psql(sql) {
  return execFileSync(
    'docker',
    ['exec', '-i', PG, 'psql', '-U', 'postgres', '-d', 'commerce', '-tAqc', sql],
    { encoding: 'utf8' },
  ).trim();
}
const literal = (s) => `'${s.replaceAll("'", "''")}'`;
const latestMail = (recipient, expr) =>
  psql(
    `SELECT ${expr} FROM mail_outbox WHERE recipient=${literal(recipient)} ORDER BY created_at DESC LIMIT 1`,
  );

const email = `reset-${Date.now()}@example.test`;
const stranger = `nobody-${Date.now()}@example.test`;
const original = 'correct horse battery';
const chosen = 'the remembered one';

const browser = await chromium.launch({ channel: 'chrome' });

try {
  // --- 1. Register, so there is an account to lock out of.
  const owner = await browser.newContext({ baseURL: BASE });
  const page = await owner.newPage();
  await page.goto('/en/account/register', { waitUntil: 'domcontentloaded' });
  await page.fill('#email', email);
  await page.fill('#password', original);
  await Promise.all([page.waitForURL('**/en/account**'), page.click('button[type=submit]')]);
  check('registration submits and lands on the account page', page.url().includes('/en/account'));

  // --- 2. A SECOND, separate browser session for the same account. This one is the
  // "stolen session" and is never touched again until the end.
  const other = await browser.newContext({ baseURL: BASE });
  const otherPage = await other.newPage();
  await otherPage.goto('/en/account/sign-in', { waitUntil: 'domcontentloaded' });
  await otherPage.fill('#email', email);
  await otherPage.fill('#password', original);
  await Promise.all([
    otherPage.waitForURL('**/en/account**'),
    otherPage.click('button[type=submit]'),
  ]);
  check('a second session signs in', otherPage.url().includes('/en/account'));

  // --- 3. A locked-out buyer must REACH the recovery page while signed out.
  const anon = await browser.newContext({ baseURL: BASE });
  const anonPage = await anon.newPage();
  const forgot = await anonPage.goto('/en/account/forgot-password', {
    waitUntil: 'domcontentloaded',
  });
  check(
    'forgot-password is reachable signed out, no redirect to sign-in',
    forgot.status() === 200 && !anonPage.url().includes('sign-in'),
    anonPage.url(),
  );

  const signInPage = await anon.newPage();
  await signInPage.goto('/en/account/sign-in', { waitUntil: 'domcontentloaded' });
  check(
    'sign-in links to it',
    (await signInPage.locator('a[href$="/account/forgot-password"]').count()) > 0,
  );

  // --- 4. Submit the request form. The answer must be the same for an unknown address.
  await anonPage.fill('#email', email);
  await anonPage.click('button[type=submit]');
  await anonPage.waitForLoadState('domcontentloaded');
  const knownText = (await anonPage.locator('body').innerText()).trim();
  check(
    'the request form submits and is acknowledged',
    knownText.includes('reset link'),
    knownText.slice(0, 120),
  );

  const unknownPage = await anon.newPage();
  await unknownPage.goto('/en/account/forgot-password', { waitUntil: 'domcontentloaded' });
  await unknownPage.fill('#email', stranger);
  await unknownPage.click('button[type=submit]');
  await unknownPage.waitForLoadState('domcontentloaded');
  const unknownText = (await unknownPage.locator('body').innerText()).trim();
  check(
    'an unknown address gets a byte-identical page',
    knownText === unknownText,
    knownText === unknownText ? '' : 'the rendered pages differ — enumeration oracle',
  );

  // --- 5. The message exists and the drainer retired it via the offline fake.
  let drained = '';
  for (let i = 0; i < 40 && drained !== 't'; i++) {
    drained = latestMail(email, 'sent_at IS NOT NULL');
    if (drained !== 't') await new Promise((r) => setTimeout(r, 500));
  }
  const enqueued = latestMail(email, '1') === '1';
  check('a message was enqueued', enqueued, enqueued ? '' : 'nothing in mail_outbox');
  check('the drainer retired it through the offline fake', drained === 't');
  // substring(... from pattern) returns the first capture group, so the token never has
  // to be parsed out of a body that may span lines.
  const token = latestMail(email, "substring(body from 'token=([A-Za-z0-9_-]+)')");
  check('the message carries a token', Boolean(token));

  check(
    'no message is enqueued for an address with no account',
    latestMail(stranger, '1') === '',
  );

  // --- 6. Open the mailed link and SUBMIT the new password. This is the check the
  // source scan cannot make: what URL does the browser actually POST to?
  const resetPage = await anon.newPage();
  let postedUrl = null;
  resetPage.on('request', (req) => {
    if (req.method() === 'POST' && req.url().includes('/account/reset-password')) {
      postedUrl = req.url();
    }
  });
  const opened = await resetPage.goto(`/en/account/reset-password?token=${token}`, {
    waitUntil: 'domcontentloaded',
  });
  check('the mailed link opens the reset form', opened.status() === 200);
  check(
    'the reset page is no-store',
    (opened.headers()['cache-control'] ?? '').includes('no-store'),
    opened.headers()['cache-control'],
  );
  // `origin`, deliberately NOT `no-referrer`: no-referrer makes Chrome send
  // `Origin: null` on the form POST below, which gate.ts refuses with a 403 before the
  // handler runs. `origin` still strips path and query from the Referer, so the token
  // never rides in one.
  check(
    'the reset page strips path and query from the referrer',
    (opened.headers()['referrer-policy'] ?? '') === 'origin',
    opened.headers()['referrer-policy'],
  );

  await resetPage.fill('#password', chosen);
  await resetPage.click('button[type=submit]');
  await resetPage.waitForLoadState('domcontentloaded');

  check('the reset form actually submitted', Boolean(postedUrl), postedUrl ?? 'no POST observed');
  check(
    'the POST url carries NO token',
    Boolean(postedUrl) && !postedUrl.includes('token='),
    postedUrl ?? '',
  );
  const done = (await resetPage.locator('body').innerText()).trim();
  check(
    'the reset is confirmed',
    done.includes('changed') || done.includes('modifié'),
    done.slice(0, 120),
  );

  // --- 7. The credential really changed, through the real sign-in form.
  const after = await browser.newContext({ baseURL: BASE });
  const afterPage = await after.newPage();
  await afterPage.goto('/en/account/sign-in', { waitUntil: 'domcontentloaded' });
  await afterPage.fill('#email', email);
  await afterPage.fill('#password', original);
  await afterPage.click('button[type=submit]');
  await afterPage.waitForLoadState('domcontentloaded');
  check('the old password no longer signs in', afterPage.url() !== `${BASE}/en/account`, afterPage.url());

  // EXACT url, not a glob. `**/en/account**` also matches `/en/account/sign-in`, so a
  // failed sign-in satisfied it and this check passed while the password had not changed.
  const accountUrl = `${BASE}/en/account`;
  await afterPage.goto('/en/account/sign-in', { waitUntil: 'domcontentloaded' });
  await afterPage.fill('#email', email);
  await afterPage.fill('#password', chosen);
  await afterPage.click('button[type=submit]');
  await afterPage.waitForLoadState('domcontentloaded');
  check('the new password signs in', afterPage.url() === accountUrl, afterPage.url());

  // --- 8. THE POINT: the untouched second session is gone.
  const evicted = await otherPage.goto('/en/account', { waitUntil: 'domcontentloaded' });
  check(
    'the other live session was signed out by the reset',
    otherPage.url().includes('sign-in'),
    `landed on ${otherPage.url()} (${evicted.status()})`,
  );

  // --- 9. The link is single-use.
  const replay = await anon.newPage();
  await replay.goto(`/en/account/reset-password?token=${token}`, { waitUntil: 'domcontentloaded' });
  await replay.fill('#password', 'a third password').catch(() => {});
  await replay.click('button[type=submit]').catch(() => {});
  await replay.waitForLoadState('domcontentloaded');
  const replayText = (await replay.locator('body').innerText()).trim();
  check(
    'a redeemed link is refused',
    replayText.includes('invalid') || replayText.includes('expired'),
    replayText.slice(0, 120),
  );
} finally {
  await browser.close();
  console.log(results.join('\n'));
  console.log(failed ? '\nRESULT: FAIL' : '\nRESULT: PASS');
  process.exit(failed ? 1 : 0);
}
