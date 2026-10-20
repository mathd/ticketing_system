// Browser, polling and comparison helpers shared by the TKT-563 journeys.
// Scan and reconcile paths match the gateway routes the active spec uses.

import { createHash } from 'node:crypto';

export const SCAN_PATH = '/api/access/scans';
export const RECONCILE_PATH = '/api/access/scans/reconciliations';
export const VOIDED_PATH = '/api/access/scans/voided-tickets';

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Polls fn until it returns a truthy value or the timeout passes. It returns
// the last value and never throws, so the caller records a named result.
export async function poll(fn, { timeout = 15000, interval = 150 } = {}) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const value = await fn();
    if (value) return value;
    if (Date.now() >= deadline) return value;
    await sleep(interval);
  }
}

// SHA-256 of a string or a byte buffer. Buffers are hashed as bytes, never re-encoded.
export const digest = (value) =>
  createHash('sha256')
    .update(typeof value === 'string' || Buffer.isBuffer(value) ? value : String(value))
    .digest('hex');

// Deterministic JSON: object keys sorted at every depth.
export const canon = (value) =>
  JSON.stringify(value, (key, item) =>
    item && typeof item === 'object' && !Array.isArray(item)
      ? Object.fromEntries(Object.keys(item).sort().map((name) => [name, item[name]]))
      : item,
  );

export const sortById = (rows) => [...rows].sort((a, b) => String(a.occurrence_id ?? a.id).localeCompare(String(b.occurrence_id ?? b.id)));

// The fact shape that Access readbacks use: occurrence, type and millisecond UTC time.
export const fact = (occurrence, eventType, time) => ({
  occurrence_id: occurrence ?? null,
  event_type: eventType,
  occurred_at: time ? new Date(time).toISOString() : null,
});

export async function setDirection(page, direction) {
  await page.getByRole('radio', { name: direction === 'exit' ? 'Exit' : 'Entry', exact: true }).check();
}

export const scanResultText = (page) =>
  page.locator('section[aria-label="Scan result"]').first().textContent();

// Submits a credential through the rendered form and waits for the server answer.
export async function scanAnswered(page, H, credential) {
  const requestSeen = page.waitForRequest((request) => request.method() === 'POST' && H.scanPath(request.url()));
  const responseSeen = page.waitForResponse((response) => response.request().method() === 'POST' && H.scanPath(response.url()));
  await H.submit(page, credential);
  const request = await requestSeen;
  const response = await responseSeen;
  const json = await response.json().catch(() => null);
  return { body: request.postDataJSON(), status: response.status(), json };
}

// Submits a credential whose transport the probe aborts. Only the request exists.
export async function scanQueued(page, H, credential) {
  const requestSeen = page.waitForRequest((request) => request.method() === 'POST' && H.scanPath(request.url()));
  await H.submit(page, credential);
  return (await requestSeen).postDataJSON();
}

// Waits until the probe saw the row for this occurrence written as QUEUED.
export const awaitQueued = (page, occurrence) =>
  page.waitForFunction(
    (id) => window.__tkt558IdbProbe.events.some((event) => event.id === id && event.state === 'QUEUED' && event.event === 'complete'),
    occurrence,
  );

// Copies a value for retained evidence. A credential-bearing field keeps only its
// SHA-256. Snake-case keys take "_sha256" and camel-case keys take "Sha256".
const CREDENTIAL_KEY = /^(qr_?payload|credential|device_?token|token)$/i;
export function sanitizeDeep(value) {
  if (Array.isArray(value)) return value.map(sanitizeDeep);
  if (value === null || typeof value !== 'object') return value;
  const copy = {};
  for (const [key, item] of Object.entries(value)) {
    if (CREDENTIAL_KEY.test(key) && typeof item === 'string') copy[`${key}${key.includes('_') ? '_sha256' : 'Sha256'}`] = digest(item);
    else copy[key] = sanitizeDeep(item);
  }
  return copy;
}

// Sync observation.
//
// The page records every native transaction on the occurrence database, with
// each request made inside it. The recorder wraps the native methods and
// forwards each call and each result unchanged. It opens no connection, writes
// no row and sends no request. A result keeps only occurrence IDs and states,
// and an operation's arguments are never kept.
//
// The read shapes come from web/scanner/src/occurrence-store.ts. recoverDeadOwners
// reads in one readonly transaction: occurrences.getAll, then
// legacy_classes.getAllKeys. queued() reads in one readonly transaction:
// occurrences.getAllKeys, occurrences.getAll, then legacy_classes.getAll. The
// probe's own readbacks read the occurrences store alone, so they never match.
export const OBSERVED_STORES = ['legacy_classes', 'occurrences'];
export const TERMINAL_STATES = ['RESOLVED', 'SYNCED'];
const OBSERVED_SCOPE = [...OBSERVED_STORES].sort();
const RECOVERY_LIST_OPS = ['occurrences.getAll', 'legacy_classes.getAllKeys'];
const QUEUED_OPS = ['occurrences.getAllKeys', 'occurrences.getAll', 'legacy_classes.getAll'];
const WRITE_METHODS = ['add', 'clear', 'delete', 'put'];

// Runs in the page before any app code. It only observes.
export function installSyncObserver(context) {
  return context.addInitScript(() => {
    const observed = ['legacy_classes', 'occurrences'];
    const observer = (window.__tkt563Observer = { seq: 0, epochStart: 0, txs: [] });
    const records = new WeakMap();
    const isObserved = (scope) => scope.some((name) => observed.includes(name));
    const pick = (item) => (item !== null && typeof item === 'object'
      ? { occurrenceId: item.occurrenceId ?? null, state: item.state ?? null, legacyClass: item.legacyClass ?? null }
      : item);
    const pickResult = (value) => (Array.isArray(value) ? value.map(pick) : pick(value ?? null));

    const nativeTransaction = IDBDatabase.prototype.transaction;
    IDBDatabase.prototype.transaction = function (...args) {
      const tx = Reflect.apply(nativeTransaction, this, args);
      if (this.name !== 'gate-occurrences') return tx;
      const record = {
        id: observer.txs.length + 1,
        mode: tx.mode,
        scope: Array.from(tx.objectStoreNames).sort(),
        state: 'pending',
        created: ++observer.seq,
        completed: null,
        error: null,
        ops: [],
      };
      observer.txs.push(record);
      records.set(tx, record);
      tx.addEventListener('complete', () => {
        record.state = 'complete';
        record.completed = ++observer.seq;
      });
      tx.addEventListener('abort', () => {
        record.state = 'aborted';
        record.completed = ++observer.seq;
        record.error = tx.error ? tx.error.name : null;
      });
      return tx;
    };

    // Cursors are not wrapped. No observed read uses one.
    for (const method of ['add', 'clear', 'count', 'delete', 'get', 'getAll', 'getAllKeys', 'getKey', 'put']) {
      const native = IDBObjectStore.prototype[method];
      IDBObjectStore.prototype[method] = function (...args) {
        const request = Reflect.apply(native, this, args);
        const record = records.get(this.transaction);
        if (record) {
          const op = { store: this.name, method, result: null, error: null };
          record.ops.push(op);
          request.addEventListener('success', () => { op.result = pickResult(request.result); });
          request.addEventListener('error', () => { op.error = request.error ? request.error.name : 'error'; });
        }
        return request;
      };
    }

    observer.snapshot = () => ({ seq: observer.seq, epochStart: observer.epochStart, txs: observer.txs });
    observer.relevantPending = () => observer.txs.filter((tx) => tx.state === 'pending' && isObserved(tx.scope)).length;
    // Arms the epoch and dispatches online in one page task. It refuses while an observed transaction is pending.
    observer.armAndDispatchOnline = () => {
      if (observer.relevantPending() > 0) return null;
      observer.epochStart = observer.seq;
      window.dispatchEvent(new Event('online'));
      return observer.epochStart;
    };
  });
}

const isObservedTx = (tx) => tx.scope.some((name) => OBSERVED_STORES.includes(name));
const sameList = (a, b) => a.length === b.length && a.every((item, index) => item === b[index]);
const resultOf = (tx, store, method) => tx.ops.find((op) => op.store === store && op.method === method)?.result ?? [];

// The scanner read that a transaction makes, or null for any other transaction.
export function readName(tx) {
  if (tx.mode !== 'readonly' || !sameList(tx.scope, OBSERVED_SCOPE)) return null;
  const names = tx.ops.map((op) => `${op.store}.${op.method}`);
  if (sameList(names, RECOVERY_LIST_OPS)) return 'recoverDeadOwners.list';
  if (sameList(names, QUEUED_OPS)) return 'queued';
  return null;
}

// True when no observed transaction is pending and the last completed write has
// been followed by a completed queued() read. syncBatch ends with that read after
// it applies a batch, so this state means the batch's own work has finished.
export function priorWorkSettled(txs) {
  if (txs.some((tx) => isObservedTx(tx) && tx.state === 'pending')) return false;
  const writes = txs.filter((tx) => isObservedTx(tx) && tx.state === 'complete' && tx.mode === 'readwrite' && tx.ops.some((op) => WRITE_METHODS.includes(op.method)));
  if (writes.length === 0) return true;
  const lastWrite = Math.max(...writes.map((tx) => tx.completed));
  return txs.some((tx) => tx.state === 'complete' && readName(tx) === 'queued' && tx.completed > lastWrite);
}

// Counts the completed reads of one name that the epoch holds.
function countReads(txs, epochStart, name) {
  return txs.filter((tx) => isObservedTx(tx) && tx.created > epochStart && tx.state === 'complete' && readName(tx) === name).length;
}

// Judges the reads that one online event causes. Each failed condition is named.
// The expected reads are two recovery-list reads and two queued reads: one pair
// from refreshQueued and one pair from syncBatch, both in App.tsx's onOnline.
export function classifyOnline(txs, epochStart, expectedIds) {
  const epoch = txs.filter((tx) => isObservedTx(tx) && tx.created > epochStart);
  const recovery = epoch.filter((tx) => tx.state === 'complete' && readName(tx) === 'recoverDeadOwners.list');
  const queued = epoch.filter((tx) => tx.state === 'complete' && readName(tx) === 'queued');
  const reads = [...recovery, ...queued];
  const expected = [...expectedIds].filter(Boolean).sort();
  const rowsOf = (tx) => resultOf(tx, 'occurrences', 'getAll');
  const failures = [];
  if (recovery.length !== 2) failures.push(`recovery-list reads ${recovery.length}, expected 2`);
  if (queued.length !== 2) failures.push(`queued reads ${queued.length}, expected 2`);
  if (!recovery.some((read) => queued.some((next) => read.completed < next.completed))) {
    failures.push('no completed recovery-list read comes before a completed queued read');
  }
  const nonTerminal = reads.flatMap((tx) => rowsOf(tx).filter((row) => !TERMINAL_STATES.includes(row.state)).map((row) => `${row.occurrenceId}=${row.state}`));
  if (nonTerminal.length > 0) failures.push(`non-terminal rows ${nonTerminal.join(' ')}`);
  const legacy = reads.flatMap((tx) => [...resultOf(tx, 'legacy_classes', 'getAllKeys'), ...resultOf(tx, 'legacy_classes', 'getAll')]);
  if (legacy.length > 0) failures.push(`legacy classification values ${legacy.length}`);
  if (!reads.every((tx) => sameList(rowsOf(tx).map((row) => row.occurrenceId).sort(), expected))) {
    failures.push('an observed read does not hold exactly the batch occurrence IDs');
  }
  if (!queued.every((tx) => sameList([...resultOf(tx, 'occurrences', 'getAllKeys')].sort(), expected))) {
    failures.push('a queued read does not list exactly the batch occurrence IDs');
  }
  const aborted = epoch.filter((tx) => tx.state === 'aborted');
  if (aborted.length > 0) failures.push(`aborted observed transactions ${aborted.length}`);
  const pending = txs.filter((tx) => isObservedTx(tx) && tx.state === 'pending');
  if (pending.length > 0) failures.push(`pending observed transactions ${pending.length}`);
  return { settled: failures.length === 0, failures, recovery, queued };
}

// Evidence for one observed transaction: scope, mode, operations, results and order.
export function summarizeTx(tx) {
  return {
    id: tx.id,
    mode: tx.mode,
    scope: tx.scope,
    state: tx.state,
    created: tx.created,
    completed: tx.completed,
    read: readName(tx),
    error: tx.error,
    ops: tx.ops.map((op) => ({ store: op.store, method: op.method, result: op.result, error: op.error })),
  };
}

// Dispatches one online event under observation and reports what the page did.
// It waits until prior work has settled, then arms and dispatches in one page
// task. It waits for two recovery-list reads and two queued reads, yields a
// task, waits a short bounded quiet period, and reads the final record. The
// quiet period adds evidence. Only the completed reads decide the result. The
// function never throws, so a failure is recorded and the readbacks still run.
export async function observeOnlineSync(page, { timeout = 15000, quietMs = 300, expectedIds = [] } = {}) {
  const result = { armed: false, epochStart: null, error: null, quietMs, reads: null, transactions: [] };
  try {
    const armDeadline = Date.now() + timeout;
    for (;;) {
      const { txs } = await page.evaluate(() => window.__tkt563Observer.snapshot());
      if (priorWorkSettled(txs)) {
        const epochStart = await page.evaluate(() => window.__tkt563Observer.armAndDispatchOnline());
        if (epochStart !== null) {
          result.armed = true;
          result.epochStart = epochStart;
          break;
        }
      }
      if (Date.now() >= armDeadline) return result;
      await sleep(50);
    }

    const readDeadline = Date.now() + timeout;
    for (;;) {
      const { txs } = await page.evaluate(() => window.__tkt563Observer.snapshot());
      if (countReads(txs, result.epochStart, 'recoverDeadOwners.list') >= 2 && countReads(txs, result.epochStart, 'queued') >= 2) break;
      if (Date.now() >= readDeadline) break;
      await sleep(50);
    }

    // A task checkpoint follows the last completion. The quiet period supplements the reads.
    await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 0)));
    await sleep(quietMs);
    const drainDeadline = Date.now() + timeout;
    while ((await page.evaluate(() => window.__tkt563Observer.relevantPending())) > 0 && Date.now() < drainDeadline) {
      await sleep(50);
    }

    const final = await page.evaluate(() => window.__tkt563Observer.snapshot());
    result.reads = classifyOnline(final.txs, result.epochStart, expectedIds);
    result.transactions = final.txs.filter((tx) => isObservedTx(tx) && tx.created > result.epochStart).map(summarizeTx);
  } catch (error) {
    result.error = String(error?.message ?? error).slice(0, 1000);
  }
  return result;
}

// Waits until no observed transaction is pending. Returns false when the wait ends first.
export async function awaitStorageIdle(page, { timeout = 10000 } = {}) {
  const idle = await poll(() => page.evaluate(() => window.__tkt563Observer.relevantPending() === 0).catch(() => false), { timeout, interval: 50 });
  return idle === true;
}
