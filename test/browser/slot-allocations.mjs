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

import { chromium } from 'playwright-core';
import {
  ORGANIZER,
  provisionAdmin as provisionAdminIn,
  resultRecorder,
  sql as sqlIn,
} from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset — run through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset — run through ./scripts/browser.sh');

const recorder = resultRecorder('slot-allocations');
const { check } = recorder;

// The shared helpers take the container explicitly; this spec talks to one Postgres
// and one catalog throughout, so bind them once rather than at every call site.
const sql = (db, statement) => sqlIn(PG, db, statement);

// browser.sh provisions no staff account (smoke.sh does, for its own suite), so this
// spec makes its own. The password goes in on STDIN and never onto a command line.
const provisionAdmin = (identifier, password) => provisionAdminIn(CATALOG, identifier, password);

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

  // --- 6b. THE RELEASE TIME, FROM A NON-UTC BROWSER (TKT-302).
  //
  // The defect: `release_at` was submitted as a bare `datetime-local` value, which
  // carries no zone, and the SSR handler resolved it with `new Date(...)` in the
  // SERVER's zone. An operator in Toronto typing 10:00 stored whatever 10:00 meant
  // where the server ran — silently, with a 303 reporting success.
  //
  // Only a browser tier can show this, and only a non-UTC one: a unit test runs in
  // whatever zone the machine has, so a server-local implementation and a correct
  // one agree whenever that zone is UTC — which is exactly what CI is. This context
  // is pinned to America/Toronto so the two answers differ by four hours.
  //
  // The value submitted is UTC-explicit, so the assertion has ONE right answer
  // regardless of where this runs. What the non-UTC context proves is that the
  // BROWSER's zone does not leak into the stored instant either.
  const tzContext = await browser.newContext({ baseURL: BASE, timezoneId: 'America/Toronto' });
  try {
    const tzPage = await tzContext.newPage();
    check(
      'the second context really is in a non-UTC zone',
      (await tzPage.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone)) ===
        'America/Toronto',
      'without this the spec would pass on a UTC-only difference and prove nothing',
    );

    await tzPage.goto('/admin/login', { waitUntil: 'domcontentloaded' });
    await tzPage.fill('#identifier', identifier);
    await tzPage.fill('#password', password);
    await Promise.all([
      tzPage.waitForURL('**/admin**'),
      tzPage.click('button[type=submit]'),
    ]);

    await tzPage.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });

    // Rendered in UTC with an explicit zone. A server-local render would show
    // whatever the SSR process's zone made of the stored instant, and the operator
    // would have no way to tell which zone the field meant.
    const renderedRelease = await tzPage
      .locator(`input[data-release-for="${plainChannel}"]`)
      .inputValue();
    check(
      'an empty release time renders empty rather than as an epoch',
      renderedRelease === '',
      `release input = ${JSON.stringify(renderedRelease)}`,
    );

    // Seconds, not a whole minute. AGENTS.md records a spec that seeded
    // whole-minute timestamps and stayed green through a truncation; a value ending
    // in :37 cannot survive one.
    const submitted = '2026-09-01T10:00:37-04:00';
    await tzPage.fill(`input[data-release-for="${plainChannel}"]`, submitted);
    await Promise.all([
      tzPage.waitForURL(`**/admin/slots/${slot}`),
      tzPage.click('button[data-action="save-allocations"]'),
    ]);

    // EXACT, to the microsecond, in UTC. -04:00 means 14:00:37Z, and the seconds
    // must survive.
    //
    // WHAT THIS DOES NOT PROVE, stated because it would otherwise read as the
    // whole point of this section: a ZONED submission parses to the same instant
    // whether or not the server-zone bug is present, because the offset decides
    // and `new Date` honours it either way. Measured, not assumed — with the fix
    // reverted AND the SSR container pinned to America/Los_Angeles, this
    // assertion still passed. It pins the round trip and the seconds; it cannot
    // see the defect.
    //
    // The case that separates fixed from broken is a ZONELESS submission, below.
    const storedRelease = sql(
      'inventory',
      `SELECT to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    check(
      'a release time submitted with an offset round-trips to THAT instant',
      storedRelease === '2026-09-01T14:00:37.000000',
      `release_at=${storedRelease}, want 2026-09-01T14:00:37.000000 — submitted ${submitted}`,
    );

    // And it round-trips: re-rendering must not shift it back.
    await tzPage.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
    const rerendered = await tzPage
      .locator(`input[data-release-for="${plainChannel}"]`)
      .inputValue();
    check(
      'the stored instant re-renders as the same instant, in UTC',
      rerendered === '2026-09-01T14:00:37Z',
      `re-rendered as ${JSON.stringify(rerendered)} — a server-local render moves it every round trip`,
    );

    // THE case that separates fixed from broken. A zoneless value is what the old
    // `datetime-local` input submitted, and resolving it took the SERVER's zone —
    // so the same keystrokes stored a different instant depending on where the
    // process ran. The fix refuses it rather than guessing.
    //
    // Submitted past the input's `pattern` with a direct DOM write, deliberately:
    // the pattern is a browser convenience and this asserts the SERVER refuses,
    // which is the half that binds a caller who ignores the markup.
    const before = sql(
      'inventory',
      `SELECT to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    await tzPage.evaluate(
      ([sel, value]) => {
        const el = document.querySelector(sel);
        el.removeAttribute('pattern');
        el.value = value;
      },
      [`input[data-release-for="${plainChannel}"]`, '2026-09-02T08:00:00'],
    );
    // NOT waitForURL: a refusal re-renders the form in place rather than redirecting,
    // which is itself part of what is under test.
    await tzPage.click('button[data-action="save-allocations"]');
    await tzPage.waitForLoadState('domcontentloaded');
    const afterZoneless = sql(
      'inventory',
      `SELECT coalesce(to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'NULL')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    // EXACTLY UNCHANGED. The first version of this assertion required the value to
    // DIFFER from 2026-09-02T08:00:00 and from its prior value — which `NULL`
    // satisfies, so it blessed the destructive outcome it was written to catch
    // (ai-review [high]): an omitted release_at on a full-set replace CLEARS the
    // gate, and the page redirects as a successful save.
    //
    // The refusal must leave the row alone, so the only safe assertion is equality
    // with what was there before.
    // `before` must be a REAL instant, not empty. Equality with an empty string
    // would hold trivially if this section were ever reordered ahead of the save
    // that sets the value, and the assertion below would then prove nothing while
    // looking exactly as it does now.
    check(
      'the row carries a release time to preserve, so the check below is not vacuous',
      /^\d{4}-\d{2}-\d{2}T/.test(before),
      `before=${JSON.stringify(before)} — this section depends on the zoned save above`,
    );
    check(
      'a zoneless release time changes NOTHING — not stored, and not cleared',
      afterZoneless === before,
      `release_at=${afterZoneless}, want it unchanged at ${before} — ` +
        'storing it means the server zone decided; NULL means the refusal destroyed the gate',
    );
    check(
      'the refusal is shown beside the release field, and the page does not redirect away',
      (await tzPage.locator(`[data-release-error="${plainChannel}"]`).count()) === 1,
      'a refusal the operator cannot see is a save that silently did nothing',
    );

    // BLANKING is refused too, and for a sharper reason: clearing a release gate is
    // destructive and a free-text field makes an accidental blank cheap — cheaper
    // than the malformed value above, which is refused outright (ai-review pass 2,
    // [medium]). Removal is an explicit act.
    await tzPage.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
    await tzPage.fill(`input[data-release-for="${plainChannel}"]`, '');
    await tzPage.click('button[data-action="save-allocations"]');
    await tzPage.waitForLoadState('domcontentloaded');
    const afterBlank = sql(
      'inventory',
      `SELECT coalesce(to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'NULL')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    check(
      'blanking the field WITHOUT the confirmation does not remove the gate',
      afterBlank === before,
      `release_at=${afterBlank}, want it unchanged at ${before}`,
    );

    // THE FORM'S OWN GRAMMAR must match the server's. The unit acceptance cases
    // call the mapper directly and so cannot see a `pattern` attribute that blocks
    // a value the server would have taken (ai-review pass 4, [medium]): the browser
    // refuses the submission, the operator sees a tooltip, and every unit test
    // still passes. Submitted through the real form, with NO DOM tampering, so the
    // browser's constraint validation is part of what is under test.
    await tzPage.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
    await tzPage.fill(`input[data-release-for="${plainChannel}"]`, '2026-09-03t08:30:00.5z');

    // checkValidity() explicitly, BEFORE submitting. Playwright's click submits
    // programmatically and bypasses HTML constraint validation, so the round trip
    // below passes whatever the `pattern` says — the first version of this
    // assertion did exactly that and stayed green with the pattern reverted to
    // uppercase-only. This is the line that actually reads the attribute a real
    // operator's browser would enforce.
    check(
      "the form's own pattern accepts what the server accepts",
      await tzPage.$eval(`input[data-release-for="${plainChannel}"]`, (el) => el.checkValidity()),
      'the input pattern rejects a value the server parses — a real operator could not submit it',
    );

    await Promise.all([
      tzPage.waitForURL(`**/admin/slots/${slot}`),
      tzPage.click('button[data-action="save-allocations"]'),
    ]);
    const afterMixedCase = sql(
      'inventory',
      `SELECT coalesce(to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'NULL')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    check(
      'a lowercase-t/z value with a fraction goes through the real form',
      afterMixedCase === '2026-09-03T08:30:00.500000',
      `release_at=${afterMixedCase}, want 2026-09-03T08:30:00.500000 — ` +
        'NULL or unchanged means the input pattern blocked a form the server accepts',
    );

    // MICROSECONDS SURVIVE THE WHOLE PATH, and only this tier can show it. The unit
    // tests assert what the mapper returns; this asserts what PostgreSQL holds after a
    // real browser submitted a real form. The two are different claims, and the defect
    // this replaces lived between them: the mapper returned `Date.toISOString()`, which
    // is millisecond-capped, so `.123456Z` was stored as `.123000` — a release gate
    // moved by 456µs, reported as a success.
    //
    // `.US` in the format string is microseconds, so a truncating implementation reads
    // back as `...123000` and this check fails on the digits it exists for.
    await tzPage.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
    await tzPage.fill(`input[data-release-for="${plainChannel}"]`, '2026-09-04T11:22:33.123456Z');
    check(
      "the form's own pattern accepts six fractional digits",
      await tzPage.$eval(`input[data-release-for="${plainChannel}"]`, (el) => el.checkValidity()),
      'the input pattern rejects a microsecond value the server parses — and the database ' +
        'itself produces exactly this shape, so an operator pasting one back would be blocked',
    );
    await Promise.all([
      tzPage.waitForURL(`**/admin/slots/${slot}`),
      tzPage.click('button[data-action="save-allocations"]'),
    ]);
    const afterMicros = sql(
      'inventory',
      `SELECT coalesce(to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'NULL')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    check(
      'a microsecond fraction is stored to the digit, not truncated to milliseconds',
      afterMicros === '2026-09-04T11:22:33.123456',
      `release_at=${afterMicros}, want 2026-09-04T11:22:33.123456 — ` +
        '...123000 means the value went through a millisecond-capped conversion',
    );

    // Put the gate back where the sections below expect it.
    await tzPage.fill(`input[data-release-for="${plainChannel}"]`, '2026-09-01T10:00:37Z');
    await Promise.all([
      tzPage.waitForURL(`**/admin/slots/${slot}`),
      tzPage.click('button[data-action="save-allocations"]'),
    ]);

    // RECOVER FROM THE REFUSAL PAGE, without reloading. This is the case the
    // previous version of this spec missed by doing a fresh GET first: the
    // checkbox was rendered only when the SUBMITTED text was non-empty, so the
    // refusal re-rendered the blank and hid the very control its error message
    // told the operator to tick (ai-review pass 3, [medium]). An error with no
    // way to act on it.
    check(
      'the confirmation checkbox is still on the page AFTER the blank was refused',
      (await tzPage.locator(`input[data-clear-release-for="${plainChannel}"]`).count()) === 1,
      'the operator must be able to act on the refusal without reloading and re-editing',
    );

    // And the confirmed removal works from that same page, or the refusal is a wall.
    await tzPage.check(`input[data-clear-release-for="${plainChannel}"]`);
    await Promise.all([
      tzPage.waitForURL(`**/admin/slots/${slot}`),
      tzPage.click('button[data-action="save-allocations"]'),
    ]);
    const afterConfirmedClear = sql(
      'inventory',
      `SELECT coalesce(to_char(release_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'NULL')
         FROM channel_allocations WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
    );
    check(
      'ticking the confirmation DOES remove the gate',
      afterConfirmedClear === 'NULL',
      `release_at=${afterConfirmedClear} — the refusal must not make removal impossible`,
    );
  } finally {
    await tzContext.close();
  }

  // --- 7. THE STALE SAVE (TKT-250). Two operators, one slot: this page was rendered
  // before someone else committed, and its save must be refused rather than silently
  // overwriting them.
  //
  // Only a browser can check this end to end. The revision travels as a hidden input in
  // the rendered HTML, and the comparison happens in inventory — so what is under test
  // is that the SSR handler sends back the revision the PAGE carried and not the one its
  // own fresh read just returned. A server-side unit test cannot see which of the two
  // was put in the form, and the wrong one matches every time.
  await page.goto(`/admin/slots/${slot}`, { waitUntil: 'domcontentloaded' });
  const staleRevision = await page.locator('input[data-allocation-revision]').inputValue();
  check(
    'the form carries the allocation-set revision',
    staleRevision !== '' && Number.isInteger(Number(staleRevision)),
    `data-allocation-revision=${JSON.stringify(staleRevision)}`,
  );

  // Someone else saves while this page sits open. Written as SQL because browser.sh
  // hands specs containers rather than credentials — and the revision is bumped
  // EXPLICITLY, because only ReplaceChannelAllocations moves it: a direct INSERT would
  // leave the counter where it was, the stale revision would still match, the save would
  // succeed and this test would pass while proving nothing.
  sql(
    'inventory',
    `UPDATE channel_allocations SET cap=33 WHERE pool_id='${slot}' AND channel_code='${plainChannel}'`,
  );
  sql(
    'inventory',
    `UPDATE inventory_pools SET allocation_revision=allocation_revision+1 WHERE slot_id='${slot}'`,
  );

  // This page now submits its pre-existing form. Its revision is one behind.
  await page.fill(`input[data-cap-for="${boundChannel}"]`, '44');
  await page.click('button[data-action="save-allocations"]');
  await page.waitForLoadState('domcontentloaded');

  // Targets [data-form-error] specifically, not the page text: the message must land at
  // the FORM level, because no field the operator can see is wrong. Scoping to `form`
  // would miss it entirely — this banner renders just above the <form> element — and
  // grepping the whole page would pass on a message rendered anywhere at all, including
  // beside a row, which is exactly the placement this assertion exists to rule out.
  const staleError = page.locator('[data-form-error]');
  check(
    'a stale save is refused with a reload instruction',
    (await staleError.count()) === 1 && /reload/i.test(await staleError.innerText()),
    'the operator must be told their view is stale, not shown a generic failure',
  );
  check(
    'the stale refusal does not land on a row',
    (await page.locator(`[data-row-error="${boundChannel}"]`).count()) === 0 &&
      (await page.locator('[data-total-error]').count()) === 0,
    'every field the operator can see is fine — pointing at one would send them to fix a value that is not the problem',
  );
  check(
    'a stale save does not redirect',
    page.url().includes(`/admin/slots/${slot}`),
    'a 303 would report success for a write that never happened',
  );

  // The database is the assertion, in BOTH directions — a full-set replace deletes as
  // well as writes, so proving the stale cap is absent is only half of it.
  const afterStale = sql(
    'inventory',
    `SELECT coalesce((SELECT cap::text FROM channel_allocations
                       WHERE pool_id='${slot}' AND channel_code='${boundChannel}'), 'GONE')
         || '|' ||
            coalesce((SELECT cap::text FROM channel_allocations
                       WHERE pool_id='${slot}' AND channel_code='${plainChannel}'), 'GONE')`,
  );
  const [boundCap, plainCap] = afterStale.split('|');
  check(
    'the stale edit was NOT applied',
    boundCap === '50',
    `cap=${boundCap}, want 50 — the value from before the stale submit`,
  );
  check(
    "the other operator's committed change SURVIVED",
    plainCap === '33',
    `cap=${plainCap}, want 33 — the stale full-set replace would have overwritten it`,
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

if (!recorder.finish()) {
  process.exit(1);
}
