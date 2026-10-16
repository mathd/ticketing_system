// Real-browser coverage for the buyer journey the service smoke suite cannot see:
// storefront reservation and checkout, ticket rendering, scanner pairing, and
// accepted and duplicate scans.

import { chromium } from 'playwright-core';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { enrolScanner, provisionAdmin, resultRecorder } from './lib/support.mjs';
import { publishOffer, waitForAvailability, waitForOffer } from './lib/checkout-offer.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const ACCESS = process.env.ACCESS_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!ACCESS) throw new Error('ACCESS_CONTAINER is unset. Run this spec through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset. Run this spec through ./scripts/browser.sh');

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
  const { eventId, performanceId } = await publishOffer(page, check, {
    identifier,
    password,
    capacity: FIXTURE_CAPACITY,
    nameEn: eventName,
    nameFr: `Paiement navigateur ${stamp}`,
    startsAt,
    timezone: 'UTC',
  });
  await waitForOffer(page, { eventId, performanceId, capacity: FIXTURE_CAPACITY, locale: 'en', reserveLabel: 'Reserve' });

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
