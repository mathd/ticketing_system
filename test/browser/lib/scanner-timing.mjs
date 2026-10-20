import { writeFileSync, mkdirSync } from 'node:fs';
import { cpus, release as osRelease, platform, arch } from 'node:os';
import { assert, pair, submit, read, scanPath, installProbe, arm, matched, release } from './scanner-controls.mjs';

async function timedContext(browser, BASE) {
  const context = await browser.newContext({ baseURL: BASE });
  await installProbe(context);
  await context.addInitScript(() => {
    const wall = Date.now.bind(Date), mono = performance.now.bind(performance), timeout = window.setTimeout.bind(window);
    const state = window.__tkt559Clock = { offset: 0, timers: [] };
    Date.now = () => wall() + state.offset;
    Object.defineProperty(performance, 'now', { value: () => mono() + state.offset });
    window.setTimeout = (callback, delay, ...args) => {
      const id = timeout(callback, delay, ...args);
      if (delay >= 1000 && delay <= 30000) state.timers.push({ id, callback, args });
      return id;
    };
    state.advance = value => { state.offset = value; };
    state.wake = () => {
      const timers = state.timers.splice(0);
      for (const timer of timers) { clearTimeout(timer.id); timer.callback(...timer.args); }
    };
    const nativeFetch = window.fetch.bind(window);
    window.__tkt559Wait = { mode: null, started: false, release: null };
    window.fetch = async (...args) => {
      const response = await nativeFetch(...args);
      if (!(args[0] instanceof Request) || new URL(args[0].url).pathname !== '/api/access/scans') return response;
      const wait = window.__tkt559Wait;
      if (wait.mode === 'headers') {
        wait.started = true;
        await new Promise(resolve => { wait.release = resolve; });
      }
      if (wait.mode === 'body') {
        const json = response.json.bind(response);
        response.json = async () => {
          wait.started = true;
          await new Promise(resolve => { wait.release = resolve; });
          return json();
        };
      }
      return response;
    };
  });
  return context;
}
const advance = (page, value, wake = true) => page.evaluate(({ value, wake }) => {
  window.__tkt559Clock.advance(value);
  if (wake) window.__tkt559Clock.wake();
}, { value, wake });
const releaseResponse = page => page.evaluate(() => window.__tkt559Wait.release?.());
const resultText = async page => await page.locator('[aria-label="Scan result"]').count()
  ? page.locator('[aria-label="Scan result"]').textContent() : null;

export async function timedEntry({ browser, BASE, token, fixture, facts, check }) {
  for (const mode of ['mint', 'headers', 'denial409', 'denial422', 'accepted body', 'mark', 'cancel', 'queue']) {
    const context = await timedContext(browser, BASE), page = await context.newPage();
    const ticket = fixture();
    try {
      await pair(page, token);
      const requests = [];
      page.on('request', request => { if (scanPath(request.url()) && request.method() === 'POST') requests.push(request.postDataJSON()); });
      if (mode === 'mint') await arm(page, { store: 'occurrences', state: 'PENDING', action: 'hold', once: true, name: 'mint' });
      if (mode === 'mark') await arm(page, { store: 'occurrences', state: 'ACTUATED', action: 'hold', once: true, name: 'mark' });
      if (mode === 'queue') {
        await arm(page, { store: 'occurrences', state: 'QUEUED', action: 'hold', once: true, name: 'queue' });
        await page.route(scanPath, route => route.fulfill({ status: 503, body: '{}' }));
      }
      if (mode.startsWith('denial')) await page.route(scanPath, route => route.fulfill({ status: Number(mode.slice(-3)), contentType: 'application/json', body: '{"decision":"rejected","reason":"refunded"}' }));
      if (['headers', 'cancel'].includes(mode)) await page.evaluate(() => { window.__tkt559Wait.mode = 'headers'; });
      if (mode.startsWith('denial') || mode === 'accepted body') await page.evaluate(() => { window.__tkt559Wait.mode = 'body'; });
      await submit(page, ticket.credential);
      if (mode === 'mint' || mode === 'mark' || mode === 'queue') {
        const state = mode === 'mint' ? 'PENDING' : mode === 'mark' ? 'ACTUATED' : 'QUEUED';
        await matched(page, { state });
        await page.waitForFunction(name => !!window.__tkt558IdbProbe.holds[name], mode);
        if (mode === 'queue') {
          const text = await page.getByRole('status', { name: 'Scan result' }).textContent();
          assert.ok(text.includes('Admit per venue offline policy.'));
          assert.equal(await page.getByRole('button', { name: 'Check ticket', exact: true }).isDisabled(), false);
          await advance(page, 31000);
          assert.equal(await page.getByRole('status', { name: 'Scan result' }).textContent(), text);
        } else await advance(page, mode === 'mint' ? 3000 : 30000);
        if (mode !== 'queue') await page.getByRole('heading', { name: mode === 'mint' ? 'Scan stopped' : 'Operator required', exact: true }).waitFor();
        await release(page, mode);
        await page.waitForFunction(name => window.__tkt558IdbProbe.events.some(event => event.event === 'complete' && event.state === (name === 'mint' ? 'RESOLVED' : name === 'mark' ? 'ACTUATED' : 'QUEUED')), mode);
      } else {
        await page.waitForFunction(() => window.__tkt559Wait.started);
        if (mode === 'cancel') {
          await page.evaluate(() => {
            localStorage.setItem('scanner.device-token', 'replacement');
            window.dispatchEvent(new StorageEvent('storage', { key: 'scanner.device-token', newValue: 'replacement' }));
          });
        } else await advance(page, 30000);
        if (mode !== 'cancel') await page.getByRole('alert', { name: 'Scan result' }).waitFor();
        const before = await resultText(page);
        await releaseResponse(page);
        await page.waitForFunction(() => window.__tkt558IdbProbe.events.some(event => event.event === 'complete' && event.state === 'RESOLVED'));
        assert.equal(await resultText(page), before);
      }
      const rows = await read(page);
      assert.equal(rows.length, 1);
      assert.equal(rows[0].state, mode === 'mark' ? 'ACTUATED' : mode === 'queue' ? 'QUEUED' : 'RESOLVED');
      assert.equal(rows[0].actuated, mode === 'mark');
      if (requests.length) assert.equal(rows[0].occurrenceId, requests[0].occurrence_id);
      assert.equal(requests.length, mode === 'mint' ? 0 : 1);
      assert.equal(facts(ticket.ticket).length, ['headers', 'accepted body', 'mark', 'cancel'].includes(mode) ? 1 : 0);
      assert.deepEqual(await page.evaluate(() => window.__tkt558IdbProbe.failures), []);
      check(`TKT-559 host Chrome timed ${mode} submits controls and retains the exact durable decision`, true);
    } finally { await context.close(); }
  }

  await measureStrictCommits({ browser, BASE, token, fixture, check });
}

export async function measureStrictCommits({ browser, BASE, token, fixture, check }) {
  // This context has native clocks and no transaction holds or response overrides.
  const context = await browser.newContext({ baseURL: BASE }), page = await context.newPage();
  await context.addInitScript(() => {
    const native = IDBDatabase.prototype.transaction;
    window.__tkt559Samples = [];
    window.__tkt559Errors = [];
    IDBDatabase.prototype.transaction = function (...args) {
      try {
      const start = performance.now(), wallStart = Date.now();
      const tx = Reflect.apply(native, this, args);
      if (this.name !== 'gate-occurrences' || args[1] !== 'readwrite' || args[2]?.durability !== 'strict') return tx;
      const states = new Set(), ids = new Set(), objectStore = tx.objectStore.bind(tx);
      tx.objectStore = name => {
        const store = objectStore(name);
        for (const method of ['put', 'add']) {
          const write = store[method].bind(store);
          store[method] = (value, ...keys) => {
            if (name === 'occurrences') { states.add(value?.state); ids.add(value?.occurrenceId); }
            try { return write(value, ...keys); } catch (error) { window.__tkt559Errors.push(error.stack || String(error)); throw error; }
          };
        }
        return store;
      };
      tx.addEventListener('complete', () => {
        for (const state of states) if (state === 'PENDING' || state === 'ACTUATED') window.__tkt559Samples.push({
          phase: state === 'PENDING' ? 'mint' : 'mark', occurrenceIds: [...ids], durability: tx.durability,
          monotonicMs: performance.now() - start, wallMs: Date.now() - wallStart,
        });
      });
      return tx;
      } catch (error) { window.__tkt559Errors.push(error.stack || String(error)); throw error; }
    };
  });
  page.on('pageerror', error => console.log('TKT-559 measurement page error:', error.message));
  page.on('response', response => { if (new URL(response.url()).pathname.endsWith('/voided-tickets') && response.status() !== 200) console.log('TKT-559 measurement feed status:', response.status()); });
  try {
    await pair(page, token);
    const ticket = fixture();
    for (let index = 0; index < 110; index++) {
      const response = page.waitForResponse(response => scanPath(response.url()));
      let received;
      try { [, received] = await Promise.all([submit(page, ticket.credential), response]); } catch (error) {
        console.log('TKT-559 measurement scan', index, 'result:', await page.locator('[aria-label="Scan result"]').textContent().catch(() => 'none'));
        console.log('TKT-559 measurement errors:', await page.evaluate(() => window.__tkt559Errors));
        console.log('TKT-559 measurement samples:', await page.evaluate(() => window.__tkt559Samples.map(sample => ({ phase: sample.phase, durability: sample.durability }))));
        throw error;
      }
      assert.equal(received.status(), 200, `measurement scan ${index}: ${await received.text()}`);
      await page.getByRole('heading', { name: 'Accepted', exact: true }).waitFor();
    }
    const all = await page.evaluate(() => window.__tkt559Samples);
    const samples = all.filter(sample => sample.phase === 'mint').slice(10).concat(all.filter(sample => sample.phase === 'mark').slice(10));
    const summaries = {};
    for (const phase of ['mint', 'mark']) {
      const rows = samples.filter(sample => sample.phase === phase), values = rows.map(row => row.monotonicMs).sort((a, b) => a - b);
      assert.equal(rows.length, 100); assert.ok(rows.every(row => row.durability === 'strict'));
      summaries[phase] = { count: values.length, minMs: values[0], medianMs: (values[49] + values[50]) / 2, p95Ms: values[94], maxMs: values.at(-1), maxBelowSendLimit: values.at(-1) < 3000 };
    }
    const evidence = { ticket: 'TKT-559', measuredAt: new Date().toISOString(), browser: browser.version(),
      host: { platform: platform(), release: osRelease(), arch: arch(), cpu: cpus()[0]?.model, logicalCPUs: cpus().length },
      reproduction: 'make browser', context: 'native Date.now/performance.now; no holds; valid multi-entry credential; production rendered controls',
      warmupScansExcluded: 10, measuredScans: 100, summaries, samples,
      limitation: 'Host observation only. The 3 s bound is not a measured venue allowance. TKT-560 venue qualification remains required.' };
    mkdirSync('docs/sprints/S-2/evidence', { recursive: true });
    writeFileSync('docs/sprints/S-2/evidence/TKT-559-strict-commits.json', JSON.stringify(evidence, null, 2) + '\n');
    check(`TKT-559 Chrome ${browser.version()} measured 100 strict mint and 100 strict mark completions with native clocks`, true);
  } finally { await context.close(); }
}

export async function timedExit({ browser, BASE, token, fixture, check }) {
  const context = await timedContext(browser, BASE), page = await context.newPage();
  const batches = [];
  page.on('request', request => { if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/reconciliations')) batches.push(request.postDataJSON().occurrences); });
  try {
    await pair(page, token);
    const control = fixture();
    await page.route(scanPath, route => route.fulfill({ status: 503, body: '{}' }));
    await submit(page, control.credential); await page.getByRole('heading', { name: 'Offline decision', exact: true }).waitFor();
    await page.waitForFunction(() => window.__tkt558IdbProbe.events.some(event => event.state === 'QUEUED' && event.event === 'complete'));
    const [queued] = await read(page);
    await page.unroute(scanPath);
    await page.getByRole('radio', { name: 'Exit', exact: true }).check();
    await page.evaluate(() => { window.__tkt559Wait.mode = 'headers'; });
    const request = page.waitForRequest(request => request.method() === 'POST' && scanPath(request.url()));
    await submit(page, fixture().credential); const target = (await request).postDataJSON();
    await page.waitForFunction(() => window.__tkt559Wait.started);
    await advance(page, 30000);
    const expected = 'Operator requiredScan sent, answer unconfirmed. Scanner did not open. Access may have recorded this scan. Get an operator; do not admit from this result.Exit result unknown; the holder may leave';
    await page.getByRole('status', { name: 'Scan result' }).waitFor();
    assert.equal(await resultText(page), expected);
    await releaseResponse(page);
    await page.waitForFunction(id => window.__tkt558IdbProbe.events.some(event => event.id === id && event.state === 'RESOLVED' && event.event === 'complete'), target.occurrence_id);
    const row = (await read(page)).find(row => row.occurrenceId === target.occurrence_id);
    assert.equal(row.direction, 'exit'); assert.equal(row.localOutcome.cause, 'stopped');
    await page.getByRole('button', { name: 'Sync queued scans' }).click();
    await page.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    assert.deepEqual(batches.flat(), [{ occurrence_id: queued.occurrenceId, qr_payload: queued.qrPayload, occurred_at: queued.occurredAt, event_type: 'entry' }]);
    assert.equal(await resultText(page), expected);
    check('TKT-399 host Chrome timed Exit publishes exact sent-unknown at 30000ms and syncs only its queued control', true);
  } finally { await context.close(); }
}
