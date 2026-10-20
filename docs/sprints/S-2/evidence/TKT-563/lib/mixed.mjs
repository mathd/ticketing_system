// Seven-row mixed sync journey (plan section 2.2).
//
// Every queued row is created through the rendered controls while live scan
// transport and reconciliation are blocked. C is pre-applied to Access with a
// real reconciliation request built from its saved row. One real batch then
// syncs all seven rows. For the AX01 and AX02 diagnostics, one request field
// changes on that batch only, after the unmodified setup has returned.
//
// The retained observations sit beside the checks and never replace one. They
// are taken where the journey reads them. A failed wire assertion does not stop
// the readbacks that follow it. The evidence object is filled in place, so a
// journey exception keeps every observation taken before it.

import { randomUUID } from 'node:crypto';
import { POLICY } from './fixtures.mjs';
import {
  awaitQueued,
  canon,
  fact,
  observeOnlineSync,
  poll,
  RECONCILE_PATH,
  sanitizeDeep,
  scanAnswered,
  scanQueued,
  setDirection,
  sortById,
  VOIDED_PATH,
} from './kit.mjs';

export const MIX_SUMMARY = 'Synced 4 offline scan(s). 1 conflict flagged for the operator. 2 rejected scans were not recorded.';
export const CONFLICT_SUBJECT = 'platform.access.admission-conflict.alarm';
const DAY_MS = 24 * 60 * 60 * 1000;
const LABELS = ['A', 'B', 'C', 'D', 'E', 'F', 'G'];
// direction: the factual type sent on the wire. result: the reconcile answer.
// local: the terminal state and result the saved row must reach.
const EXPECT = {
  A: { direction: 'entry', result: 'recorded', local: 'SYNCED' },
  B: { direction: 'exit', result: 'recorded', local: 'SYNCED' },
  C: { direction: 'entry', result: 'synced', local: 'SYNCED' },
  D: { direction: 'entry', result: 'conflict', local: 'RESOLVED' },
  E: { direction: 'exit', result: 'rejected', local: 'RESOLVED' },
  F: { direction: 'exit', result: 'rejected', local: 'RESOLVED' },
  G: { refusal: true, result: 'recorded', local: 'SYNCED' },
};
const warningFor = (occurrence) => `Reconciliation rejected for ${occurrence}. This result is not recorded.`;

// Changes one request field on the named batch. Any other shape throws, and the
// caller records the failure, so the diagnostic cannot silently do nothing.
function applyBatchMutation(batch, mutation, occurrence) {
  if (mutation === 'AX01') {
    const row = batch.occurrences.find((item) => item.occurrence_id === occurrence.A);
    if (!row || row.event_type !== 'entry') throw new Error('AX01 target row is missing or is not an entry');
    row.event_type = 'exit';
    return;
  }
  if (mutation === 'AX02') {
    const row = batch.occurrences.find((item) => item.occurrence_id === occurrence.G);
    if (!row || !('local_decision' in row)) throw new Error('AX02 target row is missing or has no local_decision');
    delete row.local_decision;
    return;
  }
  throw new Error(`unknown batch mutation ${mutation}`);
}

const sameRows = (a, b) => canon([...a].sort(byId)) === canon([...b].sort(byId));
const byId = (a, b) => String(a.occurrenceId ?? a.occurrence_id ?? a.event_id ?? a.id).localeCompare(String(b.occurrenceId ?? b.occurrence_id ?? b.event_id ?? b.id));

export async function mixedJourney(ctx, mutation = null, evidence = {}) {
  const { H, token, fx, rb, checks, newJourneyContext, assetsDir, BASE, ACCESS_PORT, INTERNAL_SERVICE_TOKEN, ORGANIZER } = ctx;
  Object.assign(evidence, { mutation, prior: {}, prep: {}, sync: {}, noResend: {}, db: {} });
  const tickets = {
    A: fx.create('MIX-A', POLICY.multi),
    B: fx.create('MIX-B', POLICY.multi),
    C: fx.create('MIX-C', POLICY.multi),
    D: fx.create('MIX-D', POLICY.single),
    E: fx.create('MIX-E', POLICY.single),
    F: fx.create('MIX-F', POLICY.single),
    G: fx.create('MIX-G', POLICY.multi),
  };
  const ticketIds = LABELS.map((label) => tickets[label].ticket);

  // D is redeemed once through a separate context and a real rendered Entry.
  const priorContext = await newJourneyContext({ assetsDir });
  try {
    const priorPage = await priorContext.newPage();
    await H.pair(priorPage, token);
    const prior = await scanAnswered(priorPage, H, tickets.D.credential);
    evidence.prior = {
      status: prior.status,
      decision: prior.json?.decision ?? null,
      occurrence: prior.body.occurrence_id,
      scannedAt: prior.json?.scanned_at ?? null,
      wire: sanitizeDeep({ body: prior.body, json: prior.json }),
    };
    checks.check(
      'MIX-D-PRIOR-REDEMPTION',
      prior.status === 200 && prior.json?.decision === 'accepted' && canon(sortById(rb.facts(tickets.D.ticket))) === canon([fact(prior.body.occurrence_id, 'redeemed', prior.json?.scanned_at)]),
    );
    evidence.prior.facts = rb.facts(tickets.D.ticket);
  } finally {
    await priorContext.close();
  }
  const priorLifecycle = rb.rows(tickets.D.ticket);
  evidence.prior.lifecycle = sanitizeDeep(priorLifecycle);

  const context = await newJourneyContext({ assetsDir });
  const page = await context.newPage();
  let scanPosts = 0;
  let reconcilePosts = 0;
  page.on('request', (request) => {
    if (request.method() !== 'POST') return;
    if (H.scanPath(request.url())) scanPosts += 1;
    if (H.reconcilePath(request.url())) reconcilePosts += 1;
  });
  try {
    await H.pair(page, token);

    // G's ticket is refunded through the real endpoint, then the real refresh control pulls the feed.
    const completedBefore = new Set((await H.read(page, 'revocation_meta')).map((row) => row.completedAt));
    const refund = await fetch(`http://localhost:${ACCESS_PORT}/internal/orders/${tickets.G.order}/refunds`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Internal-Token': INTERNAL_SERVICE_TOKEN },
      body: JSON.stringify({ organizer_id: ORGANIZER, refund_id: randomUUID(), quantity: 1 }),
    });
    const refundBody = refund.ok ? await refund.json() : null;
    checks.check(
      'MIX-G-REFUND-VOIDS-TICKET',
      refund.status === 200 && Array.isArray(refundBody?.ticket_ids) && refundBody.ticket_ids.includes(tickets.G.ticket),
      `status ${refund.status}`,
    );
    await page.getByRole('button', { name: 'Refresh revocation list', exact: true }).click();
    const feedListed = await poll(async () => {
      const meta = await H.read(page, 'revocation_meta');
      const revoked = await H.read(page, 'revocations');
      const completed = meta.some((row) => row.completedAt && !completedBefore.has(row.completedAt));
      return completed && revoked.some((row) => row.ticketID === tickets.G.ticket);
    });
    checks.check('MIX-G-FEED-COMPLETE', feedListed === true, 'a new completed pull lists the refunded ticket');
    const gLifecycleAfterRefund = rb.rows(tickets.G.ticket);

    // Preparation: live scans and reconciliation requests fail at the transport.
    await page.route(H.scanPath, (route) => (route.request().method() === 'POST' ? route.abort('internetdisconnected') : route.continue()));
    await page.route(H.reconcilePath, (route) => (route.request().method() === 'POST' ? route.abort('internetdisconnected') : route.continue()));

    const submitted = {};
    const queuedHeading = { entry: 'Offline decision', exit: 'Offline Exit' };
    for (const label of ['A', 'B', 'C', 'D', 'E', 'F']) {
      const direction = EXPECT[label].direction;
      await setDirection(page, direction);
      const body = await scanQueued(page, H, tickets[label].credential);
      submitted[label] = body;
      await page.getByRole('heading', { name: queuedHeading[direction], exact: true }).waitFor();
      await awaitQueued(page, body.occurrence_id);
    }

    // G is a refused refund. Its one reconciliation attempt is the automatic best-effort sync.
    const scanPostsBeforeG = scanPosts;
    await setDirection(page, 'entry');
    await H.submit(page, tickets.G.credential);
    const gShown = await poll(() => page.getByRole('heading', { name: 'Not valid for entry', exact: true }).isVisible());
    checks.check('MIX-PREP-G-NO-SCAN', gShown === true && scanPosts === scanPostsBeforeG, `scan requests after G ${scanPosts - scanPostsBeforeG}`);
    await poll(() => reconcilePosts >= 1);
    const failureNote = await poll(async () =>
      ((await page.locator('.sync-note').first().textContent().catch(() => null)) ?? '').startsWith('Sync failed: No connection.'),
    );
    checks.check('MIX-PREP-G-BEST-EFFORT', reconcilePosts === 1 && failureNote === true, `reconcile requests ${reconcilePosts}`);

    const prepRows = await poll(async () => {
      const rows = await H.read(page);
      return rows.length === 7 && rows.every((row) => row.state === 'QUEUED') ? rows : null;
    });
    const pre = new Map((prepRows ?? []).map((row) => [row.occurrenceId, row]));
    const gRow = (prepRows ?? []).find((row) => row.localDecision === 'revocation_refused');
    const occ = { G: gRow?.occurrenceId };
    for (const label of ['A', 'B', 'C', 'D', 'E', 'F']) occ[label] = submitted[label]?.occurrence_id;
    checks.check('MIX-PREP-ROWS', prepRows !== null && LABELS.every((label) => occ[label] && pre.has(occ[label])), `rows ${prepRows?.length ?? 0}`);
    const shapeOk = LABELS.every((label) => {
      const row = pre.get(occ[label]);
      if (!row) return false;
      if (EXPECT[label].refusal) {
        return row.localDecision === 'revocation_refused' && row.direction === undefined && row.localOutcome?.cause === 'local_refusal' && row.qrPayload === tickets.G.credential;
      }
      return row.direction === EXPECT[label].direction && row.localDecision === undefined && row.localOutcome?.cause === 'transport_failure' && row.qrPayload === tickets[label].credential && row.occurredAt === submitted[label].occurred_at;
    });
    checks.check('MIX-PREP-ROW-SHAPES', shapeOk);
    evidence.prep = {
      rowsBeforeSync: sanitizeDeep(prepRows ?? []),
      occurrences: occ,
      submitted: sanitizeDeep(submitted),
    };

    // Retained database observations, read where the journey reads them. No check depends on them.
    const observe = () => {
      const refs = new Set([...ticketIds, ...Object.values(occ).filter(Boolean)]);
      const alarms = rb.alarmRows();
      const referencing = alarms.filter((row) => refs.has(row.envelope?.data?.occurrence_id) || refs.has(row.envelope?.data?.ticket_id));
      return sanitizeDeep({
        facts: Object.fromEntries(LABELS.map((label) => [label, rb.facts(tickets[label].ticket)])),
        lifecycle: Object.fromEntries(LABELS.map((label) => [label, rb.rows(tickets[label].ticket)])),
        quarantine: Object.fromEntries(LABELS.map((label) => [label, rb.quarantineCount(tickets[label].ticket)])),
        localDecisions: {
          occurrenceG: occ.G ? rb.localDecisionsForOccurrence(occ.G) : null,
          ticketG: rb.localDecisionsForTicket(tickets.G.ticket),
        },
        priorRedemption: {
          occurrence: evidence.prior.occurrence ?? null,
          lifecycleRow: rb.rows(tickets.D.ticket).find((row) => row.id === evidence.prior.occurrence) ?? null,
        },
        alarms: {
          total: alarms.length,
          referencing,
          otherEventIds: alarms.filter((row) => !referencing.includes(row)).map((row) => row.event_id),
        },
      });
    };

    // C is pre-applied to Access with its saved values. Its local row stays QUEUED.
    const rowC = pre.get(occ.C);
    const preBody = { occurrences: [{ qr_payload: rowC.qrPayload, occurrence_id: rowC.occurrenceId, occurred_at: rowC.occurredAt, event_type: rowC.direction }] };
    const cLifecycleBeforePreapply = rb.rows(tickets.C.ticket);
    const preApply = await context.request.post(`${BASE}${RECONCILE_PATH}`, { headers: { 'X-Scanner-Token': token }, data: preBody });
    const preJson = preApply.ok() ? await preApply.json().catch(() => null) : null;
    const preResult = preJson?.results?.[0];
    checks.check(
      'MIX-C-PREAPPLY-RECORDED',
      preApply.status() === 200 && preJson?.results?.length === 1 && preResult?.occurrence_id === occ.C && preResult?.result === 'recorded' && Date.parse(preResult?.occurred_at) === Date.parse(rowC.occurredAt),
      `status ${preApply.status()}`,
    );
    checks.check('MIX-C-PREAPPLY-FACT', canon(rb.facts(tickets.C.ticket)) === canon([fact(occ.C, 'entry', rowC.occurredAt)]));
    const cLifecycleBeforeSync = rb.rows(tickets.C.ticket);
    evidence.sync.preapply = {
      request: sanitizeDeep(preBody),
      status: preApply.status(),
      json: preJson,
      lifecycleBefore: sanitizeDeep(cLifecycleBeforePreapply),
      lifecycleAfter: sanitizeDeep(cLifecycleBeforeSync),
    };
    evidence.db.beforeSync = observe();

    // The one real batch. The wire recorder keeps the body that reaches Access.
    await page.unroute(H.reconcilePath);
    const batchIds = new Set(LABELS.map((label) => occ[label]));
    const wireSent = [];
    const wireRequests = [];
    let applied = 0;
    let mutationError = null;
    await page.route(H.reconcilePath, async (route) => {
      const request = route.request();
      if (request.method() !== 'POST') return route.continue();
      const original = request.postDataJSON();
      const isBatch = Array.isArray(original.occurrences) && original.occurrences.length === batchIds.size && original.occurrences.every((row) => batchIds.has(row.occurrence_id));
      let sent = original;
      if (mutation && isBatch && applied === 0) {
        sent = structuredClone(original);
        try {
          applyBatchMutation(sent, mutation, occ);
          applied += 1;
        } catch (error) {
          mutationError = String(error?.message ?? error);
          wireSent.push(structuredClone(original));
          wireRequests.push({ batch: isBatch, original: sanitizeDeep(original), sent: null, changed: false, aborted: true });
          return route.abort('failed');
        }
      }
      wireSent.push(structuredClone(sent));
      wireRequests.push({ batch: isBatch, original: sanitizeDeep(original), sent: sanitizeDeep(sent), changed: sent !== original, aborted: false });
      return sent === original ? route.continue() : route.continue({ postData: JSON.stringify(sent) });
    });
    const syncSeen = page.waitForResponse((response) => response.request().method() === 'POST' && H.reconcilePath(response.url())).catch(() => null);
    const clickError = await page.getByRole('button', { name: 'Sync queued scans', exact: true }).click().then(
      () => null,
      (error) => String(error?.message ?? error).slice(0, 500),
    );
    const syncResponse = await syncSeen;
    const syncJson = syncResponse ? await syncResponse.json().catch(() => null) : null;
    const summary = await poll(async () => {
      const text = (await page.locator('.sync-note').first().textContent().catch(() => null)) ?? '';
      return text.startsWith('Synced ') ? text : null;
    });
    const finalRows = await poll(async () => {
      const rows = await H.read(page);
      return rows.length === 7 && rows.every((row) => row.state === 'SYNCED' || row.state === 'RESOLVED') ? rows : null;
    });
    checks.check('MIX-NO-QUEUED-AFTER-SYNC', finalRows !== null, 'every row is SYNCED or RESOLVED after the batch');
    const post = new Map((finalRows ?? []).map((row) => [row.occurrenceId, row]));
    const warnings = (await page.locator('p[role="alert"]').allTextContents()).filter((text) => text.startsWith('Reconciliation rejected for'));
    evidence.sync.request = { clickError, status: syncResponse ? syncResponse.status() : null, json: syncJson, summary, warnings };
    evidence.sync.wire = wireRequests;
    evidence.sync.rowsAfterSync = sanitizeDeep(finalRows);
    evidence.db.afterSync = observe();

    checks.check('MIX-MUTATION-BOOKKEEPING', applied === (mutation ? 1 : 0) && mutationError === null, `applied ${applied} error ${mutationError}`);
    checks.check('MIX-WIRE-ONE-BATCH', wireSent.length === 1, `observed ${wireSent.length}`);
    const batch = wireSent[0] ?? { occurrences: [] };
    const wireRow = (label) => batch.occurrences.find((row) => row.occurrence_id === occ[label]);
    checks.check(
      'MIX-WIRE-SET',
      batch.occurrences.length === 7 && canon(batch.occurrences.map((row) => row.occurrence_id).sort()) === canon([...batchIds].sort()),
    );
    checks.check(
      'MIX-WIRE-SHAPE',
      LABELS.every((label) => {
        const row = wireRow(label);
        if (!row) return false;
        const keys = Object.keys(row).sort().join(',');
        return EXPECT[label].refusal ? keys === 'local_decision,occurrence_id,occurred_at,qr_payload' : keys === 'event_type,occurrence_id,occurred_at,qr_payload';
      }),
    );
    for (const label of ['A', 'B', 'C', 'D', 'E', 'F']) {
      const row = wireRow(label);
      checks.check(
        `MIX-${label}-WIRE-IDENTITY`,
        !!row && row.qr_payload === tickets[label].credential && row.occurrence_id === occ[label] && row.occurred_at === submitted[label].occurred_at,
      );
      checks.check(`MIX-${label}-WIRE-DIRECTION`, !!row && row.event_type === EXPECT[label].direction, `event_type ${row?.event_type}`);
    }
    const gWire = wireRow('G');
    checks.check('MIX-G-WIRE-IDENTITY', !!gWire && gWire.qr_payload === tickets.G.credential && gWire.occurrence_id === occ.G && gWire.occurred_at === gRow?.occurredAt);
    checks.check('MIX-G-WIRE-LOCAL-DECISION', !!gWire && gWire.local_decision === 'revocation_refused' && !('event_type' in gWire), `keys ${gWire ? Object.keys(gWire).join(',') : 'none'}`);

    // Response pairs and times.
    const results = Array.isArray(syncJson?.results) ? syncJson.results : [];
    const pairs = results.map((item) => `${item.occurrence_id}=${item.result}`).sort();
    const expectedPairs = LABELS.map((label) => `${occ[label]}=${EXPECT[label].result}`).sort();
    checks.check('MIX-RESPONSE-PAIRS', results.length === 7 && canon(pairs) === canon(expectedPairs), `pairs ${canon(pairs)}`);
    const timesOk = LABELS.every((label) => {
      const item = results.find((entry) => entry.occurrence_id === occ[label]);
      if (!item || item.skew_flagged !== undefined) return false;
      if (EXPECT[label].result === 'rejected') return item.occurred_at === undefined;
      const claimed = label === 'G' ? gRow?.occurredAt : submitted[label].occurred_at;
      return claimed !== undefined && Date.parse(item.occurred_at) === Date.parse(claimed);
    });
    checks.check('MIX-RESPONSE-TIMES', timesOk);
    evidence.sync.results = results;

    checks.check('MIX-SUMMARY', summary === MIX_SUMMARY, `observed ${JSON.stringify(summary)}`);
    checks.check('MIX-REJECT-WARNINGS', canon([...warnings].sort()) === canon([warningFor(occ.E), warningFor(occ.F)].sort()), `observed ${canon(warnings)}`);

    // Saved rows against final rows: only the terminal state and result may change.
    for (const label of LABELS) {
      const before = pre.get(occ[label]);
      const after = post.get(occ[label]);
      const { state: _beforeState, result: _beforeResult, ...beforeRest } = before ?? {};
      const { state, result, ...afterRest } = after ?? {};
      checks.check(
        `MIX-${label}-LOCAL`,
        !!before && !!after && state === EXPECT[label].local && result === EXPECT[label].result && canon(beforeRest) === canon(afterRest),
        `state ${after?.state} result ${after?.result}`,
      );
    }

    // Access readbacks for each fixture.
    checks.check('MIX-A-DB-FACT', canon(sortById(rb.facts(tickets.A.ticket))) === canon([fact(occ.A, 'entry', submitted.A.occurred_at)]));
    checks.check('MIX-A-DB-DIRECTION', rb.facts(tickets.A.ticket).find((item) => item.occurrence_id === occ.A)?.event_type === 'entry');
    checks.check('MIX-B-DB-FACT', canon(sortById(rb.facts(tickets.B.ticket))) === canon([fact(occ.B, 'exit', submitted.B.occurred_at)]));
    checks.check('MIX-C-DB-FACT', canon(sortById(rb.facts(tickets.C.ticket))) === canon([fact(occ.C, 'entry', submitted.C.occurred_at)]));
    checks.check('MIX-C-REPLAY-ROW', canon(rb.rows(tickets.C.ticket)) === canon(cLifecycleBeforeSync), 'C lifecycle row is byte-equal before and after the batch');
    checks.check(
      'MIX-D-DB-FACT',
      canon(sortById(rb.facts(tickets.D.ticket))) === canon(sortById([fact(evidence.prior.occurrence, 'redeemed', evidence.prior.scannedAt), fact(occ.D, 'duplicate_admit', submitted.D.occurred_at)])),
    );
    const priorNow = rb.rows(tickets.D.ticket).find((row) => row.id === evidence.prior.occurrence);
    const priorThen = priorLifecycle.find((row) => row.id === evidence.prior.occurrence);
    checks.check('MIX-D-PRIOR-PRESERVED', !!priorNow && canon(priorNow) === canon(priorThen));
    checks.check('MIX-E-DB-FACT', rb.facts(tickets.E.ticket).length === 0 && !rb.facts(tickets.E.ticket).some((item) => item.occurrence_id === occ.E));
    checks.check('MIX-F-DB-FACT', rb.facts(tickets.F.ticket).length === 0 && !rb.facts(tickets.F.ticket).some((item) => item.occurrence_id === occ.F));
    checks.check(
      'MIX-G-DB-DECISION',
      canon(occ.G ? rb.localDecisionsForOccurrence(occ.G) : []) ===
        canon([{ occurrence_id: occ.G, ticket_id: tickets.G.ticket, organizer_id: ORGANIZER, occurred_at: gRow ? new Date(gRow.occurredAt).toISOString() : null, decision: 'revocation_refused' }]),
    );
    checks.check('MIX-G-LIFECYCLE', canon(rb.rows(tickets.G.ticket)) === canon(gLifecycleAfterRefund), 'no lifecycle row is written for the refusal');
    checks.check('MIX-QUARANTINE', ticketIds.every((ticket) => rb.quarantineCount(ticket) === 0));

    // The D conflict alarm is owed once. The other fixtures owe none, in any subject.
    const alarms = rb.alarmRows();
    const dRows = alarms.filter((row) => row.envelope?.data?.occurrence_id === occ.D);
    const dRow = dRows[0];
    const dData = dRow?.envelope?.data ?? {};
    checks.check('MIX-D-ALARM-ROW', dRows.length === 1, `observed ${dRows.length}`);
    checks.check('MIX-SKEW-PREMISE', Math.abs(Date.parse(submitted.D.occurred_at) - Date.now()) <= DAY_MS, 'the D claimed time is inside the 24 hour bound');
    checks.check(
      'MIX-D-ALARM-PAYLOAD',
      !!dRow &&
        dRow.subject === CONFLICT_SUBJECT &&
        dRow.envelope.type === CONFLICT_SUBJECT &&
        dRow.envelope.schema === 1 &&
        dRow.envelope.id === dRow.event_id &&
        dData.alarm_id === dRow.event_id &&
        dData.occurrence_id === occ.D &&
        dData.ticket_id === tickets.D.ticket &&
        dData.organizer_id === ORGANIZER &&
        Date.parse(dData.device_occurred_at) === Date.parse(submitted.D.occurred_at) &&
        dData.skew_flagged === false,
      `payload ${JSON.stringify(dData)}`,
    );
    const otherIds = new Set(LABELS.filter((label) => label !== 'D').flatMap((label) => [occ[label], tickets[label].ticket]));
    const referencing = alarms.filter((row) => otherIds.has(row.envelope?.data?.occurrence_id) || otherIds.has(row.envelope?.data?.ticket_id));
    checks.check('MIX-NO-ALARM-OTHERS', referencing.length === 0, `observed ${referencing.length}`);
    const dTicketRows = alarms.filter((row) => row.envelope?.data?.ticket_id === tickets.D.ticket);
    checks.check('MIX-D-ALARM-EXCLUSIVE', dTicketRows.length === 1 && dRow?.event_id === dTicketRows[0]?.event_id, `observed ${dTicketRows.length}`);

    // Re-sync check. An online event must send nothing and change nothing. The
    // baseline reads come before the event. The causal reads decide the settled check.
    const factsBefore = canon(ticketIds.map((ticket) => sortById(rb.facts(ticket))));
    const decisionsBefore = canon(occ.G ? rb.localDecisionsForOccurrence(occ.G) : []);
    const alarmsBefore = canon(rb.alarmRows().map((row) => [row.event_id, row.subject]).sort());
    const rowsBefore = await H.read(page);
    const reconcileBefore = reconcilePosts;
    const wireBefore = wireSent.length;
    const voidedSeen = page.waitForResponse((response) => response.request().method() === 'GET' && new URL(response.url()).pathname === VOIDED_PATH).then(() => true, () => false);
    const online = await observeOnlineSync(page, { expectedIds: LABELS.map((label) => occ[label]).filter(Boolean) });
    const feedPulled = await voidedSeen;
    const rowsAfter = await H.read(page);
    evidence.db.afterOnline = observe();
    const reconcileRequests = reconcilePosts - reconcileBefore;
    const causalSettled = online.armed && online.reads?.settled === true;
    checks.check('MIX-NO-RESEND-SETTLED', causalSettled && feedPulled, `armed ${online.armed} error ${online.error} feed ${feedPulled} failures ${canon(online.reads?.failures ?? [])}`);
    checks.check('MIX-NO-RESEND-REQUESTS', reconcilePosts === reconcileBefore && wireSent.length === wireBefore, `reconcile requests after online ${reconcileRequests}`);
    checks.check(
      'MIX-NO-RESEND-STATE',
      canon(ticketIds.map((ticket) => sortById(rb.facts(ticket)))) === factsBefore &&
        canon(occ.G ? rb.localDecisionsForOccurrence(occ.G) : []) === decisionsBefore &&
        canon(rb.alarmRows().map((row) => [row.event_id, row.subject]).sort()) === alarmsBefore &&
        sameRows(rowsBefore, rowsAfter),
    );
    evidence.noResend = {
      reconcileRequests,
      settled: causalSettled,
      feedPulled,
      online: { armed: online.armed, epochStart: online.epochStart, error: online.error, quietMs: online.quietMs, reads: online.reads, transactions: online.transactions },
      rowsBeforeOnline: sanitizeDeep(rowsBefore),
      rowsAfterOnline: sanitizeDeep(rowsAfter),
    };
    return evidence;
  } finally {
    await context.close();
  }
}
