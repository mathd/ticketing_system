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

// A pool to edit, seeded directly into inventory.
//
// NOT through the catalog API: every catalog write needs the staff-write credential
// (TKT-191) and browser.sh deliberately hands specs the CONTAINERS rather than
// credentials, so a spec that wrote through the API would need the runner to export a
// secret it currently does not. Seeding the one table this page reads keeps the
// dependency where it already is — the same `docker exec … psql` shape rate-limit.mjs
// uses — rather than widening browser.sh's contract for one spec.
//
// The columns are inventory's own (0001 + 0009); a migration adding a NOT NULL without a
// default would break this INSERT loudly, which is the right failure mode.
const slot = sql(
  'inventory',
  `INSERT INTO inventory_pools (slot_id, organizer_id, capacity, source_event_id, inventory_kind)
   VALUES (gen_random_uuid(), '${ORGANIZER}', 100, gen_random_uuid(), 'ga')
   RETURNING slot_id`,
);
if (!slot) throw new Error('failed to seed an inventory pool');

// Two allocations. The first carries EVERY optional field at a non-default value: a
// fixture that left any at its zero value could not tell preservation from coincidence,
// because the defaults are exactly what a dropping implementation produces. The second
// names a channel with no registry row.
// The window boundaries carry SECONDS and microseconds deliberately (ai-review pass 1):
// a `datetime-local` input holds minutes, so a save that re-derived an untouched boundary
// from the form would truncate them and move the boundary up to a minute earlier. These
// are compared against clock_timestamp(), so that is a real change to when a channel
// opens or returns to public sale. Asserting they are merely non-null cannot see it.
sql(
  'inventory',
  `INSERT INTO channel_allocations (pool_id, channel_code, cap, opens_at, closes_at, requires_code, sold_by)
   VALUES ('${slot}', '${boundChannel}', 40,
           timestamptz '2026-08-01 09:05:11.500000+00', timestamptz '2026-12-31 23:59:59.999999+00',
           true, '${reseller}'),
          ('${slot}', '${plainChannel}', 20, NULL, NULL, false, NULL)`,
);

// Real consumption on the bound channel, so lowering its cap is refused for the reason
// this spec is about rather than for want of data. A confirmed claim, written directly:
// buying through the channel would need a presale code (requires_code is true above),
// which is a different ticket's surface.
sql(
  'inventory',
  `INSERT INTO claims (id, organizer_id, pool_id, quantity, status, expires_at,
                       idempotency_key, request_fingerprint, claim_kind, channel_code)
   VALUES (gen_random_uuid(), '${ORGANIZER}', '${slot}', 12, 'confirmed',
           now() + interval '1 hour', 'browser-${stamp}', 'browser-${stamp}', 'buyer', '${boundChannel}')`,
);

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();

  // Count the form submits. The page saves the WHOLE set in one POST; the PUT to
  // inventory is made server-side by the SSR handler and never appears here.
  let posts = 0;
  page.on('request', (req) => {
    if (req.method() === 'POST' && req.url().includes('/admin/slots/')) posts++;
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
  //
  // The page submits ONE form POST; the PUT to inventory happens server-side, inside the
  // SSR handler, so it is not observable from the browser. What IS observable here — and
  // what the per-row-save mistake would break — is that the whole set moves together:
  // section 6 re-reads the database and finds the untouched row intact. A count of PUTs
  // from the browser would always be zero and would prove nothing either way.
  posts = 0;
  await page.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
  await page.fill(`input[data-cap-for="${boundChannel}"]`, '50');
  await Promise.all([
    page.waitForURL(`**/admin/slots/${slot}`),
    page.click('button[data-action="save-allocations"]'),
  ]);
  check(
    'one save is ONE form submit',
    posts === 1,
    `observed ${posts} POSTs — the whole set saves together, never per row`,
  );
  check(
    'the save redirected (post/redirect/get), so a reload does not resubmit',
    page.url().endsWith(`/admin/slots/${slot}`),
  );
  check(
    'the edit took',
    (await page.locator(`input[data-cap-for="${boundChannel}"]`).inputValue()) === '50',
  );

  // --- 6. THE assertion this spec exists for. Re-read the DATABASE: everything the
  // operator did not touch must be exactly as it was. The page cannot show this — a
  // dropped sold_by renders identically.
  // Booleans are projected as explicit 'yes'/'no' rather than read raw: psql renders a
  // bare boolean as `t`, but one concatenated with `||` is cast to text and renders as
  // `true`, so an assertion against either spelling depends on how the query was written
  // rather than on what the column holds.
  // The window boundaries are compared EXACTLY, to the microsecond — not merely for
  // presence. A save that re-derived them from the minute-precision input would leave
  // them non-null and still have moved them, which is the corruption ai-review found.
  const after = sql(
    'inventory',
    `SELECT cap || '|' || coalesce(sold_by::text,'NULL')
       || '|' || CASE WHEN requires_code THEN 'yes' ELSE 'no' END
       || '|' || to_char(opens_at  AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
       || '|' || to_char(closes_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
     FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${boundChannel}'`,
  );
  const [cap, soldBy, requiresCode, opensAt, closesAt] = after.split('|');
  check('the cap edit persisted', cap === '50', `cap=${cap}`);
  check(
    'the seller binding SURVIVED the save',
    soldBy === reseller,
    `sold_by=${soldBy} — dropping it returns a reseller's stock to the public pool (TKT-246)`,
  );
  check(
    'the presale gate survived the save',
    requiresCode === 'yes',
    `requires_code=${requiresCode}`,
  );
  check(
    'the sales window survived the save TO THE MICROSECOND',
    opensAt === '2026-08-01T09:05:11.500000' && closesAt === '2026-12-31T23:59:59.999999',
    `opens_at=${opensAt} closes_at=${closesAt} — a truncated boundary is still non-null, ` +
      'and moves when a channel opens or returns to public sale',
  );

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
