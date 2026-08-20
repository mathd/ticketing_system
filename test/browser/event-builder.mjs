// Real-browser coverage for every form in the back-office event builder.

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
const VENUE = '00000000-0000-0000-0000-0000000000a1';
const stamp = Date.now();
const identifier = `event-builder-${stamp}@example.test`;
const password = 'correct horse battery staple';
const eventName = `Browser Night ${stamp}`;
const eventNameFr = `Nuit navigateur ${stamp}`;
const startsAt = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();

provisionAdmin(CATALOG, identifier, password);

const { check, finish } = resultRecorder('event-builder browser spec');
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await signIn(page, identifier, password);
  check('admin signs in', page.url().includes('/admin'));

  await page.goto(PATH, { waitUntil: 'domcontentloaded' });
  await page.fill('#name_en', eventName);
  await page.fill('#name_fr', eventNameFr);
  let request = await submitForm(page, page.getByRole('button', { name: 'Create event' }));
  check('the event form posts to the builder', new URL(request.url()).pathname === PATH, request.url());
  const eventId = new URL(page.url()).searchParams.get('event');
  check('the event step redirects with its id', Boolean(eventId), page.url());

  await page.selectOption('#venue_id', VENUE);
  await page.fill('#starts_at', startsAt);
  await page.fill('#timezone', 'UTC');
  request = await submitForm(page, page.getByRole('button', { name: 'Add the date' }));
  check('the date form posts to the builder', new URL(request.url()).pathname === PATH, request.url());
  const performanceId = new URL(page.url()).searchParams.get('performance');
  check('the date step redirects with its id', Boolean(performanceId), page.url());

  await page.fill('#tt_name_en', 'Standard');
  await page.fill('#tt_name_fr', 'Standard');
  await page.fill('#amount', '4550');
  await page.fill('#currency', 'EUR');
  request = await submitForm(page, page.getByRole('button', { name: 'Set the price' }));
  check('the price form posts to the builder', new URL(request.url()).pathname === PATH, request.url());
  const ticketTypeId = new URL(page.url()).searchParams.get('ticket_type');
  check('the price step redirects with its id', Boolean(ticketTypeId), page.url());

  request = await submitForm(page, page.getByRole('button', { name: 'Publish' }));
  check('the publish form posts to the builder', new URL(request.url()).pathname === PATH, request.url());
  check(
    'catalog publication is reported on the page',
    (await page.getByRole('heading', { name: 'Publication accepted' }).count()) === 1 &&
      (await page.locator('.done').innerText()).includes('published'),
  );

  check(
    'the localized event names were stored exactly',
    sql(PG, 'catalog', `SELECT (name->>'en') || '|' || (name->>'fr') FROM events WHERE id='${eventId}'`) ===
      `${eventName}|${eventNameFr}`,
  );
  check(
    'the dated performance kept its venue, timezone, and published status',
    sql(
      PG,
      'catalog',
      `SELECT organizer_id::text || '|' || venue_id::text || '|' || timezone || '|' || status
       FROM performances WHERE id='${performanceId}'`,
    ) === `${ORGANIZER}|${VENUE}|UTC|published`,
  );
  check(
    'the ticket price is integer minor units with its currency',
    sql(
      PG,
      'catalog',
      `SELECT performance_id::text || '|' || price_amount::text || '|' || currency
       FROM ticket_types WHERE id='${ticketTypeId}'`,
    ) === `${performanceId}|4550|EUR`,
  );
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
