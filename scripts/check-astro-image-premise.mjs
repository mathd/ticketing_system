#!/usr/bin/env node
// Guards the premise of the accepted risk for CVE-2026-93748 (TKT-504).
//
// http-cache-semantics 4.3.0 has no fixed release. Astro uses it only in the
// build-time remote image path (astro/dist/assets/build/remote.js). That path
// runs only when an app allows remote images. The accepted-risk paragraph in
// docs/conventions/security.md depends on this premise: the web apps do not use
// Astro image optimization. This script fails when that premise stops being
// true in the places it can see.
//
//   node scripts/check-astro-image-premise.mjs [--root DIR]
//
// For web/backoffice and web/storefront it:
//   1. imports astro.config.mjs (the evaluated config, not its text) and fails
//      if image.domains or image.remotePatterns is not empty, image.service is
//      set, an integration name is not in ALLOWED_INTEGRATIONS, or the adapter
//      name is not in ALLOWED_ADAPTERS;
//   2. reads every source file (SOURCE_EXTENSIONS) under the app, except in
//      SKIPPED_DIRS, and fails on each IMPORT_MARKERS match, comments included.
// It fails closed: a missing app directory, a missing or unreadable config, a
// config that does not import, or a symlink in the source tree is a failure.
//
// Exit status: 0 the premise holds, 1 a rule fired, 2 a usage error.
//
// LIMITS. This guard does not prove that the vulnerable code is unreachable.
// - It does not read dependency code. A new Astro release, an integration's
//   hooks, or the adapter's hooks can change the image path. Read the call path
//   again at each Astro bump.
// - It sees only imports written as literal text. A computed specifier
//   (import(`astro:${x}`)) passes.
// - It imports astro.config.mjs only. Astro reads astro.config.mjs first, so a
//   second config file with another extension is not used while .mjs exists.
// - It does not read content collections, Markdown image syntax, or data that
//   the apps fetch at run time.
// - Importing a config runs that config's code.
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
export const IMPORT_MARKERS = [
  { rule: 'astro:assets', pattern: /astro:assets/ },
  { rule: 'astro/assets', pattern: /astro\/assets/ },
  { rule: 'virtual:astro:get-image', pattern: /virtual:astro:get-image/ },
  { rule: 'virtual:image-service', pattern: /virtual:image-service/ },
  { rule: 'astro/components', pattern: /astro\/components\/(Responsive)?(Image|Picture)\.astro/ },
];

function finding(where, rule, message) {
  return `${where}: rule ${rule}: ${message} (${CVE}, ${TICKET})`;
}

function isEmptyList(value) {
  return value === undefined || (Array.isArray(value) && value.length === 0);
}

// The rules on the evaluated config. Astro flattens integrations and drops
// falsy entries, so this does the same before it reads the names.
export function checkConfig(where, config) {
  const problems = [];
  const image = config.image ?? {};
  if (typeof image !== 'object') {
    problems.push(finding(where, 'image', 'image must be absent or an object'));
    return problems;
  }
  if (!isEmptyList(image.domains)) {
    problems.push(finding(where, 'image.domains', 'remote image domains are not allowed'));
  }
  if (!isEmptyList(image.remotePatterns)) {
    problems.push(finding(where, 'image.remotePatterns', 'remote image patterns are not allowed'));
  }
  if (image.service !== undefined) {
    problems.push(finding(where, 'image.service', 'an image service must not be set'));
  }
  const integrations = config.integrations ?? [];
  if (!Array.isArray(integrations)) {
    problems.push(finding(where, 'integrations', 'integrations must be an array'));
  } else {
    for (const integration of integrations.flat(Infinity).filter(Boolean)) {
      const name = integration?.name;
      if (!ALLOWED_INTEGRATIONS.includes(name)) {
        problems.push(finding(where, 'integrations', `integration ${JSON.stringify(name ?? null)} is not in [${ALLOWED_INTEGRATIONS.join(', ')}]`));
      }
    }
  }
  if (config.adapter !== undefined && !ALLOWED_ADAPTERS.includes(config.adapter?.name)) {
    problems.push(finding(where, 'adapter', `adapter ${JSON.stringify(config.adapter?.name ?? null)} is not in [${ALLOWED_ADAPTERS.join(', ')}]`));
  }
  return problems;
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
          problems.push(finding(`${relative(root, file)}:${index + 1}`, rule, 'an Astro image pipeline import is not allowed'));
        }
      }
    });
  }
  return problems;
}

export async function checkRepository(root) {
  const problems = [];
  for (const app of APPS) {
    const appDir = join(root, app);
    if (!existsSync(appDir) || !statSync(appDir).isDirectory()) {
      problems.push(finding(app, 'app-missing', 'the app directory is missing'));
      continue;
    }
    const where = `${app}/astro.config.mjs`;
    let config;
    let importError;
    try {
      config = (await import(pathToFileURL(join(appDir, 'astro.config.mjs')).href)).default;
    } catch (error) {
      importError = error;
    }
    if (importError !== undefined) {
      problems.push(finding(where, 'config-unreadable', `cannot import the config: ${String(importError.message).split('\n')[0]}`));
    } else if (config === null || typeof config !== 'object') {
      problems.push(finding(where, 'config-unreadable', 'the config has no default export object'));
    } else {
      problems.push(...checkConfig(where, config));
    }
    try {
      problems.push(...checkSources(root, appDir));
    } catch (error) {
      problems.push(finding(app, 'source-unreadable', `cannot read the source tree: ${error.message}`));
    }
  }
  return problems;
}

export async function main(argv) {
  let root = process.cwd();
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === '--root' && i + 1 < argv.length) root = resolve(argv[++i]);
    else {
      console.error(`check-astro-image-premise: unknown option ${argv[i]}`);
      console.error('usage: check-astro-image-premise.mjs [--root DIR]');
      return 2;
    }
  }
  const problems = await checkRepository(root);
  if (problems.length > 0) {
    for (const problem of problems) console.error(`check-astro-image-premise: ${problem}`);
    console.error(`check-astro-image-premise: the accepted risk for ${CVE} depends on this premise; see docs/conventions/security.md`);
    return 1;
  }
  console.log(`check-astro-image-premise: ${APPS.join(' and ')} do not use Astro image optimization (${CVE}, ${TICKET})`);
  return 0;
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = await main(process.argv.slice(2));
}
