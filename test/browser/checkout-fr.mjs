// TKT-365: one French purchase in real Chrome, from the French event page to the French
// ticket page, with a French decline. Its own offer, so the English spec's exact
// availability and sellout assertions are untouched.
//
// What "the rendered actions" means here: the purchase has no native <form>. Reserve and
// Pay are buttons whose handlers POST JSON, so this spec clicks the rendered controls and
// checks the POST targets the BROWSER sends (/api/commerce/reservations, /checkout), the
// rendered result, and the URLs. The ticket page's Access read happens server-side and
// is invisible to the browser, so the spec asserts what that read rendered instead.
//
// The offer's zone is Asia/Kolkata (UTC+05:30): neither UTC (the storefront container's
// zone) nor a whole-hour offset, so a history time that fell back to the server default,
// or to UTC, shows different minutes and fails.

import { chromium } from 'playwright-core';
import { provisionAdmin, resultRecorder } from './lib/support.mjs';
import { publishOffer, waitForAvailability, waitForOffer } from './lib/checkout-offer.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const CATALOG = process.env.CATALOG_CONTAINER;
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset. Run this spec through ./scripts/browser.sh');

const FIXTURE_CAPACITY = 2;
const ZONE = 'Asia/Kolkata';
const stamp = Date.now();
const identifier = `checkout-fr-${stamp}@example.test`;
const password = 'correct horse battery staple';
const nameFr = `Concert du navigateur ${stamp}`;
const startsAt = new Date(Date.now() + 90 * 24 * 60 * 60 * 1000).toISOString();

// The French table's values (web/storefront/src/lib/locales.ts), written out: this spec
// checks what the buyer reads, so it does not import the table it is checking.
const FR = {
  reserve: 'Réserver',
  pay: 'Payer',
  languageLabel: 'Langue',
  fakePaymentLabel: 'Paiement simulé',
  fakePaymentChoices: ['Accepté', 'Refusé', 'Délai dépassé'],
  testBuyerName: 'Acheteur test',
  heldFor: 'Réservé pendant',
  nameLabel: 'Nom',
  emailLabel: 'Courriel',
  orderConfirmed: 'Commande confirmée',
  viewMyTickets: 'Voir mes billets',
  myTickets: 'Mes billets',
  ticketQrAlt: 'Code QR du billet',
  historyIssued: 'Émis',
  paymentDeclined: 'Paiement refusé — réessayez',
};

// The storefront's history format (formatHistoryTime in web/storefront/src/lib/format.ts),
// applied here with the zone this spec CHOSE, to the instant Access recorded.
const HISTORY_TIME_OPTIONS = {
  year: 'numeric', month: 'short', day: 'numeric',
  hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short',
};
const historyTime = (iso, timeZone) =>
  new Intl.DateTimeFormat('fr', { ...HISTORY_TIME_OPTIONS, timeZone }).format(new Date(iso));

const recorder = resultRecorder('French checkout browser spec');
const { check } = recorder;

// One block per scenario: an exception records a FAIL and the next block still runs, so
// one broken step cannot hide whether the others hold (TKT-325).
async function scenario(name, body) {
  try {
    await body();
  } catch (error) {
    check(`${name}: completed without an exception`, false, error instanceof Error ? error.message : String(error));
  }
}

function postTo(page, path) {
  return page.waitForResponse((response) =>
    response.request().method() === 'POST' && new URL(response.url()).pathname === path);
}

async function reserveInFrench(page, eventId, label) {
  await page.goto(`/fr/events/${eventId}`, { waitUntil: 'domcontentloaded' });
  // The button is server-rendered before React attaches its click handler.
  await page.waitForLoadState('networkidle');
  const reservation = postTo(page, '/api/commerce/reservations');
  await page.getByRole('button', { name: FR.reserve, exact: true }).click();
  const response = await reservation;
  check(`${label}: Réserver posts the reservation through the storefront bridge`,
    response.status() === 201, `${response.status()} ${response.url()}`);
  await page.locator('.checkout-form').waitFor();
}

provisionAdmin(CATALOG, identifier, password);
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const admin = await browser.newContext({ baseURL: BASE });
  const adminPage = await admin.newPage();
  const { eventId, performanceId } = await publishOffer(adminPage, check, {
    identifier,
    password,
    capacity: FIXTURE_CAPACITY,
    nameEn: `Browser checkout fr ${stamp}`,
    nameFr,
    startsAt,
    timezone: ZONE,
  });
  await waitForOffer(adminPage, { eventId, performanceId, capacity: FIXTURE_CAPACITY, locale: 'fr', reserveLabel: FR.reserve });
  await admin.close();

  // A guest buyer, in a fresh context with no back-office session.
  const buyer = await browser.newContext({ baseURL: BASE });
  const page = await buyer.newPage();
  const eventPath = `/fr/events/${eventId}`;

  await scenario('French event page', async () => {
    await page.goto(eventPath, { waitUntil: 'domcontentloaded' });
    check('the event page is French', (await page.locator('html').getAttribute('lang')) === 'fr');
    check('the event page shows the French event name',
      (await page.getByRole('heading', { level: 1 }).innerText()) === nameFr);
    const languages = page.getByRole('navigation', { name: FR.languageLabel, exact: true });
    check('the language switch has the French accessible label', (await languages.count()) === 1);
    check('the language switch keeps the event when it changes language',
      (await languages.getByRole('link', { name: 'English', exact: true }).getAttribute('href')) === `/en/events/${eventId}`);
  });

  let guestOrderRef = null;
  await scenario('French purchase', async () => {
    await reserveInFrench(page, eventId, 'purchase');
    const payment = page.getByRole('combobox', { name: FR.fakePaymentLabel, exact: true });
    const options = await payment.locator('option').evaluateAll((nodes) =>
      nodes.map((node) => ({ label: node.textContent, value: node.value })));
    check('the simulated payment choices are French', JSON.stringify(options.map((o) => o.label)) === JSON.stringify(FR.fakePaymentChoices),
      JSON.stringify(options));
    check('the simulated payment values are unchanged tokens',
      JSON.stringify(options.map((o) => o.value)) === JSON.stringify(['fake-ok', 'fake-decline', 'fake-timeout']));
    check('the buyer name starts as the French test default',
      (await page.getByRole('textbox', { name: FR.nameLabel, exact: true }).inputValue()) === FR.testBuyerName);
    const status = (await page.locator('span[aria-live="polite"]').innerText()).trim();
    check('the hold countdown is French, with a space before the unit',
      new RegExp(`^${FR.heldFor} \\d+\\u00a0s$`).test(status), JSON.stringify(status));

    // Money is formatted from integer minor units; the expected text comes from Intl
    // with the same decimal STRING the storefront builds (no float).
    const amount = new Intl.NumberFormat('fr', { style: 'currency', currency: 'EUR' }).format('12.50');
    const pay = page.getByRole('button', { name: `${FR.pay} ${amount}`, exact: true });
    check('the Pay control shows the French amount', (await pay.count()) === 1, amount);

    await page.getByRole('textbox', { name: FR.nameLabel, exact: true }).fill('Élise Martin');
    await page.getByRole('textbox', { name: FR.emailLabel, exact: true }).fill(`buyer-fr-${stamp}@example.test`);
    await payment.selectOption({ label: 'Accepté' });
    const checkout = postTo(page, '/checkout');
    await pay.click();
    const response = await checkout;
    const body = await response.json();
    check('Payer posts the checkout through the storefront bridge', response.status() === 200, `${response.status()}`);
    await page.getByText(FR.orderConfirmed, { exact: true }).waitFor();
    check('the confirmation is French', await page.getByText(FR.orderConfirmed, { exact: true }).isVisible());
    check('the confirmation stays on the French event page', new URL(page.url()).pathname === eventPath, page.url());
    guestOrderRef = body.guest_order_ref;
    await waitForAvailability(page, performanceId, { held: 0, confirmed: 1, available: 1 });
  });

  await scenario('French ticket page', async () => {
    if (!guestOrderRef) throw new Error('no completed purchase to open');
    const ticketPath = `/fr/tickets/${guestOrderRef}`;
    const link = page.getByRole('link', { name: FR.viewMyTickets, exact: true });
    check('the ticket link is exactly the French guest link', (await link.getAttribute('href')) === ticketPath,
      await link.getAttribute('href'));
    const qr = page.waitForResponse((response) =>
      response.request().resourceType() === 'image' && new URL(response.url()).pathname.endsWith('/qr.png'));
    await link.click();
    await page.getByRole('heading', { name: FR.myTickets, exact: true }).waitFor();
    check('the ticket page opens at the French guest link', new URL(page.url()).pathname === ticketPath, page.url());
    check('the ticket page is French', (await page.locator('html').getAttribute('lang')) === 'fr');
    check('the ticket page language switch has the French accessible label',
      (await page.getByRole('navigation', { name: FR.languageLabel, exact: true }).count()) === 1);
    const image = page.getByRole('img', { name: FR.ticketQrAlt, exact: true });
    await image.first().waitFor();
    check('the QR image loads', (await qr).ok());

    // The instant Access recorded, read through the same public route the page reads.
    const bundle = await (await page.request.get(`/api/access/orders/${guestOrderRef}/tickets`)).json();
    const [ticket] = bundle.tickets;
    check('Access sends the performance zone', ticket.performance_timezone === ZONE, ticket.performance_timezone);
    const issued = ticket.history.find((entry) => entry.type === 'issued');
    if (!issued) throw new Error(`the ticket has no issued event: ${JSON.stringify(ticket.history)}`);
    const expected = `${FR.historyIssued} — ${historyTime(issued.occurred_at, ZONE)}`;
    const line = page.locator(`li:has(time[datetime="${issued.occurred_at}"])`).first();
    const rendered = (await line.innerText()).trim();
    check('the issued history line is French and in the performance zone', rendered === expected,
      `${JSON.stringify(rendered)} want ${JSON.stringify(expected)}`);
    check('the issued time is not the UTC time',
      !rendered.includes(historyTime(issued.occurred_at, 'UTC')));
    check('no raw lifecycle type is shown',
      (await page.getByText(/^(issued|delivered)\b/).count()) === 0);
  });

  await scenario('French decline', async () => {
    await reserveInFrench(page, eventId, 'decline');
    await page.getByRole('combobox', { name: FR.fakePaymentLabel, exact: true }).selectOption({ label: 'Refusé' });
    const checkout = postTo(page, '/checkout');
    await page.getByRole('button', { name: new RegExp(`^${FR.pay} `) }).click();
    const response = await checkout;
    check('a declined Payer posts through the storefront bridge', response.status() === 402, `${response.status()}`);
    await page.getByText(FR.paymentDeclined, { exact: true }).waitFor();
    check('the decline is French', await page.getByText(FR.paymentDeclined, { exact: true }).isVisible());
    await waitForAvailability(page, performanceId, { held: 0, confirmed: 1, available: 1 });
    check('a declined payment returns to a reservable state',
      await page.getByRole('button', { name: FR.reserve, exact: true }).isEnabled());
  });
} finally {
  await browser.close();
  if (!recorder.finish()) process.exitCode = 1;
}
