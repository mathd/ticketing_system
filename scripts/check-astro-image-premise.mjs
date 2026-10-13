#!/usr/bin/env node
// Guards the premise of the accepted risk for CVE-2026-93748 (TKT-504).
//
// http-cache-semantics 4.3.0 has no fixed release. Astro uses it only in the
// build-time remote image loader (astro/dist/assets/build/remote.js). Astro's
// image pipeline calls that loader only for a remote image that the config
// allows. The loader itself does not check the initial URL. The accepted-risk
// paragraph in docs/conventions/security.md depends on this premise: the web
// apps do not use Astro image optimization and do not import the loader. This
// script fails when that premise stops being true in the places it can see.
//
//   node scripts/check-astro-image-premise.mjs [--root DIR]
//
// For web/backoffice and web/storefront it:
//   1. evaluates astro.config.mjs (the evaluated config, not its text) twice:
//      with NODE_ENV unset, and with NODE_ENV=production, which astro build
//      sets before it loads the config. Each evaluation runs in a new node
//      process (no shared module cache) with the app directory as the working
//      directory. It fails if image.domains or image.remotePatterns is not
//      empty, image.service is set, an integration name is not in
//      ALLOWED_INTEGRATIONS, or the adapter name is not in ALLOWED_ADAPTERS;
//   2. reads every source file (SOURCE_EXTENSIONS) under the app, except in
//      SKIPPED_DIRS, and fails on each IMPORT_MARKERS match, comments included.
// It fails closed: a missing app directory, a missing or unreadable config, a
// config that does not evaluate, or a symlink in the source tree is a failure.
//
// Exit status: 0 the premise holds, 1 a rule fired, 2 a usage error.
//
// LIMITS. This guard does not prove that the vulnerable code is unreachable.
// - It does not read dependency code, and it does not run the hooks of the
//   allowed integration or adapter. A new Astro release or a hook can change
//   the image path. Read the call path again at each Astro bump.
// - It sees only imports written as literal text. A computed specifier
//   (import(`astro:${x}`)) passes.
// - It evaluates the config in two environments only. A config that reads
//   another variable, or a NODE_ENV value other than production, can differ
//   at build time.
// - It evaluates astro.config.mjs only. Astro reads astro.config.mjs first, so
//   a second config file with another extension is not used while .mjs exists.
// - It does not see an inline config passed to Astro's JavaScript build() API.
// - It does not read content collections, Markdown image syntax, or data that
//   the apps fetch at run time.
// - Evaluating a config runs that config's code.
import { spawnSync } from 'node:child_process';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { extname, join, relative, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const CVE = 'CVE-2026-93748';
const TICKET = 'TKT-504';

export const APPS = ['web/backoffice', 'web/storefront'];
export const ALLOWED_INTEGRATIONS = ['@astrojs/react'];
export const ALLOWED_ADAPTERS = ['@astrojs/node'];
export const SKIPPED_DIRS = ['node_modules', 'dist', '.astro'];
export const SOURCE_EXTENSIONS = ['.astro', '.js', '.mjs', '.cjs', '.jsx', '.ts', '.mts', '.cts', '.tsx', '.md', '.mdx'];
// astro/dist covers every deep import into Astro's build output, bare or
// through a relative node_modules path. A narrower astro/dist/assets would
// miss modules that import the loader, such as astro/dist/core/build.
export const IMPORT_MARKERS = [
  { rule: 'astro:assets', pattern: /astro:assets/ },
  { rule: 'astro/assets', pattern: /astro\/assets/ },
  { rule: 'virtual:astro:get-image', pattern: /virtual:astro:get-image/ },
  { rule: 'virtual:image-service', pattern: /virtual:image-service/ },
  { rule: 'astro/components', pattern: /astro\/components\/(Responsive)?(Image|Picture)\.astro/ },
  { rule: 'astro/dist', pattern: /astro\/dist\// },
  { rule: 'http-cache-semantics', pattern: /http-cache-semantics/ },
];
export const ENVIRONMENTS = [
  { label: 'NODE_ENV unset', value: undefined },
  { label: 'NODE_ENV=production', value: 'production' },
];
const EVALUATE_TIMEOUT_MS = 60_000;
const RESULT_PREFIX = '@@check-astro-image-premise@@';

// Runs in the child process. It prints only what the rules read, on one line
// with RESULT_PREFIX, so output from the config itself cannot replace it.
// Astro flattens integrations and drops falsy entries; this does the same.
const EVALUATE = `
const { pathToFileURL } = await import('node:url');
const config = (await import(pathToFileURL(process.argv[1]).href)).default;
const list = (v) => v === undefined ? 'absent' : !Array.isArray(v) ? 'not-an-array' : v.length === 0 ? 'empty' : 'not-empty';
let out;
if (config === null || typeof config !== 'object') {
  out = { error: 'the config has no default export object' };
} else {
  const image = config.image ?? {};
  const integrations = config.integrations ?? [];
  out = {
    image: typeof image !== 'object' ? null : { domains: list(image.domains), remotePatterns: list(image.remotePatterns), service: image.service !== undefined },
    integrations: !Array.isArray(integrations) ? null : integrations.flat(Infinity).filter(Boolean).map((i) => typeof i?.name === 'string' ? i.name : null),
    adapter: config.adapter === undefined ? null : { name: typeof config.adapter?.name === 'string' ? config.adapter.name : null },
  };
}
process.stdout.write('\\n${RESULT_PREFIX}' + JSON.stringify(out) + '\\n');
`;

// One config, one environment, one new process. Returns { summary } or { error }.
export function evaluateConfig(appDir, environment) {
  const env = { ...process.env };
  if (environment.value === undefined) delete env.NODE_ENV;
  else env.NODE_ENV = environment.value;
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', EVALUATE, join(appDir, 'astro.config.mjs')], {
    cwd: appDir,
    env,
    encoding: 'utf8',
    timeout: EVALUATE_TIMEOUT_MS,
  });
  if (result.error) return { error: `cannot evaluate the config: ${result.error.message}` };
  if (result.status !== 0) {
    const reason = (result.stderr ?? '').split('\n').find((line) => /Error\b/.test(line)) ?? `exit status ${result.status}`;
    return { error: `cannot evaluate the config: ${reason.trim()}` };
  }
  const line = (result.stdout ?? '').split('\n').findLast((l) => l.startsWith(RESULT_PREFIX));
  if (line === undefined) return { error: 'cannot evaluate the config: no result' };
  const summary = JSON.parse(line.slice(RESULT_PREFIX.length));
  if (summary.error !== undefined) return { error: summary.error };
  return { summary };
}

const ACCEPTED_LIST = ['absent', 'empty'];

// The rules on one evaluated config summary. Returns [{ rule, message }].
export function checkConfig(summary) {
  const problems = [];
  if (summary.image === null) {
    problems.push({ rule: 'image', message: 'image must be absent or an object' });
  } else {
    if (!ACCEPTED_LIST.includes(summary.image.domains)) {
      problems.push({ rule: 'image.domains', message: 'remote image domains are not allowed' });
    }
    if (!ACCEPTED_LIST.includes(summary.image.remotePatterns)) {
      problems.push({ rule: 'image.remotePatterns', message: 'remote image patterns are not allowed' });
    }
    if (summary.image.service) {
      problems.push({ rule: 'image.service', message: 'an image service must not be set' });
    }
  }
  if (summary.integrations === null) {
    problems.push({ rule: 'integrations', message: 'integrations must be an array' });
  } else {
    for (const name of summary.integrations) {
      if (!ALLOWED_INTEGRATIONS.includes(name)) {
        problems.push({ rule: 'integrations', message: `integration ${JSON.stringify(name)} is not in [${ALLOWED_INTEGRATIONS.join(', ')}]` });
      }
    }
  }
  if (summary.adapter !== null && !ALLOWED_ADAPTERS.includes(summary.adapter.name)) {
    problems.push({ rule: 'adapter', message: `adapter ${JSON.stringify(summary.adapter.name)} is not in [${ALLOWED_ADAPTERS.join(', ')}]` });
  }
  return problems;
}

// Evaluates the config in each environment. A problem found in more than one
// environment is reported once, with the environments that found it.
export function checkAppConfig(where, appDir) {
  const found = new Map();
  for (const environment of ENVIRONMENTS) {
    const { summary, error } = evaluateConfig(appDir, environment);
    const problems = error !== undefined ? [{ rule: 'config-unreadable', message: error }] : checkConfig(summary);
    for (const { rule, message } of problems) {
      const key = `${rule}\0${message}`;
      if (!found.has(key)) found.set(key, { rule, message, labels: [] });
      found.get(key).labels.push(environment.label);
    }
  }
  return [...found.values()].map(({ rule, message, labels }) => finding(where, rule, `${message} [${labels.join(', ')}]`));
}

function finding(where, rule, message) {
  return `${where}: rule ${rule}: ${message} (${CVE}, ${TICKET})`;
}

// Every source file under dir, except in SKIPPED_DIRS. A symlink is a finding:
// the scan does not follow it, so it must not pass in silence.
function walk(root, dir, files, problems) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isSymbolicLink()) {
      problems.push(finding(relative(root, path), 'symlink', 'the scan does not follow symlinks'));
    } else if (entry.isDirectory()) {
      if (!SKIPPED_DIRS.includes(entry.name)) walk(root, path, files, problems);
    } else if (entry.isFile() && SOURCE_EXTENSIONS.includes(extname(entry.name))) {
      files.push(path);
    }
  }
}

export function checkSources(root, appDir) {
  const problems = [];
  const files = [];
  walk(root, appDir, files, problems);
  for (const file of files.sort()) {
    const lines = readFileSync(file, 'utf8').split('\n');
    lines.forEach((line, index) => {
      for (const { rule, pattern } of IMPORT_MARKERS) {
        if (pattern.test(line)) {
          problems.push(finding(`${relative(root, file)}:${index + 1}`, rule, 'this marker can reach the Astro image pipeline or http-cache-semantics'));
        }
      }
    });
  }
  return problems;
}

export function checkRepository(root) {
  const problems = [];
  for (const app of APPS) {
    const appDir = join(root, app);
    if (!existsSync(appDir) || !statSync(appDir).isDirectory()) {
      problems.push(finding(app, 'app-missing', 'the app directory is missing'));
      continue;
    }
    const where = `${app}/astro.config.mjs`;
    problems.push(...checkAppConfig(where, appDir));
    try {
      problems.push(...checkSources(root, appDir));
    } catch (error) {
      problems.push(finding(app, 'source-unreadable', `cannot read the source tree: ${error.message}`));
    }
  }
  return problems;
}

export function main(argv) {
  let root = process.cwd();
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === '--root' && i + 1 < argv.length) root = resolve(argv[++i]);
    else {
      console.error(`check-astro-image-premise: unknown option ${argv[i]}`);
      console.error('usage: check-astro-image-premise.mjs [--root DIR]');
      return 2;
    }
  }
  const problems = checkRepository(root);
  if (problems.length > 0) {
    for (const problem of problems) console.error(`check-astro-image-premise: ${problem}`);
    console.error(`check-astro-image-premise: the accepted risk for ${CVE} depends on this premise; see docs/conventions/security.md`);
    return 1;
  }
  console.log(`check-astro-image-premise: ${APPS.join(' and ')} do not use Astro image optimization (${CVE}, ${TICKET})`);
  return 0;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv.slice(2));
}
