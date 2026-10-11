// Browser-submit verification — the channel optimistic revision (TKT-325, ADR-080).
//
// THE DEFECT. Both row forms on /admin/channels send a full replacement built
// from hidden inputs taken from the rendered row. Before TKT-325 there was no
// token: operator A opened the page, operator B disabled the channel, A renamed
// it, and A's stale hidden `enabled=true` re-enabled the channel with a 303
// reporting success. The toggle form had the mirror image: a toggle reverted a
// concurrent rename (D1).
//
// WHY TWO ACTORS. test/browser/channels.mjs is single-actor, so it structurally
// cannot see a lost update: the page it submits from is always the page it last
// loaded. Here A and B are two admins in two browser contexts (two sessions),
// and A submits from a page loaded BEFORE B's write.
//
// WHAT EACH ASSERTION OBSERVES. The browser cannot see the PUT the SSR handler
// makes to catalog, so nothing here counts PUTs (AGENTS.md, TKT-244). It asserts
// what the browser can see — the form POST it sent (with the token in its body)
// and the page that came back — and what the write left behind, by re-reading
// the row EXACTLY through psql: enabled, display_name, revision and updated_at.
//
// EVERY WRITE GOES THROUGH A FORM, B's included. A direct UPDATE in setup would
// not move the revision, so A's "stale" token would still match and the spec
// would prove nothing (AGENTS.md, TKT-250). psql only reads.
//
// Run via `make browser` (or ./scripts/browser.sh), which owns the stack and
// sets BASE, POSTGRES_CONTAINER and CATALOG_CONTAINER.

import { chromium } from 'playwright-core';
import { provisionAdmin, resultRecorder, signIn, sql } from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset — run through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset — run through ./scripts/browser.sh');

const STALE = 'This channel changed since this page was loaded, so nothing was saved.';

const recorder = resultRecorder('channel-revision');
const { check } = recorder;

const stamp = Date.now();
const password = 'correct horse battery staple';
const adminA = `revision-a-${stamp}@example.test`;
const adminB = `revision-b-${stamp}@example.test`;
// Unique per run: the registry has no delete. One channel per direction, so the
// two scenarios cannot affect each other.
const codeA = `rev-toggle-${stamp}`; // direction (a): B toggles, A renames
const codeB = `rev-rename-${stamp}`; // direction (b): B renames, A toggles

provisionAdmin(CATALOG, adminA, password);
provisionAdmin(CATALOG, adminB, password);

/** The stored row, read exactly. Codes are generated above, so quoting is safe. */
function stored(code) {
  const out = sql(
    PG,
    'catalog',
    `SELECT enabled, display_name, revision, updated_at::text FROM channels WHERE code = '${code}'`,
  );
  const [enabled, displayName, revision, updatedAt] = out.split('|');
  return { enabled: enabled === 't', displayName, revision: Number(revision), updatedAt };
}

function same(a, b) {
  return a.enabled === b.enabled && a.displayName === b.displayName &&
    a.revision === b.revision && a.updatedAt === b.updatedAt;
}

function show(row) {
  return JSON.stringify(row);
}

const rowOf = (page, code) => page.locator(`tr[data-channel-code="${code}"]`);
const tokens = async (page, code) =>
  rowOf(page, code).locator('input[name="expected_revision"]').evaluateAll((inputs) => inputs.map((i) => i.value));
const renameButton = (page, code) => rowOf(page, code).locator('form[data-form="rename"] button[type=submit]');
const toggleButton = (page, code) => rowOf(page, code).locator('form[data-form="toggle"] button[type=submit]');
const rowError = async (page, code) => {
  const el = page.locator(`[data-row-error="${code}"]`);
  return (await el.count()) === 1 ? (await el.innerText()).trim() : '';
};
const postedToken = (request) => new URLSearchParams(request.postData() ?? '').get('expected_revision');

/** Create a channel through the create form, as the operator does. */
async function createThroughForm(page, code, name) {
  await page.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
  await page.fill('#code', code);
  await page.fill('#display_name', name);
  await page.selectOption('#kind', 'web');
  const { status } = await submit(page, page.locator('form:has(input[value="create"]) button[type=submit]'));
  check(`creating ${code} through the form is saved (303)`, status === 303, `status ${status}`);
}

/**
 * Click a form's submit button and wait for the NEW document.
 *
 * `domcontentloaded` is awaited as an EVENT, which fires only for the document
 * that replaces this one — the page is already loaded, so `waitForLoadState`
 * could resolve against the old document and the assertions would read the
 * page as it was before the submit. Returns the browser's POST and its status:
 * 303 is a saved change (post/redirect/get), 200 a re-rendered refusal.
 */
async function submit(page, button) {
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && r.url().endsWith('/admin/channels')),
    page.waitForEvent('domcontentloaded'),
    button.click(),
  ]);
  return { request: response.request(), status: response.status() };
}

/** Submit a row form that must be saved: a 303, then the list again. */
async function submitAndRedirect(page, button, label) {
  const { status } = await submit(page, button);
  check(`${label} is saved (303)`, status === 303, `status ${status}`);
}

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const contextA = await browser.newContext({ baseURL: BASE });
  const contextB = await browser.newContext({ baseURL: BASE });
  const pageA = await contextA.newPage();
  const pageB = await contextB.newPage();
  await signIn(pageA, adminA, password);
  await signIn(pageB, adminB, password);

  await createThroughForm(pageB, codeA, 'Website');
  await createThroughForm(pageB, codeB, 'Box office');
  check('both channels start at revision 0', stored(codeA).revision === 0 && stored(codeB).revision === 0);

  // ---------------------------------------------------------------------------
  // Direction (a): A opens; B DISABLES through its form; A RENAMES from the
  // still-open page. The reported defect.
  // ---------------------------------------------------------------------------
  {
    const code = codeA;
    const attempted = `Renamed by A ${stamp}`;
    await pageA.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
    check('(a) A\'s page renders revision 0 on both of the row\'s forms',
      JSON.stringify(await tokens(pageA, code)) === '["0","0"]');

    await pageB.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
    await submitAndRedirect(pageB, rowOf(pageB, code).locator('button[data-action="disable"]'), '(a) B\'s disable');
    const afterB = stored(code);
    check('(a) B\'s disable applied through the form and moved the revision',
      afterB.enabled === false && afterB.displayName === 'Website' && afterB.revision === 1, show(afterB));

    // A renames from the page loaded before B's write.
    await pageA.fill(`input[data-rename-for="${code}"]`, attempted);
    const first = await submit(pageA, renameButton(pageA, code));
    check('(a) A\'s browser submitted the token it rendered (0)', postedToken(first.request) === '0');
    check('(a) A\'s stale rename re-renders (200), it does not redirect', first.status === 200, `status ${first.status}`);
    check('(a) A\'s stale rename is refused with the neutral message', (await rowError(pageA, code)) === STALE);
    const afterRefusal = stored(code);
    check('(a) B\'s disable SURVIVES: the row is exactly as B left it',
      same(afterRefusal, afterB), `${show(afterRefusal)} vs ${show(afterB)}`);
    check('(a) the refusal keeps the SUBMITTED token on both forms',
      JSON.stringify(await tokens(pageA, code)) === '["0","0"]');
    check('(a) the refusal keeps the attempted name in the rename input',
      (await pageA.locator(`input[data-rename-for="${code}"]`).inputValue()) === attempted);
    check('(a) the refused row shows the current state (disabled)',
      (await rowOf(pageA, code).getAttribute('data-enabled')) === 'false');

    // The habitual second click: refused again, still nothing written.
    const second = await submit(pageA, renameButton(pageA, code));
    check('(a) the second click sends the same stale token', postedToken(second.request) === '0' && second.status === 200);
    check('(a) the second click is refused again', (await rowError(pageA, code)) === STALE);
    const afterSecond = stored(code);
    check('(a) the second refusal leaves revision and updated_at unchanged',
      same(afterSecond, afterB), `${show(afterSecond)} vs ${show(afterB)}`);

    // Review: a read. No write, fresh token, B's values on screen.
    const review = await submit(pageA, pageA.locator(`[data-review-for="${code}"]`));
    check('(a) Review is a POST that sends no token', review.request.postData()?.includes('_action=review') === true && postedToken(review.request) === null);
    const afterReview = stored(code);
    check('(a) Review writes nothing: revision and updated_at unchanged',
      same(afterReview, afterB), `${show(afterReview)} vs ${show(afterB)}`);
    check('(a) Review renders the fresh token (1) on both forms',
      JSON.stringify(await tokens(pageA, code)) === '["1","1"]');
    check('(a) Review shows B\'s values: the row is disabled and offers Enable',
      (await rowOf(pageA, code).getAttribute('data-enabled')) === 'false' &&
        (await rowOf(pageA, code).locator('button[data-action="enable"]').count()) === 1);
    check('(a) Review keeps the attempted name for a deliberate resubmit',
      (await pageA.locator(`input[data-rename-for="${code}"]`).inputValue()) === attempted);

    // The deliberate save: applies, and B's disable survives it.
    await submitAndRedirect(pageA, renameButton(pageA, code), '(a) A\'s deliberate rename');
    const afterSave = stored(code);
    check('(a) the deliberate save applies A\'s rename and keeps B\'s disable',
      afterSave.displayName === attempted && afterSave.enabled === false && afterSave.revision === 2, show(afterSave));
  }

  // ---------------------------------------------------------------------------
  // Direction (b): A opens; B RENAMES through its form; A TOGGLES from the
  // still-open page. The mirror image (D1).
  // ---------------------------------------------------------------------------
  {
    const code = codeB;
    const renamedByB = `Renamed by B ${stamp}`;
    await pageA.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
    check('(b) A\'s page renders revision 0 on both of the row\'s forms',
      JSON.stringify(await tokens(pageA, code)) === '["0","0"]');

    await pageB.goto('/admin/channels', { waitUntil: 'domcontentloaded' });
    await pageB.fill(`input[data-rename-for="${code}"]`, renamedByB);
    await submitAndRedirect(pageB, renameButton(pageB, code), '(b) B\'s rename');
    const afterB = stored(code);
    check('(b) B\'s rename applied through the form and moved the revision',
      afterB.displayName === renamedByB && afterB.enabled === true && afterB.revision === 1, show(afterB));

    // A disables from the page loaded before B's write; its hidden name is stale.
    const first = await submit(pageA, toggleButton(pageA, code));
    check('(b) A\'s browser submitted the token it rendered (0)', postedToken(first.request) === '0');
    check('(b) A\'s stale toggle re-renders (200), it does not redirect', first.status === 200, `status ${first.status}`);
    check('(b) A\'s stale toggle is refused with the neutral message', (await rowError(pageA, code)) === STALE);
    const afterRefusal = stored(code);
    check('(b) B\'s rename SURVIVES: the row is exactly as B left it',
      same(afterRefusal, afterB), `${show(afterRefusal)} vs ${show(afterB)}`);
    check('(b) the refusal keeps the SUBMITTED token on both forms',
      JSON.stringify(await tokens(pageA, code)) === '["0","0"]');
    check('(b) the rename input shows B\'s name, not A\'s stale one',
      (await pageA.locator(`input[data-rename-for="${code}"]`).inputValue()) === renamedByB);

    const second = await submit(pageA, toggleButton(pageA, code));
    check('(b) the second click sends the same stale token', postedToken(second.request) === '0' && second.status === 200);
    check('(b) the second click is refused again', (await rowError(pageA, code)) === STALE);
    const afterSecond = stored(code);
    check('(b) the second refusal leaves revision and updated_at unchanged',
      same(afterSecond, afterB), `${show(afterSecond)} vs ${show(afterB)}`);

    const review = await submit(pageA, pageA.locator(`[data-review-for="${code}"]`));
    check('(b) Review is a POST that sends no token', review.request.postData()?.includes('_action=review') === true && postedToken(review.request) === null);
    const afterReview = stored(code);
    check('(b) Review writes nothing: revision and updated_at unchanged',
      same(afterReview, afterB), `${show(afterReview)} vs ${show(afterB)}`);
    check('(b) Review renders the fresh token (1) on both forms',
      JSON.stringify(await tokens(pageA, code)) === '["1","1"]');
    check('(b) Review shows B\'s name and the channel still enabled',
      (await pageA.locator(`input[data-rename-for="${code}"]`).inputValue()) === renamedByB &&
        (await rowOf(pageA, code).getAttribute('data-enabled')) === 'true');

    await submitAndRedirect(pageA, rowOf(pageA, code).locator('button[data-action="disable"]'), '(b) A\'s deliberate disable');
    const afterSave = stored(code);
    check('(b) the deliberate save disables the channel and keeps B\'s rename',
      afterSave.enabled === false && afterSave.displayName === renamedByB && afterSave.revision === 2, show(afterSave));
  }
} finally {
  await browser.close();
  if (!recorder.finish()) {
    process.exitCode = 1;
  }
}
