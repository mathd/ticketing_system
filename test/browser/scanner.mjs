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
const lifecycleTypesBeforeRefusal = sql(
  PG,
  'access',
  `SELECT COALESCE(json_agg(event_type ORDER BY occurred_at, id), '[]'::json)::text
     FROM lifecycle_events WHERE ticket_id='${ticketID}'`,
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

  // TKT-315: a failed sync SAYS so, and leaves the queue exactly as it was. The offline
  // best-effort attempt above could not reach anything: that is the "No connection" note.
  await page.getByText(/^Sync failed: No connection\. 1 scan is saved on this device/).waitFor();
  check('an offline sync attempt tells the operator it failed', true);
  const queuedRows = () => page.evaluate(() => new Promise((resolve, reject) => {
    const open = indexedDB.open('gate-occurrences');
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const request = open.result.transaction('occurrences', 'readonly').objectStore('occurrences').getAll();
      request.onerror = () => reject(request.error);
      request.onsuccess = () => {
        open.result.close();
        resolve(request.result.filter((row) => row.state === 'QUEUED'));
      };
    };
  }));
  const rowsBefore = await queuedRows();
  check('one queued occurrence is stored before the failing sync', rowsBefore.length === 1, `observed ${rowsBefore.length}`);

  // Back online, the gateway answers the reconcile POST with a 502 that is not JSON. The online
  // edge syncs by itself, so the response wait is registered BEFORE going online.
  const isReconcile = (url) => new URL(url).pathname === '/api/access/scans/reconciliations';
  const reconcileRoute = (url) => isReconcile(url.href);
  await page.route(reconcileRoute, (route) =>
    route.request().method() === 'POST'
      ? route.fulfill({ status: 502, contentType: 'text/html', body: '<html><body>Bad Gateway</body></html>' })
      : route.continue());
  const failedSync = page.waitForResponse((response) => isReconcile(response.url()) && response.status() === 502);
  await context.setOffline(false);
  await failedSync;
  await page.getByText(/^Sync failed: The server answered but the reply could not be read\. 1 scan is saved on this device and will be sent on the next sync\. Last successful sync this session: none\./).waitFor();
  check('a 502 on sync renders the unreadable note', true);
  check(
    'the queue count still reads one after the failed sync',
    (await page.locator('.queue-note').innerText()).includes('1 queued offline scan'),
  );
  const rowsAfter = await queuedRows();
  check('the failed sync left the stored queue exactly as it was', JSON.stringify(rowsAfter) === JSON.stringify(rowsBefore),
    `before ${JSON.stringify(rowsBefore)} after ${JSON.stringify(rowsAfter)}`);
  await page.unroute(reconcileRoute);

  const syncResponse = page.waitForResponse((response) => isReconcile(response.url()) && response.status() === 200);
  await page.getByRole('button', { name: 'Sync queued scans' }).click();
  await syncResponse;
  await page.getByText(/Synced 1 offline scan/).waitFor();
  check('a successful sync replaces the failure note', (await page.getByText(/^Sync failed:/).count()) === 0);
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
    'the refusal leaves the full ordered lifecycle unchanged',
    sql(PG, 'access', `SELECT COALESCE(json_agg(event_type ORDER BY occurred_at, id), '[]'::json)::text
      FROM lifecycle_events WHERE ticket_id='${ticketID}'`) === lifecycleTypesBeforeRefusal,
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

  // TKT-399: Exit mode, through the rendered control, against Access's real pass
  // policy path. The fixture follows the one above: a random slot, a signed
  // credential and a SQL ticket row, plus ONE policy row for that slot (D9). No
  // publication names a random slot, so nothing can overwrite the policy. The
  // spec proves the scanner-to-Access path only, not catalog policy publication.
  const seedTicket = (slot) => {
    const id = randomUUID();
    const order = randomUUID();
    const passClaims = toBase64URL(JSON.stringify({
      v: 1, tid: id, oid: order, org: ORGANIZER, sid: slot, iat: Math.floor(issuedAt.getTime() / 1000),
    }));
    const body = `${header}.${passClaims}`;
    const qr = `${body}.${sign(null, Buffer.from(body), privateKey).toString('base64url')}`;
    sql(
      PG,
      'access',
      `INSERT INTO tickets
         (id, order_id, guest_order_ref, organizer_id, buyer_id, slot_id, ticket_type_id, qr_payload, issued_at)
       VALUES
         ('${id}', '${order}', '${randomUUID()}', '${ORGANIZER}', '${randomUUID()}', '${slot}',
          '${randomUUID()}', '${qr}', '${issuedAt.toISOString()}');`,
    );
    return { id, qr };
  };
  const passSlotID = randomUUID();
  const singleSlotID = randomUUID();
  sql(
    PG,
    'access',
    `INSERT INTO slot_re_entry_policies (slot_id, organizer_id, mode, requires_exit)
     VALUES ('${passSlotID}', '${ORGANIZER}', 'multi', true);`,
  );
  const pass = seedTicket(passSlotID);
  const single = seedTicket(singleSlotID);
  const lifecycleByID = (occurrenceID) => JSON.parse(sql(
    PG,
    'access',
    `SELECT COALESCE(json_agg(json_build_object(
       'ticket_id', ticket_id::text,
       'event_type', event_type,
       'occurred_at', to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
     )), '[]'::json)::text FROM lifecycle_events WHERE id='${occurrenceID}'`,
  ));
  const lifecycleTypes = (id) => JSON.parse(sql(
    PG,
    'access',
    `SELECT COALESCE(json_agg(event_type ORDER BY occurred_at, id), '[]'::json)::text
       FROM lifecycle_events WHERE ticket_id='${id}'`,
  ));

  const exitContext = await browser.newContext({ baseURL: BASE });
  const exitPage = await exitContext.newPage();
  // A mutation that makes the control unusable must fail in seconds, not minutes.
  exitPage.setDefaultTimeout(15_000);
  const isExitScan = (request) => request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans';
  const isExitReconcile = (request) => request.method() === 'POST'
    && new URL(request.url()).pathname === '/api/access/scans/reconciliations';
  const exitToken = enrolScanner(ACCESS, `browser exit gate ${Date.now()}`);
  await exitPage.goto('/scanner/', { waitUntil: 'domcontentloaded' });
  await exitPage.fill('#pairing-token', exitToken);
  const exitPull = exitPage.waitForResponse((response) =>
    new URL(response.url()).pathname === '/api/access/scans/voided-tickets');
  await exitPage.click('button[type=submit]');
  await exitPull;
  await exitPage.getByText('This device holds no revocation list yet.').waitFor({ state: 'detached' });
  check('a second device pairs and completes its own revocation pull', true);

  const entryRadio = exitPage.getByRole('radio', { name: 'Entry' });
  const exitRadio = exitPage.getByRole('radio', { name: 'Exit' });
  const scanWith = async (qr) => {
    await exitPage.getByLabel('Ticket credential').fill(qr);
    const sent = exitPage.waitForRequest(isExitScan);
    const answered = exitPage.waitForResponse((response) => isExitScan(response.request()));
    await exitPage.getByRole('button', { name: 'Check ticket' }).click();
    const request = await sent;
    await answered;
    return request.postDataJSON();
  };
  const resultText = () => exitPage.locator('section.result').innerText();

  check('the scanner starts in Entry mode', await entryRadio.isChecked() && !(await exitRadio.isChecked()));
  const firstEntry = await scanWith(pass.qr);
  await exitPage.getByText(/^Entry recorded at /).waitFor();
  check('an entry scan sends direction entry', firstEntry.direction === 'entry', `direction ${firstEntry.direction}`);
  check(
    'Access records the entry for that occurrence',
    JSON.stringify(lifecycleByID(firstEntry.occurrence_id))
      === JSON.stringify([{ ticket_id: pass.id, event_type: 'entry', occurred_at: firstEntry.occurred_at }]),
    JSON.stringify(lifecycleByID(firstEntry.occurrence_id)),
  );

  await exitRadio.check();
  await exitPage.getByText('Exit mode').waitFor();
  const liveExit = await scanWith(pass.qr);
  await exitPage.getByText(/^Exit recorded at /).waitFor();
  check('the Exit control makes the scan send direction exit', liveExit.direction === 'exit', `direction ${liveExit.direction}`);
  check('an accepted exit shows the Accepted heading', await exitPage.getByRole('heading', { name: 'Accepted' }).isVisible());
  check(
    'Access records the exit for that occurrence',
    JSON.stringify(lifecycleByID(liveExit.occurrence_id))
      === JSON.stringify([{ ticket_id: pass.id, event_type: 'exit', occurred_at: liveExit.occurred_at }]),
    JSON.stringify(lifecycleByID(liveExit.occurrence_id)),
  );

  const secondExit = await scanWith(pass.qr);
  await exitPage.getByRole('heading', { name: 'Exit not recorded' }).waitFor();
  const notInside = await resultText();
  check(
    'a second exit is not recorded and names not_inside',
    secondExit.direction === 'exit' && notInside.includes('Reason: not_inside.') && notInside.includes('The holder may leave.'),
    notInside,
  );
  check('the refused exit appends nothing', lifecycleByID(secondExit.occurrence_id).length === 0);

  await exitPage.reload({ waitUntil: 'domcontentloaded' });
  await exitPage.getByLabel('Ticket credential').waitFor();
  check('a reload returns the scanner to Entry mode', await entryRadio.isChecked() && !(await exitRadio.isChecked()));
  const secondEntry = await scanWith(pass.qr);
  await exitPage.getByText(/^Entry recorded at /).waitFor();
  check('the holder enters again', lifecycleByID(secondEntry.occurrence_id)[0]?.event_type === 'entry');

  // Offline: the exit is committed on the device, then the operator switches back
  // to Entry before the queue syncs. The queued row must still reach Access as an exit.
  await exitContext.setOffline(true);
  await exitRadio.check();
  await exitPage.getByLabel('Ticket credential').fill(pass.qr);
  await exitPage.getByRole('button', { name: 'Check ticket' }).click();
  await exitPage.getByRole('heading', { name: 'Queued offline' }).waitFor();
  check(
    'the queued exit tells staff the holder may leave',
    (await resultText()).includes('the exit is saved on this device and will sync when back online. The holder may leave.'),
    await resultText(),
  );
  const queuedExitRows = await exitPage.evaluate(() => new Promise((resolve, reject) => {
    const open = indexedDB.open('gate-occurrences');
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const request = open.result.transaction('occurrences', 'readonly').objectStore('occurrences').getAll();
      request.onerror = () => reject(request.error);
      request.onsuccess = () => {
        open.result.close();
        resolve(request.result.filter((row) => row.state === 'QUEUED'));
      };
    };
  }));
  const queuedExit = queuedExitRows[0];
  check(
    'the device committed one queued exit',
    queuedExitRows.length === 1 && queuedExit.direction === 'exit' && queuedExit.qrPayload === pass.qr,
    JSON.stringify(queuedExitRows.map((row) => ({ state: row.state, direction: row.direction }))),
  );
  check('Access has no row for the queued exit yet', lifecycleByID(queuedExit.occurrenceId).length === 0);

  await entryRadio.check();
  check('the operator is back in Entry mode before the sync', await entryRadio.isChecked());
  // The online edge syncs by itself, so both waits are registered BEFORE going online.
  const exitReconcile = exitPage.waitForRequest(isExitReconcile);
  const exitReconciled = exitPage.waitForResponse((response) => isExitReconcile(response.request()) && response.status() === 200);
  await exitContext.setOffline(false);
  const reconcileBody = (await exitReconcile).postDataJSON();
  const reconcileAnswer = await (await exitReconciled).json();
  const sentExit = reconcileBody.occurrences?.find((row) => row.occurrence_id === queuedExit.occurrenceId);
  check(
    'reconciliation sends the stored exit with event_type exit',
    JSON.stringify(sentExit) === JSON.stringify({
      qr_payload: pass.qr,
      occurrence_id: queuedExit.occurrenceId,
      occurred_at: queuedExit.occurredAt,
      event_type: 'exit',
    }),
    JSON.stringify(reconcileBody.occurrences?.map((row) => ({ occurrence_id: row.occurrence_id, event_type: row.event_type }))),
  );
  check(
    'Access answers recorded for the queued exit',
    reconcileAnswer.results?.some((row) => row.occurrence_id === queuedExit.occurrenceId && row.result === 'recorded'),
    JSON.stringify(reconcileAnswer),
  );
  check(
    'Access stores exactly one exit for that occurrence, at the device time',
    JSON.stringify(lifecycleByID(queuedExit.occurrenceId))
      === JSON.stringify([{ ticket_id: pass.id, event_type: 'exit', occurred_at: queuedExit.occurredAt }]),
    JSON.stringify(lifecycleByID(queuedExit.occurrenceId)),
  );
  check(
    'the pass trail is entry, exit, entry, exit and holds no redeemed or duplicate_admit',
    JSON.stringify(lifecycleTypes(pass.id)) === JSON.stringify(['entry', 'exit', 'entry', 'exit']),
    JSON.stringify(lifecycleTypes(pass.id)),
  );

  // A slot with no policy row reads as single: an exit has no meaning there.
  await exitRadio.check();
  const singleExit = await scanWith(single.qr);
  await exitPage.getByRole('heading', { name: 'Exit not recorded' }).waitFor();
  const notApplicable = await resultText();
  check(
    'an exit on a single-entry slot names exit_not_applicable',
    singleExit.direction === 'exit' && notApplicable.includes('Reason: exit_not_applicable.') && notApplicable.includes('The holder may leave.'),
    notApplicable,
  );
  check('the single-entry ticket has no admission event', lifecycleTypes(single.id).length === 0, JSON.stringify(lifecycleTypes(single.id)));
} catch (error) {
  // A step that throws must not leave the summary saying "passed".
  check('the spec ran to the end', false, String(error?.message ?? error).split('\n')[0]);
} finally {
  await browser.close();
  if (!recorder.finish()) process.exitCode = 1;
}
