import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { assert, pair, submit, read, scanPath, reconcilePath } from './scanner-controls.mjs';

export async function oldReader({ browser, BASE, token, check, fixture }) {
  const root = resolve(import.meta.dirname, '../../..');
  const base = 'ec336db814d719130cdefd263ca84be73b9f06e2';
  const source = mkdtempSync(join(tmpdir(), 'tkt558-frozen-'));
  const sha = bytes => createHash('sha256').update(bytes).digest('hex');
  const git = args => execFileSync('git', args, { cwd: root });
  const files = git(['ls-tree','-r','--name-only',base]).toString().trim().split('\n').filter(path => path.startsWith('web/scanner/') || ['package.json','pnpm-lock.yaml','pnpm-workspace.yaml'].includes(path) || path.endsWith('/package.json'));
  const manifest = files.map(path => {
    const bytes = git(['show', `${base}:${path}`]), target = join(source,path);
    mkdirSync(dirname(target), { recursive: true }); writeFileSync(target, bytes);
    return { path, sha256: sha(bytes) };
  });
  assert.equal(manifest.find(row => row.path === 'web/scanner/src/occurrences.ts').sha256, '0b8ea0c89c4f75ece7ce306d27d1a28ec4824365adef154faac172c624413bfc');
  assert.equal(execFileSync('pnpm',['--version']).toString().trim(),'11.12.0');
  try {
    for (const args of [['install','--offline','--frozen-lockfile'],['--filter','scanner','build']]) {
      try { execFileSync('pnpm',args,{ cwd: source, stdio: 'pipe', timeout: 120000 }); }
      catch (error) { throw new Error(`B3 prerequisite failed: pnpm ${args.join(' ')}: ${error.stderr?.toString() || error.message}`); }
    }
    const dist = join(source,'web/scanner/dist');
    const assets = new Map([['/scanner/', readFileSync(join(dist,'index.html'))], ['/scanner/favicon.svg', readFileSync(join(dist,'favicon.svg'))], ...readdirSync(join(dist,'assets')).map(name => [`/scanner/assets/${name}`, readFileSync(join(dist,'assets',name))])]);
    console.log('B3 frozen source manifest', JSON.stringify({ base, pnpm: '11.12.0', combined: sha(JSON.stringify(manifest)), files: manifest, assets: [...assets].map(([path, bytes]) => ({ path, sha256: sha(bytes) })) }));
    const context = await browser.newContext({ baseURL: BASE });
    const old = await context.newPage(), loaded = [], routeErrors = [];
    const scans = [], reconciliations = [];
    context.on('request', request => { if (request.method() === 'POST' && scanPath(request.url())) scans.push(request); if (request.method() === 'POST' && reconcilePath(request.url())) reconciliations.push(request); });
    await context.addInitScript(() => {
      window.__tkt558OpenErrors = [];
      const native = indexedDB.open.bind(indexedDB);
      indexedDB.open = (...args) => { const request = native(...args); request.addEventListener('error', () => window.__tkt558OpenErrors.push(request.error?.name)); return request; };
    });
    await context.route('**/scanner/**', async route => {
      if (route.request().frame().page() !== old) return route.continue();
      const path = new URL(route.request().url()).pathname, bytes = assets.get(path);
      if (!bytes) { routeErrors.push(`B3 unrecorded old asset ${path}`); return route.fulfill({ status:500, body:'unrecorded frozen asset' }); }
      loaded.push({ path, sha256: sha(bytes) });
      await route.fulfill({ body: bytes, contentType: path.endsWith('.js') ? 'text/javascript' : path.endsWith('.css') ? 'text/css' : 'text/html' });
    });
    try {
      await old.goto('/scanner/'); await old.getByLabel('Pairing token').fill(token); await old.getByRole('button',{ name:'Pair device' }).click();
      await old.getByLabel('Ticket credential').waitFor();
      await old.getByText('This device holds no revocation list yet.').waitFor({ state: 'hidden' });
      const a = await context.newPage(), b = await context.newPage();
      await Promise.all([a.goto('/scanner/'), b.goto('/scanner/')]);
      for (const page of [a,b]) {
        await page.getByText('Scanner storage is not ready. No ticket was checked.').waitFor();
        assert.equal(await page.getByRole('button',{ name:'Check ticket',exact:true }).isDisabled(),true);
        assert.equal(await page.getByRole('button',{ name:'Use camera' }).isDisabled(),true);
        await page.getByLabel('Ticket credential').fill('input-while-blocked');
        await page.locator('form[aria-label="Ticket scan"]').evaluate(form => form.requestSubmit());
      }
      assert.equal(scans.length,0);
      await old.goto('about:blank');
      for (const page of [a,b]) await page.waitForFunction(() => !document.querySelector('form[aria-label="Ticket scan"] button[type="submit"]')?.disabled);
      assert.equal(scans.length,0); assert.deepEqual(await read(a),[]);
      const ticket = fixture(); await submit(a,ticket.credential); await a.getByRole('heading',{ name:'Accepted',exact:true }).waitFor();
      const before = await read(a); assert.equal(before.length,1);
      const scanCount = scans.length, syncCount = reconciliations.length;
      await old.goto('/scanner/'); await old.reload();
      await old.getByText('This device cannot save scans right now. No ticket was checked. Try again after restoring browser storage.').waitFor();
      await old.getByLabel('Ticket credential').fill('old-must-fail');
      await old.getByRole('button',{ name:'Check ticket',exact:true }).click();
      await old.getByText('This device cannot save scans right now. No ticket was checked. Try again after restoring browser storage.').waitFor();
      assert.ok((await old.evaluate(() => window.__tkt558OpenErrors)).includes('VersionError'));
      assert.equal(scans.length,scanCount); assert.equal(reconciliations.length,syncCount); assert.deepEqual(await read(a),before);
      assert.deepEqual(routeErrors,[]);
      assert.ok(loaded.some(row => row.path.endsWith('.js')));
      for (const row of loaded) assert.equal(row.sha256,sha(assets.get(row.path)));
      check('B3 actual frozen v2 reader blocks both new pages; fresh input works after closure; old reload fails closed with VersionError',true);
    } finally { await context.close(); }
  } finally { rmSync(source,{recursive:true,force:true}); }
}
