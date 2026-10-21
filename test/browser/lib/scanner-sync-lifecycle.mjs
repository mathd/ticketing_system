// Same-context Chrome journeys for TKT-512: shared queue refresh, carried failures and initial storage waiting.
// Every journey uses a fresh browser context. Tabs in one journey share that context and origin.
// Event suppression, probe rules and holds are removed in finally blocks. Holds end by explicit release,
// and the watchdog is only emergency cleanup.

import { assert, pair, submit, read, scanPath, reconcilePath, installProbe, arm } from './scanner-controls.mjs';

const WAITING_TEXT = 'Waiting for local storage to finish before this sync can continue.';
const REPAIR_TEXT = 'The outcome write needs repair. Keep this page open and retry storage.';
const FAILED_TEXT = /^Sync failed: The server answered but the reply could not be read\. 1 scan is saved/;
const SYNC_BUTTON = 'Sync queued scans';
const feedPath = (url) => new URL(typeof url === 'string' ? url : url.href).pathname === '/api/access/scans/voided-tickets';
const HOLD_WATCHDOG_MS = 20_000;
const BOUND_MS = 10_000;

// Page-local instrumentation, installed before App startup. It records one ordered sequence per page. The sequence holds
// each gate-occurrences transaction's creation, its requests in order (puts with their value), its complete or abort,
// each reconciliation fetch and each button click. It also records every focus, blur, visibility and online event with
// its trust and suppression state. It changes no result.
// A put's value is captured on the transaction's objectStore. The probe's own put wrapper refuses some puts before any
// prototype-level wrapper runs, so capturing there would miss the value that was attempted.
async function instrument(context) {
  await context.addInitScript(() => {
    const entries = [];
    const sequence = [];
    const owners = new WeakMap();
    window.__tkt512Tx = { entries, sequence };
    const copy = (value) => JSON.parse(JSON.stringify(value));
    const nativeTransaction = IDBDatabase.prototype.transaction;
    IDBDatabase.prototype.transaction = function (storeNames, mode, options) {
      const tx = nativeTransaction.call(this, storeNames, mode, options);
      const entry = {
        ordinal: entries.length,
        db: this.name,
        stores: typeof storeNames === 'string' ? [storeNames] : [...storeNames],
        mode: mode ?? 'readonly',
        durability: options?.durability ?? 'default',
        requests: [],
        events: [],
      };
      entries.push(entry);
      owners.set(tx, entry);
      const observed = this.name === 'gate-occurrences';
      if (observed) sequence.push({ kind: 'create', ordinal: entry.ordinal, db: entry.db, stores: entry.stores, mode: entry.mode, durability: entry.durability });
      const finish = (kind) => {
        entry.events.push(kind);
        if (observed) sequence.push({ kind, ordinal: entry.ordinal });
      };
      tx.addEventListener('complete', () => finish('complete'));
      tx.addEventListener('abort', () => finish('abort'));
      const objectStore = tx.objectStore.bind(tx);
      tx.objectStore = (name) => {
        const store = objectStore(name);
        const put = store.put.bind(store);
        store.put = (value, ...keys) => {
          entry.requests.push(`${name}.put`);
          if (observed) sequence.push({ kind: 'request', ordinal: entry.ordinal, op: `${name}.put`, value: copy(value) });
          return put(value, ...keys);
        };
        return store;
      };
      return tx;
    };
    for (const method of ['get', 'getAll', 'getAllKeys', 'count', 'add', 'delete']) {
      const native = IDBObjectStore.prototype[method];
      IDBObjectStore.prototype[method] = function (...args) {
        const entry = owners.get(this.transaction);
        if (entry) {
          entry.requests.push(`${this.name}.${method}`);
          if (entry.db === 'gate-occurrences') sequence.push({ kind: 'request', ordinal: entry.ordinal, op: `${this.name}.${method}` });
        }
        return native.apply(this, args);
      };
    }
  });
  await context.addInitScript(() => {
    window.__tkt512Suppress = {};
    window.__tkt512Events = [];
    window.__tkt512Visibility = 'visible';
    // A rule is true to drop every event of that type, or 'trusted' to drop only native events. Synthetic events pass.
    const suppressedEvent = (type, event) => {
      const rule = window.__tkt512Suppress[type];
      return rule === true || (rule === 'trusted' && event.isTrusted);
    };
    for (const type of ['focus', 'blur', 'visibilitychange', 'online']) {
      window.addEventListener(type, (event) => {
        const suppressed = suppressedEvent(type, event);
        window.__tkt512Events.push({ type, trusted: event.isTrusted, suppressed, state: document.visibilityState });
        if (suppressed) event.stopImmediatePropagation();
      }, true);
    }
    // The capture observer runs before the page's own listeners, so the click is in the sequence before any work it starts.
    window.addEventListener('click', (event) => {
      const button = event.target instanceof Element ? event.target.closest('button') : null;
      if (button) window.__tkt512Tx.sequence.push({ kind: 'click', text: button.textContent.trim() });
    }, true);
    const nativeFetch = window.fetch.bind(window);
    window.fetch = (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input.url, location.href);
      if (url.pathname === '/api/access/scans/reconciliations') window.__tkt512Tx.sequence.push({ kind: 'fetch', method: init?.method ?? 'GET' });
      return nativeFetch(input, init);
    };
  });
}

const txLog = (page) => page.evaluate(() => window.__tkt512Tx.entries.filter((entry) => entry.db === 'gate-occurrences'));
const eventLog = (page) => page.evaluate(() => window.__tkt512Events.slice());
const sequenceOf = (page) => page.evaluate(() => window.__tkt512Tx.sequence.slice());
const setSuppression = (page, rules) => page.evaluate((rules) => { window.__tkt512Suppress = rules; }, rules);
const setSuppressed = (page, types) => setSuppression(page, Object.fromEntries(types.map((type) => [type, true])));
const clearSuppression = (page) => setSuppression(page, {});
const scanResult = (page) => page.locator('[aria-label="Scan result"]').textContent();

// The production queue read: getAllKeys and getAll on occurrences, then getAll on legacy classes.
const isQueueRead = (entry) => entry.mode === 'readonly' && entry.stores.includes('occurrences')
  && entry.stores.includes('legacy_classes') && entry.requests.includes('occurrences.getAllKeys')
  && entry.requests.includes('occurrences.getAll') && entry.requests.includes('legacy_classes.getAll');
// The recovery-list read of the initial storage prelude. It reads legacy class keys, which the queue read does not.
const isRecoveryList = (entry) => entry.mode === 'readonly' && entry.requests.includes('legacy_classes.getAllKeys');
// A strict readwrite transaction on occurrences only. The ordering checks confirm its get and put separately.
const isStrictRepair = (entry) => entry.durability === 'strict' && entry.mode === 'readwrite'
  && entry.stores.length === 1 && entry.stores[0] === 'occurrences';
// The DEFAULT-durability owner renewal. Ordering checks exclude it explicitly.
const isHeartbeat = (entry) => entry.durability === 'default' && entry.mode === 'readwrite'
  && entry.stores.length === 1 && entry.stores[0] === 'occurrences';
const completed = (entry) => entry.events.includes('complete') && !entry.events.includes('abort');

// Groups an ordered sequence into one view per transaction. Each view keeps the index of its creation, its requests in
// order with their indexes, and the index of its complete or abort. Ordering assertions compare those indexes.
function transactionsOf(seq) {
  const views = new Map();
  seq.forEach((event, index) => {
    if (event.kind === 'create') {
      views.set(event.ordinal, { ordinal: event.ordinal, stores: event.stores, mode: event.mode, durability: event.durability, createdAt: index, requests: [], requestAt: [], events: [], doneAt: -1 });
      return;
    }
    const view = views.get(event.ordinal);
    if (!view) return;
    if (event.kind === 'request') {
      view.requests.push(event.op);
      view.requestAt.push(index);
    } else if (event.kind === 'complete' || event.kind === 'abort') {
      view.events.push(event.kind);
      view.doneAt = index;
    }
  });
  return [...views.values()];
}

async function waitUntil(check, label, ms = BOUND_MS) {
  const deadline = Date.now() + ms;
  for (;;) {
    if (await check()) return;
    if (Date.now() > deadline) throw new Error(`bounded wait expired: ${label}`);
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
}

const completedQueueReadAfter = async (page, mark, label) => waitUntil(async () => (await txLog(page)).slice(mark).some((entry) => isQueueRead(entry) && completed(entry)), label);

function recordReconciles(page) {
  const seen = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && reconcilePath(request.url())) seen.push(request);
  });
  return seen;
}

// One real scan through the rendered form. The gateway is replaced by a 503 so the scan is queued offline.
// It waits for the new row to be stored, not for a heading that an earlier scan may already have rendered.
async function queueOneOffline(page, credential) {
  const before = (await read(page)).length;
  await page.route(scanPath, (route) => route.fulfill({ status: 503, body: 'not decided' }));
  try {
    await submit(page, credential);
    await waitUntil(async () => (await read(page)).some((row) => row.qrPayload === credential && row.state === 'QUEUED'), 'the offline row is stored as queued');
    assert.equal((await read(page)).length, before + 1);
  } finally {
    await page.unroute(scanPath);
  }
}

async function failOneSync(page) {
  await page.route(reconcilePath, (route) => route.fulfill({ status: 502, contentType: 'text/html', body: '<html>Bad Gateway</html>' }));
  try {
    await page.getByRole('button', { name: SYNC_BUTTON }).click();
    await page.getByText(FAILED_TEXT).waitFor();
  } finally {
    await page.unroute(reconcilePath);
  }
}

async function pairedPage(context, token) {
  const page = await context.newPage();
  await pair(page, token);
  await page.getByRole('button', { name: SYNC_BUTTON }).waitFor();
  return page;
}

// The wire row for one queued scan, as the page sends it. The expected value comes from the row read before the send.
const wireRow = (row) => ({ occurrence_id: row.occurrenceId, qr_payload: row.qrPayload, occurred_at: row.occurredAt, event_type: 'entry' });

// B1. Trusted native focus with focus emulation disabled. Focus is the only trigger enabled at its boundary.
// A keeps its visibility and online suppression throughout. Exact Access facts, rows and a causal queue read are required.
async function nativeFocus({ browser, BASE, token, fixture, facts, quarantine }) {
  const context = await browser.newContext({ baseURL: BASE });
  await installProbe(context);
  await instrument(context);
  try {
    const a = await context.newPage();
    const aReconciles = recordReconciles(a);
    await pair(a, token);
    const ticket = fixture();
    await queueOneOffline(a, ticket.credential);
    await a.getByRole('button', { name: SYNC_BUTTON }).waitFor();
    const [queued] = await read(a);
    await failOneSync(a);
    await a.getByText(FAILED_TEXT).waitFor();
    const reconcilesBeforeB = aReconciles.length;
    const resultBefore = await scanResult(a);
    assert.match(resultBefore, /Offline decision/);

    // A's focus, visibility and online delivery are suppressed before B opens and syncs.
    await setSuppressed(a, ['focus', 'visibilitychange', 'online']);
    const aStale = (await txLog(a)).length;
    const b = await pairedPage(context, token);
    const bReconcile = b.waitForRequest((request) => request.method() === 'POST' && reconcilePath(request.url()));
    await b.getByRole('button', { name: SYNC_BUTTON }).click();
    const batch = (await bReconcile).postDataJSON().occurrences;
    await b.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    assert.deepEqual(batch, [wireRow(queued)]);
    assert.deepEqual(facts(ticket.ticket), [{ occurrence_id: queued.occurrenceId, event_type: 'entry', occurred_at: batch[0].occurred_at }]);
    assert.equal(quarantine(ticket.ticket), 0);
    const synced = { ...queued, state: 'SYNCED', result: 'recorded' };
    assert.deepEqual((await read(b))[0], synced);
    // Retirement elsewhere does not reach A. A still shows its failure and makes no queue read.
    await a.getByText(FAILED_TEXT).waitFor();
    assert.equal((await txLog(a)).slice(aStale).some(isQueueRead), false, 'A observed the queue while its delivery was suppressed');

    const cdpA = await context.newCDPSession(a);
    const cdpB = await context.newCDPSession(b);
    await cdpA.send('Emulation.setFocusEmulationEnabled', { enabled: false });
    await cdpB.send('Emulation.setFocusEmulationEnabled', { enabled: false });

    // Step 1. Focus moves to A with A's focus delivery suppressed, so this move reads nothing. Focus leaves B first
    // because bringing a page that already has focus emits no new focus event.
    const aForceMark = (await eventLog(a)).length;
    const bHadFocus = await b.evaluate(() => document.hasFocus());
    const bForceMark = (await eventLog(b)).length;
    await a.bringToFront();
    await waitUntil(async () => (await eventLog(a)).slice(aForceMark).some((event) => event.type === 'focus' && event.trusted && event.suppressed), 'A trusted focus while suppressed');
    await waitUntil(async () => a.evaluate(() => document.hasFocus()), 'A has focus after the forced move');
    if (bHadFocus) await waitUntil(async () => (await eventLog(b)).slice(bForceMark).some((event) => event.type === 'blur' && event.trusted), 'B trusted blur on the forced move');
    await waitUntil(async () => !(await b.evaluate(() => document.hasFocus())), 'B lost focus on the forced move');
    assert.equal((await txLog(a)).slice(aStale).some(isQueueRead), false, 'A read the queue before the target move');

    // Step 2. B takes focus. Its trusted focus is recorded from before the move, so the move is observed.
    const bFocusMark = (await eventLog(b)).length;
    await b.bringToFront();
    await waitUntil(async () => (await eventLog(b)).slice(bFocusMark).some((event) => event.type === 'focus' && event.trusted), 'B trusted focus');
    await waitUntil(async () => b.evaluate(() => document.hasFocus()), 'B has focus before the target move');
    await waitUntil(async () => !(await a.evaluate(() => document.hasFocus())), 'A lost focus to B');
    assert.equal((await txLog(a)).slice(aStale).some(isQueueRead), false, 'A read the queue before the target move');

    // Step 3. The target move. A's focus is the only trigger enabled, and the move that gives A focus is its boundary.
    const aQueueMark = (await txLog(a)).length;
    const aEventMark = (await eventLog(a)).length;
    const bBlurMark = (await eventLog(b)).length;
    await setSuppressed(a, ['visibilitychange', 'online']);
    await a.bringToFront();
    await waitUntil(async () => (await eventLog(b)).slice(bBlurMark).some((event) => event.type === 'blur' && event.trusted), 'B trusted blur at the target move');
    await waitUntil(async () => (await eventLog(a)).slice(aEventMark).some((event) => event.type === 'focus' && event.trusted && !event.suppressed), 'A trusted focus at the target move');
    await waitUntil(async () => a.evaluate(() => document.hasFocus()), 'A has focus at the target move');
    assert.equal(await b.evaluate(() => document.hasFocus()), false, 'B kept focus at the target move');
    await completedQueueReadAfter(a, aQueueMark, 'A completed causal queue read after focus');
    await a.getByText(/^Sync failed:/).waitFor({ state: 'detached' });
    await a.getByRole('button', { name: SYNC_BUTTON }).waitFor({ state: 'detached' });
    assert.equal(aReconciles.length, reconcilesBeforeB, 'A started a reconciliation from a focus refresh');
    assert.deepEqual(await read(a), [synced]);
    assert.equal(await scanResult(a), resultBefore, 'the focus refresh changed the scan result');
    await clearSuppression(a);
    return true;
  } finally {
    await context.close();
  }
}

// B2. Synthetic visibility wiring only. Focus, online and native visibility are suppressed on A before B opens.
// After the override is installed, only synthetic visibility reaches A. Native visibility remains unverified.
async function visibilityWiring({ browser, BASE, token, fixture }) {
  const context = await browser.newContext({ baseURL: BASE });
  await installProbe(context);
  await instrument(context);
  try {
    const a = await context.newPage();
    await pair(a, token);
    await queueOneOffline(a, fixture().credential);
    await failOneSync(a);
    await a.getByText(FAILED_TEXT).waitFor();
    await setSuppressed(a, ['focus', 'visibilitychange', 'online']);
    const staleMark = (await txLog(a)).length;
    // B syncs the only row. A keeps its stale failure, and A makes no queue read while every trigger is suppressed.
    const b = await pairedPage(context, token);
    await b.getByRole('button', { name: SYNC_BUTTON }).click();
    await b.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    await a.getByText(FAILED_TEXT).waitFor();
    assert.equal((await txLog(a)).slice(staleMark).some(isQueueRead), false, 'A read the queue while every trigger was suppressed');

    // The override is installed first. Then only synthetic visibility is delivered. Native visibility stays suppressed,
    // and focus and online stay suppressed.
    await a.evaluate(() => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => window.__tkt512Visibility });
    });
    await setSuppression(a, { focus: true, online: true, visibilitychange: 'trusted' });
    try {
      const hiddenMark = (await txLog(a)).length;
      const hiddenEvents = (await eventLog(a)).length;
      await a.evaluate(() => {
        window.__tkt512Visibility = 'hidden';
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await waitUntil(async () => (await eventLog(a)).slice(hiddenEvents).some((event) => event.type === 'visibilitychange' && event.state === 'hidden' && !event.trusted && !event.suppressed), 'synthetic hidden receipt');
      await new Promise((resolve) => setTimeout(resolve, 500));
      assert.equal((await txLog(a)).slice(hiddenMark).some(isQueueRead), false, 'a hidden transition read the queue');
      assert.ok(await a.getByText(FAILED_TEXT).isVisible());

      const visibleMark = (await txLog(a)).length;
      const visibleEvents = (await eventLog(a)).length;
      await a.evaluate(() => {
        window.__tkt512Visibility = 'visible';
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await waitUntil(async () => (await eventLog(a)).slice(visibleEvents).some((event) => event.type === 'visibilitychange' && event.state === 'visible' && !event.trusted && !event.suppressed), 'synthetic visible receipt');
      await completedQueueReadAfter(a, visibleMark, 'visible transition queue read');
      await a.getByText(/^Sync failed:/).waitFor({ state: 'detached' });
      await a.getByRole('button', { name: SYNC_BUTTON }).waitFor({ state: 'detached' });
      const rows = await read(a);
      assert.equal(rows.length, 1);
      assert.equal(rows[0].state, 'SYNCED');
    } finally {
      await a.evaluate(() => { delete document.visibilityState; window.__tkt512Visibility = 'visible'; });
      await clearSuppression(a);
    }
    return true;
  } finally {
    await context.close();
  }
}

// B3. Local pairing after a cross-tab sync. A keeps its stale failure until its own pairing refresh reads the queue.
// B's sync is the only Access write. A's local 401 and its local pairing are the only triggers on A.
async function localPairingRefresh({ browser, BASE, token, fixture, facts, quarantine }) {
  const context = await browser.newContext({ baseURL: BASE });
  await installProbe(context);
  await instrument(context);
  try {
    const a = await context.newPage();
    const aReconciles = recordReconciles(a);
    await pair(a, token);
    const ticket = fixture();
    await queueOneOffline(a, ticket.credential);
    await a.getByRole('button', { name: SYNC_BUTTON }).waitFor();
    const [queued] = await read(a);
    await failOneSync(a);
    await a.getByText(FAILED_TEXT).waitFor();
    const staleMark = (await txLog(a)).length;
    const reconcilesBeforeB = aReconciles.length;
    await setSuppressed(a, ['focus', 'visibilitychange', 'online']);

    // B syncs the captured row through the gateway. A is not told, and A makes no queue observation.
    const b = await pairedPage(context, token);
    const bReconcile = b.waitForRequest((request) => request.method() === 'POST' && reconcilePath(request.url()));
    await b.getByRole('button', { name: SYNC_BUTTON }).click();
    const batch = (await bReconcile).postDataJSON().occurrences;
    await b.getByText('Synced 1 offline scan(s).', { exact: true }).waitFor();
    assert.deepEqual(batch, [wireRow(queued)]);
    const synced = { ...queued, state: 'SYNCED', result: 'recorded' };
    assert.deepEqual(await read(b), [synced]);
    assert.deepEqual(facts(ticket.ticket), [{ occurrence_id: queued.occurrenceId, event_type: 'entry', occurred_at: batch[0].occurred_at }]);
    assert.equal(quarantine(ticket.ticket), 0);
    assert.equal((await txLog(a)).slice(staleMark).some(isQueueRead), false, 'A observed the queue before its refresh trigger');
    assert.ok(await a.getByText(FAILED_TEXT).isVisible());

    // A's revocation refresh returns 401. A becomes unpaired and keeps its failure. Local authentication loss adds no refresh.
    await a.route(feedPath, (route) => route.fulfill({ status: 401, contentType: 'application/json', body: '{"error":"scanner device is not enrolled"}' }));
    const unauthMark = (await txLog(a)).length;
    await a.getByRole('button', { name: 'Refresh revocation list' }).click();
    await a.getByText(/This device is not paired\. Enter its pairing token/).waitFor();
    await b.getByLabel('Pairing token').waitFor();
    await new Promise((resolve) => setTimeout(resolve, 300));
    assert.equal((await txLog(a)).slice(unauthMark).some(isQueueRead), false, 'a local 401 started a queue refresh');
    await a.unroute(feedPath);

    // A's queue observer starts here. The rendered form submits on this page: no navigation, no localStorage edit.
    const pairMark = (await txLog(a)).length;
    let navigated = false;
    const onNavigate = (frame) => { if (frame === a.mainFrame()) navigated = true; };
    a.on('framenavigated', onNavigate);
    try {
      await a.getByLabel('Pairing token').fill(token);
      await a.getByRole('button', { name: 'Pair device' }).click();
      await a.getByRole('button', { name: 'Check ticket' }).waitFor();
      await b.getByRole('button', { name: 'Check ticket' }).waitFor();
      await completedQueueReadAfter(a, pairMark, 'A own pairing refresh');
    } finally {
      a.off('framenavigated', onNavigate);
    }
    assert.equal(navigated, false, 'the pairing form navigated');
    await a.getByText(/^Sync failed:/).waitFor({ state: 'detached' });
    await a.getByRole('button', { name: SYNC_BUTTON }).waitFor({ state: 'detached' });
    assert.equal(aReconciles.length, reconcilesBeforeB, 'A started a reconciliation from pairing');
    assert.deepEqual(await read(a), [synced]);
    return true;
  } finally {
    await context.close();
  }
}

// B4. Forced IndexedDB contention. B holds a real readwrite transaction over occurrences. A's sync waits behind it.
async function idbContention({ browser, BASE, token, fixture, facts, quarantine }) {
  const context = await browser.newContext({ baseURL: BASE });
  await installProbe(context);
  await instrument(context);
  const b = await context.newPage();
  let holding = false;
  try {
    const a = await context.newPage();
    const aReconciles = recordReconciles(a);
    await pair(a, token);
    const control = fixture();
    await queueOneOffline(a, control.credential);
    await a.getByRole('button', { name: SYNC_BUTTON }).waitFor();
    const [controlBefore] = (await read(a)).filter((row) => row.qrPayload === control.credential);

    // The next queue decision's persistence fails through the native write probe. Its PENDING row and repair warning remain.
    const pending = fixture();
    await arm(a, { store: 'occurrences', state: 'QUEUED', action: 'throw', name: 'queue-write' });
    const submitMark = (await txLog(a)).length;
    await a.route(scanPath, (route) => route.fulfill({ status: 503, body: 'not decided' }));
    try {
      await submit(a, pending.credential);
      await a.getByText(REPAIR_TEXT).waitFor();
    } finally {
      await a.unroute(scanPath);
    }
    await completedQueueReadAfter(a, submitMark, 'automatic refresh after the failing decision');
    const [pendingBefore] = (await read(a)).filter((row) => row.qrPayload === pending.credential);
    assert.equal(pendingBefore.state, 'PENDING');
    assert.deepEqual((await read(a)).find((row) => row.qrPayload === control.credential), controlBefore);
    // The queue write that the probe refused. Its value was captured before the refusal, and it is the decision target.
    const attempts = queueWriteAttempts(await sequenceOf(a), pending.credential);
    assert.ok(attempts.length >= 1, 'the refused queue write was not captured');
    const target = attempts.at(-1);
    assertDecisionTarget(target, pendingBefore);

    // B opens the same database. Its pairing carries the same token, so A's storage event changes nothing.
    await pair(b, token);
    // A's focus, visibility and online delivery are suppressed before the probe is disarmed.
    await setSuppressed(a, ['focus', 'visibilitychange', 'online']);
    // Disarming changes no row. The sequence length at this point starts the disarm-to-click interval.
    const disarmAt = await a.evaluate(() => { window.__tkt558IdbProbe.rules = []; return window.__tkt512Tx.sequence.length; });

    holding = true;
    await b.evaluate((watchdogMs) => new Promise((resolve, reject) => {
      const open = indexedDB.open('gate-occurrences');
      open.onerror = () => reject(open.error);
      open.onsuccess = () => {
        const db = open.result;
        const tx = db.transaction('occurrences', 'readwrite', { durability: 'strict' });
        const state = window.__tkt512Hold = { released: false, abort: false, count: 0, outcome: null, watchdog: false };
        const deadline = Date.now() + watchdogMs;
        const pump = () => {
          if (state.released) { if (state.abort) tx.abort(); return; }
          if (Date.now() > deadline) { state.watchdog = true; tx.abort(); return; }
          const request = tx.objectStore('occurrences').count();
          request.onsuccess = () => { state.count += 1; pump(); };
        };
        tx.oncomplete = () => { state.outcome = 'complete'; db.close(); };
        tx.onabort = () => { state.outcome = 'abort'; db.close(); };
        pump();
        resolve(true);
      };
    }), HOLD_WATCHDOG_MS);
    await b.waitForFunction(() => window.__tkt512Hold.count >= 1, null, { timeout: BOUND_MS });

    // The rendered sync click. The capture observer records its position in the same sequence as the transactions.
    const reconcilesBefore = aReconciles.length;
    await a.getByRole('button', { name: SYNC_BUTTON }).click();
    let seq = await sequenceOf(a);
    const clickAt = seq.findLastIndex((event) => event.kind === 'click' && event.text === SYNC_BUTTON);
    assert.ok(clickAt > disarmAt, 'the sync click was not recorded after the disarm');
    const beforeClick = transactionsOf(seq).filter((view) => view.createdAt >= disarmAt && view.createdAt < clickAt);
    assert.equal(beforeClick.some((view) => (isStrictRepair(view) && !isHeartbeat(view)) || isQueueRead(view) || isRecoveryList(view)), false,
      'a repair or queue read ran between disarm and the actual sync click');

    await waitUntil(async () => transactionsOf(await sequenceOf(a)).some((view) => view.createdAt > clickAt && isStrictRepair(view) && !isHeartbeat(view)), 'A strict repair transaction');
    const blocked = transactionsOf(await sequenceOf(a)).find((view) => view.createdAt > clickAt && isStrictRepair(view) && !isHeartbeat(view));
    assert.ok(blocked.requests.includes('occurrences.get'), 'the repair did not request its row before it was blocked');
    await a.getByText(WAITING_TEXT).waitFor();
    assert.equal(aReconciles.length, reconcilesBefore, 'A sent a reconciliation while B held storage');

    // Explicit release. The transaction must commit. The watchdog is emergency cleanup, not release evidence.
    await b.evaluate(() => { window.__tkt512Hold.released = true; });
    await b.waitForFunction(() => window.__tkt512Hold.outcome !== null, null, { timeout: BOUND_MS });
    holding = false;
    assert.equal(await b.evaluate(() => window.__tkt512Hold.outcome), 'complete');
    assert.equal(await b.evaluate(() => window.__tkt512Hold.watchdog), false);
    await a.getByText(WAITING_TEXT).waitFor({ state: 'detached' });
    await waitUntil(() => aReconciles.length > reconcilesBefore, 'A reconciliation after contention');

    // Global order after the click. The strict repair gets, then puts, then completes. The recovery list completes
    // before the queue read is created. The queue read completes before the send. DEFAULT renewals are not in this order.
    seq = await sequenceOf(a);
    const after = transactionsOf(seq).filter((view) => view.createdAt > clickAt);
    const repair = after.find((view) => isStrictRepair(view) && !isHeartbeat(view));
    assert.ok(repair && completed(repair), 'the strict repair did not complete');
    const repairGet = repair.requestAt[repair.requests.indexOf('occurrences.get')];
    const repairPut = repair.requestAt[repair.requests.indexOf('occurrences.put')];
    assert.ok(repairGet > clickAt && repairPut > repairGet && repair.doneAt > repairPut, 'the repair did not get, put and complete in that order');
    const recovery = after.find((view) => isRecoveryList(view) && view.createdAt > repair.doneAt);
    assert.ok(recovery && completed(recovery), 'the recovery list did not complete after the repair');
    const queue = after.find((view) => isQueueRead(view) && view.createdAt > recovery.doneAt);
    assert.ok(queue && completed(queue), 'the queue read was not created after the recovery list completed');
    const sendAt = seq.findIndex((event, index) => index > queue.doneAt && event.kind === 'fetch' && event.method === 'POST');
    assert.ok(sendAt > queue.doneAt, 'the reconciliation was not sent after the queue read completed');

    await a.getByText('Synced 2 offline scan(s).', { exact: true }).waitFor();
    const batch = aReconciles[aReconciles.length - 1].postDataJSON().occurrences;
    assert.deepEqual([...batch].sort(byOccurrence), [wireRow(controlBefore), wireRow(pendingBefore)].sort(byOccurrence));
    // The control row changes only in state and result. The pending row changes only in state and result, and its lease may move
    // forward while the owner renews it. Its identity and decision come from the captured queue write.
    const rows = await read(a);
    assert.deepEqual(rows.find((row) => row.qrPayload === control.credential), { ...controlBefore, state: 'SYNCED', result: 'recorded' });
    const { leaseExpiresAt: leaseAtAttempt, ...expectedPending } = target;
    const { leaseExpiresAt: leaseNow, ...actualPending } = rows.find((row) => row.qrPayload === pending.credential);
    assert.deepEqual(actualPending, { ...expectedPending, state: 'SYNCED', result: 'recorded' });
    assert.ok(leaseNow >= leaseAtAttempt, 'the stored lease moved backward');
    for (const [before, scan] of [[controlBefore, control], [pendingBefore, pending]]) {
      assert.deepEqual(facts(scan.ticket), [{ occurrence_id: before.occurrenceId, event_type: 'entry', occurred_at: wireRow(before).occurred_at }]);
      assert.equal(quarantine(scan.ticket), 0);
    }
    assert.equal(await a.getByText(WAITING_TEXT).count(), 0);
    await clearSuppression(a);
    return true;
  } finally {
    if (holding) {
      // The error path also releases B, and waits for the bounded outcome before the context closes.
      await b.evaluate(() => { if (window.__tkt512Hold) window.__tkt512Hold.released = true; }).catch(() => undefined);
      await b.waitForFunction(() => !window.__tkt512Hold || window.__tkt512Hold.outcome !== null, null, { timeout: BOUND_MS }).catch(() => undefined);
    }
    await context.close();
  }
}

const byOccurrence = (x, y) => (x.occurrence_id < y.occurrence_id ? -1 : 1);

// The queue writes the probe refused for one credential, in order. Only QUEUED writes are decision targets.
const queueWriteAttempts = (seq, credential) => seq
  .filter((event) => event.kind === 'request' && event.op === 'occurrences.put' && event.value?.qrPayload === credential && event.value?.state === 'QUEUED')
  .map((event) => event.value);

// A decision target keeps the pending row's immutable identity. Its state, outcome and time are asserted here as literals.
// The row read before the write supplies the identity, so the captured value is never its own expectation.
function assertDecisionTarget(target, pendingBefore) {
  const immutable = (row) => ({
    occurrenceId: row.occurrenceId,
    qrPayload: row.qrPayload,
    occurredAt: row.occurredAt,
    createdAt: row.createdAt,
    ownerId: row.ownerId,
    direction: row.direction,
    actuated: row.actuated,
    recordFormat: row.recordFormat,
  });
  assert.deepEqual(immutable(target), immutable(pendingBefore));
  assert.equal(target.state, 'QUEUED');
  assert.deepEqual(target.localOutcome, { family: 'queue', cause: 'server_unavailable', status: 503 });
  assert.ok(Number.isFinite(Date.parse(target.decidedAt)), 'the decision time is not a timestamp');
}

// A journey reports its names only after every assertion in it has passed. scanner.mjs keeps the required-name list.
export async function syncLifecycle(shared) {
  const { check } = shared;
  const journeys = [
    [['TKT-512 native-focus'], nativeFocus],
    [['TKT-512 visibility-wiring'], visibilityWiring],
    [['TKT-512 local-pairing-refresh', 'TKT-512 shared-access-readback'], localPairingRefresh],
    [['TKT-512 idb-contention'], idbContention],
  ];
  for (const [names, journey] of journeys) {
    await journey(shared);
    for (const name of names) check(name, true);
  }
}
