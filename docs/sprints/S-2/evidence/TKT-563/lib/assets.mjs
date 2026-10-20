// Served-asset checks for the TKT-563 probe.
//
// The stock check compares what the stack serves with web/scanner/dist from the
// candidate build. The diagnostic route serves the disposable mutant build to
// one browser context only. The Access backend stays the real stack.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { extname, join, normalize, sep } from 'node:path';
import { digest } from './kit.mjs';

const CONTENT_TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.json': 'application/json',
  '.woff2': 'font/woff2',
};

const scannerUrl = (url) => {
  const { pathname } = new URL(typeof url === 'string' ? url : url.href);
  return pathname === '/scanner' || pathname.startsWith('/scanner/');
};

// Maps a /scanner/ URL path to a file inside dist, or null when it escapes dist.
export function distFile(distDir, pathname) {
  const root = normalize(distDir);
  const relative = pathname.replace(/^\/scanner\/?/, '') || 'index.html';
  let target = normalize(join(root, relative));
  if (target !== root && !target.startsWith(root + sep)) return null;
  if (existsSync(target) && statSync(target).isDirectory()) target = join(target, 'index.html');
  return existsSync(target) && statSync(target).isFile() ? target : null;
}

// Digests of every file under dist, used to show that a served file is not a stock file.
export function treeDigests(dir) {
  const found = new Set();
  const walk = (current) => {
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      const full = join(current, entry.name);
      if (entry.isDirectory()) walk(full);
      else if (entry.isFile()) found.add(digest(readFileSync(full)));
    }
  };
  walk(dir);
  return found;
}

// Compares the index and every /scanner/assets reference with dist. Requests use
// the same browser context as the journeys, so they hit the real stack.
export async function checkStockAssets(request, base, distDir) {
  const indexResponse = await request.get(`${base}/scanner/`);
  const indexBytes = await indexResponse.body();
  const distIndex = readFileSync(join(distDir, 'index.html'));
  const refs = [...new Set([...indexBytes.toString('utf8').matchAll(/(?:src|href)="(\/scanner\/assets\/[^"]+)"/g)].map((match) => match[1]))];
  const assets = [];
  for (const ref of refs) {
    const response = await request.get(`${base}${ref}`);
    const bytes = await response.body();
    const file = distFile(distDir, ref);
    assets.push({ path: ref, status: response.status(), sha256: digest(bytes), distSha256: file ? digest(readFileSync(file)) : null });
  }
  return {
    index: { status: indexResponse.status(), sha256: digest(indexBytes), distSha256: digest(distIndex) },
    assets,
    indexMatches: indexResponse.status() === 200 && digest(indexBytes) === digest(distIndex),
    assetsMatch: refs.length > 0 && assets.every((asset) => asset.status === 200 && asset.distSha256 !== null && asset.sha256 === asset.distSha256),
  };
}

// Serves the disposable build for /scanner/ requests in this context only. Each
// fulfilled body is recorded, so the evidence shows the bytes the page ran.
export async function installDiagnosticAssets(context, distDir, served) {
  await context.route(scannerUrl, (route) => {
    const { pathname } = new URL(route.request().url());
    const file = distFile(distDir, pathname);
    if (!file) return route.fulfill({ status: 404, body: 'not found' });
    const body = readFileSync(file);
    served.push({ path: pathname, sha256: digest(body) });
    return route.fulfill({ status: 200, contentType: CONTENT_TYPES[extname(file)] ?? 'application/octet-stream', body });
  });
}
