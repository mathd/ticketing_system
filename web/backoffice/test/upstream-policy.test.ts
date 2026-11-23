// @vitest-environment node
//
// TKT-602 (COS1), ADR-088: every Back Office upstream request goes through
// fetchUpstream, which refuses redirects. This scan fails when any other
// first-party runtime or configuration code refers to `fetch`, so a new call site
// cannot keep the default redirect policy. It does not depend on which headers a
// call sends: a header scan would miss a computed name such as Inventory's
// `[headerName]`.
//
// Scope: every script and Astro file in the package, src/ and the package root
// (start.mjs, credentials.mjs, astro.config.mjs, ...) included, and any new
// directory. Only test/, dist/, node_modules/ and the generated .astro/ at the
// package root are out. A directory with one of these names deeper down, such as
// src/lib/test/, is scanned.
//
// The scan parses each file with the parsers that the build uses: Astro's own
// compiler for .astro (frontmatter, template expressions and <script> bodies),
// and rolldown's parser for scripts. It then looks at the syntax tree, so
// comments, page text and JSX text cannot hide or fake a reference. It flags:
//   - every identifier named `fetch`: a call, an optional call, `globalThis.fetch`,
//     `fetch.call`/`.apply`/`.bind`, an alias, `fetch` passed as a value, and an
//     imported `fetch`;
//   - every string or plain template literal equal to `fetch`, as in
//     `globalThis['fetch']`.
// It errs toward a false failure (an object key or a type named `fetch` also
// counts).
//
// A second scan closes the other way out: an outbound transport that is not
// fetch (R2-4). No first-party file may load node:http, node:https, node:http2,
// node:net or node:tls (with or without the `node:` prefix), undici, or a common
// HTTP client package (TRANSPORTS below), also by a subpath. It flags a static
// import or re-export, `import x = require()`, a dynamic `import()`, `require()`,
// `module.require()`, `createRequire(...)()` and `process.getBuiltinModule()`, also
// with the member name in brackets (`process['getBuiltinModule']()`). A module
// load whose name is not a literal is flagged too, because the scan cannot tell
// what it loads (ticket decision AD4). Declarations that the build removes load
// nothing and do not count: `import type`, `export type ... from` and
// `export type * from`. `import { type X } from` still counts: the package
// tsconfig sets verbatimModuleSyntax, so the build keeps it as `import 'x'`.
//
// Known limits, recorded in ADR-088: a name built at run time
// (`globalThis['fe' + 'tch']`), a loader reached through an alias
// (`const r = require`), a transport that is not in TRANSPORTS, and a request
// that a dependency makes with its own fetch or HTTP client.
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';

const PACKAGE = fileURLToPath(new URL('..', import.meta.url));
const SCANNED = /\.(?:[cm]?[jt]sx?|astro)$/;
// Tests, build output, dependencies and Astro's generated types, at the package
// root only.
const EXCLUDED_DIRS = new Set(['test', 'dist', 'node_modules', '.astro']);

// The parsers come from the Back Office's own `astro` dependency, so the scan
// reads each file the way the build does.
const fromAstro = createRequire(createRequire(join(PACKAGE, 'package.json')).resolve('astro/package.json'));
const { parse: parseAstro } = (await import(pathToFileURL(fromAstro.resolve('@astrojs/compiler-rs')).href)) as {
  parse(input: string): { ast: unknown; diagnostics: Array<{ severity?: unknown; text?: string }> };
};
const { parseAst } = (await import(pathToFileURL(fromAstro.resolve('rolldown/parseAst')).href)) as {
  parseAst(code: string, options: { lang: 'js' | 'jsx' | 'ts' | 'tsx'; sourceType: 'module' }): unknown;
};

type Node = { type: string; start: number; end: number; [key: string]: unknown };
type Site = { file: string; line: number; offset: number; text: string };

function sourceFiles(root: string, dir = root): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return dir === root && EXCLUDED_DIRS.has(entry.name) ? [] : sourceFiles(root, path);
    return SCANNED.test(entry.name) ? [path] : [];
  });
}

function isError(severity: unknown): boolean {
  return severity === 'error' || severity === 'Error' || severity === 1;
}

/** The syntax tree of one file. A file that does not parse fails the scan. */
function syntaxTree(name: string, source: string): Node {
  if (name.endsWith('.astro')) {
    const { ast, diagnostics } = parseAstro(source);
    const errors = diagnostics.filter((d) => isError(d.severity));
    if (errors.length > 0) throw new Error(`${name}: Astro parse errors: ${errors.map((d) => d.text).join('; ')}`);
    return ast as Node;
  }
  const lang = name.endsWith('.tsx') ? 'tsx' : name.endsWith('.jsx') ? 'jsx' : /\.[cm]?ts$/.test(name) ? 'ts' : 'js';
  return parseAst(source, { lang, sourceType: 'module' }) as Node;
}

function* nodes(value: unknown): Generator<Node> {
  if (Array.isArray(value)) {
    for (const item of value) yield* nodes(item);
  } else if (value !== null && typeof value === 'object') {
    if (typeof (value as Node).type === 'string') yield value as Node;
    for (const child of Object.values(value)) yield* nodes(child);
  }
}

function refersToFetch(node: Node): boolean {
  if (node.type === 'Identifier') return node.name === 'fetch';
  if (node.type === 'Literal') return node.value === 'fetch';
  if (node.type === 'TemplateLiteral') {
    const quasis = node.quasis as Array<{ value: { cooked: string | null } }>;
    return (node.expressions as unknown[]).length === 0 && quasis[0]?.value.cooked === 'fetch';
  }
  return false;
}

/**
 * Every reference to `fetch` in one file, by source offset. One offset counts
 * once: `import { fetch }` holds the imported and the local name at one place.
 */
function fetchReferences(name: string, source: string): number[] {
  const offsets = [...nodes(syntaxTree(name, source))].filter(refersToFetch).map((node) => node.start);
  return [...new Set(offsets)].sort((a, b) => a - b);
}

/**
 * Outbound transports other than fetch, by bare module name. A subpath of one
 * (`undici/...`) counts as the module.
 */
const TRANSPORTS = [
  'http',
  'https',
  'http2',
  'net',
  'tls',
  'undici',
  'axios',
  'got',
  'node-fetch',
  'ky',
  'superagent',
  'cross-fetch',
  'isomorphic-fetch',
  'ofetch',
  'needle',
  'request',
  'wretch',
];

function isTransport(specifier: string): boolean {
  const bare = specifier.replace(/^node:/, '');
  return TRANSPORTS.some((name) => bare === name || bare.startsWith(`${name}/`));
}

/** The literal value of a module name, or null when it is computed. */
function literalName(node: Node | null | undefined): string | null {
  if (!node) return null;
  if (node.type === 'Literal' && typeof node.value === 'string') return node.value;
  if (node.type === 'TemplateLiteral' && (node.expressions as unknown[]).length === 0) {
    return (node.quasis as Array<{ value: { cooked: string | null } }>)[0]?.value.cooked ?? null;
  }
  return null;
}

function isNamed(node: Node | undefined, name: string): boolean {
  return node?.type === 'Identifier' && node.name === name;
}

/** The member name of `x.name`, `x['name']` or x[`name`], or null. */
function memberName(member: Node): string | null {
  const property = member.property as Node;
  if (property.type === 'Identifier') return property.name as string;
  return member.computed ? literalName(property) : null;
}

/**
 * The module name node of a module load, or undefined when `node` loads no
 * module. Covers import and export declarations, `import x = require()`,
 * `import()`, `require()`, `x.require()`, `createRequire(...)()` and
 * `x.getBuiltinModule()`. A type-only declaration (`import type`,
 * `export type ... from`) loads nothing: the build removes it.
 */
function loadedModule(node: Node): Node | null | undefined {
  switch (node.type) {
    case 'ImportDeclaration':
      return node.importKind === 'type' ? undefined : (node.source as Node);
    case 'ExportAllDeclaration':
      return node.exportKind === 'type' ? undefined : (node.source as Node);
    case 'ImportExpression':
      return node.source as Node;
    case 'ExportNamedDeclaration':
      // `export const x = ...` loads nothing.
      return node.exportKind === 'type' ? undefined : ((node.source as Node | null) ?? undefined);
    case 'TSExternalModuleReference':
      return node.expression as Node;
    case 'CallExpression': {
      const callee = node.callee as Node;
      const loads =
        isNamed(callee, 'require') ||
        (callee.type === 'MemberExpression' &&
          (memberName(callee) === 'require' || memberName(callee) === 'getBuiltinModule')) ||
        (callee.type === 'CallExpression' && isNamed(callee.callee as Node, 'createRequire'));
      return loads ? (((node.arguments as Node[])[0] ?? null) as Node | null) : undefined;
    }
    default:
      return undefined;
  }
}

/**
 * Every module load in one file that names an outbound transport, or whose
 * name the scan cannot read. Each entry is the module name, or `<computed>`.
 */
function transportLoads(name: string, source: string): Array<{ offset: number; specifier: string }> {
  return [...nodes(syntaxTree(name, source))].flatMap((node) => {
    const module = loadedModule(node);
    if (module === undefined) return [];
    const specifier = literalName(module);
    if (specifier === null) return [{ offset: node.start, specifier: '<computed>' }];
    return isTransport(specifier) ? [{ offset: node.start, specifier }] : [];
  });
}

function siteAt(path: string, source: string, offset: number): Site {
  const line = source.slice(0, offset).split('\n').length;
  return { file: relative(PACKAGE, path), line, offset, text: source.split('\n')[line - 1].trim() };
}

function transportSites(): Site[] {
  return sourceFiles(PACKAGE).flatMap((path) => {
    const source = readFileSync(path, 'utf8');
    return transportLoads(path, source).map(({ offset }) => siteAt(path, source, offset));
  });
}

function directFetchSites(): Site[] {
  return sourceFiles(PACKAGE).flatMap((path) => {
    const source = readFileSync(path, 'utf8');
    return fetchReferences(path, source).map((offset) => siteAt(path, source, offset));
  });
}

/** The [start, end) offsets of `function fetchUpstream` in upstream.ts. */
function fetchUpstreamBody(source: string): [number, number] {
  const found = [...nodes(syntaxTree('upstream.ts', source))].filter(
    (node) => node.type === 'FunctionDeclaration' && (node.id as Node | null)?.name === 'fetchUpstream',
  );
  if (found.length !== 1) throw new Error(`upstream.ts has ${found.length} functions named fetchUpstream`);
  return [found[0].start, found[0].end];
}

describe('Back Office upstream request policy', () => {
  it('scans all first-party runtime and configuration code, and no tests or build output', () => {
    const files = sourceFiles(PACKAGE).map((path) => relative(PACKAGE, path));
    for (const file of [
      'start.mjs',
      'credentials.mjs',
      'astro.config.mjs',
      join('src', 'middleware.ts'),
      join('src', 'lib', 'upstream.ts'),
    ]) {
      expect(files).toContain(file);
    }
    expect(files.some((file) => file.startsWith('src') && file.endsWith('.tsx'))).toBe(true);
    expect(files.some((file) => file.startsWith('src') && file.endsWith('.astro'))).toBe(true);
    const outside = files.filter((file) => /^(test|dist|node_modules|\.astro)\b/.test(file));
    expect(outside).toEqual([]);
  });

  it('excludes test/, dist/, node_modules/ and .astro/ only at the package root', () => {
    const root = mkdtempSync(join(tmpdir(), 'upstream-policy-'));
    try {
      const all = ['test/a.ts', 'dist/b.mjs', 'node_modules/c.js', '.astro/d.ts', 'src/lib/test/e.ts', 'src/pages/dist/f.astro'];
      for (const file of all) {
        mkdirSync(dirname(join(root, file)), { recursive: true });
        writeFileSync(join(root, file), '');
      }
      const found = sourceFiles(root).map((path) => relative(root, path).split('\\').join('/'));
      expect(found.sort()).toEqual(['src/lib/test/e.ts', 'src/pages/dist/f.astro']);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it('has exactly one reference to fetch, inside fetchUpstream', () => {
    const sites = directFetchSites();
    const report = sites.map((site) => `${site.file}:${site.line}: ${site.text}`);
    expect(sites.length, `call fetchUpstream from src/lib/upstream.ts instead of fetch:\n${report.join('\n')}`).toBe(1);

    const [site] = sites;
    expect(site.file).toBe(join('src', 'lib', 'upstream.ts'));
    const [start, end] = fetchUpstreamBody(readFileSync(join(PACKAGE, 'src', 'lib', 'upstream.ts'), 'utf8'));
    expect(
      site.offset > start && site.offset < end,
      `${site.file}:${site.line} is outside fetchUpstream`,
    ).toBe(true);
  });

  it('loads no other outbound transport', () => {
    const report = transportSites().map((site) => `${site.file}:${site.line}: ${site.text}`);
    expect(report, 'send upstream requests through fetchUpstream in src/lib/upstream.ts').toEqual([]);
  });
});

// The detector itself, on synthetic sources. Each form must be found once, and
// look-alikes in comments, strings, page text and other names must not count.
describe('the fetch reference detector', () => {
  it.each([
    ['a direct call', 'x.ts', 'fetch(url);'],
    ['an optional call', 'x.ts', 'fetch?.(url);'],
    ['globalThis.fetch', 'x.ts', 'globalThis.fetch(url);'],
    ["globalThis['fetch']", 'x.ts', "globalThis['fetch'](url);"],
    ['globalThis[`fetch`]', 'x.ts', 'globalThis[`fetch`](url);'],
    ['fetch.call', 'x.ts', 'fetch.call(globalThis, url);'],
    ['fetch.apply', 'x.ts', 'fetch.apply(globalThis, [url]);'],
    ['fetch.bind', 'x.ts', 'const f = fetch.bind(globalThis);'],
    ['an alias', 'x.ts', 'const f = fetch; f(url);'],
    ['fetch passed as a value', 'x.ts', 'run(fetch);'],
    ['a unicode-escaped name', 'x.ts', 'f\\u0065tch(url);'],
    ['an import', 'x.mjs', "import { fetch } from 'undici';"],
    ['a call in a tsx expression after JSX text', 'x.tsx', "const a = <p>it's {fetch(url)}</p>;"],
    ['a call in Astro frontmatter', 'x.astro', '---\nconst r = await fetch(url);\n---\n<p>{r}</p>'],
    ['a call in an Astro template expression', 'x.astro', "---\n---\n<p>it's {fetch(url)}</p>"],
    ['an alias in an Astro script', 'x.astro', '<script>\nconst f = fetch;\n</script>'],
    ['a call in an inline Astro script', 'x.astro', "<script is:inline>window.fetch('x')</script>"],
  ])('finds %s', (_name, file, source) => {
    expect(fetchReferences(file, source)).toHaveLength(1);
  });

  it.each([
    ['the helper', 'x.ts', 'fetchUpstream(url, {});'],
    ['another name', 'x.ts', 'refetch(); prefetch; fetched;'],
    ['a comment', 'x.ts', '// fetch(url)\n/* globalThis.fetch(url) */'],
    ['a longer string', 'x.ts', "const s = 'fetch(url) failed';"],
    ['JSX text', 'x.tsx', 'const a = <p>fetch(url)</p>;'],
    ['Astro page text and an HTML comment', 'x.astro', '<p>fetch(url)</p>\n<!-- fetch(url) -->'],
  ])('ignores %s', (_name, file, source) => {
    expect(fetchReferences(file, source)).toEqual([]);
  });

  it.each([
    ['a script', 'x.ts', 'const = fetch;'],
    ['an Astro file', 'x.astro', '---\nconst = fetch;\n---\n<p>{fetch(</p>'],
  ])('fails, rather than skips, %s that does not parse', (_name, file, source) => {
    expect(() => fetchReferences(file, source)).toThrow();
  });
});

// The transport detector, on synthetic sources. Each load must be found once,
// and other modules and look-alike strings must not count.
describe('the outbound transport detector', () => {
  it.each([
    ['a default import of node:https', 'x.ts', "import https from 'node:https';"],
    ['a named import of node:http', 'x.ts', "import { request } from 'node:http';"],
    ['a namespace import of http', 'x.ts', "import * as http from 'http';"],
    ['https without the prefix', 'x.ts', "import https from 'https';"],
    ['node:http2', 'x.ts', "import { connect } from 'node:http2';"],
    ['http2', 'x.ts', "import { connect } from 'http2';"],
    ['node:net', 'x.ts', "import { Socket } from 'node:net';"],
    ['net', 'x.ts', "import net from 'net';"],
    ['node:tls', 'x.ts', "import { connect } from 'node:tls';"],
    ['tls', 'x.ts', "import tls from 'tls';"],
    ['undici', 'x.ts', "import { request } from 'undici';"],
    ['an undici subpath', 'x.ts', "import { Agent } from 'undici/lib/agent.js';"],
    ['axios', 'x.ts', "import axios from 'axios';"],
    ['got', 'x.ts', "import got from 'got';"],
    ['node-fetch', 'x.ts', "import nodeFetch from 'node-fetch';"],
    ['ky', 'x.ts', "import ky from 'ky';"],
    ['superagent', 'x.ts', "import superagent from 'superagent';"],
    ['a side-effect import', 'x.ts', "import 'node:https';"],
    // verbatimModuleSyntax: the build keeps this as `import 'node:https'`.
    ['an import whose every specifier is a type', 'x.ts', "import { type Agent } from 'node:https';"],
    ['a re-export of all', 'x.ts', "export * from 'got';"],
    ['a named re-export', 'x.ts', "export { request } from 'undici';"],
    ['import = require', 'x.ts', "import https = require('https');"],
    ['a dynamic import', 'x.ts', "const https = await import('node:https');"],
    ['a dynamic import of a template', 'x.ts', 'const https = await import(`node:https`);'],
    ['require', 'x.cjs', "const https = require('https');"],
    ['module.require', 'x.cjs', "const net = module.require('net');"],
    ['createRequire', 'x.mjs', "const tls = createRequire(import.meta.url)('node:tls');"],
    ['process.getBuiltinModule', 'x.mjs', "const https = process.getBuiltinModule('node:https');"],
    ["process['getBuiltinModule']", 'x.mjs', "const https = process['getBuiltinModule']('node:https');"],
    ["module['require']", 'x.cjs', "const net = module['require']('net');"],
    ['process[`getBuiltinModule`]', 'x.mjs', "const tls = process[`getBuiltinModule`]('node:tls');"],
    ['a dynamic import with a computed name', 'x.ts', 'const m = await import(name);'],
    ['a require with a computed name', 'x.cjs', 'const m = require(name);'],
    ['an import in Astro frontmatter', 'x.astro', "---\nimport https from 'node:https';\n---\n<p>x</p>"],
    ['a dynamic import in Astro frontmatter', 'x.astro', "---\nconst u = await import('undici');\n---\n<p>x</p>"],
    ['an import in an Astro script', 'x.astro', "<script>\nimport ky from 'ky';\n</script>"],
    ['an import in a tsx island', 'x.tsx', "import axios from 'axios';\nexport const A = () => <p>x</p>;"],
  ])('finds %s', (_name, file, source) => {
    expect(transportLoads(file, source)).toHaveLength(1);
  });

  it.each([
    ['another built-in module', 'x.ts', "import { randomUUID } from 'node:crypto';"],
    ['a relative module with a transport name', 'x.ts', "import { x } from './http';\nimport { y } from '../https/index.ts';"],
    ['a package whose name starts with a transport name', 'x.ts', "import kysely from 'kysely';\nimport httpx from 'httpx';"],
    ['a string equal to a transport name', 'x.ts', "const scheme = 'https';\nconst ok = url.protocol === 'https:';"],
    ['a comment', 'x.ts', "// import https from 'node:https';\n/* require('net') */"],
    ['a local export', 'x.ts', 'export const https = 1;\nexport function net() {}'],
    ['a literal dynamic import of the build output', 'x.mjs', "await import('./dist/server/entry.mjs');"],
    ['a type-only import', 'x.ts', "import type { Agent } from 'node:https';"],
    ['type-only re-exports', 'x.ts', "export type { Agent } from 'node:https';\nexport type * from 'undici';"],
    ['Astro page text', 'x.astro', "<p>import https from 'node:https'</p>"],
  ])('ignores %s', (_name, file, source) => {
    expect(transportLoads(file, source)).toEqual([]);
  });
});
