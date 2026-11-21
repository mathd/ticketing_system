// Real-browser coverage for the staff refund form after an operator records a
// refund as abandoned (TKT-525 S2, ADR-087, D12).
//
// The saved retry of an abandoned refund gets commerce's 409 `refund_abandoned`.
// The page must retire THAT saved key and render a blank refund form with a
// fresh key, and the new refund must go through. A generic 409 on a saved retry
// must keep the saved key (the control order).
//
// Staff handoff (R2-1): staff A's refund is ambiguous and saved; staff B of the
// same organizer submits that saved retry after the abandonment. Commerce
// compares the actor-bound fingerprint and answers a generic 409, so the page
// asks commerce's staff order detail, retires the saved retry, and B's
// replacement goes through under B's identity and a fresh key. Both staff
// members type the order UUID in uppercase; commerce stores and answers it in
// canonical lowercase, so the abandonment read compares it case-insensitively
// (R3-2).
//
// Run through ./scripts/browser.sh (make browser) with host Chrome.

import { createHash, randomUUID } from 'node:crypto';
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
const identifier = `refund-abandoned-${stamp}@example.test`;
const identifierB = `refund-abandoned-b-${stamp}@example.test`;
const password = 'correct horse battery staple';
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// payments' RefundLegKey (services/payments/internal/store/refund_legs.go): the leg's
// provider idempotency key, derived exactly as the store derives it.
function refundLegKey(org, sourceKey, refundKey) {
  return `psp-refund-leg-v1:${createHash('sha256').update(`${org}\0${sourceKey}\0${refundKey}`).digest('hex')}`;
}

// A paid, completed commerce order with NO payments operation behind it yet:
// payments answers its first refund with 404, which commerce reports as 502, and
// the page saves the request as an unresolved retry.
function seedPaidOrder(label, quantity, unit) {
  const o = {
    reservation: randomUUID(),
    order: randomUUID(),
    ref: randomUUID(),
    buyer: randomUUID(),
    chargeKey: `browser-${label}-${stamp}`,
    quantity,
    unit,
  };
  sql(
    PG,
    'commerce',
    `INSERT INTO reservations
       (id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
        unit_amount, total_amount, face_value_amount, currency, status)
     VALUES
       ('${o.reservation}', '${ORGANIZER}', '${randomUUID()}', '${randomUUID()}',
        '${randomUUID()}', '${o.buyer}', ${quantity}, ${unit}, ${quantity * unit}, ${quantity * unit}, 'EUR', 'completed');
     INSERT INTO orders
       (id, reservation_id, status, idempotency_key, request_fingerprint, guest_order_ref)
     VALUES
       ('${o.order}', '${o.reservation}', 'completed', '${o.chargeKey}', '${o.chargeKey}', '${o.ref}');`,
  );
  return o;
}

// The captured charge behind an order, as payments records it after checkout.
function seedCapturedOperation(o, captured) {
  sql(
    PG,
    'payments',
    `INSERT INTO payment_operations
       (organizer_id, idempotency_key, request_fingerprint, status, order_id, buyer_id,
        request_amount, request_currency, provider_payment_ref, provider_state,
        authorized_amount, captured_amount, provider_state_at)
     VALUES
       ('${ORGANIZER}', '${o.chargeKey}', 'browser', 'captured', '${o.order}', '${o.buyer}',
        ${o.quantity * o.unit}, 'EUR', 'pi_${o.chargeKey}', 'captured', ${captured}, ${captured}, now());`,
  );
}

// What the operator command leaves behind (record-refund-outcome ... abandoned):
// payments holds the captured charge, the old leg still bound, and its marker;
// commerce holds the abandoned outcome. Seeded by SQL because this spec is about
// the Back Office; the command itself is covered against real PostgreSQL.
function seedAbandonment(o, refund) {
  seedCapturedOperation(o, o.quantity * o.unit);
  sql(
    PG,
    'payments',
    `INSERT INTO payment_refund_legs
       (organizer_id, source_idempotency_key, refund_idempotency_key, provider_idempotency_key, amount, currency)
     VALUES
       ('${ORGANIZER}', '${o.chargeKey}', '${refund}',
        '${refundLegKey(ORGANIZER, o.chargeKey, refund)}', ${o.unit}, 'EUR');
     INSERT INTO payment_refund_leg_abandonments
       (organizer_id, source_idempotency_key, refund_idempotency_key, provider_ref)
     VALUES
       ('${ORGANIZER}', '${o.chargeKey}', '${refund}', 're_browser_failed_${refund.slice(0, 8)}');`,
  );
  sql(
    PG,
    'commerce',
    `INSERT INTO refund_outcomes
       (organizer_id, subject_kind, subject_id, kind, amount, currency, external_ref,
        operator_name, session_user_name, reason)
     VALUES
       ('${ORGANIZER}', 'order_refund', '${refund}', 'abandoned', ${o.unit}, 'EUR',
        're_browser_failed_${refund.slice(0, 8)}', 'Browser Operator', session_user, 'nobody will refund this attempt');`,
  );
}

const staffId = (who) =>
  sql(PG, 'catalog', `SELECT id FROM staff_accounts WHERE identifier_key=lower('${who}')`);

const refundRows = (order) =>
  sql(PG, 'commerce', `SELECT count(*) FROM order_refunds WHERE order_id='${order}'`);

const { check, finish } = resultRecorder('refund-abandoned browser spec');
provisionAdmin(CATALOG, identifier, password);
provisionAdmin(CATALOG, identifierB, password);
const abandonedOrder = seedPaidOrder('abandoned', 2, 1250);
const controlOrder = seedPaidOrder('control', 1, 1250);
const handoffOrder = seedPaidOrder('handoff', 2, 1250);
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await signIn(page, identifier, password);

  const freshForm = () => page.locator('form.refund:has(h2:text("Refund this order"))');
  const retryForm = () => page.locator('form.refund:has(h2:text("Retry this refund"))');

  // Submit a fresh refund that payments cannot resolve, so the page saves it.
  async function saveUnresolvedRefund(o, label, typedOrder = o.order) {
    await page.goto(PATH, { waitUntil: 'domcontentloaded' });
    await page.fill('#order_id', typedOrder);
    await submitForm(page, page.getByRole('button', { name: 'Look it up' }));
    const form = freshForm();
    check(`${label}: the completed order renders the refund form`, (await form.count()) === 1);
    const key = await form.locator('input[name="idempotency_key"]').inputValue();
    await form.locator('#quantity').fill('1');
    await form.locator('#reason').fill(`browser ${label} first attempt`);
    const submitted = await submitForm(page, form.getByRole('button', { name: 'Refund' }));
    check(`${label}: the refund form posts to the order console`, new URL(submitted.url()).pathname === PATH, submitted.url());
    const saved = await retryForm().locator('input[name="idempotency_key"]').inputValue();
    check(`${label}: the unresolved refund is saved as a retry under the submitted key`, saved === key, `${saved} vs ${key}`);
    check(`${label}: the unresolved attempt bound one pending refund`, refundRows(o.order) === '1');
    return key;
  }

  // ---- The abandoned refund.
  const savedKey = await saveUnresolvedRefund(abandonedOrder, 'abandoned');
  const abandonedRefund = sql(PG, 'commerce', `SELECT id FROM order_refunds WHERE order_id='${abandonedOrder.order}'`);
  check('the saved refund has a commerce refund id', UUID.test(abandonedRefund), abandonedRefund);

  seedAbandonment(abandonedOrder, abandonedRefund);

  // Submit the saved retry: commerce answers 409 refund_abandoned.
  let submitted = await submitForm(page, retryForm().getByRole('button', { name: 'Retry the same refund' }));
  check(
    'the saved retry submits its saved key',
    new URLSearchParams(submitted.postData() ?? '').get('idempotency_key') === savedKey,
  );
  const response = await submitted.response();
  check('the abandoned retry answers 200', response?.status() === 200, String(response?.status()));
  const alert = (await page.getByRole('alert').innerText()).trim();
  check('the page says the refund was recorded as abandoned', alert.includes('An operator recorded this refund as abandoned.'), alert);
  check('the page does not claim nothing reached the provider', !alert.includes('Nothing was sent'), alert);
  check('the page does not call it ambiguous', !alert.includes('may or may not have been applied'), alert);
  check('the saved retry is retired', (await retryForm().count()) === 0);
  check('a fresh refund form is offered', (await freshForm().count()) === 1);
  const freshKey = await freshForm().locator('input[name="idempotency_key"]').inputValue();
  check('the fresh form carries a new server-minted key', UUID.test(freshKey) && freshKey !== savedKey, freshKey);
  check('the fresh form is blank: quantity', (await freshForm().locator('#quantity').inputValue()) === '');
  check('the fresh form is blank: reason', (await freshForm().locator('#reason').inputValue()) === '');
  check(
    'the abandoned retry moved nothing in payments',
    sql(PG, 'payments', `SELECT count(*) || '|' || string_agg(status, ',') FROM payment_refund_legs WHERE organizer_id='${ORGANIZER}' AND source_idempotency_key='${abandonedOrder.chargeKey}'`) === '1|bound',
  );
  check(
    'the abandoned refund stays pending',
    sql(PG, 'commerce', `SELECT status FROM order_refunds WHERE id='${abandonedRefund}'`) === 'pending',
  );

  // The deliberate replacement, under the new key. The abandoned unit is released
  // in both ceilings, and the stack's fake PSP refunds the new leg.
  await freshForm().locator('#quantity').fill('1');
  await freshForm().locator('#reason').fill('browser replacement after abandonment');
  submitted = await submitForm(page, freshForm().getByRole('button', { name: 'Refund' }));
  check(
    'the replacement submits the fresh key',
    new URLSearchParams(submitted.postData() ?? '').get('idempotency_key') === freshKey,
  );
  const result = (await page.locator('.refund-result').innerText()).trim();
  check('the replacement shows commerce\'s refund', result.includes('Commerce refunded 1 ticket(s)') && result.includes('1250 EUR'), result);
  check(
    'the replacement persisted one completed refund for one ticket',
    sql(
      PG,
      'commerce',
      `SELECT status || '|' || quantity::text || '|' || amount::text || '|' || currency
       FROM order_refunds WHERE order_id='${abandonedOrder.order}' AND id <> '${abandonedRefund}'`,
    ) === 'completed|1|1250|EUR',
  );
  check(
    'the order projection counts only the replacement',
    sql(
      PG,
      'commerce',
      `SELECT refund_status || '|' || refunded_quantity::text || '|' || refunded_amount::text
       FROM orders WHERE id='${abandonedOrder.order}'`,
    ) === 'partial|1|1250',
  );
  check(
    'payments completed the new leg and kept the abandoned one bound',
    sql(
      PG,
      'payments',
      `SELECT string_agg(status, ',' ORDER BY status) FROM payment_refund_legs
       WHERE organizer_id='${ORGANIZER}' AND source_idempotency_key='${abandonedOrder.chargeKey}'`,
    ) === 'bound,refunded',
  );

  // ---- Staff handoff (R2-1). Staff A saves an unresolved refund, typing the
  // order UUID in uppercase (R3-2).
  const handoffTyped = handoffOrder.order.toUpperCase();
  check('handoff: the typed order UUID differs from commerce\'s lowercase form', handoffTyped !== handoffOrder.order, handoffTyped);
  const handoffKey = await saveUnresolvedRefund(handoffOrder, 'handoff', handoffTyped);
  const handoffRefund = sql(PG, 'commerce', `SELECT id FROM order_refunds WHERE order_id='${handoffOrder.order}'`);
  const staffA = staffId(identifier);
  const staffB = staffId(identifierB);
  check('handoff: two distinct staff members', UUID.test(staffA) && UUID.test(staffB) && staffA !== staffB, `${staffA} ${staffB}`);

  // Staff B, in a second session, looks the order up before the abandonment:
  // A's saved retry is offered, because it is the order's and not A's.
  const contextB = await browser.newContext({ baseURL: BASE });
  const pageB = await contextB.newPage();
  await signIn(pageB, identifierB, password);
  const freshFormB = () => pageB.locator('form.refund:has(h2:text("Refund this order"))');
  const retryFormB = () => pageB.locator('form.refund:has(h2:text("Retry this refund"))');
  await pageB.goto(PATH, { waitUntil: 'domcontentloaded' });
  await pageB.fill('#order_id', handoffTyped);
  await submitForm(pageB, pageB.getByRole('button', { name: 'Look it up' }));
  check('handoff: staff B is offered the saved retry', (await retryFormB().count()) === 1);
  check(
    'handoff: the saved retry carries staff A\'s key',
    (await retryFormB().locator('input[name="idempotency_key"]').inputValue()) === handoffKey,
  );

  // The operator abandons A's refund. Staff B then submits the saved retry.
  seedAbandonment(handoffOrder, handoffRefund);
  submitted = await submitForm(pageB, retryFormB().getByRole('button', { name: 'Retry the same refund' }));
  check(
    'handoff: staff B\'s retry sends the saved key',
    new URLSearchParams(submitted.postData() ?? '').get('idempotency_key') === handoffKey,
  );
  check(
    'handoff: staff B\'s retry sends the uppercase order UUID',
    new URLSearchParams(submitted.postData() ?? '').get('order_id') === handoffTyped,
  );
  const handoffStatus = (await pageB.getByRole('status').innerText()).trim();
  check('handoff: the page says the refund was abandoned', handoffStatus.includes('abandoned'), handoffStatus);
  check('handoff: the saved retry is retired', (await retryFormB().count()) === 0);
  check('handoff: a fresh refund form is offered to staff B', (await freshFormB().count()) === 1);
  const freshKeyB = await freshFormB().locator('input[name="idempotency_key"]').inputValue();
  check('handoff: the fresh form carries a new key', UUID.test(freshKeyB) && freshKeyB !== handoffKey, freshKeyB);
  check('handoff: the fresh form is blank: quantity', (await freshFormB().locator('#quantity').inputValue()) === '');
  check('handoff: the fresh form is blank: reason', (await freshFormB().locator('#reason').inputValue()) === '');
  check(
    'handoff: the abandoned refund keeps staff A\'s original request and stays pending',
    sql(PG, 'commerce', `SELECT actor || '|' || idempotency_key || '|' || status FROM order_refunds WHERE id='${handoffRefund}'`) ===
      `${staffA}|${handoffKey}|pending`,
  );

  // Staff B's replacement goes through under B's identity and the fresh key.
  await freshFormB().locator('#quantity').fill('1');
  await freshFormB().locator('#reason').fill('browser handoff replacement');
  submitted = await submitForm(pageB, freshFormB().getByRole('button', { name: 'Refund' }));
  check(
    'handoff: the replacement submits the fresh key',
    new URLSearchParams(submitted.postData() ?? '').get('idempotency_key') === freshKeyB,
  );
  const handoffResult = (await pageB.locator('.refund-result').innerText()).trim();
  check('handoff: the replacement shows commerce\'s refund', handoffResult.includes('Commerce refunded 1 ticket(s)'), handoffResult);
  check(
    'handoff: the replacement is staff B\'s, under the fresh key, completed',
    sql(
      PG,
      'commerce',
      `SELECT actor || '|' || idempotency_key || '|' || status || '|' || quantity::text
       FROM order_refunds WHERE order_id='${handoffOrder.order}' AND id <> '${handoffRefund}'`,
    ) === `${staffB}|${freshKeyB}|completed|1`,
  );
  await contextB.close();

  // ---- The control: a generic 409 on a saved retry keeps the saved key.
  const controlKey = await saveUnresolvedRefund(controlOrder, 'control');
  // A captured amount below the refund: payments refuses the leg with an ordinary
  // 409, which commerce passes on as a generic 409 with no code.
  seedCapturedOperation(controlOrder, 1);
  await submitForm(page, retryForm().getByRole('button', { name: 'Retry the same refund' }));
  const controlAlert = (await page.getByRole('alert').innerText()).trim();
  check('control: the generic refusal is shown', controlAlert.includes('refund unresolved'), controlAlert);
  check('control: the saved retry is still offered', (await retryForm().count()) === 1);
  check(
    'control: the saved retry keeps its key',
    (await retryForm().locator('input[name="idempotency_key"]').inputValue()) === controlKey,
  );
  check('control: no fresh refund form is offered', (await freshForm().count()) === 0);
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
