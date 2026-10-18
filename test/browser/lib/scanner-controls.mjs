import assert from 'node:assert/strict';
export { assert };
export async function blank(context) {
  const page = await context.newPage();
  await page.route('**/scanner/', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>fixture</title>' }));
  await page.goto('/scanner/');
  await page.unroute('**/scanner/');
  return page;
}
export async function read(page, store = 'occurrences') {
  return page.evaluate(store => new Promise((resolve, reject) => {
    const request = indexedDB.open('gate-occurrences');
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      const db = request.result;
      const tx = db.transaction(store, 'readonly');
      const rows = tx.objectStore(store).getAll();
      tx.oncomplete = () => { db.close(); resolve(rows.result); };
      tx.onabort = () => { db.close(); reject(tx.error); };
    };
  }), store);
}
export async function seedLegacy(page, version, rows) {
  await page.evaluate(({ version, rows }) => new Promise((resolve, reject) => {
    const request = indexedDB.open('gate-occurrences', version);
    request.onupgradeneeded = () => {
      request.result.createObjectStore('occurrences', { keyPath: 'occurrenceId' });
      if (version >= 2) { request.result.createObjectStore('revocations', { keyPath: 'ticketID' }); request.result.createObjectStore('revocation_meta', { keyPath: 'key' }); }
    };
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      const db = request.result, tx = db.transaction('occurrences', 'readwrite');
      rows.forEach(row => tx.objectStore('occurrences').add(row));
      tx.oncomplete = () => { db.close(); resolve(); };
      tx.onabort = () => { db.close(); reject(tx.error); };
    };
  }), { version, rows });
}
export async function pair(page, token) {
  await page.goto('/scanner/');
  const input = page.getByLabel('Pairing token');
  if (await input.count()) { await input.fill(token); await page.getByRole('button', { name: 'Pair device' }).click(); }
  await page.getByRole('button', { name: 'Check ticket' }).waitFor();
  await page.getByText('Scanner storage is not ready. No ticket was checked.').waitFor({ state: 'hidden' });
  await page.getByText('This device holds no revocation list yet.').waitFor({ state: 'hidden' });
}
export async function submit(page, credential) {
  await page.getByLabel('Ticket credential').fill(credential);
  await page.getByRole('button', { name: 'Check ticket', exact: true }).click();
}
export const scanPath = url => new URL(typeof url === 'string' ? url : url.href).pathname === '/api/access/scans';
export const reconcilePath = url => new URL(typeof url === 'string' ? url : url.href).pathname === '/api/access/scans/reconciliations';
// Installed by the runner before the production script. The product never reads it.
export async function installProbe(context) {
  await context.addInitScript(() => {
    const probe = window.__tkt558IdbProbe = { rules: [], events: [], connections: [], holds: {}, failures: [] };
    const nativeTx = IDBDatabase.prototype.transaction;
    const nativeOpen = indexedDB.open.bind(indexedDB);
    indexedDB.open = (...args) => {
      const request = nativeOpen(...args);
      request.addEventListener('success', () => { if (request.result.name === 'gate-occurrences') probe.connections.push(request.result); });
      return request;
    };
    IDBDatabase.prototype.transaction = function (...args) {
      const tx = Reflect.apply(nativeTx, this, args);
      if (this.name !== 'gate-occurrences') return tx;
      const observations = [];
      tx.addEventListener('complete', () => observations.forEach(row => probe.events.push({ ...row, event: 'complete' })));
      tx.addEventListener('abort', () => observations.forEach(row => probe.events.push({ ...row, event: 'abort' })));
      const objectStore = tx.objectStore.bind(tx);
      tx.objectStore = name => {
        const store = objectStore(name);
        for (const method of ['put', 'add']) {
          const native = store[method].bind(store);
          store[method] = (value, ...keys) => {
            const id = value?.occurrenceId ?? keys[0] ?? value?.receiptId;
            const observed = { id, state: value?.state, store: name, durability: tx.durability };
            observations.push(observed);
            const rule = probe.rules.find(rule => (!rule.id || rule.id === id) && (!rule.store || rule.store === name) && (!rule.state || rule.state === value?.state) && (!rule.reason || rule.reason === value?.localOutcome?.reason) && (!rule.once || !rule.used));
            if (rule) { rule.used = true; probe.events.push({ ...observed, event: 'matched', action: rule.action }); }
            if (rule?.action === 'throw') throw new DOMException('Injected write failure', 'QuotaExceededError');
            const request = native(value, ...keys);
            request.addEventListener('success', () => {
              probe.events.push({ ...observed, event: 'write-success' });
              if (rule?.action === 'abort') tx.abort();
              if (rule?.action === 'hold') {
                const hold = probe.holds[rule.name] = { released: false, abort: false };
                let count = 0;
                const pump = () => {
                  if (hold.released) { if (hold.abort) tx.abort(); return; }
                  if (++count > 200000) { probe.failures.push('hold watchdog ' + rule.name); tx.abort(); return; }
                  const read = objectStore(name).count(); read.onsuccess = pump;
                };
                pump();
              }
            });
            return request;
          };
        }
        return store;
      };
      return tx;
    };
  });
}
export const arm = (page, rule) => page.evaluate(rule => window.__tkt558IdbProbe.rules.push(rule), rule);
export const release = (page, name, abort = false) => page.evaluate(({ name, abort }) => Object.assign(window.__tkt558IdbProbe.holds[name], { released: true, abort }), { name, abort });
export const matched = (page, rule) => page.waitForFunction(rule => window.__tkt558IdbProbe.events.some(e => e.event === 'matched' && (!rule.id || e.id === rule.id) && (!rule.state || e.state === rule.state)), rule);
