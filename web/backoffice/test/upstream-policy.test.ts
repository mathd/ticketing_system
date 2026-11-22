// @vitest-environment node
//
// TKT-602 (COS1), ADR-088: every Back Office upstream request goes through
// fetchUpstream, which refuses redirects. This scan fails when any other code in
// src calls fetch directly, so a new call site cannot keep the default redirect
// policy. It covers every module in src, including .tsx islands and .astro pages,
// and does not depend on which headers a call sends: a header scan would miss a
// computed name such as Inventory's `[headerName]`.
//
// Known limits, recorded in ADR-088: the scan does not see an alias
// (`const f = fetch`) or `fetch.call(...)`. It also counts the text `fetch(` in a
// comment or string, which errs toward a false failure.
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const SRC = fileURLToPath(new URL('../src', import.meta.url));
const SCANNED = /\.(?:[cm]?[jt]sx?|astro)$/;

// Bare `fetch(`, a qualified `globalThis.fetch(` or `window.fetch(`, and a
// literal bracket form such as `globalThis['fetch'](`. `fetchUpstream(` and
// `refetch(` do not match.
const DIRECT_FETCH = [
  /(?<![\w$.])fetch\s*\(/g,
  /\.\s*fetch\s*\(/g,
  /\[\s*(['"`])fetch\1\s*\]\s*\(/g,
];

type Site = { file: string; line: number; offset: number; text: string };

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(path);
    return SCANNED.test(entry.name) ? [path] : [];
  });
}

function directFetchSites(): Site[] {
  return sourceFiles(SRC).flatMap((path) => {
    const source = readFileSync(path, 'utf8');
    const offsets = new Set<number>();
    for (const pattern of DIRECT_FETCH) {
      for (const match of source.matchAll(pattern)) offsets.add(match.index);
    }
    return [...offsets].sort((a, b) => a - b).map((offset) => {
      const line = source.slice(0, offset).split('\n').length;
      return {
        file: relative(SRC, path),
        line,
        offset,
        text: source.split('\n')[line - 1].trim(),
      };
    });
  });
}

/** The [start, end) offsets of `function fetchUpstream` in upstream.ts. */
function fetchUpstreamBody(source: string): [number, number] {
  const start = source.indexOf('export function fetchUpstream(');
  if (start < 0) throw new Error('upstream.ts has no exported function fetchUpstream');
  const open = source.indexOf('{', source.indexOf(')', start));
  let depth = 0;
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++;
    if (source[i] === '}' && --depth === 0) return [start, i + 1];
  }
  throw new Error('fetchUpstream has no closing brace');
}

describe('Back Office upstream request policy', () => {
  it('scans the whole source tree, islands included', () => {
    const files = sourceFiles(SRC).map((path) => relative(SRC, path));
    expect(files).toContain(join('lib', 'upstream.ts'));
    expect(files.some((file) => file.endsWith('.tsx'))).toBe(true);
    expect(files.some((file) => file.endsWith('.astro'))).toBe(true);
  });

  it('has exactly one direct fetch, inside fetchUpstream', () => {
    const sites = directFetchSites();
    const report = sites.map((site) => `${site.file}:${site.line}: ${site.text}`);
    expect(report, 'call fetchUpstream from lib/upstream.ts instead of fetch').toHaveLength(1);

    const [site] = sites;
    expect(site.file).toBe(join('lib', 'upstream.ts'));
    const [start, end] = fetchUpstreamBody(readFileSync(join(SRC, 'lib', 'upstream.ts'), 'utf8'));
    expect(
      site.offset > start && site.offset < end,
      `${site.file}:${site.line} is outside fetchUpstream`,
    ).toBe(true);
  });
});
