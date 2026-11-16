// TKT-289: a reservation that arrives already expired must not be replayed.
//
// The reserve call is made BY THE BROWSER: HoldPicker is a client island (ADR-006) whose
// click handler fetches /api/commerce/reservations directly, so unlike checkout's
// server-side upstream call, Playwright can see the request and its Idempotency-Key.
// The two reservation responses are route-fulfilled — a dead-on-arrival hold, then a live
// one — because the property under test is what the RENDERED island sends on the second
// press, not how inventory produces a dead hold (that is ADR-024's, tested in inventory).
// Everything else is real: the published event page, hydration, and the buyer's clicks.

import { randomUUID } from 'node:crypto';
import { chromium } from 'playwright-core';
import { provisionAdmin, resultRecorder, signIn, submitForm } from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const CATALOG = process.env.CATALOG_CONTAINER;
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset. Run this spec through ./scripts/browser.sh');

const VENUE = '00000000-0000-0000-0000-0000000000a1';
const stamp = Date.now();
const identifier = `dead-hold-${stamp}@example.test`;
const password = 'correct horse battery staple';
const startsAt = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();

const { check, finish } = resultRecorder('dead-hold-retry browser spec');
provisionAdmin(CATALOG, identifier, password);

function reservationBody(remainingMs) {
  const now = new Date();
  return {
    hold_id: randomUUID(),
    reservation_id: randomUUID(),
    buyer_id: randomUUID(),
    status: 'held',
    amount: 1250,
    currency: 'EUR',
    server_time: now.toISOString(),
    expires_at: new Date(now.getTime() + remainingMs).toISOString(),
  };
}

async function publishEvent(page) {
  await signIn(page, identifier, password);
  await page.goto('/admin/events/new', { waitUntil: 'domcontentloaded' });
  await page.fill('#name_en', `Dead hold ${stamp}`);
  await page.fill('#name_fr', `Réservation morte ${stamp}`);
  await submitForm(page, page.getByRole('button', { name: 'Create event' }));
  const eventId = new URL(page.url()).searchParams.get('event');
  if (!eventId) throw new Error(`event creation did not return an event id: ${page.url()}`);
  await page.selectOption('#venue_id', VENUE);
  await page.fill('#starts_at', startsAt);
  await page.fill('#timezone', 'UTC');
  await submitForm(page, page.getByRole('button', { name: 'Add the date' }));
  await page.fill('#tt_name_en', 'General admission');
  await page.fill('#tt_name_fr', 'Admission générale');
  await page.fill('#amount', '1250');
  await page.fill('#currency', 'EUR');
  await submitForm(page, page.getByRole('button', { name: 'Set the price' }));
  await submitForm(page, page.getByRole('button', { name: 'Publish' }));
  await page.getByRole('heading', { name: 'Publication accepted' }).waitFor();
  return eventId;
}

const browser = await chromium.launch({ channel: 'chrome' });
try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  const eventId = await publishEvent(page);

  // Wait for the published offer to reach the storefront.
  const deadline = Date.now() + 20_000;
  for (;;) {
    const response = await page.goto(`/en/events/${eventId}`, { waitUntil: 'domcontentloaded' });
    if (response?.ok() && (await page.getByRole('button', { name: 'Reserve', exact: true }).count())) break;
    if (Date.now() > deadline) throw new Error(`event ${eventId} never offered a Reserve button`);
    await page.waitForTimeout(250);
  }
  // The button is server-rendered before React attaches its handler.
  await page.waitForLoadState('networkidle');

  const requests = [];
  const bodies = [reservationBody(0), reservationBody(10 * 60_000)];
  await page.route('**/api/commerce/reservations', async (route) => {
    const request = route.request();
    if (request.method() !== 'POST') return route.continue();
    requests.push({ key: request.headers()['idempotency-key'] ?? '', body: request.postData() ?? '' });
    const body = bodies[Math.min(requests.length - 1, bodies.length - 1)];
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
  });

  const reserve = page.getByRole('button', { name: 'Reserve', exact: true });
  await reserve.click();
  await page.getByText('Hold expired').waitFor();
  check('a dead-on-arrival reservation renders as expired', true);
  check('a dead-on-arrival reservation renders no checkout form', (await page.locator('.checkout-form').count()) === 0);
  check('a dead-on-arrival reservation never says held', (await page.getByText(/Held for/).count()) === 0);

  await reserve.click();
  await page.locator('.checkout-form').waitFor();
  check('the second reserve renders the live hold', true);

  check('two reservation requests were sent', requests.length === 2, String(requests.length));
  const [first, second] = requests;
  check('both requests carry an Idempotency-Key', Boolean(first?.key) && Boolean(second?.key));
  check('the second request asks for the same terms', first?.body === second?.body, `${first?.body} | ${second?.body}`);
  check(
    'the second request uses a DIFFERENT Idempotency-Key, so the dead hold is not replayed',
    first?.key !== second?.key,
    `${first?.key} -> ${second?.key}`,
  );
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
