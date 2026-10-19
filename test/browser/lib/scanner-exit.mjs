import { randomUUID } from 'node:crypto';
import { ORGANIZER, sql } from './support.mjs';
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe } from './scanner-controls.mjs';

export async function exitJourneys({ browser, BASE, PG, token, fixture, facts, check, ACCESS_PORT, INTERNAL_TOKEN }) {
  const pass = () => {
    const ticket = fixture();
    sql(PG, 'access', `UPDATE slot_re_entry_policies SET requires_exit=true WHERE slot_id=(SELECT slot_id FROM tickets WHERE id='${ticket.ticket}')`);
    return ticket;
  };
  const single = () => {
    const ticket = fixture();
    sql(PG, 'access', `UPDATE slot_re_entry_policies SET mode='single',requires_exit=false WHERE slot_id=(SELECT slot_id FROM tickets WHERE id='${ticket.ticket}')`);
    return ticket;
  };
  const context = await browser.newContext({ baseURL: BASE }); await installProbe(context);
  const page = await context.newPage();
  const requests = [], batches = [];
  context.on('request', request => {
    if (request.method() !== 'POST') return;
    if (scanPath(request.url())) requests.push(request.postDataJSON());
    if (reconcilePath(request.url())) batches.push(request.postDataJSON().occurrences);
  });
  const send = async (ticket, direction, heading) => {
    await page.getByRole('radio', { name: direction, exact: true }).check();
    const waiting = page.waitForResponse(response => scanPath(response.url()));
    await submit(page, ticket.credential);
    const response = await waiting, body = await response.json();
    await page.getByRole('heading', { name: heading, exact: true }).waitFor();
    return { request: requests.at(-1), response: body };
  };
  const row = id => read(page).then(rows => rows.find(row => row.occurrenceId === id));
  const waitState = async (id, state) => page.waitForFunction(({ id, state }) => window.__tkt558IdbProbe.events.some(event => event.id === id && event.state === state && event.event === 'complete'), { id, state });
  try {
    await pair(page, token);
    assert.equal(await page.getByRole('radio', { name: 'Entry', exact: true }).isChecked(), true);
    const ticket = pass();
    const entry = await send(ticket, 'Entry', 'Accepted');
    const exit = await send(ticket, 'Exit', 'Exit recorded');
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), `Exit recordedExit recorded. The holder may leave.Exit recorded at ${new Date(exit.response.scanned_at).toLocaleString('en-US')}.`);
    assert.deepEqual(facts(ticket.ticket), [entry, exit].map(({ request, response }) => ({ occurrence_id: request.occurrence_id, event_type: request.direction, occurred_at: new Date(response.scanned_at).toISOString() })));
    assert.equal(exit.request.direction, 'exit');
    assert.deepEqual(await row(exit.request.occurrence_id), { ...(await row(exit.request.occurrence_id)), direction: 'exit', state: 'ACTUATED', actuated: true });
    const reentry = await send(ticket, 'Entry', 'Accepted');
    assert.equal(facts(ticket.ticket).length, 3);
    assert.equal(reentry.request.direction, 'entry');
    check('TKT-399 Chrome Entry, Exit and re-entry create exact Access directions and server times', true);

    await page.getByRole('radio', { name: 'Exit', exact: true }).check();
    await context.setOffline(true);
    await submit(page, ticket.credential);
    await page.getByRole('heading', { name: 'Offline Exit', exact: true }).waitFor();
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), 'Offline ExitNo response was received.Exit result unknown; the holder may leave');
    const offline = requests.at(-1); await waitState(offline.occurrence_id, 'QUEUED');
    const queued = await row(offline.occurrence_id);
    assert.equal(queued.direction, 'exit');
    await page.getByRole('radio', { name: 'Entry', exact: true }).check();
    const sync = page.waitForResponse(response => reconcilePath(response.url()) && response.status() === 200);
    await context.setOffline(false); await sync;
    await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    assert.deepEqual(batches.at(-1), [{ occurrence_id: offline.occurrence_id, qr_payload: ticket.credential, occurred_at: offline.occurred_at, event_type: 'exit' }]);
    assert.deepEqual(await row(offline.occurrence_id), { ...queued, state: 'SYNCED', result: 'recorded' });
    assert.deepEqual(facts(ticket.ticket).find(fact => fact.occurrence_id === offline.occurrence_id), { occurrence_id: offline.occurrence_id, event_type: 'exit', occurred_at: offline.occurred_at });
    const count = batches.length;
    await page.evaluate(() => {
      window.__exitQueueReads = 0;
      const native = IDBDatabase.prototype.transaction;
      IDBDatabase.prototype.transaction = function (...args) {
        const tx = Reflect.apply(native, this, args);
        if (this.name === 'gate-occurrences' && tx.mode === 'readonly'
          && tx.objectStoreNames.contains('occurrences') && tx.objectStoreNames.contains('legacy_classes')) {
          tx.addEventListener('complete', () => { window.__exitQueueReads++; });
        }
        return tx;
      };
      window.dispatchEvent(new Event('online'));
    });
    await page.waitForFunction(() => window.__exitQueueReads >= 2);
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)));
    assert.equal(batches.length, count);
    check('TKT-399 offline Exit keeps its id and claimed time across mode change and retires without resend', true);

    const beforeOutside = facts(ticket.ticket);
    await send(ticket, 'Exit', 'Exit not recorded');
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), 'Exit not recordedExit not recorded; reason: not_inside. The holder may leave.');
    assert.deepEqual(facts(ticket.ticket), beforeOutside);
    const one = single(); await send(one, 'Exit', 'Exit not recorded');
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), 'Exit not recordedExit not recorded; reason: exit_not_applicable. The holder may leave.');
    assert.deepEqual(facts(one.ticket), []);
    check('TKT-399 Chrome not_inside and single-policy Exit are neutral and create zero facts', true);

    const offlineSingle = single();
    await context.setOffline(true); await submit(page, offlineSingle.credential);
    await page.getByRole('heading', { name: 'Offline Exit', exact: true }).waitFor();
    const rejected = requests.at(-1); await waitState(rejected.occurrence_id, 'QUEUED');
    const singleBefore = await row(rejected.occurrence_id);
    await context.setOffline(false);
    await page.getByText(`Reconciliation rejected for ${rejected.occurrence_id}. This result is not recorded.`, { exact: true }).waitFor();
    assert.deepEqual(await row(rejected.occurrence_id), { ...singleBefore, state: 'RESOLVED', result: 'rejected' });
    assert.deepEqual(facts(offlineSingle.ticket), []);
    check('TKT-399 offline single-policy Exit retires as rejected with a warning and zero Access facts', true);

    const revoked = pass();
    const refund = await fetch(`http://localhost:${ACCESS_PORT}/internal/orders/${revoked.order}/refunds`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Internal-Token': INTERNAL_TOKEN }, body: JSON.stringify({ organizer_id: ORGANIZER, refund_id: randomUUID(), quantity: 1 }) });
    assert.equal(refund.status, 200);
    const priorMeta = await page.evaluate(() => window.__tkt558IdbProbe.events.filter(event => event.store === 'revocation_meta' && event.event === 'complete').length);
    const feed = page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/voided-tickets'));
    await page.getByRole('button', { name: 'Refresh revocation list' }).click(); await feed;
    await page.waitForFunction(prior => window.__tkt558IdbProbe.events.filter(event => event.store === 'revocation_meta' && event.event === 'complete').length > prior, priorMeta);
    assert.ok((await read(page, 'revocations')).some(row => row.ticketID === revoked.ticket));
    const beforeRows = await read(page), beforeFacts = facts(revoked.ticket), beforeRequests = requests.length, beforeBatches = batches.length;
    await submit(page, revoked.credential);
    await page.getByText('Exit not recorded. This ticket is no longer valid for entry. The holder may leave.', { exact: true }).waitFor();
    assert.deepEqual(await read(page), beforeRows); assert.deepEqual(facts(revoked.ticket), beforeFacts);
    assert.equal(requests.length, beforeRequests); assert.equal(batches.length, beforeBatches);
    check('TKT-399 listed revoked Exit submits the control and creates no local row, scan or reconcile', true);
  } finally { await context.close(); }

  const reloadContext = await browser.newContext({ baseURL: BASE }); await installProbe(reloadContext);
  const reload = await reloadContext.newPage();
  try {
    const ticket = pass(); await pair(reload, token);
    await reload.getByRole('radio', { name: 'Exit', exact: true }).check();
    await reload.evaluate(() => {
      const native = fetch;
      window.fetch = (input, init) => input instanceof Request && new URL(input.url).pathname === '/api/access/scans' ? new Promise(() => {}) : native(input, init);
    });
    await submit(reload, ticket.credential);
    await reload.waitForFunction(() => window.__tkt558IdbProbe.events.some(event => event.state === 'PENDING' && event.event === 'complete'));
    const [pending] = await read(reload);
    await reload.reload();
    const notice = reload.getByRole('region', { name: `Recovered scan ${pending.occurrenceId}` });
    await notice.waitFor();
    assert.equal(await notice.textContent(), `Recovered scanScan ${pending.occurrenceId} at ${pending.occurredAt}. Type: exit. Recovery cause: uncertain_recovery. Sync state: QUEUED.The prior result and physical passage are unknown. Do not admit from this notice.Acknowledge recovery notice`);
    assert.equal(await reload.getByRole('status', { name: 'Scan result' }).count(), 0);
    const sent = reload.waitForRequest(request => request.method() === 'POST' && reconcilePath(request.url()));
    await reload.getByRole('button', { name: 'Sync queued scans' }).click();
    assert.deepEqual((await sent).postDataJSON().occurrences, [{ occurrence_id: pending.occurrenceId, qr_payload: ticket.credential, occurred_at: pending.occurredAt, event_type: 'exit' }]);
    await reload.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    assert.deepEqual(facts(ticket.ticket), [{ occurrence_id: pending.occurrenceId, event_type: 'exit', occurred_at: pending.occurredAt }]);
    check('TKT-399 genuine reload recovers exact Exit as an uncertainty notice and reconciles its claimed time', true);
  } finally { await reloadContext.close(); }
}
