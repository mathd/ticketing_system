import { assert, blank, pair, submit, read, seedLegacy, scanPath, reconcilePath, installProbe, arm, matched, release } from './scanner-controls.mjs';
import { randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';

export async function storageCore({ browser, BASE, token, check, fixture }) {
  // B2: only the terminal id fails. The queued control and other pages remain usable.
  const context = await browser.newContext({ baseURL: BASE });
  await installProbe(context);
  const page = await context.newPage();
  try {
    await pair(page, token);
    await page.route(scanPath, route => route.fulfill({ status: 503, body: 'not decided' }));
    const controlRequest = page.waitForRequest(r => scanPath(r.url()) && r.method() === 'POST');
    await submit(page, fixture().credential);
    const control = (await controlRequest).postDataJSON().occurrence_id;
    await page.getByRole('heading', { name: 'Offline decision' }).waitFor();
    await page.waitForFunction(id => window.__tkt558IdbProbe.events.some(e => e.id === id && e.state === 'QUEUED' && e.event === 'complete'), control);
    await page.unroute(scanPath);
    await page.route(scanPath, route => route.fulfill({ status: 409, contentType: 'application/json', body: '{"decision":"rejected","reason":"refunded"}' }));
    // The refusal id is captured at the transport boundary, before its response.
    let terminal;
    await page.unroute(scanPath);
    await page.route(scanPath, async route => {
      terminal = route.request().postDataJSON().occurrence_id;
      await arm(page, { id: terminal, state: 'RESOLVED', action: 'throw' });
      await route.fulfill({ status: 409, contentType: 'application/json', body: '{"decision":"rejected","reason":"refunded"}' });
    });
    await submit(page, 'denial-with-failed-write');
    await page.getByRole('heading', { name: 'Rejected', exact: true }).waitFor();
    await matched(page, { id: terminal, state: 'RESOLVED' });
    await page.getByText('The outcome write needs repair. Keep this page open and retry storage.').waitFor();
    const pending = (await read(page)).find(row => row.occurrenceId === terminal);
    assert.equal(pending.state, 'PENDING');
    await page.clock.install({ time: new Date() });
    await arm(page, { id: terminal, state: 'PENDING', action: 'abort' });
    await page.clock.runFor(31_001);
    assert.ok((await read(page)).find(row => row.occurrenceId === terminal).leaseExpiresAt < await page.evaluate(() => Date.now()));
    await page.evaluate(() => { for (const db of window.__tkt558IdbProbe.connections) { db.close(); db.dispatchEvent(new Event('close')); } });
    await page.getByRole('button', { name: 'Retry storage' }).click();
    await page.waitForFunction(() => !document.querySelector('form[aria-label="Ticket scan"] button[type="submit"]')?.disabled);
    const second = await context.newPage(); await pair(second, token);
    assert.equal(await second.evaluate(owner => navigator.locks.request(`ticketing.scanner.occurrence-owner.${owner}`, { ifAvailable: true }, lock => !!lock), pending.ownerId), false);
    const seen = [];
    context.on('request', request => { if (request.method() === 'POST' && reconcilePath(request.url())) seen.push(...request.postDataJSON().occurrences.map(row => row.occurrence_id)); });
    await page.getByRole('button', { name: 'Sync queued scans' }).click();
    await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    await second.getByRole('button', { name: 'Sync queued scans' }).click();
    assert.deepEqual(seen, [control]);
    assert.equal((await read(second)).find(row => row.occurrenceId === terminal).state, 'PENDING');
    await page.evaluate(() => { window.__tkt558IdbProbe.rules = []; });
    await page.getByRole('button', { name: 'Retry storage' }).click();
    await page.waitForFunction(id => window.__tkt558IdbProbe.events.some(e => e.id === id && e.state === 'RESOLVED' && e.event === 'complete'), terminal);
    await page.reload();
    await page.getByRole('button', { name: 'Check ticket' }).waitFor();
    const { latchId, ...outcome } = (await read(page)).find(row => row.occurrenceId === terminal).localOutcome;
    assert.match(latchId, /^[0-9a-f-]{36}$/);
    assert.deepEqual(outcome, { family: 'terminal', cause: 'denied', reason: 'refunded', status: 409 });
    check('B2 failed TERM stays protected through expiry, connection replacement and genuine cross-page owner exclusion', true);
  } finally { await context.close(); }

  // B2 mark completion survives an actual versionchange event while the write is held.
  for (const finish of ['commit', 'abort']) {
    const context = await browser.newContext({ baseURL: BASE }); await installProbe(context);
    const page = await context.newPage();
    try {
      await pair(page, token);
      await arm(page, { state: 'ACTUATED', action: 'hold', once: true, name: 'mark' });
      await page.route(scanPath, route => route.fulfill({ status: 200, contentType: 'application/json', body: '{"decision":"accepted","scanned_at":"2026-10-18T12:34:56.789Z"}' }));
      await submit(page, 'mark-boundary');
      await matched(page, { state: 'ACTUATED' });
      assert.equal(await page.getByRole('heading', { name: 'Accepted', exact: true }).count(), 0);
      await page.evaluate(() => {
        const request = indexedDB.open('gate-occurrences', 4);
        window.__tkt558Upgrade = request;
        request.onsuccess = () => request.result.close();
      });
      await page.getByText('Scanner storage is not ready. No ticket was checked.').waitFor();
      await release(page, 'mark', finish === 'abort');
      await page.getByRole('heading', { name: finish === 'commit' ? 'Accepted' : 'Operator required', exact: true }).waitFor();
      const events = await page.evaluate(() => window.__tkt558IdbProbe.events.filter(e => e.state === 'ACTUATED'));
      assert.ok(events.some(e => e.event === (finish === 'commit' ? 'complete' : 'abort')));
      assert.equal(await page.getByText('Entry accepted. Admit.', { exact: true }).count(), finish === 'commit' ? 1 : 0);
      check(`B2 real Chrome mark ${finish} is observed through versionchange before an instruction`, true);
    } finally { await context.close(); }
  }

  // B4 uses literal old rows. The export expectation does not use the serializer.
  for (const version of [1, 2]) {
    const context = await browser.newContext({ baseURL: BASE, acceptDownloads: true }); await installProbe(context);
    const page = await blank(context);
    const heldA = { occurrenceId: `held-a-${randomUUID()}`, state: 'PENDING', qrPayload: 'private-credential', occurredAt: '2026-10-18T12:34:56.789Z', actuated: false, ownerId: undefined, extra: { preserved: true } };
    const heldB = { occurrenceId: `held-b-${randomUUID()}`, state: 'unknown', qrPayload: 'other-private-credential' };
    const m02 = { occurrenceId: randomUUID(), state: 'QUEUED', qrPayload: fixture().credential, occurredAt: '2026-10-18T12:34:56.123Z', actuated: false };
    try {
      await seedLegacy(page, version, [heldA, heldB, m02]); await pair(page, token);
      await page.getByText('2 held record(s) need operator review. These records will not sync.').waitFor();
      const selected = page.getByLabel(`Held record ${heldA.occurrenceId}`);
      await selected.check();
      assert.equal(await page.getByRole('button', { name: 'Acknowledge selected held records' }).isDisabled(), true);
      const [download] = await Promise.all([page.waitForEvent('download'),
        page.getByRole('button', { name: 'Export local records' }).click({ timeout: 5000 })]);
      const file = JSON.parse(await readFile(await download.path(), 'utf8'));
      await page.getByText('Export requested. The browser cannot confirm that the file was saved.').waitFor();
      assert.equal(file.schemaVersion, 1);
      assert.deepEqual(file.source, { databaseName: 'gate-occurrences', databaseVersion: 3, legacyFormat: 0, currentFormat: 1 });
      assert.equal(file.held.length, 2);
      const exportedA = file.held.find(row => row.occurrenceId === heldA.occurrenceId);
      const fields = {};
      for (const key of ['occurrenceId','qrPayload','occurredAt','createdAt','state','actuated','result','ownerId','leaseExpiresAt','localDecision','direction','extra']) fields[key] = !Object.hasOwn(heldA, key) ? { presence: 'absent' } : heldA[key] === undefined ? { presence: 'undefined' } : { presence: 'value', value: heldA[key] };
      assert.deepEqual(exportedA, { occurrenceId: heldA.occurrenceId, legacyClass: 'M01', holdReason: 'legacy_pending', format: 0, type: 'normal', fields, disposal: { status: 'unresolved' } });
      assert.deepEqual(file.legacyQueued, [{ occurrenceId: m02.occurrenceId, legacyClass: 'M02', state: { presence: 'value', value: 'QUEUED' }, occurredAt: { presence: 'value', value: m02.occurredAt } }]);
      await arm(page, { store: 'held_disposals', action: 'abort', once: true });
      await page.getByRole('button', { name: 'Acknowledge selected held records' }).click();
      await page.getByText('Acknowledgement failed. Held records remain unresolved.').waitFor();
      assert.deepEqual(await read(page, 'held_disposals'), []);
      await page.getByRole('button', { name: 'Acknowledge selected held records' }).click();
      await page.getByText('1 held record(s) need operator review. These records will not sync.').waitFor();
      assert.equal((await read(page, 'held_disposals')).length, 1);
      assert.deepEqual((await read(page)).find(row => row.occurrenceId === heldA.occurrenceId), heldA);
      const seen = [];
      page.on('request', request => { if (request.method() === 'POST' && reconcilePath(request.url())) seen.push(...request.postDataJSON().occurrences); });
      await page.getByRole('button', { name: 'Sync queued scans' }).click();
      await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
      assert.deepEqual(seen.map(row => row.occurrence_id), [m02.occurrenceId]);
      await page.reload(); await page.getByText('1 held record(s) need operator review. These records will not sync.').waitFor();
      const [nextDownload] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Export local records' }).click({ timeout: 5000 })]);
      const nextFile = JSON.parse(await readFile(await nextDownload.path(), 'utf8'));
      assert.equal(nextFile.held.find(row => row.occurrenceId === heldA.occurrenceId).disposal.status, 'acknowledged');
      assert.equal(nextFile.held.find(row => row.occurrenceId === heldB.occurrenceId).disposal.status, 'unresolved');
      assert.equal(nextFile.legacyQueued[0].state.value, 'SYNCED');
      check(`B4 v${version} real download preserves credentials, subsecond time and own undefined; selected acknowledgement survives reload`, true);
    } finally { await context.close(); }
  }
  // Isolated M02 stays exportable at held zero, including after its valid sibling retires.
  const legacyContext = await browser.newContext({ baseURL: BASE, acceptDownloads: true });
  const legacyPage = await blank(legacyContext);
  try {
    const goodTicket = fixture();
    const odd = { occurrenceId: randomUUID(), qrPayload: { invalid: 'credential' }, occurredAt: '2026-10-18T12:34:56.234Z', state: 'QUEUED', actuated: false };
    const good = { occurrenceId: randomUUID(), qrPayload: goodTicket.credential, occurredAt: '2026-10-18T12:34:56.345Z', state: 'QUEUED', actuated: false };
    await seedLegacy(legacyPage, 2, [odd, good]); await pair(legacyPage, token);
    const batches = []; legacyPage.on('request', request => { if(request.method()==='POST' && reconcilePath(request.url())) batches.push(request.postDataJSON().occurrences); });
    for(const state of ['QUEUED','SYNCED']) {
      assert.equal(await legacyPage.getByRole('button',{name:'Acknowledge selected held records'}).count(),0);
      const [download]=await Promise.all([legacyPage.waitForEvent('download'),legacyPage.getByRole('button',{name:'Export local records'}).click({ timeout: 5000 })]);
      const file=JSON.parse(await readFile(await download.path(),'utf8'));
      assert.deepEqual(file.held,[]);
      assert.deepEqual(file.legacyQueued.sort((a,b)=>a.occurrenceId.localeCompare(b.occurrenceId)),[
        {occurrenceId:odd.occurrenceId,legacyClass:'M02',state:{presence:'value',value:'QUEUED'},occurredAt:{presence:'value',value:odd.occurredAt}},
        {occurrenceId:good.occurrenceId,legacyClass:'M02',state:{presence:'value',value:state},occurredAt:{presence:'value',value:good.occurredAt}},
      ].sort((a,b)=>a.occurrenceId.localeCompare(b.occurrenceId)));
      if(state==='QUEUED'){
        await legacyPage.getByRole('button',{name:'Sync queued scans'}).click();await legacyPage.getByText('Synced 1 offline scan(s).',{exact:true}).waitFor();
        await legacyPage.getByText(`Pre-upgrade record ${odd.occurrenceId} could not sync. It remains queued and listed in the export.`,{exact:true}).waitFor();
      }
    }
    assert.ok(batches.some(rows=>rows.length===1 && rows[0].occurrence_id===odd.occurrenceId && typeof rows[0].qr_payload==='object'));
    assert.ok(batches.some(rows=>rows.length===1 && rows[0].occurrence_id===good.occurrenceId));
    check('B4 isolated odd M02 stays queued and exportable at held zero while the valid control reaches Access and remains exported after retirement',true);
  } finally { await legacyContext.close(); }

}
