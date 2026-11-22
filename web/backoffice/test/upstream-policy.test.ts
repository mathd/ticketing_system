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
// directory. Only test/, dist/, node_modules/ and the generated .astro/ are out.
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
// counts). Known limits, recorded in ADR-088: a name built at run time
// (`globalThis['fe' + 'tch']`), and a fetch inside a dependency.
import { readdirSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join, relative } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';

const PACKAGE = fileURLToPath(new URL('..', import.meta.url));
const SCANNED = /\.(?:[cm]?[jt]sx?|astro)$/;
// Tests, build output, dependencies and Astro's generated types.
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

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return EXCLUDED_DIRS.has(entry.name) ? [] : sourceFiles(path);
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

function directFetchSites(): Site[] {
  return sourceFiles(PACKAGE).flatMap((path) => {
    const source = readFileSync(path, 'utf8');
    return fetchReferences(path, source).map((offset) => {
      const line = source.slice(0, offset).split('\n').length;
      return { file: relative(PACKAGE, path), line, offset, text: source.split('\n')[line - 1].trim() };
    });
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
