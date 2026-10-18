import { createPrivateKey, randomUUID, sign } from 'node:crypto';
import { installTransportMutation } from './scanner-mutations.mjs';
import { createServer } from 'node:http';
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe, arm, matched, release, blank, seedLegacy } from './scanner-controls.mjs';
import { ORGANIZER, sql } from './support.mjs';

export async function entryCore({ browser, BASE, PG, QR_SEED, QR_KID, token, check }) {
  const privateKey = createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(QR_SEED, 'base64')]), format: 'der', type: 'pkcs8' });
  const fixture = () => {
    const ticket = randomUUID(), order = randomUUID(), slot = randomUUID();
    const header = Buffer.from(JSON.stringify({ alg: 'EdDSA', kid: QR_KID, typ: 'TKT' })).toString('base64url');
    const claims = Buffer.from(JSON.stringify({ v: 1, tid: ticket, oid: order, org: ORGANIZER, sid: slot, iat: Math.floor(Date.now() / 1000) })).toString('base64url');
    const body = `${header}.${claims}`;
    const credential = `${body}.${sign(null, Buffer.from(body), privateKey).toString('base64url')}`;
    sql(PG, 'access', `INSERT INTO tickets (id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at) VALUES ('${ticket}','${order}','${randomUUID()}','${ORGANIZER}','${randomUUID()}','${slot}','${randomUUID()}','${credential}',now()); INSERT INTO slot_re_entry_policies(slot_id,organizer_id,mode,requires_exit) VALUES('${slot}','${ORGANIZER}','multi',false)`);
    return { ticket, credential, order };
  };
  const facts = ticket => JSON.parse(sql(PG, 'access', `SELECT COALESCE(json_agg(json_build_object('occurrence_id',id,'event_type',event_type,'occurred_at',to_char(occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')) ORDER BY occurred_at,id),'[]'::json)::text FROM lifecycle_events WHERE ticket_id='${ticket}'`));
  const quarantine = ticket => Number(sql(PG, 'access', `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id='${ticket}'`));
  const context = await browser.newContext({ baseURL: BASE, acceptDownloads: true });
  await installProbe(context);
  await installTransportMutation(context);
  const page = await context.newPage();
  try {
    await pair(page, token);
    const ticket = fixture();
    const request = page.waitForRequest(r => scanPath(r.url()) && r.method() === 'POST');
    const response = page.waitForResponse(r => scanPath(r.url()) && r.status() === 200);
    await submit(page, ticket.credential);
    const online = (await request).postDataJSON(), accepted = await (await response).json();
    await page.getByRole('heading', { name: 'Accepted', exact: true }).waitFor();
    assert.equal(online.direction, 'entry');
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), `AcceptedEntry accepted. Admit.Entry recorded at ${new Date(accepted.scanned_at).toLocaleString('en-US')}.`);
    const onlineFacts = facts(ticket.ticket);
    assert.deepEqual(onlineFacts, [{ occurrence_id: online.occurrence_id, event_type: 'entry', occurred_at: new Date(accepted.scanned_at).toISOString() }]);
    assert.equal(Number(sql(PG, 'access', `SELECT count(*) FROM lifecycle_events WHERE ticket_id='${ticket.ticket}' AND id='${online.occurrence_id}' AND occurred_at='${accepted.scanned_at}'::timestamptz`)), 1);
    assert.equal(quarantine(ticket.ticket), 0);
    assert.equal((await read(page)).filter(row => row.occurrenceId === online.occurrence_id && row.state === 'ACTUATED').length, 1);
    check('B1 online Entry submits the rendered form and stores exactly one Entry at the returned time', true);

    await context.setOffline(true);
    const offlineRequest = page.waitForRequest(r => scanPath(r.url()) && r.method() === 'POST');
    await submit(page, ticket.credential);
    const offline = (await offlineRequest).postDataJSON();
    await page.getByRole('heading', { name: 'Offline decision' }).waitFor();
    const instruction = await page.getByRole('status', { name: 'Scan result' }).textContent();
    assert.equal(instruction, 'Offline decisionNo response was received.Admit per venue offline policy.');
    await page.waitForFunction(id => window.__tkt558IdbProbe.events.some(e => e.id === id && e.state === 'QUEUED' && e.event === 'complete'), offline.occurrence_id);
    const reconciliation = page.waitForRequest(r => reconcilePath(r.url()) && r.method() === 'POST');
    await context.setOffline(false);
    const batch = (await reconciliation).postDataJSON().occurrences;
    assert.deepEqual(batch, [{ occurrence_id: offline.occurrence_id, qr_payload: ticket.credential, occurred_at: offline.occurred_at, event_type: 'entry' }]);
    await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), instruction);
    assert.deepEqual(facts(ticket.ticket), [onlineFacts[0], { occurrence_id: offline.occurrence_id, event_type: 'entry', occurred_at: offline.occurred_at }]);
    assert.equal(quarantine(ticket.ticket), 0);
    check('B1 offline Entry reconciliation preserves the exact id, type and claimed time with no second instruction', true);

    // Each terminal case has an independent real queued control.
    for (const status of [401, 500]) {
      const control = fixture(), terminal = fixture();
      await page.route(scanPath, route => route.fulfill({ status: 503, body: 'unavailable' }));
      const controlRequest = page.waitForRequest(r => scanPath(r.url()) && r.method() === 'POST');
      await submit(page, control.credential);
      const queuedID = (await controlRequest).postDataJSON().occurrence_id;
      await page.getByRole('heading', { name: 'Offline decision' }).waitFor();
      await page.unroute(scanPath);
      await page.route(scanPath, route => route.fulfill({ status, contentType: 'application/json', body: '{unreadable' }));
      const terminalRequest = page.waitForRequest(r => scanPath(r.url()) && r.method() === 'POST');
      await submit(page, terminal.credential);
      const terminalID = (await terminalRequest).postDataJSON().occurrence_id;
      await page.getByRole('heading', { name: status === 401 ? 'Pair this device' : 'Operator required', exact: true }).waitFor();
      await page.waitForFunction(id => window.__tkt558IdbProbe.events.some(e => e.id === id && e.state === 'RESOLVED' && e.event === 'complete'), terminalID);
      await page.unroute(scanPath);
      if (status === 401) { await page.getByLabel('Pairing token').fill(token); await page.getByRole('button', { name: 'Pair device' }).click(); }
      const syncRequest = page.waitForRequest(r => reconcilePath(r.url()) && r.method() === 'POST');
      await page.getByRole('button', { name: 'Sync queued scans' }).click();
      assert.deepEqual((await syncRequest).postDataJSON().occurrences.map(row => row.occurrence_id), [queuedID]);
      await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
      assert.deepEqual(facts(terminal.ticket), []); assert.equal(quarantine(terminal.ticket), 0);
      check(`B1 client ${status} stays terminal while its independent queued control reaches Access`, true);
    }
  } finally { await context.close(); }

  // B6 observes the native promise rejection without changing the rejection.
  for (const failure of ['offline', 'route abort', 'complete POST then socket close']) {
    const context = await browser.newContext({ baseURL: BASE });
    const page = await context.newPage();
    let server, received;
    try {
      await pair(page, token);
      await page.evaluate(() => {
        const native = fetch;
        window.__tkt558NativeRejections = [];
        window.fetch = (...args) => native(...args).catch(error => {
          if (args[0] instanceof Request && new URL(args[0].url).pathname === '/api/access/scans') window.__tkt558NativeRejections.push({ name: error.name, constructor: error.constructor.name, nativeTypeError: error.constructor === TypeError, message: error.message });
          throw error;
        });
      });
      const ticket = fixture();
      if (failure === 'offline') await context.setOffline(true);
      if (failure === 'route abort') await page.route(scanPath, route => route.abort('internetdisconnected'));
      if (failure === 'complete POST then socket close') {
        server = createServer((request, response) => {
          if (request.method === 'OPTIONS') { response.writeHead(204, { 'Access-Control-Allow-Origin': BASE, 'Access-Control-Allow-Headers': 'content-type,x-scanner-token', 'Access-Control-Allow-Methods': 'POST' }); response.end(); return; }
          let body = ''; request.on('data', chunk => { body += chunk; });
          request.on('end', () => { received = JSON.parse(body); request.socket.destroy(); });
        });
        await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
        await page.route(scanPath, route => route.continue({ url: `http://127.0.0.1:${server.address().port}/complete-post` }));
      }
      await submit(page, ticket.credential);
      await page.getByRole('heading', { name: 'Offline decision' }).waitFor();
      assert.deepEqual(await page.evaluate(() => window.__tkt558NativeRejections), [{ name: 'TypeError', constructor: 'TypeError', nativeTypeError: true, message: 'Failed to fetch' }]);
      const rows = await read(page); assert.equal(rows.length, 1); assert.equal(rows[0].state, 'QUEUED');
      if (server) { assert.equal(received.occurrence_id, rows[0].occurrenceId); assert.equal(received.qr_payload, ticket.credential); assert.equal(received.direction, 'entry'); }
      check(`B6 Chrome ${browser.version()} native ${failure} has the exact selected rejection shape`, true);
    } finally { await context.close(); if (server) await new Promise(resolve => server.close(resolve)); }
  }
  return { fixture, facts, quarantine };
}
