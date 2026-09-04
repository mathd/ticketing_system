// Real-browser coverage for every write action on the venue authoring page.

import { chromium } from 'playwright-core';
import {
  provisionAdmin,
  resultRecorder,
  signIn,
  sql,
  submitForm,
} from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset; run through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset; run through ./scripts/browser.sh');

const VENUE = '00000000-0000-0000-0000-0000000000a1';
const PATH = `/admin/venues/${VENUE}`;
const stamp = Date.now();
const identifier = `venue-authoring-${stamp}@example.test`;
const password = 'correct horse battery staple';
const mapName = `Browser map ${stamp}`;
const capacity = 2600 + (stamp % 100);

provisionAdmin(CATALOG, identifier, password);

const { check, finish } = resultRecorder('venue-authoring browser spec');
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await signIn(page, identifier, password);

  await page.goto(PATH, { waitUntil: 'domcontentloaded' });

  const gaForm = page.locator('form:has(input[value="set-ga"])');
  await gaForm.locator('input[name="ga_capacity"]').fill(String(capacity));
  let request = await submitForm(page, gaForm.getByRole('button', { name: 'Save GA capacity' }));
  check('the GA form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());
  check(
    'the submitted GA capacity was stored exactly',
    sql(PG, 'catalog', `SELECT ga_capacity FROM venues WHERE id='${VENUE}'`) === String(capacity),
  );

  const mapForm = page.locator('form:has(input[value="create-map"])');
  await mapForm.locator('input[name="name"]').fill(mapName);
  request = await submitForm(page, mapForm.getByRole('button', { name: 'Create draft map' }));
  check('the map form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());
  const mapId = new URL(page.url()).searchParams.get('map');
  check('the map step redirects with its id', Boolean(mapId), page.url());

  const sectionForm = page.locator('form:has(input[value="add-section"])');
  await sectionForm.locator('input[name="name"]').fill('Main');
  await sectionForm.locator('input[name="position"]').fill('1');
  request = await submitForm(page, sectionForm.getByRole('button', { name: 'Add section' }));
  check('the section form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());

  const rowForm = page.locator('form:has(input[value="add-row"])');
  await rowForm.locator('input[name="label"]').fill('A');
  await rowForm.locator('input[name="position"]').fill('1');
  request = await submitForm(page, rowForm.getByRole('button', { name: 'Add row' }));
  check('the row form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());

  const seatForm = page.locator('form:has(input[value="add-seat"])');
  await seatForm.locator('input[name="label"]').fill('1');
  await seatForm.locator('input[name="position"]').fill('1');
  request = await submitForm(page, seatForm.getByRole('button', { name: 'Add seat' }));
  check('the seat form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());

  const publishForm = page.locator('form:has(input[value="publish-map"])');
  request = await submitForm(page, publishForm.getByRole('button', { name: 'Publish this map' }));
  check('the publish form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());
  check(
    'the first map version is published',
    sql(PG, 'catalog', `SELECT version::text || '|' || status FROM seat_maps WHERE id='${mapId}'`) ===
      '1|published',
  );

  // The editor is a React island. Wait for hydration before reading or changing it.
  await page.waitForLoadState('networkidle');
  const seatLabel = page.getByLabel('Seat Main/A label');
  await seatLabel.fill('101');
  request = await submitForm(page, page.getByRole('button', { name: 'Save as new version' }));
  check('the edit form posts to the venue page', new URL(request.url()).pathname === PATH, request.url());
  const editedMapId = new URL(page.url()).searchParams.get('map');
  check('editing selects a different map version', Boolean(editedMapId) && editedMapId !== mapId, page.url());

  const lineage = sql(
    PG,
    'catalog',
    `SELECT string_agg(version::text || ':' || status || ':' || id::text, ',' ORDER BY version)
     FROM seat_maps
     WHERE map_family_id=(SELECT map_family_id FROM seat_maps WHERE id='${mapId}')`,
  );
  check(
    'the edit kept version one and created published version two',
    lineage === `1:published:${mapId},2:published:${editedMapId}`,
    lineage,
  );
  check(
    'version one kept its original seat',
    sql(PG, 'catalog', `SELECT label FROM seat_map_seats WHERE seat_map_id='${mapId}'`) === '1',
  );
  check(
    'version two stored the edited seat',
    sql(PG, 'catalog', `SELECT label FROM seat_map_seats WHERE seat_map_id='${editedMapId}'`) === '101',
  );
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
