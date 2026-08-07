// Browser-submit verification — the public customer surface is rate limited
// (TKT-224, ADR-051).
//
// Two things here can only be checked by a real browser against the real stack:
//
//   * A 429 must reach the buyer as a WAIT, not as a credential verdict and not
//     as an outage. That mapping crosses commerce's handler, the gateway, the
//     storefront's SSR fetch and the page's error branch — four layers, none of
//     which the unit tests see together.
//   * The refusal must be the SAME for an address that exists and one that does
//     not. The Go test proves that at the handler; this proves the rendered page
//     does not put the difference back.
//
// Run via `make browser`, which sets BASE and POSTGRES_CONTAINER.

import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const REPO = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset — run through ./scripts/browser.sh');

// Must match customerSubjectBurst in services/commerce/internal/api/ratelimit.go.
// Read from the source rather than copied, so a threshold change cannot leave this
// spec silently passing against the wrong number.
// Resolved from this file's own location, not the working directory: the spec must
// read the same number whoever runs it and from wherever.
const BURST = Number(
  /customerSubjectBurst\s*=\s*(\d+)/.exec(
    readFileSync(join(REPO, 'services/commerce/internal/api/ratelimit.go'), 'utf8'),
  )?.[1],
);
if (!Number.isInteger(BURST) || BURST < 1) {
  throw new Error('could not read customerSubjectBurst from the Go source');
}

const results = [];
let failed = false;
function check(name, ok, detail = '') {
  results.push(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failed = true;
}

const registered = `limited-${Date.now()}@example.test`;
const stranger = `nobody-${Date.now()}@example.test`;
const password = 'correct horse battery';

const browser = await chromium.launch({ channel: 'chrome' });

// Submit the sign-in form once and return the page's visible text.
async function signInOnce(context, email, pw) {
  const page = await context.newPage();
  await page.goto('/en/account/sign-in', { waitUntil: 'domcontentloaded' });
  await page.fill('#email', email);
  await page.fill('#password', pw);
  await page.click('button[type=submit]');
  await page.waitForLoadState('domcontentloaded');
  const text = (await page.locator('body').innerText()).trim();
  const url = page.url();
  await page.close();
  return { text, url };
}

try {
  // --- 1. An account to be refused against.
  const owner = await browser.newContext({ baseURL: BASE });
  const reg = await owner.newPage();
  await reg.goto('/en/account/register', { waitUntil: 'domcontentloaded' });
  await reg.fill('#email', registered);
  await reg.fill('#password', password);
  await Promise.all([reg.waitForURL('**/en/account**'), reg.click('button[type=submit]')]);
  check('registration submits and lands on the account page', reg.url().includes('/en/account'));

  // --- 2. Spend the budget with WRONG passwords. Registration already spent one
  // token for this address, so the budget is nearly gone; go well past it.
  const anon = await browser.newContext({ baseURL: BASE });
  let limitedText = null;
  for (let i = 0; i < BURST + 2 && !limitedText; i++) {
    const { text } = await signInOnce(anon, registered, 'wrong password');
    if (/too many/i.test(text)) limitedText = text;
  }
  check(
    'the sign-in form is eventually rate limited',
    Boolean(limitedText),
    limitedText ? '' : `never refused within ${BURST + 2} submissions`,
  );

  // --- 3. THE POINT. The refusal must not be a credential verdict, and must not
  // be an outage — those are the two wrong renderings, in both directions.
  if (limitedText) {
    check(
      'the refusal is not rendered as a credential verdict',
      !/not valid|invalid/i.test(limitedText),
      limitedText.slice(0, 160),
    );
    check(
      'the refusal is not rendered as an outage',
      !/unavailable/i.test(limitedText),
      limitedText.slice(0, 160),
    );
  }

  // --- 4. The rendered refusal must be identical for an address with no account.
  // The Go suite proves the handler answers identically; this proves the page
  // does not reintroduce the difference on the way to the buyer.
  const other = await browser.newContext({ baseURL: BASE });
  let strangerText = null;
  for (let i = 0; i < BURST + 2 && !strangerText; i++) {
    const { text } = await signInOnce(other, stranger, 'wrong password');
    if (/too many/i.test(text)) strangerText = text;
  }
  check('an unknown address is limited too', Boolean(strangerText));
  if (limitedText && strangerText) {
    check(
      'the rendered refusal is identical for a known and an unknown address',
      limitedText === strangerText,
      limitedText === strangerText ? '' : 'the pages differ — an account-existence oracle',
    );
  }

  // --- 5. A throttled reset request must NOT claim a link is on its way. That
  // acknowledgement is the one lie this page cannot afford: nothing was enqueued.
  const resetCtx = await browser.newContext({ baseURL: BASE });
  let resetText = null;
  for (let i = 0; i < BURST + 2 && !resetText; i++) {
    const page = await resetCtx.newPage();
    await page.goto('/en/account/forgot-password', { waitUntil: 'domcontentloaded' });
    await page.fill('#email', registered);
    await page.click('button[type=submit]');
    await page.waitForLoadState('domcontentloaded');
    const text = (await page.locator('body').innerText()).trim();
    await page.close();
    if (/too many/i.test(text)) resetText = text;
  }
  check('the reset request form is rate limited', Boolean(resetText));
  if (resetText) {
    check(
      'a throttled reset request does not claim a link was sent',
      !/on its way/i.test(resetText),
      resetText.slice(0, 160),
    );
  }

  // --- 6. The recovery budget is SEPARATE from the credential one, which is what
  // stops the limiter becoming a lockout: step 2 exhausted this address's sign-in
  // budget, so if the two shared a bucket nothing here would ever have been
  // enqueued. That is exactly what this spec caught on its first run.
  const enqueued = Number(
    execFileSync(
      'docker',
      [
        'exec', '-i', PG, 'psql', '-U', 'postgres', '-d', 'commerce', '-tAqc',
        `SELECT count(*) FROM mail_outbox WHERE recipient='${registered}'`,
      ],
      { encoding: 'utf8' },
    ).trim(),
  );
  check(
    'a locked-out buyer could still reach recovery, and it was itself bounded',
    enqueued > 0 && enqueued <= BURST,
    `${enqueued} message(s) enqueued for ${BURST + 2} reset submissions ` +
      `(want 1..${BURST}: some got through despite the spent sign-in budget, and the rest were throttled)`,
  );
} finally {
  await browser.close();
  console.log(results.join('\n'));
  console.log(failed ? '\nRESULT: FAIL' : '\nRESULT: PASS');
  process.exit(failed ? 1 : 0);
}
