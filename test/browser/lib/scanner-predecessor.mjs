import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe } from './scanner-controls.mjs';

const BASELINE = '3752a96a16a2ed6e7fe267206c3dbd05bf5271bf';
const PINNED = {
  'occurrence-model.ts': '392f528a2f5f0dc8109d676666ccd837d3f3599e989fe60c0e680842b731a17b',
  'occurrence-store.ts': 'b8cbcab7356d25847b6604145bf81a55c5026518b10cc3769cf59c2d7d66a31a',
  'reconcile-batch.ts': '8fb67d8b6a8818fb010d0dc6aac17c2d7649bfbb669f8e3449f12d1eeed1e504',
  'OccurrenceRecords.tsx': '724558a6f77ef6b2a611e7d3930a69a372e6c70b33ca11e3ef0f28ce759d6f46',
  'App.tsx': 'd070f4d9af1045a4d4331781729fdef60584dc01539bfe9fef4bcf55b4439140',
};
export async function predecessorExit({ browser, BASE, token, fixture, check }) {
  const root = resolve(import.meta.dirname, '../../..'), source = mkdtempSync(join(tmpdir(), 'tkt399-predecessor-'));
  const hash = bytes => createHash('sha256').update(bytes).digest('hex');
  const git = args => execFileSync('git', args, { cwd: root });
  const names = git(['ls-tree', '-r', '--name-only', BASELINE]).toString().trim().split('\n').filter(path => path.startsWith('web/scanner/') || ['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'].includes(path) || path.endsWith('/package.json'));
  const manifest = names.map(path => {
    const bytes = git(['show', `${BASELINE}:${path}`]); mkdirSync(dirname(join(source, path)), { recursive: true }); writeFileSync(join(source, path), bytes);
    return { path, sha256: hash(bytes) };
  });
  for (const [name, expected] of Object.entries(PINNED)) assert.equal(manifest.find(row => row.path === `web/scanner/src/${name}`)?.sha256, expected);
  const run = args => {
    try { execFileSync('pnpm', args, { cwd: source, stdio: 'pipe', timeout: 120000 }); }
    catch (error) { throw new Error(`TKT-399 predecessor prerequisite pnpm ${args.join(' ')}: ${error.stdout?.toString()} ${error.stderr?.toString()}`); }
  };
  try {
    assert.equal(execFileSync('pnpm', ['--version']).toString().trim(), '11.12.0');
    run(['install', '--offline', '--frozen-lockfile']); run(['--filter', 'scanner', 'build']);
    const app = join(source, 'web/scanner');
    // The App build above is unchanged. This second entry calls the exact extracted modules.
    writeFileSync(join(app, 'proof.html'), '<!doctype html><script type="module" src="/src/proof.js"></script>');
    writeFileSync(join(app, 'proof.config.mjs'), "export default {base:'/scanner/',build:{outDir:'proof-dist',rollupOptions:{input:'proof.html'}}};");
    writeFileSync(join(app, 'src/proof.js'), `
import { readCurrentRecord } from './occurrence-model';
import { createStorageController } from './occurrence-readiness';
import { pageCoordinator } from './occurrence-coordinator';
import { planReconcileBatches } from './reconcile-batch';
window.__predecessorProof = async raw => {
 const storage = createStorageController({coordinator:pageCoordinator});
 storage.start();
 await new Promise((resolve,reject) => {const off=storage.subscribe(state=>{if(state.status==='ready'){off();resolve();}else if(state.status==='failed'){off();reject(new Error('old storage failed'));}});});
 try {const store=storage.readyStore(), queue=await store.queued();return {decoded:readCurrentRecord(raw),queue,plan:planReconcileBatches(queue.entries),notices:await store.readNotices()};}
 finally {storage.stop();}
};`);
    run(['--filter', 'scanner', 'exec', 'vite', 'build', '--config', 'proof.config.mjs']);
    const assets = new Map();
    for (const directory of ['dist', 'proof-dist']) {
      const dist = join(app, directory);
      for (const name of readdirSync(dist)) if (name !== 'assets') assets.set(`/scanner/${name === 'index.html' ? '' : name}`, readFileSync(join(dist, name)));
      for (const name of readdirSync(join(dist, 'assets'))) assets.set(`/scanner/assets/${name}`, readFileSync(join(dist, 'assets', name)));
    }
    console.log('TKT-399 exact predecessor manifest', JSON.stringify({ baseline: BASELINE, sourceDigest: hash(JSON.stringify(manifest)), sources: manifest, assets: [...assets].map(([path, bytes]) => ({ path, sha256: hash(bytes) })) }));
    const context = await browser.newContext({ baseURL: BASE }); await installProbe(context);
    const pending = await context.newPage(), queued = await context.newPage(), recovered = await context.newPage();
    const leaseTime = new Date();
    for (const page of [pending, queued, recovered]) await page.addInitScript(now => { Date.now = () => now; }, leaseTime.getTime());
    try {
      await pair(pending, token); await pending.getByRole('radio', { name: 'Exit', exact: true }).check();
      await pending.evaluate(() => { const native=fetch;window.fetch=(input,init)=>input instanceof Request&&new URL(input.url).pathname==='/api/access/scans'?new Promise(()=>{}):native(input,init); });
      await submit(pending, fixture().credential);
      await pending.waitForFunction(() => window.__tkt558IdbProbe.events.some(event => event.state === 'PENDING' && event.event === 'complete'));
      const original = (await read(pending))[0]; await pending.close();
      await pair(queued, token); await queued.getByRole('radio', { name: 'Exit', exact: true }).check();
      await queued.route(scanPath, route => route.fulfill({ status: 503, body: '{}' })); await submit(queued, fixture().credential);
      await queued.getByRole('heading', { name: 'Offline Exit' }).waitFor();
      await queued.waitForFunction(() => window.__tkt558IdbProbe.events.some(event => event.state === 'QUEUED' && event.event === 'complete'));
      await queued.close();
      await pair(recovered, token); await recovered.getByRole('radio', { name: 'Exit', exact: true }).check();
      await recovered.evaluate(() => { const native=fetch;window.fetch=(input,init)=>input instanceof Request&&new URL(input.url).pathname==='/api/access/scans'?new Promise(()=>{}):native(input,init); });
      await submit(recovered, fixture().credential);
      await recovered.waitForFunction(() => window.__tkt558IdbProbe.events.some(event => event.state === 'PENDING' && event.event === 'complete'));
      const recoveryId = (await read(recovered)).find(row => row.occurrenceId !== original.occurrenceId && row.state === 'PENDING').occurrenceId;
      await recovered.reload();
      assert.equal(await recovered.evaluate(() => performance.getEntriesByType('navigation')[0]?.type), 'reload');
      await recovered.getByRole('region', { name: `Recovered scan ${recoveryId}` }).waitFor();
      const before = await read(recovered); await recovered.close();
      const old = await context.newPage(); await old.clock.install({ time: leaseTime }); await old.clock.fastForward(31000);
      const loaded = [], errors = [], scans = [], syncs = [];
      old.on('request', request => { if (request.method() === 'POST' && scanPath(request.url())) scans.push(request); if (request.method() === 'POST' && reconcilePath(request.url())) syncs.push(request); });
      await old.route('**/scanner/**', async route => {
        const path = new URL(route.request().url()).pathname, bytes = assets.get(path);
        if (!bytes) { errors.push(path); return route.fulfill({ status: 500, body: 'unlisted old asset' }); }
        loaded.push({ path, sha256: hash(bytes) });
        return route.fulfill({ body: bytes, contentType: path.endsWith('.js') ? 'text/javascript' : path.endsWith('.css') ? 'text/css' : 'text/html' });
      });
      await old.goto('/scanner/proof.html'); await old.waitForFunction(() => typeof window.__predecessorProof === 'function');
      const proof = await old.evaluate(raw => window.__predecessorProof(raw), original);
      assert.equal(proof.decoded.ok, false); assert.deepEqual(proof.queue.entries, []); assert.equal(proof.queue.warnings.length, 3);
      assert.deepEqual(proof.plan.batches, []); assert.equal(proof.notices[0].syncState, 'unreadable'); assert.equal(proof.notices[0].type, undefined);
      assert.deepEqual(await read(old), before);
      await old.goto('/scanner/'); await old.getByLabel('Ticket credential').waitFor();
      await old.getByRole('region', { name: `Recovered scan ${recoveryId}` }).waitFor();
      assert.equal(await old.getByText(/Type: unknown/).count(), 1);
      assert.equal(await old.getByRole('button', { name: 'Sync queued scans' }).count(), 0);
      assert.equal(await old.getByText(/Local record .*cannot be read/).count(), 0);
      await old.evaluate(() => window.dispatchEvent(new Event('online')));
      assert.deepEqual(await read(old), before); assert.equal(scans.length, 0); assert.equal(syncs.length, 0);
      assert.deepEqual(errors, []); assert.ok(loaded.some(row => row.path.endsWith('.js')));
      await old.close();
      const current = await context.newPage(); await current.addInitScript(now => { Date.now = () => now; }, leaseTime.getTime()); await pair(current, token);
      assert.ok(original.leaseExpiresAt > await current.evaluate(() => Date.now()));
      assert.deepEqual(await read(current), before);
      const eligible = before.filter(row => row.state === 'QUEUED');
      assert.equal(eligible.length, 2);
      assert.equal(before.find(row => row.occurrenceId === original.occurrenceId).state, 'PENDING');
      const sent = current.waitForRequest(request => request.method() === 'POST' && reconcilePath(request.url()));
      await current.getByRole('button', { name: 'Sync queued scans' }).click();
      const sorted = rows => [...rows].sort((a, b) => a.occurrence_id.localeCompare(b.occurrence_id));
      assert.deepEqual(sorted((await sent).postDataJSON().occurrences), sorted(eligible.map(row => ({
        occurrence_id: row.occurrenceId, qr_payload: row.qrPayload, occurred_at: row.occurredAt, event_type: 'exit',
      }))));
      await current.getByText('Synced 2 offline scan(s).', { exact: true }).waitFor();
      const expected = before.map(row => row.state === 'QUEUED' ? { ...row, state: 'SYNCED', result: 'recorded' } : row);
      await current.waitForFunction(ids => ids.every(id => window.__tkt558IdbProbe.events.some(event => event.id === id && event.state === 'SYNCED' && event.event === 'complete')), eligible.map(row => row.occurrenceId));
      assert.deepEqual(await read(current), expected);
      check('TKT-399 exact old v3 decoder/store/App silently exclude Exit, leave rows intact and never call it Entry; current client syncs Exit', true);
    } finally { await context.close(); }
  } finally { rmSync(source, { recursive: true, force: true }); }
}
