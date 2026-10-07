// Real-browser coverage for the order lookup and refund forms.

import { randomUUID } from 'node:crypto';
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

const PATH = '/admin/orders';
const stamp = Date.now();
const identifier = `order-console-${stamp}@example.test`;
const password = 'correct horse battery staple';
const reservationId = randomUUID();
const orderId = randomUUID();
const guestRef = randomUUID();

provisionAdmin(CATALOG, identifier, password);
sql(
  PG,
  'commerce',
  `INSERT INTO reservations
     (id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
      unit_amount, total_amount, face_value_amount, currency, status)
   VALUES
     ('${reservationId}', '${ORGANIZER}', '${randomUUID()}', '${randomUUID()}',
      '${randomUUID()}', '${randomUUID()}', 2, 1250, 2500, 2500, 'EUR', 'completed');
   INSERT INTO orders
     (id, reservation_id, status, idempotency_key, request_fingerprint, guest_order_ref)
   VALUES
     ('${orderId}', '${reservationId}', 'completed', 'browser-${stamp}',
      'browser-${stamp}', '${guestRef}');`,
);

// TKT-287: two zero-price completed orders. A refund of one reaches commerce's
// money check only AFTER the guard has accepted the staff credential AND the
// session's organizer assertion, and is then refused for having no captured money
// (409) — so that exact refusal proves the browser submit carried the assertion
// through the SSR layer to commerce. The other order belongs to ANOTHER organizer:
// commerce must answer it as not found (404). Neither writes a refund row.
const ownZero = { reservation: randomUUID(), order: randomUUID(), ref: randomUUID() };
const foreignZero = { reservation: randomUUID(), order: randomUUID(), ref: randomUUID() };
const OTHER_ORGANIZER = randomUUID();
for (const [o, organizer] of [[ownZero, ORGANIZER], [foreignZero, OTHER_ORGANIZER]]) {
  sql(
    PG,
    'commerce',
    `INSERT INTO reservations
       (id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
        unit_amount, total_amount, face_value_amount, currency, status)
     VALUES
       ('${o.reservation}', '${organizer}', '${randomUUID()}', '${randomUUID()}',
        '${randomUUID()}', '${randomUUID()}', 1, 0, 0, 0, 'EUR', 'completed');
     INSERT INTO orders
       (id, reservation_id, status, idempotency_key, request_fingerprint, guest_order_ref)
     VALUES
       ('${o.order}', '${o.reservation}', 'completed', 'browser-${o.order}',
        'browser-${o.order}', '${o.ref}');`,
  );
}

const { check, finish } = resultRecorder('order-console browser spec');
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await signIn(page, identifier, password);

  await page.goto(PATH, { waitUntil: 'domcontentloaded' });
  await page.fill('#order_id', orderId);
  let request = await submitForm(page, page.getByRole('button', { name: 'Look it up' }));
  check('the lookup form posts to the order console', new URL(request.url()).pathname === PATH, request.url());
  check(
    'the lookup renders commerce status for the seeded order',
    (await page.locator('.result').innerText()).includes('completed'),
  );

  const refundForm = page.locator('form.refund:has(h2:text("Refund this order"))');
  check('a completed order renders the refund form', (await refundForm.count()) === 1);
  const key = await refundForm.locator('input[name="idempotency_key"]').inputValue();
  check('the rendered refund form carries a server-minted key', /^[0-9a-f-]{36}$/i.test(key), key);

  await refundForm.locator('#quantity').fill('0');
  await refundForm.locator('#reason').fill('browser refusal proof');
  request = await submitForm(page, refundForm.getByRole('button', { name: 'Refund' }));
  check('the refund form posts to the order console', new URL(request.url()).pathname === PATH, request.url());
  const alert = await page.getByRole('alert').innerText();
  check(
    'the invalid quantity gets the exact refund refusal',
    alert.trim() === 'Quantity must be a whole number between 1 and 50.',
    alert.trim(),
  );
  check('the refused refund keeps the submitted quantity', (await page.locator('#quantity').inputValue()) === '0');
  check('the refused refund keeps the submitted reason', (await page.locator('#reason').inputValue()) === 'browser refusal proof');
  check(
    'the refused submit wrote no refund row',
    sql(PG, 'commerce', `SELECT count(*) FROM order_refunds WHERE order_id='${orderId}'`) === '0',
  );
  check(
    'the order refund projection stayed untouched',
    sql(
      PG,
      'commerce',
      `SELECT refund_status || '|' || refunded_quantity::text || '|' || refunded_amount::text
       FROM orders WHERE id='${orderId}'`,
    ) === 'none|0|0',
  );

  // TKT-287: a valid refund of the signed-in organizer's own zero-price order.
  for (const [label, target, wantStatus, wantAlert] of [
    ['own', ownZero.order, 200, 'order has no captured money to refund'],
    ['another organizer\'s', foreignZero.order, 404, 'not found'],
  ]) {
    await page.goto(PATH, { waitUntil: 'domcontentloaded' });
    await page.fill('#order_id', target);
    await submitForm(page, page.getByRole('button', { name: 'Look it up' }));
    const form = page.locator('form.refund:has(h2:text("Refund this order"))');
    check(`${label} zero-price order renders the refund form`, (await form.count()) === 1);
    await form.locator('#quantity').fill('1');
    await form.locator('#reason').fill(`browser tenancy proof (${label})`);
    // The assertion is a bearer credential held server-side (ADR-058): it must
    // appear neither in the rendered page nor in what the browser submits.
    const decoded = (x) => {
      try {
        return decodeURIComponent(x);
      } catch {
        return x; // a stray % in markup is not a URL; check it as written
      }
    };
    const html = await page.content();
    const actions = await page.locator('form').evaluateAll((forms) => forms.map((f) => f.action));
    check(
      `the ${label} order page renders no organizer assertion, in markup or any form action`,
      ![html, decoded(html), ...actions.map(decoded)].some((x) =>
        x.includes('catalog-org/'),
      ),
    );
    const submitted = await submitForm(page, form.getByRole('button', { name: 'Refund' }));
    check(
      `the ${label} refund submit carries no organizer assertion`,
      !decoded(submitted.url()).includes('catalog-org') &&
        !decoded(submitted.postData() ?? '').includes('catalog-org') &&
        !Object.keys(submitted.headers()).some((h) => h.toLowerCase() === 'x-catalog-organizer-assertion'),
    );
    const response = await submitted.response();
    check(
      `the refund of the ${label} order answers ${wantStatus}`,
      response?.status() === wantStatus,
      String(response?.status()),
    );
    const text = (await page.getByRole('alert').innerText()).trim();
    check(`the refund of the ${label} order shows commerce's exact refusal`, text.includes(wantAlert), text);
    check(
      `the refund of the ${label} order wrote no refund row`,
      sql(PG, 'commerce', `SELECT count(*) FROM order_refunds WHERE order_id='${target}'`) === '0',
    );
  }
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
