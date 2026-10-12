// Real-browser coverage for the back-office UI language (TKT-368).
//
// What only this tier can show: the switch is a real form POST through the
// gateway and the gate's origin check; the browser stores the cookie it is
// given and sends it on the next page; the forms carry the action the browser
// actually resolves; and the event a French page submits is saved with both
// content locales, unchanged. The container tests render the same pages, but
// they construct their own request and never meet the proxy, the gate or a
// cookie jar.
//
// A browser spec cannot see the catalog write the SSR handler makes, so every
// claim about what was SAVED is read back from PostgreSQL, exactly.

import { chromium } from 'playwright-core';
import {
  ORGANIZER,
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

const PATH = '/admin/events/new';
const stamp = Date.now();
const identifier = `event-builder-language-${stamp}@example.test`;
const password = 'correct horse battery staple';
const nameEn = `Language Night ${stamp}`;
const nameFr = `Soirée langue ${stamp}`;
// Distinct per locale on purpose: equal values would hide a swapped pair.
const ticketEn = `Balcony ${stamp}`;
const ticketFr = `Balcon ${stamp}`;
const VENUE = '00000000-0000-0000-0000-0000000000a1';
const startsAt = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();

provisionAdmin(CATALOG, identifier, password);

const { check, finish } = resultRecorder('event-builder-language browser spec');
const browser = await chromium.launch({ channel: 'chrome' });

const cookieNamed = async (context, name) => (await context.cookies()).find((c) => c.name === name);
const alertText = async (page) => (await page.locator('[role=alert]').innerText()).trim();

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  // A missing French element should fail in seconds, not after the default 30 s.
  page.setDefaultTimeout(10_000);

  // The login page offers no switch (D1(b)), so the shared sign-in helper still
  // finds exactly one submit button there.
  await page.goto('/admin/login', { waitUntil: 'domcontentloaded' });
  check('the login page offers no language switch', (await page.locator('form.ui-language').count()) === 0);
  check('the login page is English', (await page.getAttribute('html', 'lang')) === 'en');
  await signIn(page, identifier, password);
  const sessionBefore = (await cookieNamed(context, 'bo_sid'))?.value;

  await page.goto(PATH, { waitUntil: 'domcontentloaded' });
  check('the builder starts in English', (await page.textContent('h1')) === 'Create an event');

  // --- Switch to French on step 1 --------------------------------------------
  const switchForm = page.locator('form.ui-language');
  check('the switch posts to its own endpoint', (await switchForm.getAttribute('action')) === '/admin/ui-language');
  let request = await submitForm(page, page.getByRole('button', { name: 'Français', exact: true }));
  check(
    'the switch POST went to /admin/ui-language',
    request.method() === 'POST' && new URL(request.url()).pathname === '/admin/ui-language',
    request.url(),
  );
  check('the switch lands back on the builder', new URL(page.url()).pathname === PATH && new URL(page.url()).search === '', page.url());
  const languageCookie = await cookieNamed(context, 'bo_ui_language');
  check(
    'the browser stored the language cookie for the whole back office',
    languageCookie?.value === 'fr' &&
      languageCookie.path === '/admin' &&
      languageCookie.httpOnly === true &&
      languageCookie.sameSite === 'Lax',
    JSON.stringify(languageCookie),
  );
  check('the switch left the session cookie unchanged', (await cookieNamed(context, 'bo_sid'))?.value === sessionBefore);
  check('the builder is now French', (await page.getAttribute('html', 'lang')) === 'fr');
  check('the French heading renders', (await page.textContent('h1')) === 'Créer un événement');
  check('the French content-field label renders', (await page.textContent('label[for="name_fr"]')) === 'Nom (français)');
  check('the English content-field label renders in French', (await page.textContent('label[for="name_en"]')) === 'Nom (anglais)');
  check('the switch now offers English', (await page.getByRole('button', { name: 'English', exact: true }).count()) === 1);
  // Each content field carries the language of its CONTENT, not the UI's.
  check(
    'the event content fields keep their content language under a French UI',
    (await page.getAttribute('#name_en', 'lang')) === 'en' && (await page.getAttribute('#name_fr', 'lang')) === 'fr',
  );

  // --- The French validation state --------------------------------------------
  // A whitespace-only name satisfies the browser's `required` (it is not empty)
  // and reaches the server, which is where the French message comes from. The
  // browser's own constraint bubbles follow the browser's language (D1(e)).
  const eventForm = page.locator('form:has(input[name="_action"][value="create-event"])');
  check('the event form action is literally relative', (await eventForm.getAttribute('action')) === '');
  const eventKey = await eventForm.locator('input[name="idempotency_key"]').inputValue();
  await page.fill('#name_en', nameEn);
  await page.fill('#name_fr', '   ');
  request = await submitForm(page, page.getByRole('button', { name: 'Créer l’événement', exact: true }));
  check('the refused event form posted to the builder', new URL(request.url()).pathname === PATH, request.url());
  check('the refusal is said in French', (await alertText(page)) === 'Le nom (FR) est obligatoire.', await alertText(page));
  check('the refusal flags the French content field', (await page.getAttribute('#name_fr', 'aria-invalid')) === 'true');
  check('the refusal does not flag the English content field', (await page.getAttribute('#name_en', 'aria-invalid')) === null);
  check('the English content value is kept', (await page.inputValue('#name_en')) === nameEn);
  check(
    'the refused form keeps its idempotency key',
    (await eventForm.locator('input[name="idempotency_key"]').inputValue()) === eventKey,
  );
  check(
    'the refused submit saved nothing',
    sql(PG, 'catalog', `SELECT count(*)::text FROM events WHERE idempotency_key='${eventKey}'`) === '0',
  );

  // --- The corrected submit, read back exactly ---------------------------------
  await page.fill('#name_fr', nameFr);
  request = await submitForm(page, page.getByRole('button', { name: 'Créer l’événement', exact: true }));
  check('the corrected event form posted to the builder', new URL(request.url()).pathname === PATH, request.url());
  const eventId = new URL(page.url()).searchParams.get('event');
  check('the event step redirects with its id', Boolean(eventId), page.url());
  check(
    'the saved name carries exactly the two content locales',
    sql(PG, 'catalog', `SELECT string_agg(k, ',' ORDER BY k) FROM events, jsonb_object_keys(name) k WHERE id='${eventId}'`) === 'en,fr',
  );
  check(
    'the saved names, organizer and key are exactly what the French page submitted',
    sql(
      PG,
      'catalog',
      `SELECT (name->>'en') || '|' || (name->>'fr') || '|' || organizer_id::text || '|' || idempotency_key
       FROM events WHERE id='${eventId}'`,
    ) === `${nameEn}|${nameFr}|${ORGANIZER}|${eventKey}`,
  );
  check('step 2 renders in French', (await page.textContent('h2')) === '2. Date et lieu');
  check(
    'the date form action is literally relative',
    (await page.locator('form:has(input[name="_action"][value="create-performance"])').getAttribute('action')) === '',
  );

  // --- Steps 2 and 3 in French, filled by LABEL as an operator does -----------
  // Filling by label is what sees a render that swaps the two content names
  // behind correct labels (codex review finding 2).
  await page.getByLabel('Salle', { exact: true }).selectOption(VENUE);
  await page.getByLabel('Début', { exact: true }).fill(startsAt);
  await page.getByLabel('Fuseau horaire (IANA)', { exact: true }).fill('UTC');
  request = await submitForm(page, page.getByRole('button', { name: 'Ajouter la date', exact: true }));
  check('the French date form posted to the builder', new URL(request.url()).pathname === PATH, request.url());
  const performanceId = new URL(page.url()).searchParams.get('performance');
  check('the date step redirects with its id', Boolean(performanceId), page.url());
  check('step 3 renders in French', (await page.textContent('h2')) === '3. Le prix');
  check(
    'the ticket content fields keep their content language under a French UI',
    (await page.getAttribute('#tt_name_en', 'lang')) === 'en' && (await page.getAttribute('#tt_name_fr', 'lang')) === 'fr',
  );
  const priceForm = page.locator('form:has(input[name="_action"][value="create-ticket-type"])');
  check('the price form action is literally relative', (await priceForm.getAttribute('action')) === '');
  await page.getByLabel('Nom du billet (anglais)', { exact: true }).fill(ticketEn);
  await page.getByLabel('Nom du billet (français)', { exact: true }).fill(ticketFr);
  await page.getByLabel('Montant en unités mineures', { exact: true }).fill('4550');
  await page.getByLabel('Devise', { exact: true }).fill('EUR');
  request = await submitForm(page, page.getByRole('button', { name: 'Définir le prix', exact: true }));
  check('the French price form posted to the builder', new URL(request.url()).pathname === PATH, request.url());
  const ticketTypeId = new URL(page.url()).searchParams.get('ticket_type');
  check('the price step redirects with its id', Boolean(ticketTypeId), page.url());
  check(
    'the saved ticket name carries exactly the two content locales',
    sql(PG, 'catalog', `SELECT string_agg(k, ',' ORDER BY k) FROM ticket_types, jsonb_object_keys(name) k WHERE id='${ticketTypeId}'`) === 'en,fr',
  );
  check(
    'the saved ticket names and price are exactly what the French page submitted',
    sql(
      PG,
      'catalog',
      `SELECT (name->>'en') || '|' || (name->>'fr') || '|' || performance_id::text || '|' || price_amount::text || '|' || currency
       FROM ticket_types WHERE id='${ticketTypeId}'`,
    ) === `${ticketEn}|${ticketFr}|${performanceId}|4550|EUR`,
  );
  check('step 4 renders in French', (await page.textContent('h2')) === '4. Publication');

  // --- The shell in French ------------------------------------------------------
  await page.goto('/admin/', { waitUntil: 'domcontentloaded' });
  check('the venue list is French', (await page.getAttribute('html', 'lang')) === 'fr');
  check('the French nav link renders', (await page.getByRole('link', { name: 'Créer un événement', exact: true }).count()) === 1);
  check('the French sign-out renders', (await page.getByRole('button', { name: 'Se déconnecter', exact: true }).count()) === 1);
  check('the venue table caption is French', (await page.textContent('caption')) === 'Salles');

  // An out-of-scope page keeps an English body under lang="en" (D1(c)), and the
  // preference still reaches it.
  await page.goto('/admin/orders', { waitUntil: 'domcontentloaded' });
  check('an English page keeps lang="en"', (await page.getAttribute('html', 'lang')) === 'en');
  check('its header follows the UI language', (await page.getAttribute('header', 'lang')) === 'fr');

  // --- Switch back to English on step 4: the progress survives -----------------
  const progress = `${PATH}?event=${eventId}&performance=${performanceId}&ticket_type=${ticketTypeId}`;
  await page.goto(progress, { waitUntil: 'domcontentloaded' });
  request = await submitForm(page, page.getByRole('button', { name: 'English', exact: true }));
  check('the second switch POST went to /admin/ui-language', new URL(request.url()).pathname === '/admin/ui-language');
  check('the switch keeps the page and its progress', page.url() === `${BASE}${progress}`, page.url());
  check('English remains available', (await page.getAttribute('html', 'lang')) === 'en');
  check('step 4 renders in English again', (await page.textContent('h2')) === '4. Publish');
  check('the cookie now says English', (await cookieNamed(context, 'bo_ui_language'))?.value === 'en');
  check('the session survived both switches', (await cookieNamed(context, 'bo_sid'))?.value === sessionBefore);
} catch (error) {
  // Record the step that could not run instead of throwing past finish(): a
  // mutated page usually times out on the next locator, and the checks already
  // recorded are the evidence of WHICH property broke.
  check('the spec ran to completion', false, String(error?.message ?? error).split('\n')[0]);
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
