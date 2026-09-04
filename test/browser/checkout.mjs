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
const stamp = Date.now();
const identifier = `checkout-${stamp}@example.test`;
const password = 'correct horse battery staple';
const eventName = `Browser checkout ${stamp}`;
const startsAt = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();

const recorder = resultRecorder('checkout browser spec');
const { check } = recorder;

provisionAdmin(CATALOG, identifier, password);
const scannerToken = enrolScanner(ACCESS, `checkout browser ${stamp}`);

async function publishOffer(page) {
  await signIn(page, identifier, password);
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
}

async function waitForOffer(page, eventId, performanceId) {
  const availability = `/api/inventory/slots/${performanceId}/availability?organizer_id=${ORGANIZER}`;
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const inventory = await page.request.get(availability);
    const event = await page.goto(`/en/events/${eventId}`, { waitUntil: 'domcontentloaded' });
    if (inventory.ok() && event?.ok() && await page.getByRole('button', { name: 'Reserve', exact: true }).count()) {
      return;
    }
    await page.waitForTimeout(250);
  }
  throw new Error(`published offer ${eventId} did not become available within 20 seconds`);
}

async function reserve(page) {
  await page.getByRole('button', { name: 'Reserve', exact: true }).click();
  await page.locator('.checkout-form').waitFor();
  await page.getByLabel('Name').fill('Browser Buyer');
  await page.getByLabel('Email').fill(`buyer-${stamp}@example.test`);
}

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  const { eventId, performanceId } = await publishOffer(page);
  await waitForOffer(page, eventId, performanceId);

  await reserve(page);
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
  await reserve(page);
  await page.getByLabel('Fake payment').selectOption('fake-decline');
  await page.getByRole('button', { name: /^Pay / }).click();
  await page.getByText('Payment declined — try again', { exact: true }).waitFor();
  check('a declined payment returns to a reservable state', await page.getByRole('button', { name: 'Reserve', exact: true }).isEnabled());
} finally {
  await browser.close();
  if (!recorder.finish()) process.exitCode = 1;
}
