// TKT-566 keeps the real seven-row reconciliation journey in the browser suite.
import { randomUUID } from 'node:crypto';
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe } from './scanner-controls.mjs';
import { ORGANIZER, sql } from './support.mjs';

const expected = {
  A: { direction: 'entry', result: 'recorded', state: 'SYNCED' },
  B: { direction: 'exit', result: 'recorded', state: 'SYNCED' },
  C: { direction: 'entry', result: 'synced', state: 'SYNCED' },
  D: { direction: 'entry', result: 'conflict', state: 'RESOLVED' },
  E: { direction: 'exit', result: 'rejected', state: 'RESOLVED' },
  F: { direction: 'exit', result: 'rejected', state: 'RESOLVED' },
  G: { result: 'recorded', state: 'SYNCED' },
};
const labels = Object.keys(expected);
const normal = labels.filter(label => label !== 'G');
const summary = 'Synced 4 offline scan(s). 1 conflict flagged for the operator. 2 rejected scans were not recorded.';
const conflictSubject = 'platform.access.admission-conflict.alarm';
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const fact = (id, type, time) => ({ occurrence_id: id, event_type: type, occurred_at: new Date(time).toISOString() });
const byId = rows => [...rows].sort((a, b) => a.occurrence_id.localeCompare(b.occurrence_id));
const direction = (page, value) => page.getByRole('radio', { name: value === 'entry' ? 'Entry' : 'Exit', exact: true }).check();
const committed = (page, id, state) => page.waitForFunction(({ id, state }) =>
  window.__tkt558IdbProbe.events.some(event => event.id === id && event.state === state && event.event === 'complete'), { id, state });
const feedPath = url => new URL(url).pathname === '/api/access/scans/voided-tickets';

async function answered(page, credential) {
  const [request, response] = await Promise.all([
    page.waitForRequest(request => request.method() === 'POST' && scanPath(request.url())),
    page.waitForResponse(response => response.request().method() === 'POST' && scanPath(response.url())),
    submit(page, credential),
  ]);
  const body = request.postDataJSON();
  assert.match(body.occurrence_id, uuid);
  assert.ok(Number.isFinite(Date.parse(body.occurred_at)));
  assert.deepEqual(body, { occurrence_id: body.occurrence_id, qr_payload: credential, occurred_at: body.occurred_at, direction: 'entry' });
  assert.equal(response.status(), 200);
  const json = await response.json();
  assert.deepEqual(json, { decision: 'accepted', scanned_at: json.scanned_at });
  await committed(page, body.occurrence_id, 'ACTUATED');
  await page.getByRole('heading', { name: 'Accepted', exact: true }).waitFor();
  return { body, json };
}

export async function mixedReconciliation(shared) {
  const { browser, BASE, PG, token, fixture, facts, quarantine, ACCESS_PORT, INTERNAL_TOKEN } = shared;
  const json = statement => JSON.parse(sql(PG, 'access', statement));
  const lifecycle = ticket => json(`SELECT COALESCE(json_agg(json_build_object('event',row_to_json(e),'integrity',row_to_json(i)) ORDER BY i.sequence NULLS LAST,e.id),'[]'::json)::text FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i ON i.event_id=e.id WHERE e.ticket_id='${ticket}'`);
  const tickets = Object.fromEntries(labels.map(label => [label, fixture()]));
  for (const label of ['D', 'E', 'F']) {
    const updated = sql(PG, 'access', `UPDATE slot_re_entry_policies SET mode='single',requires_exit=false
      WHERE slot_id=(SELECT slot_id FROM tickets WHERE id='${tickets[label].ticket}') RETURNING slot_id`);
    assert.equal(updated.trim().split('\n').filter(Boolean).length, 1);
  }
  const newContext = async () => {
    const context = await browser.newContext({ baseURL: BASE, locale: 'en-US', timezoneId: 'UTC' });
    await installProbe(context);
    return context;
  };

  // D's first admission comes from another real scanner page.
  const priorContext = await newContext();
  let prior;
  try {
    const page = await priorContext.newPage();
    await pair(page, token);
    prior = await answered(page, tickets.D.credential);
    assert.deepEqual(facts(tickets.D.ticket), [fact(prior.body.occurrence_id, 'redeemed', prior.json.scanned_at)]);
    assert.deepEqual(await page.evaluate(() => window.__tkt558IdbProbe.failures), []);
  } finally { await priorContext.close(); }
  const priorLifecycle = lifecycle(tickets.D.ticket);

  const context = await newContext();
  try {
    const page = await context.newPage();
    const scans = [], attempts = [], wire = [];
    let blocked = true;
    page.on('request', request => {
      if (request.method() !== 'POST') return;
      if (scanPath(request.url())) scans.push(request.postDataJSON());
      if (reconcilePath(request.url())) attempts.push(request.postDataJSON());
    });
    await pair(page, token);

    // Refund through Access, then wait for the refresh's committed completion marker.
    const refund = await fetch(`http://localhost:${ACCESS_PORT}/internal/orders/${tickets.G.order}/refunds`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Internal-Token': INTERNAL_TOKEN },
      body: JSON.stringify({ organizer_id: ORGANIZER, refund_id: randomUUID(), quantity: 1 }),
    });
    assert.equal(refund.status, 200);
    assert.deepEqual((await refund.json()).ticket_ids, [tickets.G.ticket]);
    const metaCommits = await page.evaluate(() => window.__tkt558IdbProbe.events.filter(event => event.store === 'revocation_meta' && event.event === 'complete').length);
    const feed = page.waitForResponse(response => feedPath(response.url()) && response.request().method() === 'GET');
    await page.getByRole('button', { name: 'Refresh revocation list', exact: true }).click();
    assert.equal((await feed).status(), 200);
    await page.waitForFunction(count => window.__tkt558IdbProbe.events.filter(event => event.store === 'revocation_meta' && event.event === 'complete').length > count, metaCommits);
    assert.ok((await read(page, 'revocations')).some(row => row.ticketID === tickets.G.ticket));
    assert.ok((await read(page, 'revocation_meta')).some(row => Number.isFinite(Date.parse(row.completedAt))));
    const refundedLifecycle = lifecycle(tickets.G.ticket);

    await page.route(scanPath, route => route.request().method() === 'POST' ? route.abort('internetdisconnected') : route.continue());
    // The recorder is installed before transport is unblocked. It retains exactly
    // the body forwarded to Access, so request mutations can use this same boundary.
    await page.route(reconcilePath, route => {
      const request = route.request();
      if (request.method() !== 'POST') return route.continue();
      if (blocked) return route.abort('internetdisconnected');
      const sent = request.postDataJSON();
      wire.push(structuredClone(sent));
      return route.continue({ postData: JSON.stringify(sent) });
    });
    const submitted = {};
    for (const label of normal) {
      await direction(page, expected[label].direction);
      const requestSeen = page.waitForRequest(request => request.method() === 'POST' && scanPath(request.url()));
      await submit(page, tickets[label].credential);
      submitted[label] = (await requestSeen).postDataJSON();
      assert.match(submitted[label].occurrence_id, uuid);
      assert.ok(Number.isFinite(Date.parse(submitted[label].occurred_at)));
      assert.deepEqual(submitted[label], { occurrence_id: submitted[label].occurrence_id, qr_payload: tickets[label].credential,
        occurred_at: submitted[label].occurred_at, direction: expected[label].direction });
      await page.getByRole('heading', { name: expected[label].direction === 'entry' ? 'Offline decision' : 'Offline Exit', exact: true }).waitFor();
      await committed(page, submitted[label].occurrence_id, 'QUEUED');
    }
    assert.equal(attempts.length, 0);
    await direction(page, 'entry');
    const failedAutomatic = page.waitForEvent('requestfailed', { predicate: request => request.method() === 'POST' && reconcilePath(request.url()) });
    await submit(page, tickets.G.credential);
    await page.getByRole('heading', { name: 'Not valid for entry', exact: true }).waitFor();
    await failedAutomatic;
    // D6: a committed G row alone does not settle its automatic sync.
    await page.getByText('Sync failed: No connection. 7 scans are saved on this device and will be sent on the next sync. Last successful sync this session: none.', { exact: true }).waitFor();
    assert.equal(scans.length, 6, 'TKT-566 G must send no live scan');
    assert.equal(attempts.length, 1, 'TKT-566 G automatic sync must fail before manual sync');

    const before = await read(page);
    assert.equal(before.length, 7);
    const g = before.find(row => row.localDecision === 'revocation_refused');
    assert.ok(g);
    await committed(page, g.occurrenceId, 'QUEUED');
    const rows = Object.fromEntries(labels.map(label => [label, before.find(row => row.occurrenceId === (label === 'G' ? g.occurrenceId : submitted[label].occurrence_id))]));
    assert.equal(new Set(labels.map(label => rows[label]?.occurrenceId)).size, 7);
    for (const label of labels) {
      const row = rows[label];
      assert.ok(row, `TKT-566 ${label} queued row`);
      assert.match(row.occurrenceId, uuid);
      assert.match(row.ownerId, uuid);
      assert.ok(Number.isFinite(row.leaseExpiresAt));
      assert.ok(Number.isFinite(Date.parse(row.decidedAt)));
      assert.ok(Number.isFinite(Date.parse(row.occurredAt)));
      assert.deepEqual(row, { occurrenceId: row.occurrenceId, qrPayload: tickets[label].credential,
        occurredAt: label === 'G' ? g.occurredAt : submitted[label].occurred_at,
        createdAt: row.occurredAt, ownerId: row.ownerId, leaseExpiresAt: row.leaseExpiresAt,
        recordFormat: 1, state: 'QUEUED', actuated: false, decidedAt: row.decidedAt,
        localOutcome: { family: 'queue', cause: label === 'G' ? 'local_refusal' : 'transport_failure' },
        ...(label === 'G' ? { localDecision: 'revocation_refused' } : { direction: expected[label].direction }) }, `TKT-566 ${label} complete queued row`);
    }
    const wireRow = label => ({ occurrence_id: rows[label].occurrenceId, qr_payload: tickets[label].credential,
      occurred_at: rows[label].occurredAt, ...(label === 'G' ? { local_decision: 'revocation_refused' } : { event_type: expected[label].direction }) });

    // Pre-apply C through the real endpoint, leaving its saved row queued.
    const preApply = await context.request.post(`${BASE}/api/access/scans/reconciliations`, {
      headers: { 'X-Scanner-Token': token }, data: { occurrences: [wireRow('C')] },
    });
    assert.equal(preApply.status(), 200);
    const preJson = await preApply.json();
    assert.deepEqual(preJson, { results: [{ occurrence_id: rows.C.occurrenceId, result: 'recorded', occurred_at: preJson.results[0].occurred_at }] });
    assert.equal(Date.parse(preJson.results[0].occurred_at), Date.parse(rows.C.occurredAt));
    assert.deepEqual(facts(tickets.C.ticket), [fact(rows.C.occurrenceId, 'entry', rows.C.occurredAt)]);
    const replayLifecycle = lifecycle(tickets.C.ticket);
    assert.deepEqual(await read(page), before, 'TKT-566 all seven rows remain queued after the automatic failure and C pre-apply');

    const responseSeen = page.waitForResponse(response => response.request().method() === 'POST' && reconcilePath(response.url()));
    blocked = false;
    await page.getByRole('button', { name: 'Sync queued scans', exact: true }).click();
    const response = await responseSeen;
    assert.equal(response.status(), 200);
    const responseBody = await response.json();
    await response.finished();
    assert.equal(wire.length, 1, 'TKT-566 mixed wire must contain one real batch');
    assert.deepEqual({ ...wire[0], occurrences: byId(wire[0].occurrences) }, { occurrences: byId(labels.map(wireRow)) }, 'TKT-566 mixed wire identity, direction and local_decision');
    await page.locator('.sync-note').filter({ hasText: /^Synced / }).waitFor();
    for (const label of labels) await committed(page, rows[label].occurrenceId, expected[label].state);
    const after = await read(page);
    assert.equal(after.length, 7);
    assert.equal(attempts.length, 2);

    assert.deepEqual(Object.keys(responseBody), ['results']);
    assert.equal(responseBody.results.length, 7);
    assert.deepEqual(byId(responseBody.results).map(({ occurrence_id, result }) => ({ occurrence_id, result })),
      byId(labels.map(label => ({ occurrence_id: rows[label].occurrenceId, result: expected[label].result }))), 'TKT-566 mixed response pairs');
    for (const label of labels) {
      const result = responseBody.results.find(result => result.occurrence_id === rows[label].occurrenceId);
      const answer = { occurrence_id: rows[label].occurrenceId, result: expected[label].result };
      if (expected[label].result !== 'rejected') {
        assert.equal(Date.parse(result.occurred_at), Date.parse(rows[label].occurredAt), `TKT-566 ${label} response time`);
        answer.occurred_at = result.occurred_at;
      }
      assert.deepEqual(result, answer, `TKT-566 ${label} exact response with no skew flag`);
      assert.deepEqual(after.find(row => row.occurrenceId === rows[label].occurrenceId),
        { ...rows[label], state: expected[label].state, result: expected[label].result }, `TKT-566 ${label} complete local transition`);
    }
    assert.equal(await page.locator('.sync-note').textContent(), summary, 'TKT-566 exact mixed aggregate');
    const warnings = (await page.locator('p[role="alert"]').allTextContents()).filter(text => text.startsWith('Reconciliation rejected for'));
    assert.deepEqual(warnings.sort(), ['E', 'F'].map(label => `Reconciliation rejected for ${rows[label].occurrenceId}. This result is not recorded.`).sort(), 'TKT-566 exact rejection warnings');

    for (const label of ['A', 'B', 'C']) assert.deepEqual(facts(tickets[label].ticket), [fact(rows[label].occurrenceId, expected[label].direction, rows[label].occurredAt)], `TKT-566 ${label} exact Access fact`);
    assert.deepEqual(lifecycle(tickets.C.ticket), replayLifecycle, 'TKT-566 C replay preserves the complete lifecycle');
    assert.deepEqual(byId(facts(tickets.D.ticket)), byId([fact(prior.body.occurrence_id, 'redeemed', prior.json.scanned_at), fact(rows.D.occurrenceId, 'duplicate_admit', rows.D.occurredAt)]));
    assert.deepEqual(lifecycle(tickets.D.ticket).find(row => row.event.id === prior.body.occurrence_id), priorLifecycle.find(row => row.event.id === prior.body.occurrence_id), 'TKT-566 D prior redemption remains unchanged');
    for (const label of ['E', 'F']) assert.deepEqual(facts(tickets[label].ticket), [], `TKT-566 ${label} rejection records no fact`);
    const decisions = json(`SELECT COALESCE(json_agg(json_build_object('occurrence_id',occurrence_id::text,'ticket_id',ticket_id::text,'organizer_id',organizer_id::text,'occurred_at',to_char(occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),'decision',decision)),'[]'::json)::text FROM scanner_local_decisions WHERE ticket_id='${tickets.G.ticket}' OR occurrence_id='${rows.G.occurrenceId}'`);
    assert.deepEqual(decisions, [{ occurrence_id: rows.G.occurrenceId, ticket_id: tickets.G.ticket, organizer_id: ORGANIZER,
      occurred_at: new Date(rows.G.occurredAt).toISOString(), decision: 'revocation_refused' }], 'TKT-566 G exact Access refusal');
    assert.deepEqual(lifecycle(tickets.G.ticket), refundedLifecycle, 'TKT-566 G refusal preserves the post-refund lifecycle');
    for (const label of labels) assert.equal(quarantine(tickets[label].ticket), 0, `TKT-566 ${label} no quarantine`);

    // Read every delivery state, scoped to these fixture tickets and occurrences.
    const ticketIds = labels.map(label => `'${tickets[label].ticket}'`).join(',');
    const occurrenceIds = labels.map(label => `'${rows[label].occurrenceId}'`).join(',');
    const alarms = json(`SELECT COALESCE(json_agg(json_build_object('event_id',event_id::text,'subject',subject,'envelope',envelope)),'[]'::json)::text FROM lifecycle_integrity_alarm_outbox WHERE envelope->'data'->>'ticket_id' IN (${ticketIds}) OR envelope->'data'->>'occurrence_id' IN (${occurrenceIds})`);
    assert.equal(alarms.length, 1, 'TKT-566 exactly D owes an alarm across all delivery states');
    const alarm = alarms[0];
    assert.match(alarm.event_id, uuid);
    assert.equal(alarm.subject, conflictSubject);
    assert.equal(alarm.envelope.type, conflictSubject);
    assert.equal(alarm.envelope.schema, 1);
    assert.equal(alarm.envelope.id, alarm.event_id);
    assert.deepEqual(alarm.envelope.data, { alarm_id: alarm.event_id, occurrence_id: rows.D.occurrenceId,
      ticket_id: tickets.D.ticket, organizer_id: ORGANIZER, device_occurred_at: alarm.envelope.data.device_occurred_at,
      skew_flagged: false }, 'TKT-566 D conflict alarm identity');
    assert.ok(Math.abs(Date.parse(rows.D.occurredAt) - Date.now()) <= 24 * 60 * 60 * 1000);
    assert.equal(Date.parse(alarm.envelope.data.device_occurred_at), Date.parse(rows.D.occurredAt));
    assert.deepEqual(await page.evaluate(() => window.__tkt558IdbProbe.failures), []);
    shared.check('TKT-566 mixed-reconciliation', true);
  } finally { await context.close(); }
}
