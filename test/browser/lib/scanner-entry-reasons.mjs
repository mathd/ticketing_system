// TKT-565 submits the rendered form. Injected replies prove client compatibility;
// the separate live journeys check the current Access producers.
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe, arm, matched } from './scanner-controls.mjs';
import { sql } from './support.mjs';

const repairText = 'The outcome write needs repair. Keep this page open and retry storage.';
const originalScanAt = '2026-10-18T09:00:01.456Z';
const submittedAt = '2035-02-03T04:05:06.789Z';
const decidedAt = '2035-02-03T04:05:07.123Z';
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const localized = value => new Intl.DateTimeFormat('en-US', { timeZone: 'UTC', year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' }).format(new Date(value));
const entryDenials = [
  { reason: "already_redeemed", lines: ["Already redeemed.", "Do not admit. Get an operator.", "Reason: already_redeemed."] },
  { reason: "entry_limit_reached", lines: ["Do not admit. No entries remain on this pass.", "Reason: entry_limit_reached."] },
  { reason: "exit_required", lines: ["Do not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.", "Reason: exit_required."] },
  { reason: "not_inside", lines: ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: not_inside."] },
  { reason: "exit_not_applicable", lines: ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: exit_not_applicable."] },
  { reason: "invalid_credential", lines: ["Do not admit. Access did not accept this credential. Get an operator.", "Reason: invalid_credential."] },
  { reason: "revoked", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: revoked."] },
  { reason: "refunded", lines: ["Do not admit. This ticket was voided by a refund. Get an operator.", "Reason: refunded."] },
  { reason: "exchanged", lines: ["Do not admit. This ticket was replaced by an exchange. Ask for the replacement ticket and scan it.", "Reason: exchanged."] },
  { reason: "exit_unverified", lines: ["Do not admit. Access returned an Exit refusal for this Entry scan. Get an operator.", "Reason: exit_unverified."] },
  { reason: "integrity_quarantined", lines: ["Do not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.", "Reason: integrity_quarantined."] },
  { reason: "integrity_operator_controlled", lines: ["Do not admit. Access refused this ticket under operator-controlled integrity handling. Get an operator.", "Reason: integrity_operator_controlled."] },
  { reason: "occurrence_required", lines: ["Do not admit. This scan is missing its required occurrence ID. Get an operator to check the scanner.", "Reason: occurrence_required."] },
  { reason: "unknown_future_reason", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: unknown_future_reason."] },
  { reason: "invalid_occurrence", lines: ["Do not admit. This scan has invalid occurrence data. Get an operator to check the scanner.", "Reason: invalid_occurrence."] },
  { reason: "occurrence_collision", lines: ["Do not admit. This scan's occurrence ID conflicts with an existing record. Get an operator.", "Reason: occurrence_collision."] },
  { reason: "future_reason", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: future_reason."] },
  { reason: "__proto__", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: __proto__."] },
  { reason: "<b>future_reason</b>", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason: <b>future_reason</b>."] },
  { reason: " Future_Reason ", lines: ["Do not admit. Access refused this scan. Get an operator.", "Reason:  Future_Reason ."] },
];

// Install after pair(), so this observer wraps the existing probe in a known order.
// Capture attempted values before a probe can throw. No result or stored row is injected.
async function observeWrites(page) {
  await page.evaluate(() => {
    const state = window.__tkt565 = { writes: [], claimedTime: null };
    const NativeDate = Date;
    window.Date = class extends NativeDate {
      constructor(...args) { super(...(args.length ? args : state.claimedTime === null ? [] : [state.claimedTime])); }
    };
    // Date.now and timers stay real, independent of the claimed presentation time.
    const transaction = IDBDatabase.prototype.transaction;
    IDBDatabase.prototype.transaction = function (...args) {
      const tx = Reflect.apply(transaction, this, args);
      if (this.name !== 'gate-occurrences') return tx;
      const objectStore = tx.objectStore.bind(tx);
      tx.objectStore = name => {
        const store = objectStore(name);
        if (name !== 'occurrences') return store;
        for (const method of ['add', 'put']) {
          const native = store[method].bind(store);
          store[method] = (value, ...keys) => {
            const entry = { method, value: structuredClone(value), events: [] };
            state.writes.push(entry);
            tx.addEventListener('complete', () => entry.events.push('complete'));
            tx.addEventListener('abort', () => entry.events.push('abort'));
            if (state.claimedTime !== null && value?.localOutcome?.cause === 'denied') state.claimedTime = '2035-02-03T04:05:08.456Z';
            return native(value, ...keys);
          };
        }
        return store;
      };
      return tx;
    };
  });
}

async function withPage(shared, journey) {
  const context = await shared.browser.newContext({ baseURL: shared.BASE, locale: 'en-US', timezoneId: 'UTC' });
  try {
    await installProbe(context);
    const page = await context.newPage();
    const scans = [], batches = [];
    context.on('request', request => {
      if (request.method() !== 'POST') return;
      if (scanPath(request.url())) scans.push(request.postDataJSON());
      if (reconcilePath(request.url())) batches.push(request.postDataJSON().occurrences);
    });
    await pair(page, shared.token);
    await observeWrites(page);
    await journey({ context, page, scans, batches });
    assert.deepEqual(await page.evaluate(() => window.__tkt558IdbProbe.failures), []);
  } finally { await context.close(); }
}

const lifecycle = (PG, ticket) => JSON.parse(sql(PG, 'access', `SELECT COALESCE(json_agg(json_build_object('event',row_to_json(e),'integrity',row_to_json(i)) ORDER BY i.sequence NULLS LAST,e.id),'[]'::json)::text FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i ON i.event_id=e.id WHERE e.ticket_id='${ticket}'`));
const localRow = async (page, id) => (await read(page)).find(row => row.occurrenceId === id);
const setClaimedTime = (page, time) => page.evaluate(time => { window.__tkt565.claimedTime = time; }, time);
const completedState = (page, id, state) => page.waitForFunction(({ id, state }) => window.__tkt558IdbProbe.events.some(event => event.id === id && event.state === state && event.event === 'complete'), { id, state });
const completedReason = (page, id, reason) => page.waitForFunction(({ id, reason }) => window.__tkt565.writes.some(write => write.value?.occurrenceId === id && write.value?.localOutcome?.reason === reason && write.events.includes('complete')), { id, reason });

async function send(page, credential) {
  const [request, response] = await Promise.all([
    page.waitForRequest(request => scanPath(request.url()) && request.method() === 'POST'),
    page.waitForResponse(response => scanPath(response.url()) && response.request().method() === 'POST'),
    submit(page, credential),
  ]);
  const body = request.postDataJSON();
  assert.match(body.occurrence_id, uuid);
  assert.ok(Number.isFinite(Date.parse(body.occurred_at)));
  assert.deepEqual(body, { occurrence_id: body.occurrence_id, qr_payload: credential, occurred_at: body.occurred_at, direction: await page.getByRole('radio', { name: 'Entry', exact: true }).isChecked() ? 'entry' : 'exit' });
  return { request: body, response, body: await response.json() };
}

async function assertResult(page, lines, direction = 'entry') {
  const result = page.getByRole(direction === 'entry' ? 'alert' : 'status', { name: 'Scan result' });
  assert.equal(await result.getAttribute('class'), `result ${direction === 'entry' ? 'rejected' : 'neutral'}`);
  assert.equal(await result.textContent(), `${direction === 'entry' ? 'Rejected' : 'Exit not recorded'}${lines.join('')}`);
  assert.deepEqual(await result.locator('p').allTextContents(), lines);
  assert.equal(await result.locator('b').count(), 0);
  assert.equal(await page.getByRole('radio', { name: direction === 'entry' ? 'Entry' : 'Exit', exact: true }).isChecked(), true);
}

// The request and mint supply identity; the first attempted terminal write supplies
// the original latch and decision time. The repaired row never supplies its own expectation.
async function decisionEvidence(page, request, status) {
  const writes = await page.evaluate(id => window.__tkt565.writes.filter(write => write.value?.occurrenceId === id), request.occurrence_id);
  const mint = writes.find(write => write.method === 'add');
  assert.ok(mint?.events.includes('complete'), 'the submitted request must follow mint completion');
  const pending = mint.value;
  assert.match(pending.ownerId, uuid);
  assert.ok(Number.isFinite(pending.leaseExpiresAt));
  assert.deepEqual(pending, { occurrenceId: request.occurrence_id, qrPayload: request.qr_payload, occurredAt: request.occurred_at,
    createdAt: request.occurred_at, ownerId: pending.ownerId, leaseExpiresAt: pending.leaseExpiresAt,
    direction: request.direction, recordFormat: 1, state: 'PENDING', actuated: false });
  // Renewal may run while an injected failure is armed. Each committed renewal
  // may change only the lease, and supplies that lease independently of the final readback.
  const pendingAfter = entries => {
    let latest = pending;
    for (const write of entries) {
      if (write.value?.state !== 'PENDING' || !write.events.includes('complete')) continue;
      assert.ok(Number.isFinite(write.value.leaseExpiresAt));
      assert.deepEqual(write.value, { ...pending, leaseExpiresAt: write.value.leaseExpiresAt });
      latest = write.value;
    }
    return latest;
  };
  const firstIndex = writes.findIndex(write => write.value?.localOutcome?.family === 'terminal');
  const first = writes[firstIndex]?.value;
  assert.ok(first, 'the original terminal write must have been attempted');
  assert.match(first.localOutcome.latchId, uuid);
  assert.ok(Number.isFinite(Date.parse(first.decidedAt)));
  assert.deepEqual(first, { ...pendingAfter(writes.slice(0, firstIndex)), state: 'RESOLVED', decidedAt: first.decidedAt,
    localOutcome: { family: 'terminal', cause: 'denied', status, latchId: first.localOutcome.latchId, reason: null } });
  assert.equal(writes.some(write => write.value?.actuated === true), false);
  return { pending: pendingAfter(writes), first };
}

async function assertResolved(page, request, status, reason) {
  await completedReason(page, request.occurrence_id, reason);
  const evidence = await decisionEvidence(page, request, status);
  assert.deepEqual(await localRow(page, request.occurrence_id), { ...evidence.pending, state: 'RESOLVED', decidedAt: evidence.first.decidedAt,
    localOutcome: { family: 'terminal', cause: 'denied', status, latchId: evidence.first.localOutcome.latchId, reason } });
  return evidence;
}

async function assertPending(page, request, status) {
  const evidence = await decisionEvidence(page, request, status);
  assert.deepEqual(await localRow(page, request.occurrence_id), evidence.pending);
  return evidence;
}

async function readableMatrix(shared) {
  await withPage(shared, async ({ page, scans, batches }) => {
    const cases = [...entryDenials,
      { reason: 'already_redeemed', originalScanAt, lines: [`Already redeemed at ${localized(originalScanAt)}.`, 'Do not admit. Get an operator.', 'Reason: already_redeemed.'] },
      { reason: 'integrity_quarantined', originalScanAt, lines: ['Do not admit. Access has blocked another admission after an earlier integrity exception. Get an operator.', 'Reason: integrity_quarantined.'] },
    ];
    for (const status of [409, 422]) for (const entry of cases) {
      const ticket = shared.fixture();
      const before = lifecycle(shared.PG, ticket.ticket), beforeCount = scans.length;
      assert.equal(shared.quarantine(ticket.ticket), 0);
      await setClaimedTime(page, submittedAt);
      const injected = { decision: 'rejected', reason: entry.reason, ...(entry.originalScanAt ? { original_scan_at: entry.originalScanAt } : {}) };
      await page.route(scanPath, async route => {
        await setClaimedTime(page, decidedAt);
        await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(injected) });
      });
      try {
        const sent = await send(page, ticket.credential);
        assert.equal(sent.response.status(), status);
        assert.deepEqual(sent.body, injected);
        assert.equal(sent.request.direction, 'entry');
        assert.equal(sent.request.occurred_at, submittedAt);
        const evidence = await assertResolved(page, sent.request, status, entry.reason);
        assert.equal(evidence.first.decidedAt, decidedAt);
        await assertResult(page, entry.lines);
        assert.equal(scans.length, beforeCount + 1);
        assert.deepEqual(batches, []);
        assert.deepEqual(lifecycle(shared.PG, ticket.ticket), before);
        assert.equal(shared.quarantine(ticket.ticket), 0);
      } finally { await page.unroute(scanPath); }
    }
  });
}

async function liveAccess(shared) {
  await withPage(shared, async ({ page, scans, batches }) => {
    const invalid = shared.fixture(), beforeInvalid = lifecycle(shared.PG, invalid.ticket);
    const refused = await send(page, `${invalid.credential}-invalid`);
    assert.equal(refused.response.status(), 422);
    assert.equal(refused.request.direction, 'entry');
    assert.deepEqual(refused.body, { decision: 'rejected', reason: 'invalid_credential' });
    await assertResolved(page, refused.request, 422, 'invalid_credential');
    await assertResult(page, ['Do not admit. Access did not accept this credential. Get an operator.', 'Reason: invalid_credential.']);
    assert.deepEqual(lifecycle(shared.PG, invalid.ticket), beforeInvalid);
    assert.equal(shared.quarantine(invalid.ticket), 0);

    const ticket = shared.fixture();
    const updated = sql(shared.PG, 'access', `UPDATE slot_re_entry_policies SET mode='single',requires_exit=false WHERE slot_id=(SELECT slot_id FROM tickets WHERE id='${ticket.ticket}') RETURNING slot_id`);
    assert.equal(updated.trim().split('\n').filter(Boolean).length, 1);
    assert.deepEqual(shared.facts(ticket.ticket), []);
    const accepted = await send(page, ticket.credential);
    assert.equal(accepted.response.status(), 200);
    assert.equal(accepted.request.direction, 'entry');
    assert.deepEqual(accepted.body, { decision: 'accepted', scanned_at: accepted.body.scanned_at });
    await completedState(page, accepted.request.occurrence_id, 'ACTUATED');
    assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), `AcceptedEntry accepted. Admit.Entry recorded at ${localized(accepted.body.scanned_at)}.`);
    assert.deepEqual(shared.facts(ticket.ticket), [{ occurrence_id: accepted.request.occurrence_id, event_type: 'redeemed', occurred_at: new Date(accepted.body.scanned_at).toISOString() }]);
    const beforeDuplicate = lifecycle(shared.PG, ticket.ticket);
    const duplicate = await send(page, ticket.credential);
    assert.equal(duplicate.response.status(), 409);
    assert.equal(duplicate.request.direction, 'entry');
    assert.notEqual(duplicate.request.occurrence_id, accepted.request.occurrence_id);
    assert.deepEqual(duplicate.body, { decision: 'rejected', reason: 'already_redeemed', original_scan_at: accepted.body.scanned_at });
    await assertResolved(page, duplicate.request, 409, 'already_redeemed');
    await assertResult(page, [`Already redeemed at ${localized(duplicate.body.original_scan_at)}.`, 'Do not admit. Get an operator.', 'Reason: already_redeemed.']);
    assert.deepEqual(lifecycle(shared.PG, ticket.ticket), beforeDuplicate);
    assert.equal(shared.quarantine(ticket.ticket), 0);
    assert.equal(scans.length, 3);
    assert.deepEqual(batches, []);
  });
}

async function writeFailure(shared) {
  const cases = [
    { reason: 'refunded', status: 409, lines: ['Do not admit. This ticket was voided by a refund. Get an operator.', 'Reason: refunded.'] },
    { reason: 'unknown_future_reason', status: 422, lines: ['Do not admit. Access refused this scan. Get an operator.', 'Reason: unknown_future_reason.'] },
  ];
  for (const action of ['throw', 'abort']) for (const entry of cases) {
    await withPage(shared, async ({ page, scans, batches }) => {
      const control = shared.fixture(), denied = shared.fixture();
      const beforeDenied = lifecycle(shared.PG, denied.ticket);
      assert.equal(shared.quarantine(denied.ticket), 0);
      await page.route(scanPath, route => route.fulfill({ status: 503, contentType: 'application/json', body: '{"error":"unavailable"}' }));
      const queued = await send(page, control.credential);
      assert.equal(queued.response.status(), 503);
      await completedState(page, queued.request.occurrence_id, 'QUEUED');
      assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), 'Offline decisionServer response 503.Admit per venue offline policy.');
      const queuedRow = await localRow(page, queued.request.occurrence_id);
      await page.unroute(scanPath);
      await setClaimedTime(page, submittedAt);
      await page.route(scanPath, async route => {
        await arm(page, { id: route.request().postDataJSON().occurrence_id, state: 'RESOLVED', action });
        await setClaimedTime(page, decidedAt);
        await route.fulfill({ status: entry.status, contentType: 'application/json', body: JSON.stringify({ decision: 'rejected', reason: entry.reason }) });
      });
      const sent = await send(page, denied.credential);
      assert.equal(sent.response.status(), entry.status);
      assert.equal(sent.request.direction, 'entry');
      assert.equal(sent.request.occurred_at, submittedAt);
      await matched(page, { id: sent.request.occurrence_id, state: 'RESOLVED' });
      await page.getByText(repairText, { exact: true }).waitFor();
      await page.getByRole('heading', { name: 'Rejected', exact: true }).waitFor();
      await assertResult(page, entry.lines);
      const faultEvents = await page.evaluate(id => window.__tkt558IdbProbe.events.filter(event => event.id === id && event.state === 'RESOLVED'), sent.request.occurrence_id);
      assert.ok(faultEvents.some(event => event.event === 'matched' && event.action === action));
      if (action === 'abort') assert.ok(faultEvents.some(event => event.event === 'abort'));
      const { first } = await assertPending(page, sent.request, entry.status);
      assert.equal(first.decidedAt, decidedAt);
      await assertPending(page, sent.request, entry.status);
      assert.deepEqual(lifecycle(shared.PG, denied.ticket), beforeDenied);
      assert.equal(shared.quarantine(denied.ticket), 0);

      const attemptsBefore = await page.evaluate(id => window.__tkt558IdbProbe.events.filter(event => event.id === id && event.event === 'matched').length, sent.request.occurrence_id);
      await page.getByRole('button', { name: 'Retry storage' }).click();
      await page.waitForFunction(({ id, prior }) => window.__tkt558IdbProbe.events.filter(event => event.id === id && event.event === 'matched').length > prior, { id: sent.request.occurrence_id, prior: attemptsBefore });
      await page.getByText(repairText, { exact: true }).waitFor();
      await assertResult(page, entry.lines);
      await assertPending(page, sent.request, entry.status);
      const [syncRequest] = await Promise.all([
        page.waitForRequest(request => reconcilePath(request.url()) && request.method() === 'POST'),
        page.getByRole('button', { name: 'Sync queued scans' }).click(),
      ]);
      await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
      const expectedBatch = [{ occurrence_id: queued.request.occurrence_id, qr_payload: control.credential,
        occurred_at: queued.request.occurred_at, event_type: 'entry' }];
      assert.deepEqual(syncRequest.postDataJSON(), { occurrences: expectedBatch });
      assert.deepEqual(batches, [expectedBatch]);
      assert.deepEqual(await localRow(page, queued.request.occurrence_id), { ...queuedRow, state: 'SYNCED', result: 'recorded' });
      assert.deepEqual(shared.facts(control.ticket), [{ occurrence_id: queued.request.occurrence_id, event_type: 'entry', occurred_at: queued.request.occurred_at }]);
      assert.equal(shared.quarantine(control.ticket), 0);
      assert.deepEqual(lifecycle(shared.PG, denied.ticket), beforeDenied);
      assert.equal(shared.quarantine(denied.ticket), 0);
      await assertPending(page, sent.request, entry.status);
      await assertResult(page, entry.lines);
      await page.getByText(repairText, { exact: true }).waitFor();
      await page.evaluate(() => { window.__tkt558IdbProbe.rules = []; });
      await page.getByRole('button', { name: 'Retry storage' }).click();
      const repaired = await assertResolved(page, sent.request, entry.status, entry.reason);
      assert.deepEqual(repaired.first, first);
      await assertResult(page, entry.lines);
      assert.equal(scans.length, 2);
      assert.deepEqual(batches, [expectedBatch]);
      assert.deepEqual(lifecycle(shared.PG, denied.ticket), beforeDenied);
      assert.equal(shared.quarantine(denied.ticket), 0);
    });
  }
}

async function exitControl(shared) {
  await withPage(shared, async ({ page, scans, batches }) => {
    const ticket = shared.fixture(), before = lifecycle(shared.PG, ticket.ticket);
    await page.getByRole('radio', { name: 'Exit', exact: true }).check();
    const sent = await send(page, ticket.credential);
    assert.equal(sent.response.status(), 409);
    assert.equal(sent.request.direction, 'exit');
    assert.deepEqual(sent.body, { decision: 'rejected', reason: 'not_inside' });
    await assertResolved(page, sent.request, 409, 'not_inside');
    await assertResult(page, ['Exit not recorded; reason: not_inside. The holder may leave.'], 'exit');
    assert.deepEqual(lifecycle(shared.PG, ticket.ticket), before);
    assert.equal(shared.quarantine(ticket.ticket), 0);
    assert.equal(scans.length, 1);
    assert.deepEqual(batches, []);
  });
}

export async function entryReasons(shared) {
  for (const [name, journey] of [
    ['TKT-565 readable-matrix', readableMatrix],
    ['TKT-565 live-access', liveAccess],
    ['TKT-565 write-failure', writeFailure],
    ['TKT-565 exit-control', exitControl],
  ]) {
    await journey(shared);
    shared.check(name, true);
  }
}
