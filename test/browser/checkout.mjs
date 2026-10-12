// Real-browser coverage for the buyer journey the service smoke suite cannot see:
// storefront reservation and checkout, ticket rendering, scanner pairing, and
// accepted and duplicate scans.

import { chromium } from 'playwright-core';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  ORGANIZER,
  enrolScanner,
  provisionAdmin,
  resultRecorder,
  signIn,
  submitForm,
} from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const ACCESS = process.env.ACCESS_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!ACCESS) throw new Error('ACCESS_CONTAINER is unset. Run this spec through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset. Run this spec through ./scripts/browser.sh');

const ADMIN_PATH = '/admin/events/new';
const VENUE = '00000000-0000-0000-0000-0000000000a1';
const FIXTURE_CAPACITY = 2;
const stamp = Date.now();
const identifier = `checkout-${stamp}@example.test`;
const password = 'correct horse battery staple';
const eventName = `Browser checkout ${stamp}`;
const startsAt = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();

const recorder = resultRecorder('checkout browser spec');
const { check } = recorder;

provisionAdmin(CATALOG, identifier, password);
const scannerToken = enrolScanner(ACCESS, `checkout browser ${stamp}`);

async function saveVenueCapacity(page, capacity, assertion) {
  const venuePath = `/admin/venues/${VENUE}`;
  await page.goto(venuePath, { waitUntil: 'domcontentloaded' });
  const gaForm = page.locator('form:has(input[value="set-ga"])');
  await gaForm.locator('input[name="ga_capacity"]').fill(String(capacity));
  const capacityRequest = await submitForm(
    page,
    gaForm.getByRole('button', { name: 'Save GA capacity' }),
  );
  const storedCapacity = await gaForm.locator('input[name="ga_capacity"]').inputValue();
  const saved = new URL(capacityRequest.url()).pathname === venuePath && storedCapacity === String(capacity);
  check(assertion, saved, `${capacity}`);
  if (!saved) {
    throw new Error(
      `venue capacity save did not persist ${capacity}: POST ${capacityRequest.url()}, rendered ${storedCapacity}`,
    );
  }
}

async function publishOffer(page) {
  await signIn(page, identifier, password);

  const venuePath = `/admin/venues/${VENUE}`;
  await page.goto(venuePath, { waitUntil: 'domcontentloaded' });
  const originalCapacity = Number(
    await page.locator('form:has(input[value="set-ga"]) input[name="ga_capacity"]').inputValue(),
  );
  if (!Number.isSafeInteger(originalCapacity) || originalCapacity < 1) {
    throw new Error(`the shared browser venue has invalid GA capacity ${originalCapacity}`);
  }

  try {
    await saveVenueCapacity(
      page,
      FIXTURE_CAPACITY,
      'the checkout fixture sets its own venue capacity through the browser',
    );

    await page.goto(ADMIN_PATH, { waitUntil: 'domcontentloaded' });

    await page.fill('#name_en', eventName);
    await page.fill('#name_fr', `Paiement navigateur ${stamp}`);
    await submitForm(page, page.getByRole('button', { name: 'Create event' }));
    const eventId = new URL(page.url()).searchParams.get('event');
    if (!eventId) throw new Error(`event creation did not return an event id: ${page.url()}`);

    await page.selectOption('#venue_id', VENUE);
    await page.fill('#starts_at', startsAt);
    await page.fill('#timezone', 'UTC');
    await submitForm(page, page.getByRole('button', { name: 'Add the date' }));
    const performanceId = new URL(page.url()).searchParams.get('performance');
    if (!performanceId) throw new Error(`performance creation did not return an id: ${page.url()}`);

    await page.fill('#tt_name_en', 'General admission');
    await page.fill('#tt_name_fr', 'Admission générale');
    await page.fill('#amount', '1250');
    await page.fill('#currency', 'EUR');
    await submitForm(page, page.getByRole('button', { name: 'Set the price' }));

    await submitForm(page, page.getByRole('button', { name: 'Publish' }));
    await page.getByRole('heading', { name: 'Publication accepted' }).waitFor();
    return { eventId, performanceId };
  } finally {
    await saveVenueCapacity(
      page,
      originalCapacity,
      'the checkout fixture restores the shared venue capacity',
    );
  }
}

async function readAvailability(page, performanceId) {
  const path = `/api/inventory/slots/${performanceId}/availability?organizer_id=${ORGANIZER}`;
  const response = await page.request.get(path);
  if (!response.ok()) {
    throw new Error(`inventory availability returned ${response.status()}: ${await response.text()}`);
  }
  const body = await response.json();
  for (const field of ['capacity', 'held', 'confirmed', 'available']) {
    if (!Number.isSafeInteger(body[field])) {
      throw new Error(`inventory availability has invalid ${field}: ${JSON.stringify(body)}`);
    }
  }
  return body;
}

async function waitForAvailability(page, performanceId, expected) {
  const deadline = Date.now() + 20_000;
  let observed;
  while (Date.now() < deadline) {
    try {
      observed = await readAvailability(page, performanceId);
      if (Object.entries(expected).every(([field, value]) => observed[field] === value)) return observed;
    } catch (error) {
      observed = error instanceof Error ? error.message : String(error);
    }
    await page.waitForTimeout(250);
  }
  throw new Error(
    `inventory did not reach ${JSON.stringify(expected)} within 20 seconds; last observed ${JSON.stringify(observed)}`,
  );
}

async function waitForOffer(page, eventId, performanceId) {
  const deadline = Date.now() + 20_000;
  let observed;
  while (Date.now() < deadline) {
    try {
      observed = await readAvailability(page, performanceId);
    } catch (error) {
      observed = error instanceof Error ? error.message : String(error);
    }
    const event = await page.goto(`/en/events/${eventId}`, { waitUntil: 'domcontentloaded' });
    if (
      event?.ok() &&
      observed?.capacity === FIXTURE_CAPACITY &&
      observed?.held === 0 &&
      observed?.confirmed === 0 &&
      observed?.available === FIXTURE_CAPACITY &&
      observed?.offering_status === 'open' &&
      await page.getByRole('button', { name: 'Reserve', exact: true }).count()
    ) {
      return;
    }
    await page.waitForTimeout(250);
  }
  throw new Error(
    `published offer ${eventId} did not become available with capacity ${FIXTURE_CAPACITY} within 20 seconds; ` +
    `last observed ${JSON.stringify(observed)}`,
  );
}

async function reserve(page, scenario) {
  // The button is server-rendered before React attaches its click handler. Waiting
  // only for DOMContentLoaded can click inert markup, especially after returning
  // from Scanner, and then misreport the missing checkout form as a capacity issue.
  await page.waitForLoadState('networkidle');
  const reservationResponse = page.waitForResponse((response) =>
    response.request().method() === 'POST' &&
    new URL(response.url()).pathname === '/api/commerce/reservations');
  await page.getByRole('button', { name: 'Reserve', exact: true }).click();
  const response = await reservationResponse;
  const body = await response.text();
  if (!response.ok()) {
    throw new Error(`${scenario} reservation returned ${response.status()}: ${body}`);
  }
  try {
    await page.locator('.checkout-form').waitFor();
  } catch {
    throw new Error(`${scenario} reservation returned ${response.status()} but rendered no checkout form: ${body}`);
  }
  check(
    `${scenario} reservation posts through the storefront bridge`,
    new URL(response.url()).pathname === '/api/commerce/reservations',
    response.url(),
  );
  await page.getByLabel('Name').fill('Browser Buyer');
  await page.getByLabel('Email').fill(`buyer-${stamp}@example.test`);
}

// TKT-341 / ADR-082. Click the real Reserve control and return the refusal the page
// received and rendered. A refusal renders in the picker's live status region; it never
// renders a checkout form.
async function reserveRefused(page, scenario, reserveLabel) {
  await page.waitForLoadState('networkidle');
  const reservationResponse = page.waitForResponse((response) =>
    response.request().method() === 'POST' &&
    new URL(response.url()).pathname === '/api/commerce/reservations');
  await page.getByRole('button', { name: reserveLabel, exact: true }).click();
  const response = await reservationResponse;
  const text = await response.text();
  let body = null;
  try {
    body = JSON.parse(text);
  } catch {
    // Not JSON: the checks below report it.
  }
  const status = page.locator('span[aria-live="polite"]');
  await status.filter({ hasText: /\S/ }).waitFor();
  const rendered = (await status.innerText()).trim();
  check(`${scenario}: the refusal posts through the storefront bridge`,
    new URL(response.url()).pathname === '/api/commerce/reservations', response.url());
  check(`${scenario}: no checkout form renders`, (await page.locator('.checkout-form').count()) === 0);
  check(`${scenario}: no waitlist action renders`,
    (await page.getByText(/waitlist|liste d'attente/i).count()) === 0);
  return { status: response.status(), body, text, rendered };
}

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  const { eventId, performanceId } = await publishOffer(page);
  await waitForOffer(page, eventId, performanceId);

  await reserve(page, 'successful purchase');
  await waitForAvailability(page, performanceId, {
    capacity: FIXTURE_CAPACITY,
    held: 1,
    confirmed: 0,
    available: 1,
    offering_status: 'open',
  });
  const checkoutResponse = page.waitForResponse((response) =>
    response.request().method() === 'POST' && new URL(response.url()).pathname === '/checkout');
  await page.getByRole('button', { name: /^Pay / }).click();
  const submittedCheckout = await checkoutResponse;
  const checkoutBody = await submittedCheckout.text();
  try {
    await page.getByText('Order confirmed', { exact: true }).waitFor();
  } catch {
    throw new Error(`checkout returned ${submittedCheckout.status()} without completing: ${checkoutBody}`);
  }
  check('the checkout form posts through the storefront bridge', new URL(submittedCheckout.url()).pathname === '/checkout');
  check('the browser reports a completed order', await page.getByText('Order confirmed', { exact: true }).isVisible());
  await waitForAvailability(page, performanceId, {
    capacity: FIXTURE_CAPACITY,
    held: 0,
    confirmed: 1,
    available: 1,
    offering_status: 'open',
  });

  const ticketLink = page.getByRole('link', { name: 'View my tickets', exact: true });
  const ticketPath = await ticketLink.getAttribute('href');
  if (!ticketPath) throw new Error('completed checkout rendered no ticket link');
  const orderRef = ticketPath.split('/').at(-1);
  if (!orderRef) throw new Error(`ticket link has no order reference: ${ticketPath}`);
  const qrResponsePromise = page.waitForResponse((response) =>
    response.request().resourceType() === 'image' &&
    new URL(response.url()).pathname.endsWith('/qr.png'));
  await ticketLink.click();
  await page.getByRole('heading', { name: 'My tickets', exact: true }).waitFor();
  const qrImage = page.getByRole('img', { name: 'Ticket QR code', exact: true });
  await qrImage.waitFor();
  await page.waitForFunction(
    (image) => image instanceof HTMLImageElement && image.complete && image.naturalWidth > 0,
    await qrImage.elementHandle(),
  );
  const qrResponse = await qrResponsePromise;
  check('the rendered QR image request succeeds', qrResponse.ok());
  check('the rendered QR response is a PNG', qrResponse.headers()['content-type'] === 'image/png');
  check('the issued ticket renders through its guest link', page.url().endsWith(ticketPath));

  const qrDirectory = mkdtempSync(join(tmpdir(), 'ticketing-checkout-qr-'));
  const qrPath = join(qrDirectory, 'ticket.png');
  let credential;
  try {
    writeFileSync(qrPath, await qrResponse.body());
    credential = execFileSync('zbarimg', ['--quiet', '--raw', qrPath], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim();
  } finally {
    rmSync(qrDirectory, { recursive: true, force: true });
  }
  if (!credential) throw new Error('the rendered QR image contains no scanner credential');

  await page.goto('/scanner/', { waitUntil: 'domcontentloaded' });
  await page.fill('#pairing-token', scannerToken);
  await page.click('button[type=submit]');
  await page.getByLabel('Ticket credential').waitFor();
  check('the scanner pairs before receiving a ticket', await page.getByLabel('Ticket credential').isVisible());

  await page.getByLabel('Ticket credential').fill(credential);
  await page.getByRole('button', { name: 'Check ticket', exact: true }).click();
  await page.getByRole('heading', { name: 'Accepted', exact: true }).waitFor();
  check('the purchased ticket is accepted at the gate', await page.getByRole('heading', { name: 'Accepted', exact: true }).isVisible());

  await page.getByRole('button', { name: 'Check ticket', exact: true }).click();
  await page.getByRole('heading', { name: 'Rejected', exact: true }).waitFor();
  check('a second scan is rejected as a duplicate', (await page.getByRole('alert').innerText()).includes('Already redeemed'));

  await page.goto(`/en/events/${eventId}`, { waitUntil: 'domcontentloaded' });
  await reserve(page, 'decline scenario');
  await waitForAvailability(page, performanceId, {
    capacity: FIXTURE_CAPACITY,
    held: 1,
    confirmed: 1,
    available: 0,
    offering_status: 'open',
  });
  await page.getByLabel('Fake payment').selectOption('fake-decline');
  await page.getByRole('button', { name: /^Pay / }).click();
  await page.getByText('Payment declined — try again', { exact: true }).waitFor();
  await waitForAvailability(page, performanceId, {
    capacity: FIXTURE_CAPACITY,
    held: 0,
    confirmed: 1,
    available: 1,
    offering_status: 'open',
  });
  check('a declined payment returns to a reservable state', await page.getByRole('button', { name: 'Reserve', exact: true }).isEnabled());

  // TKT-341 / ADR-082. One unit is left. Two asked for is a partial shortage: the
  // buyer can buy fewer now, so it is not a sellout.
  await page.goto(`/en/events/${eventId}`, { waitUntil: 'domcontentloaded' });
  await page.waitForLoadState('networkidle');
  await page.getByLabel('Quantity').fill('2');
  const partial = await reserveRefused(page, 'partial shortage', 'Reserve');
  check('a partial shortage is a code-less 409',
    partial.status === 409 && partial.body?.error === 'inventory unavailable' && partial.body?.code === undefined,
    `${partial.status} ${partial.text}`);
  check('a partial shortage renders the neutral refusal', partial.rendered === 'Quantity unavailable', partial.rendered);

  // A live hold takes the last unit. The pool is held out, not sold: that is a
  // sellout at the locked decision (point-in-time, ADR-082).
  await page.goto(`/en/events/${eventId}`, { waitUntil: 'domcontentloaded' });
  await reserve(page, 'exhausting hold');
  await waitForAvailability(page, performanceId, {
    capacity: FIXTURE_CAPACITY,
    held: 1,
    confirmed: 1,
    available: 0,
    offering_status: 'open',
  });

  // A different buyer, in a fresh browser context, in each locale.
  for (const [locale, reserveLabel, soldOut] of [['en', 'Reserve', 'Sold out'], ['fr', 'Réserver', 'Complet']]) {
    const buyer = await browser.newContext({ baseURL: BASE });
    try {
      const fresh = await buyer.newPage();
      await fresh.goto(`/${locale}/events/${eventId}`, { waitUntil: 'domcontentloaded' });
      const refused = await reserveRefused(fresh, `sellout (${locale})`, reserveLabel);
      check(`an exhausted pool answers 409 sold_out (${locale})`,
        refused.status === 409 && refused.body?.code === 'sold_out' && refused.body?.error === 'sold out',
        `${refused.status} ${refused.text}`);
      check(`an exhausted pool renders the localized sellout (${locale})`, refused.rendered === soldOut, refused.rendered);
    } finally {
      await buyer.close();
    }
  }
} finally {
  await browser.close();
  if (!recorder.finish()) process.exitCode = 1;
}
