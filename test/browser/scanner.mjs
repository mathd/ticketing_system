// Real-browser submission coverage for the Scanner write path.
// The unit suite controls storage failures. This spec proves that Chrome can
// pair a device and send the rendered form through the gateway.

import { createPrivateKey, randomUUID, sign } from 'node:crypto';
import { chromium } from 'playwright-core';
import { ORGANIZER, enrolScanner, resultRecorder, sql } from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const ACCESS = process.env.ACCESS_CONTAINER;
const PG = process.env.POSTGRES_CONTAINER;
const ACCESS_PORT = process.env.ACCESS_PORT;
const QR_SEED = process.env.ACCESS_QR_PRIVATE_KEY;
const QR_KID = process.env.ACCESS_QR_KID;
const INTERNAL_TOKEN = process.env.INTERNAL_SERVICE_TOKEN;
if (!ACCESS) throw new Error('ACCESS_CONTAINER is unset. Run this spec through ./scripts/browser.sh');
if (!PG || !ACCESS_PORT || !QR_SEED || !QR_KID || !INTERNAL_TOKEN) {
  throw new Error('scanner browser credentials are unset. Run this spec through ./scripts/browser.sh');
}

const recorder = resultRecorder('scanner');
const { check } = recorder;
const token = enrolScanner(ACCESS, `browser gate ${Date.now()}`);
const ticketID = randomUUID();
const orderID = randomUUID();
const guestRef = randomUUID();
const buyerID = randomUUID();
const slotID = randomUUID();
const ticketTypeID = randomUUID();
const issuedAt = new Date();
const toBase64URL = (value) => Buffer.from(value).toString('base64url');
const header = toBase64URL(JSON.stringify({ alg: 'EdDSA', kid: QR_KID, typ: 'TKT' }));
const claims = toBase64URL(JSON.stringify({
  v: 1,
  tid: ticketID,
  oid: orderID,
  org: ORGANIZER,
  sid: slotID,
  iat: Math.floor(issuedAt.getTime() / 1000),
}));
const privateKey = createPrivateKey({
  key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(QR_SEED, 'base64')]),
  format: 'der',
  type: 'pkcs8',
});
const credentialBody = `${header}.${claims}`;
const credential = `${credentialBody}.${sign(null, Buffer.from(credentialBody), privateKey).toString('base64url')}`;

// Seed the ticket row, then use Access's refund route so the voided lifecycle fact
// still goes through appendLifecycle.
sql(
  PG,
  'access',
  `INSERT INTO tickets
     (id, order_id, guest_order_ref, organizer_id, buyer_id, slot_id, ticket_type_id, qr_payload, issued_at)
   VALUES
     ('${ticketID}', '${orderID}', '${guestRef}', '${ORGANIZER}', '${buyerID}', '${slotID}',
      '${ticketTypeID}', '${credential}', '${issuedAt.toISOString()}');`,
);
const refundResponse = await fetch(`http://localhost:${ACCESS_PORT}/internal/orders/${orderID}/refunds`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', 'X-Internal-Token': INTERNAL_TOKEN },
  body: JSON.stringify({ organizer_id: ORGANIZER, refund_id: randomUUID(), quantity: 1 }),
});
if (!refundResponse.ok) throw new Error(`Access refund returned ${refundResponse.status}: ${await refundResponse.text()}`);
const voidedEventCount = sql(
  PG,
  'access',
  `SELECT count(*) FROM lifecycle_events WHERE ticket_id='${ticketID}' AND event_type='refunded'`,
);

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  const scanRequests = [];
  const reconcileRequests = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans') {
      scanRequests.push(request);
    }
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans/reconciliations') {
      reconcileRequests.push(request);
    }
  });

  await page.goto('/scanner/', { waitUntil: 'domcontentloaded' });
  await page.fill('#pairing-token', token);
  await page.click('button[type=submit]');
  await page.getByLabel('Ticket credential').waitFor();
  check('an enrolled device can pair through the rendered form', await page.getByLabel('Ticket credential').isVisible());

  const feedResponse = page.waitForResponse((response) =>
    new URL(response.url()).pathname === '/api/access/scans/voided-tickets');
  await page.getByRole('button', { name: 'Refresh revocation list' }).click();
  const pulled = await (await feedResponse).json();
  check('the completed feed lists the voided ticket', pulled.ticket_ids.includes(ticketID));

  await context.setOffline(true);
  await page.getByLabel('Ticket credential').fill(credential);
  const offlineReconcile = page.waitForRequest((request) =>
    request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans/reconciliations');
  await page.getByRole('button', { name: 'Check ticket' }).click();
  const offlineRequest = await offlineReconcile;
  await page.getByRole('heading', { name: 'Not valid for entry' }).waitFor();
  check(
    'the browser shows the local refusal and tells staff not to admit',
    (await page.getByRole('alert').innerText()).includes("This ticket is on the device's revocation list. Do not admit."),
  );
  check('a listed ticket makes no live scan request', scanRequests.length === 0, `observed ${scanRequests.length}`);
  await page.waitForFunction(() => document.querySelector('.queue-note')?.textContent?.includes('1 queued offline scan'));
  check('the offline refusal attempted best-effort reconciliation', reconcileRequests.length === 1, `observed ${reconcileRequests.length}`);

  const syncResponse = page.waitForResponse((response) =>
    new URL(response.url()).pathname === '/api/access/scans/reconciliations' && response.status() === 200);
  await context.setOffline(false);
  await syncResponse;
  await page.getByText(/Synced 1 offline scan/).waitFor();
  const refusal = offlineRequest.postDataJSON().occurrences?.[0];
  if (!refusal) throw new Error('reconciliation did not carry the queued refusal');
  check('reconciliation carries the refusal decision', refusal.local_decision === 'revocation_refused');
  const exactRow = JSON.parse(sql(
    PG,
    'access',
    `SELECT json_build_object(
       'occurrence_id', occurrence_id::text,
       'ticket_id', ticket_id::text,
       'organizer_id', organizer_id::text,
       'occurred_at', to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
       'decision', decision
     )::text FROM scanner_local_decisions WHERE occurrence_id='${refusal.occurrence_id}'`,
  ));
  check(
    'Access stores exactly the synced refusal row',
    JSON.stringify(exactRow) === JSON.stringify({
      occurrence_id: refusal.occurrence_id,
      ticket_id: ticketID,
      organizer_id: ORGANIZER,
      occurred_at: refusal.occurred_at,
      decision: 'revocation_refused',
    }),
    JSON.stringify(exactRow),
  );
  check(
    'the refusal leaves the ticket lifecycle unchanged',
    sql(PG, 'access', `SELECT count(*) FROM lifecycle_events WHERE ticket_id='${ticketID}' AND event_type='refunded'`) === voidedEventCount,
  );

  await page.getByLabel('Ticket credential').fill('not-a-ticket');
  const requestPromise = page.waitForRequest((request) =>
    request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans');
  await page.getByRole('button', { name: 'Check ticket' }).click();
  const request = await requestPromise;
  await page.getByRole('heading', { name: 'Rejected' }).waitFor();

  const body = request.postDataJSON();
  check('the malformed credential follows the existing scan path', scanRequests.length === 1, `observed ${scanRequests.length}`);
  check('the request carries the paired device token', request.headers()['x-scanner-token'] === token);
  check('the request carries the entered credential', body.qr_payload === 'not-a-ticket');
  check(
    'the request carries a scanner-minted occurrence id',
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(body.occurrence_id),
  );
  check('the request carries a valid occurrence time', !Number.isNaN(Date.parse(body.occurred_at)));
  check('the rejected result is rendered', await page.getByRole('heading', { name: 'Rejected' }).isVisible());
  check(
    'the form is usable after the request finishes',
    !(await page.getByRole('button', { name: 'Check ticket' }).isDisabled()),
  );
} finally {
  await browser.close();
  if (!recorder.finish()) process.exitCode = 1;
}
