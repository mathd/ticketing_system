// Browser-submit verification — the slot channel-allocation editor (TKT-244).
//
// AGENTS.md: "A web-UI ticket isn't verified until a browser has *submitted* its forms."
// `make check` renders back-office pages and never submits one, so the whole class of
// "the SSR layer rejects or mangles the write before the handler runs" — the proxy-aware
// origin check with `checkOrigin: false`, the `/admin` base path, relative form actions,
// redirects, cookie paths — is invisible to it. This drives real Chrome against the real
// stack.
//
// Four things here can ONLY be checked by a browser, and each is a COS:
//
//   * ONE SAVE IS ONE PUT. The endpoint replaces the slot's COMPLETE allocation set
//     atomically under the pool lock (ADR-024), so a per-row save would round-trip a
//     stale set and delete the allocations the operator was not looking at. Counted from
//     the network, not inferred from the code.
//   * THE UNRENDERED FIELDS SURVIVE. The store DELETEs and re-INSERTs from whatever
//     arrives, so `sold_by`, `requires_code` and the sales window are destroyed by any
//     form that does not carry them. For `sold_by` that is an AUTHORIZATION regression
//     (TKT-246 judges it under the pool row lock), and it is invisible in a screenshot —
//     the row still renders, with the same cap. Only re-reading the database shows it.
//   * A REFUSAL LANDS BESIDE THE RIGHT ROW. A generic banner satisfies any assertion that
//     greps the page for a message; the COS asks for it on the field that must change.
//   * AN UNREGISTERED CHANNEL CODE STILL RENDERS AND STILL SAVES. The registry is a
//     lookup, not a constraint (TKT-235): codes recorded on live claims may have no
//     registry row, and a page that hid them would make them uneditable.
//
// Run via `make browser` (or ./scripts/browser.sh), which owns the stack and sets BASE,
// POSTGRES_CONTAINER and CATALOG_CONTAINER.

import { execFileSync } from 'node:child_process';
import { chromium } from 'playwright-core';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset — run through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset — run through ./scripts/browser.sh');

const ORGANIZER = '00000000-0000-0000-0000-000000000001';
const results = [];
let failed = false;

function check(name, ok, detail = '') {
  results.push(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failed = true;
}

function sql(db, statement) {
  return execFileSync(
    'docker',
    ['exec', '-i', PG, 'psql', '-U', 'postgres', '-d', db, '-tAqc', statement],
    { encoding: 'utf8' },
  ).trim();
}

// browser.sh provisions no staff account (smoke.sh does, for its own suite), so this
// spec makes its own. The password goes in on STDIN and never onto a command line.
function provisionAdmin(identifier, password) {
  execFileSync(
    'docker',
    [
      'exec', '-i', CATALOG, '/app', 'provision-staff',
      '--organizer-id', ORGANIZER,
      '--identifier', identifier,
      '--role', 'admin',
    ],
    { input: password, encoding: 'utf8' },
  );
}

const stamp = Date.now();
const identifier = `slot-allocations-${stamp}@example.test`;
const password = 'correct horse battery staple';
// Unique per run so a second `make browser` does not collide with the first — a test
// that only passes on a clean database is a test that will be quietly disabled.
const boundChannel = `reseller-${stamp}`;
const plainChannel = `pos-${stamp}`;
const reseller = '33333333-3333-3333-3333-333333333333';

provisionAdmin(identifier, password);

// A pool to edit, seeded through the REAL catalog path (venue → event → performance →
// ticket type → publish) rather than by INSERTing into inventory_pools. Inventory
// provisions the pool from the published performance, and its slot_id IS the performance
// id. Going through the API keeps this spec off inventory's private schema, where a
// hand-written INSERT would have to track every column a migration adds.
async function api(path, body) {
  const res = await fetch(`${BASE}/api/catalog${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`catalog ${path}: ${res.status} ${await res.text()}`);
  return res.json();
}

const venue = await api('/venues', { organizer_id: ORGANIZER, name: `Allocations ${stamp}`, ga_capacity: 100 });
const event = await api('/events', {
  organizer_id: ORGANIZER,
  name: { fr: `Allocations ${stamp}`, en: `Allocations ${stamp}` },
});
const performance = await api('/performances', {
  organizer_id: ORGANIZER,
  event_id: event.id,
  venue_id: venue.id,
  starts_at: '2026-11-01T20:00:00Z',
  timezone: 'UTC',
});
await api('/ticket-types', {
  organizer_id: ORGANIZER,
  performance_id: performance.id,
  name: { fr: 'GA', en: 'GA' },
  price: { amount: 2500, currency: 'EUR' },
});
await api(`/performances/${performance.id}/publish`, null);

const slot = performance.id;

// Inventory provisions the pool asynchronously off the published event.
for (let i = 0; ; i++) {
  const res = await fetch(`${BASE}/api/inventory/slots/${slot}/availability?organizer_id=${ORGANIZER}`);
  if (res.ok) break;
  if (i > 40) throw new Error('inventory never provisioned a pool for the published performance');
  await new Promise((r) => setTimeout(r, 500));
}

// Two allocations. The first carries EVERY optional field at a non-default value: a
// fixture that left any at its zero value could not tell preservation from coincidence,
// because the defaults are exactly what a dropping implementation produces. The second
// names a channel with no registry row.
sql(
  'inventory',
  `INSERT INTO channel_allocations (pool_id, channel_code, cap, opens_at, closes_at, requires_code, sold_by)
   VALUES ('${slot}', '${boundChannel}', 40, now() - interval '1 hour', now() + interval '30 days', true, '${reseller}'),
          ('${slot}', '${plainChannel}', 20, NULL, NULL, false, NULL)`,
);

// Real consumption on the bound channel, so lowering its cap is refused for the reason
// this spec is about rather than for want of data. A confirmed claim, written directly:
// buying through the channel would need a presale code (requires_code is true above),
// which is a different ticket's surface.
sql(
  'inventory',
  `INSERT INTO claims (id, pool_id, organizer_id, quantity, status, channel_code, expires_at,
                       idempotency_key, request_fingerprint)
   VALUES (gen_random_uuid(), '${slot}', '${ORGANIZER}', 12, 'confirmed', '${boundChannel}',
           now() + interval '1 hour', 'browser-${stamp}', 'browser-${stamp}')`,
);

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();

  // Count the PUTs the page actually makes. One save must be one full-set replace.
  let puts = 0;
  page.on('request', (req) => {
    if (req.method() === 'PUT') puts++;
  });

  // --- 1. Sign in. A real submit, so the session cookie is set by the server on the
  // /admin path rather than fabricated here.
  await page.goto('/admin/login', { waitUntil: 'domcontentloaded' });
  await page.fill('#identifier', identifier);
  await page.fill('#password', password);
  await Promise.all([page.waitForURL('**/admin**'), page.click('button[type=submit]')]);
  check('admin signs in and lands in the back office', page.url().includes('/admin'));

  // --- 2. The editor renders, with consumption and the unregistered marker.
  await page.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
  check('the allocation editor renders for an admin', page.url().includes(`/admin/slots/${slot}`));

  const boundRow = page.locator(`tr[data-channel="${boundChannel}"]`);
  const plainRow = page.locator(`tr[data-channel="${plainChannel}"]`);
  check('both allocations are listed', (await boundRow.count()) === 1 && (await plainRow.count()) === 1);
  check(
    'current consumption is shown',
    (await page.locator(`[data-consumption="${boundChannel}"]`).innerText()).trim() === '12',
    'an operator lowering a cap needs to see what the channel already sold',
  );
  check(
    'a channel with no registry row is marked, not hidden',
    (await plainRow.locator('[data-unregistered]').count()) === 1,
    'the registry is a lookup, not a constraint (TKT-235) — hiding the row would make it uneditable',
  );

  // --- 3. A REFUSAL lands beside the row the server named. Lower the bound channel
  // below its 12 confirmed: inventory answers a coded 409 carrying the channel.
  puts = 0;
  await page.fill(`input[data-cap-for="${boundChannel}"]`, '5');
  await page.click('button[data-action="save-allocations"]');
  await page.waitForLoadState('domcontentloaded');
  check(
    'a cap below consumption is refused beside THAT row',
    (await page.locator(`[data-row-error="${boundChannel}"]`).count()) === 1,
    'a generic banner would satisfy a naive assertion and fail the COS',
  );
  check(
    'the refusal does not land on the untouched row',
    (await page.locator(`[data-row-error="${plainChannel}"]`).count()) === 0,
  );
  check(
    'the refused submit keeps what the operator typed',
    (await page.locator(`input[data-cap-for="${boundChannel}"]`).inputValue()) === '5',
  );

  // --- 4. The over-capacity refusal lands on the TOTAL, not on a row: the sum is a
  // property of the whole set, so blaming one row would point at an arbitrary field.
  await page.fill(`input[data-cap-for="${boundChannel}"]`, '90');
  await page.fill(`input[data-cap-for="${plainChannel}"]`, '90');
  await page.click('button[data-action="save-allocations"]');
  await page.waitForLoadState('domcontentloaded');
  check(
    'caps summing above capacity are refused on the total',
    (await page.locator('[data-total-error]').count()) === 1,
  );

  // --- 5. THE SAVE. A single valid edit to the bound channel's cap.
  puts = 0;
  await page.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
  await page.fill(`input[data-cap-for="${boundChannel}"]`, '50');
  await Promise.all([
    page.waitForURL(`**/admin/slots/${slot}`),
    page.click('button[data-action="save-allocations"]'),
  ]);
  check(
    'one save is ONE full-set replace',
    puts === 1,
    `observed ${puts} PUTs — per-row saves would round-trip a stale set`,
  );
  check(
    'the edit took',
    (await page.locator(`input[data-cap-for="${boundChannel}"]`).inputValue()) === '50',
  );

  // --- 6. THE assertion this spec exists for. Re-read the DATABASE: everything the
  // operator did not touch must be exactly as it was. The page cannot show this — a
  // dropped sold_by renders identically.
  const after = sql(
    'inventory',
    `SELECT cap || '|' || coalesce(sold_by::text,'NULL') || '|' || requires_code
       || '|' || (opens_at IS NOT NULL) || '|' || (closes_at IS NOT NULL)
     FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${boundChannel}'`,
  );
  const [cap, soldBy, requiresCode, hasOpens, hasCloses] = after.split('|');
  check('the cap edit persisted', cap === '50', `cap=${cap}`);
  check(
    'the seller binding SURVIVED the save',
    soldBy === reseller,
    `sold_by=${soldBy} — dropping it returns a reseller's stock to the public pool (TKT-246)`,
  );
  check('the presale gate survived the save', requiresCode === 't', `requires_code=${requiresCode}`);
  check('the sales window survived the save', hasOpens === 't' && hasCloses === 't');

  // The untouched row is untouched.
  const other = sql(
    'inventory',
    `SELECT cap FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
  );
  check('the row the operator did not edit is unchanged', other === '20', `cap=${other}`);
  check(
    'the unregistered channel still has its allocation',
    other !== '',
    'a full-set replace that dropped unknown codes would delete it',
  );
} finally {
  await browser.close();
  // Remove only what this spec wrote directly. The pool, performance and venue are left
  // alone: they are unique per run, and deleting a pool out from under the catalog rows
  // that own it would leave inventory inconsistent for any later spec.
  try {
    sql('inventory', `DELETE FROM claims WHERE pool_id='${slot}'`);
    sql('inventory', `DELETE FROM channel_allocations WHERE pool_id='${slot}'`);
  } catch {
    // Cleanup failure must not mask a real result.
  }
}

console.log(results.join('\n'));
if (failed) {
  console.error('slot-allocations: FAILED');
  process.exit(1);
}
console.log('slot-allocations: OK');
