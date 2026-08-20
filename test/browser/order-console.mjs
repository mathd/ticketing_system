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

const { check, finish } = resultRecorder('order-console browser spec');
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await signIn(page, identifier, password);
  check('admin signs in', page.url().includes('/admin'));

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
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
