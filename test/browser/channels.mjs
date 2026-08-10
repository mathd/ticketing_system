// Browser-submit verification — the back-office channel registry (TKT-236).
//
// AGENTS.md: "A web-UI ticket isn't verified until a browser has *submitted* its
// forms." `make check` renders back-office pages and never submits one, so the
// whole class of "the SSR layer rejects or mangles the write before the handler
// runs" — the proxy-aware origin check with `checkOrigin: false`, the `/admin`
// base path, relative form actions, redirects, cookie paths — is invisible to it.
// This drives real Chrome against the real stack through the real gateway.
//
// Three things here can ONLY be checked by a browser, and each is a COS:
//
//   * A DISABLED CHANNEL IS STILL LISTED. This is the assertion the ticket turns
//     on. A page that filtered disabled rows out would look identical in a
//     screenshot to one that disabled correctly — the row simply vanishes either
//     way — and an operator could never re-enable it. Only reloading after a real
//     submit distinguishes "disabled" from "gone".
//   * A REFUSAL LANDS BESIDE THE FIELD, with the typed value preserved. A generic
//     banner satisfies any assertion that only greps the page for the message.
//   * The round trip DISABLE -> ENABLE returns the row to enabled. An unchecked
//     HTML checkbox submits nothing, so a save that read `enabled` from a
//     checkbox would silently re-enable what was just disabled.
//
// Run via `make browser` (or ./scripts/browser.sh), which owns the stack and
// sets BASE, POSTGRES_CONTAINER and CATALOG_CONTAINER.

import { execFileSync } from 'node:child_process';
import { chromium } from 'playwright-core';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const CATALOG = process.env.CATALOG_CONTAINER;
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset — run through ./scripts/browser.sh');

const results = [];
let failed = false;

function check(name, ok, detail = '') {
  results.push(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failed = true;
}

// browser.sh provisions no staff account (smoke.sh does, for its own suite), so
// this spec makes its own. The password goes in on STDIN and never onto a
// command line, matching smoke.sh's provision_staff.
function provisionAdmin(identifier, password) {
  execFileSync(
    'docker',
    [
      'exec', '-i', CATALOG, '/app', 'provision-staff',
      '--organizer-id', '00000000-0000-0000-0000-000000000001',
      '--identifier', identifier,
      '--role', 'admin',
    ],
    { input: password, encoding: 'utf8' },
  );
}

const stamp = Date.now();
const identifier = `channels-${stamp}@example.test`;
const password = 'correct horse battery staple';
// Unique per run: the registry has no delete, so a fixed code would collide with
// the previous run's row on the second `make browser` — and a test that only
// passes on a clean database is a test that will be quietly disabled.
const code = `pos-${stamp}`;

provisionAdmin(identifier, password);

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();

  // --- 1. Sign in. A real submit, so the session cookie is set by the server on
  // the /admin path rather than fabricated here.
  await page.goto('/admin/login', { waitUntil: 'domcontentloaded' });
  await page.fill('#identifier', identifier);
  await page.fill('#password', password);
  await Promise.all([page.waitForURL('**/admin**'), page.click('button[type=submit]')]);
  check('admin signs in and lands in the back office', page.url().includes('/admin'));

  // --- 2. The page is reachable and linked. The link is a courtesy; the route
  // gate is the control — but a page nobody can find is not shipped.
  await page.goto('/admin/', { waitUntil: 'domcontentloaded' });
  const linked = await page.locator('a[href="/admin/channels"]').count();
  check('the home page links to the channels admin for an admin', linked === 1);

  await page.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
  check('the channels page renders for an admin', page.url().endsWith('/admin/channels'));

  // --- 3. CREATE. The submit that proves the origin check, the relative action
  // and the base path all agree — the failure class make check cannot see.
  await page.fill('#code', code);
  await page.fill('#display_name', 'Box office');
  await page.selectOption('#kind', 'pos');
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.click('form:has(input[value="create"]) button[type=submit]'),
  ]);
  const row = page.locator(`tr[data-channel-code="${code}"]`);
  check('a submitted create appears in the list', (await row.count()) === 1);
  check('the new channel is enabled', (await row.getAttribute('data-enabled')) === 'true');

  // --- 4. A REFUSAL lands beside the field, with the typed value kept.
  // The same code again: catalog answers 409, and this must not become a banner.
  await page.fill('#code', code);
  await page.fill('#display_name', 'Duplicate');
  await page.selectOption('#kind', 'web');
  await page.click('form:has(input[value="create"]) button[type=submit]');
  await page.waitForLoadState('domcontentloaded');
  const codeInvalid = await page.locator('#code[aria-invalid="true"]').count();
  const codeError = await page.locator('#code-error').count();
  check('a duplicate code is refused beside the code field', codeInvalid === 1 && codeError === 1);
  check(
    'the refused submit keeps what the operator typed',
    (await page.locator('#code').inputValue()) === code &&
      (await page.locator('#display_name').inputValue()) === 'Duplicate',
  );

  // --- 5. DISABLE, then reload. THE assertion: the row is still there.
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.click(`tr[data-channel-code="${code}"] button[data-action="disable"]`),
  ]);
  const afterDisable = page.locator(`tr[data-channel-code="${code}"]`);
  check(
    'a disabled channel is STILL LISTED — not filtered out',
    (await afterDisable.count()) === 1,
    'a vanished row is indistinguishable from a correct disable in a screenshot',
  );
  check('the row reports itself disabled', (await afterDisable.getAttribute('data-enabled')) === 'false');
  check(
    'the disabled row is visibly distinct',
    (await afterDisable.locator('[data-status="disabled"]').count()) === 1,
  );

  // --- 6. RE-ENABLE from the same screen. Disabling must not be a one-way door;
  // this is also what catches an `enabled` flag read from an unchecked checkbox,
  // which submits nothing and would round-trip as false.
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.click(`tr[data-channel-code="${code}"] button[data-action="enable"]`),
  ]);
  const afterEnable = page.locator(`tr[data-channel-code="${code}"]`);
  check('a disabled channel can be re-enabled from the list', (await afterEnable.getAttribute('data-enabled')) === 'true');

  // --- 6b. RENAME WHILE DISABLED. The case ai-review found, and the reason the
  // rename form's `enabled` is an explicit boolean string rather than a
  // checkbox value: a hidden input ALWAYS submits, so `value=""` is
  // present-and-empty, which the checkbox convention reads as true — and
  // renaming a disabled channel silently re-enabled it.
  //
  // The original spec could not catch this: it re-enabled the row before
  // testing rename, so the rename never ran against a disabled channel. An
  // ordering that made the defect unreachable, in a spec that looked complete.
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.click(`tr[data-channel-code="${code}"] button[data-action="disable"]`),
  ]);
  await page.fill(`input[data-rename-for="${code}"]`, `Renamed while off ${stamp}`);
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.locator(`tr[data-channel-code="${code}"] form:has(input[value="update"]) button[type=submit]`).click(),
  ]);
  const afterOffRename = page.locator(`tr[data-channel-code="${code}"]`);
  check(
    'renaming a DISABLED channel does not re-enable it',
    (await afterOffRename.getAttribute('data-enabled')) === 'false',
    'a hidden enabled="" reads as true under the checkbox convention',
  );
  check(
    'the rename applied while disabled',
    (await afterOffRename.innerText()).includes(`Renamed while off ${stamp}`),
  );

  // Back to enabled for the remaining assertions.
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.click(`tr[data-channel-code="${code}"] button[data-action="enable"]`),
  ]);

  // --- 7. RENAME the display name, and confirm the code did not move with it.
  const renamed = `Counter ${stamp}`;
  await page.fill(`input[data-rename-for="${code}"]`, renamed);
  await Promise.all([
    page.waitForURL('**/admin/channels'),
    page.locator(`tr[data-channel-code="${code}"] form:has(input[value="update"]) button[type=submit]`).click(),
  ]);
  const afterRename = page.locator(`tr[data-channel-code="${code}"]`);
  check('the display name is renamed', (await afterRename.innerText()).includes(renamed));
  check(
    'the immutable code survives a rename',
    (await afterRename.count()) === 1,
    'the row is still found by its original code',
  );
  check(
    'a rename does not disable the channel',
    (await afterRename.getAttribute('data-enabled')) === 'true',
    'the full PUT must carry enabled, or a rename silently disables',
  );
} finally {
  await browser.close();
  console.log(results.join('\n'));
}

if (failed) {
  process.exitCode = 1;
}
