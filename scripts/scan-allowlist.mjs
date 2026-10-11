#!/usr/bin/env node
// Checks the vulnerability scan allowlist, its pnpm mirror and their wiring
// (TKT-335, ADR-081).
//
//   node scripts/scan-allowlist.mjs check [--now] [--effective] [--root DIR]
//   node scripts/scan-allowlist.mjs sync [--root DIR]
//   node scripts/scan-allowlist.mjs wiring [--workflow FILE] [--root DIR]
//
// `check` reads files only, and never reads the clock: it refuses a bad schema, a
// bad date, a lifetime that is not 1 to 30 days, a mirror that is not the derived
// list, a pnpm setting outside the allowed set, and the other files that can
// suppress a finding. `make check` runs `check`.
//
// `check --now` adds the two checks that need the clock: an approval date in the
// future, and an entry that expires within MARGIN_MS from now. Trivy drops an
// expired entry by itself; pnpm does not, so this check is what stops an expired
// GHSA from staying in `auditConfig.ignoreGhsas`.
//
// `check --effective` adds a check of the configuration that pnpm builds before
// `pnpm audit` runs (`pnpm config list --json`). pnpm expands `${...}` in keys,
// runs pnpmfile `updateConfig` hooks and reads settings from more than one place,
// so the committed text is not the whole story. The effective auditConfig must
// be the derived mirror, the registry must be the default one, and no setting
// that changes what `pnpm audit` reports may be set.
//
// The pnpm-audit job runs `check --now`, then `check --effective`, then
// `pnpm audit --audit-level high` (ADR-081). `wiring` checks that order, and the
// Trivy step, by parsing the workflow.
//
// `sync` writes `auditConfig.ignoreGhsas` in pnpm-workspace.yaml from the
// allowlist. It refuses to write from an allowlist that `check` refuses.
//
// The allowlist is read by Trivy (Go, yaml.v3) and by this script (JS, yaml 2,
// YAML 1.2). The two parsers do not agree on all of YAML, so this script refuses
// every YAML feature where they can disagree: quoted or non-canonical dates (a
// quoted date makes Trivy stop with a fatal error), anchors, aliases, tags,
// directives, more than one document and duplicate keys.
//
// Exit status: 0 = accepted, 1 = refused, 2 = usage error.
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { isMap, isScalar, isSeq, parseAllDocuments, parseDocument, visit } from 'yaml';

export const ALLOWLIST = '.trivyignore.yaml';
export const WORKSPACE = 'pnpm-workspace.yaml';
// Each of these can suppress a Trivy finding without this check. The action
// sets TRIVY_IGNOREFILE, so a `.trivyignore` is not read today, but it is read
// again the moment the `trivyignores` input goes. Trivy reads `trivy.yaml` from
// the working directory by default, and `scan.skip-files` in it hides findings.
// It also reads `trivy-secret.yaml` by default, and `disable-rules` in it turns
// secret rules off (both verified with Trivy 0.72.0 in TKT-335). pnpm runs the
// `updateConfig` hook of `.pnpmfile.cjs` before `pnpm audit`, and the hook can
// replace auditConfig (verified with pnpm 11.12.0); `.pnpmfile.mjs` and
// `.pnpmfile.js` are refused too, so a later pnpm cannot start to read them
// unseen. pnpm reads the registry from `.npmrc`, and a registry that answers
// "no advisories" makes the audit green.
export const REFUSED_FILES = [
  'trivy.yaml',
  'trivy.yml',
  'trivy-secret.yaml',
  '.trivyignore',
  '.pnpmfile.cjs',
  '.pnpmfile.mjs',
  '.pnpmfile.js',
  '.npmrc',
];
// pnpm reads every top-level key of pnpm-workspace.yaml as a setting, and some
// settings change what `pnpm audit` reports (`ignore`, `ignoreUnfixable`,
// `ignoreRegistryErrors`, `registry`, `pnpmfile`...). This is an allowlist of the
// keys that this repository uses, so a new key fails closed: adding a pnpm
// setting needs a change here, and a review of what it does to the audit.
export const WORKSPACE_KEYS = ['packages', 'allowBuilds', 'overrides', 'minimumReleaseAgeExclude', 'auditConfig'];
// Effective settings that change what `pnpm audit` reports or how it reads the
// lockfile (the audit command's own options, and the pnpmfile settings). The
// names are compared without "-" and case, so `ignore-unfixable` is
// `ignoreUnfixable`.
export const EFFECTIVE_DENIED = [
  'ignore',
  'ignoreUnfixable',
  'ignoreRegistryErrors',
  'auditLevel',
  'fix',
  'production',
  'dev',
  'only',
  'optional',
  'pnpmfile',
  'globalPnpmfile',
];
export const DEFAULT_REGISTRY = 'https://registry.npmjs.org/';
export const ALLOWED_OWNERS = ['mathd'];
export const MAX_LIFETIME_DAYS = 30;
// `check --now` refuses an entry from one hour before its expiry. The pnpm-audit
// job runs the audit seconds after the check, so an audit that starts less than
// one hour after the check never applies an entry that has expired.
export const MARGIN_MS = 3_600_000;

const DAY = 86_400_000;
const REQUIRED = ['id', 'statement', 'owner', 'approved_at', 'expired_at'];
const FIELDS = new Set([...REQUIRED, 'pnpm_ids']);
const CVE = /^CVE-\d{4}-\d{4,}$/;
const GHSA = /^GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$/;
const DATE = /^(\d{4})-(\d{2})-(\d{2})$/;

// A problem found in the allowlist, with the file it is in.
class Refusal extends Error {}

function readText(root, name, problems) {
  const path = join(root, name);
  if (!existsSync(path)) {
    problems.push(`${name}: missing`);
    return undefined;
  }
  try {
    return readFileSync(path, 'utf8');
  } catch (error) {
    problems.push(`${name}: cannot read (${error.message})`);
    return undefined;
  }
}

// One YAML document with no feature that the two parsers can read differently.
function parseStrict(name, text, problems, { wholeFile }) {
  if (/^%/m.test(text)) {
    problems.push(`${name}: a YAML directive is not allowed`);
    return undefined;
  }
  const docs = parseAllDocuments(text);
  if (docs.length !== 1) {
    problems.push(`${name}: must hold exactly one YAML document, found ${docs.length} (empty file or a second document)`);
    return undefined;
  }
  const [doc] = docs;
  const errors = [...doc.errors, ...doc.warnings];
  if (errors.length > 0) {
    for (const error of errors) problems.push(`${name}: parse error ${error.code}: ${error.message.split('\n')[0]}`);
    return undefined;
  }
  if (wholeFile) refuseFeatures(name, doc, problems);
  return doc;
}

function refuseFeatures(name, node, problems) {
  visit(node, {
    Alias(_, alias) {
      problems.push(`${name}: an alias (*${alias.source}) is not allowed`);
    },
    Node(_, n) {
      if (n.anchor) problems.push(`${name}: an anchor (&${n.anchor}) is not allowed`);
      if (n.tag) problems.push(`${name}: an explicit tag (${n.tag}) is not allowed`);
    },
  });
}

function keyName(pair) {
  return isScalar(pair.key) ? String(pair.key.value) : String(pair.key);
}

function plainDate(where, field, node, problems) {
  if (!isScalar(node) || node.type !== 'PLAIN' || typeof node.value !== 'string') {
    problems.push(`${where}: "${field}" must be an unquoted plain YYYY-MM-DD date (Trivy refuses a quoted date)`);
    return undefined;
  }
  const match = DATE.exec(node.value);
  if (!match) {
    problems.push(`${where}: "${field}" must be YYYY-MM-DD, got ${node.value}`);
    return undefined;
  }
  const time = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]));
  if (new Date(time).toISOString().slice(0, 10) !== node.value) {
    problems.push(`${where}: "${field}" is not a calendar date: ${node.value}`);
    return undefined;
  }
  return time;
}

function startOfDay(date) {
  return Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate());
}

function checkEntry(index, node, { now, expiry }, problems) {
  const where = `${ALLOWLIST}: entry ${index + 1}`;
  if (!isMap(node)) {
    problems.push(`${where}: must be a map`);
    return undefined;
  }
  const fields = new Map();
  for (const pair of node.items) {
    const key = keyName(pair);
    if (!FIELDS.has(key)) {
      problems.push(`${where}: unsupported field "${key}"`);
      continue;
    }
    fields.set(key, pair.value);
  }
  for (const field of REQUIRED) {
    if (!fields.has(field) || fields.get(field) === null) problems.push(`${where}: missing "${field}"`);
  }

  const idNode = fields.get('id');
  const id = isScalar(idNode) ? idNode.value : undefined;
  const label = typeof id === 'string' && id !== '' ? `${where} (${id})` : where;
  if (idNode && (typeof id !== 'string' || !(CVE.test(id) || GHSA.test(id)))) {
    problems.push(`${label}: malformed id ${JSON.stringify(id ?? null)}; want CVE-YYYY-NNNN or GHSA-xxxx-xxxx-xxxx`);
  }

  const statement = fields.get('statement');
  if (statement) {
    if (!isScalar(statement) || typeof statement.value !== 'string') {
      problems.push(`${label}: "statement" must be a string`);
    } else if (statement.value.trim() === '') {
      problems.push(`${label}: "statement" is blank`);
    }
  }

  const owner = fields.get('owner');
  if (owner) {
    const value = isScalar(owner) ? owner.value : undefined;
    if (!ALLOWED_OWNERS.includes(value)) {
      problems.push(`${label}: owner ${JSON.stringify(value ?? null)} is not in the allowed set (${ALLOWED_OWNERS.join(', ')})`);
    }
  }

  const approved = fields.get('approved_at') ? plainDate(label, 'approved_at', fields.get('approved_at'), problems) : undefined;
  const expires = fields.get('expired_at') ? plainDate(label, 'expired_at', fields.get('expired_at'), problems) : undefined;
  if (expiry && approved !== undefined && approved > startOfDay(now)) {
    problems.push(`${label}: "approved_at" is in the future`);
  }
  if (approved !== undefined && expires !== undefined) {
    const days = (expires - approved) / DAY;
    if (days < 1 || days > MAX_LIFETIME_DAYS) {
      problems.push(`${label}: lifetime is ${days} days; it must be 1 to ${MAX_LIFETIME_DAYS} days from "approved_at"`);
    }
  }
  if (expiry && expires !== undefined && expires - MARGIN_MS <= now.getTime()) {
    problems.push(
      `${label}: expired, or expires within one hour, at the start of ${new Date(expires).toISOString().slice(0, 10)} UTC; ` +
        'remove it or renew it with a new review',
    );
  }

  const ghsas = [];
  if (typeof id === 'string' && GHSA.test(id)) ghsas.push(id);
  const aliases = fields.get('pnpm_ids');
  if (aliases !== undefined) {
    if (!isSeq(aliases)) {
      problems.push(`${label}: "pnpm_ids" must be a list of GHSA ids`);
    } else {
      for (const item of aliases.items) {
        const value = isScalar(item) ? item.value : undefined;
        if (typeof value !== 'string' || !GHSA.test(value)) {
          problems.push(`${label}: malformed GHSA alias ${JSON.stringify(value ?? null)}; want lower-case GHSA-xxxx-xxxx-xxxx`);
        } else {
          ghsas.push(value);
        }
      }
    }
  }
  return { id, ghsas };
}

// Reads and checks the allowlist. Returns the derived GHSA list (sorted), or
// undefined when the allowlist cannot be read.
function checkAllowlist(root, options, problems) {
  const text = readText(root, ALLOWLIST, problems);
  if (text === undefined) return undefined;
  const doc = parseStrict(ALLOWLIST, text, problems, { wholeFile: true });
  if (!doc) return undefined;
  if (!isMap(doc.contents)) {
    problems.push(`${ALLOWLIST}: the top level must be a map with only "vulnerabilities"`);
    return undefined;
  }
  let list;
  for (const pair of doc.contents.items) {
    const key = keyName(pair);
    if (key === 'vulnerabilities') list = pair.value;
    else problems.push(`${ALLOWLIST}: unsupported section "${key}"; only "vulnerabilities" is allowed`);
  }
  if (!isSeq(list)) {
    problems.push(`${ALLOWLIST}: "vulnerabilities" must be a list ([] when empty)`);
    return undefined;
  }

  const ids = new Set();
  const ghsas = new Set();
  list.items.forEach((node, index) => {
    const result = checkEntry(index, node, options, problems);
    if (!result) return;
    if (typeof result.id === 'string' && result.id !== '') {
      if (ids.has(result.id)) problems.push(`${ALLOWLIST}: duplicate id ${result.id}`);
      ids.add(result.id);
    }
    for (const ghsa of result.ghsas) {
      if (ghsas.has(ghsa)) problems.push(`${ALLOWLIST}: duplicate GHSA mapping ${ghsa}`);
      ghsas.add(ghsa);
    }
  });
  return [...ghsas].sort();
}

function checkMirror(root, derived, problems) {
  const text = readText(root, WORKSPACE, problems);
  if (text === undefined) return;
  const doc = parseStrict(WORKSPACE, text, problems, { wholeFile: true });
  if (!doc) return;
  // pnpm expands ${...} in keys from the environment, so a key such as
  // "audit${X:-Config}" becomes a second auditConfig that this file does not show.
  visit(doc, {
    Pair(_, pair) {
      if (keyName(pair).includes('${')) {
        problems.push(`${WORKSPACE}: key ${JSON.stringify(keyName(pair))} contains "\${", and pnpm can expand it from the environment`);
      }
    },
  });
  if (isMap(doc.contents)) {
    for (const pair of doc.contents.items) {
      const key = keyName(pair);
      if (!WORKSPACE_KEYS.includes(key) && !key.includes('${')) {
        problems.push(
          `${WORKSPACE}: top-level key "${key}" is not allowed; a pnpm setting can change what \`pnpm audit\` reports, ` +
            'so a new setting needs a change to scripts/scan-allowlist.mjs (ADR-081)',
        );
      }
    }
  }
  const audit = isMap(doc.contents) ? doc.contents.items.find((pair) => keyName(pair) === 'auditConfig') : undefined;
  if (!audit) {
    problems.push(`${WORKSPACE}: missing auditConfig.ignoreGhsas; run 'node scripts/scan-allowlist.mjs sync'`);
    return;
  }
  if (!isMap(audit.value)) {
    problems.push(`${WORKSPACE}: auditConfig must be a map with only "ignoreGhsas"`);
    return;
  }
  let list;
  for (const pair of audit.value.items) {
    const key = keyName(pair);
    if (key === 'ignoreGhsas') list = pair.value;
    else problems.push(`${WORKSPACE}: auditConfig key "${key}" is not allowed; only "ignoreGhsas", generated from ${ALLOWLIST}`);
  }
  if (!isSeq(list)) {
    problems.push(`${WORKSPACE}: auditConfig.ignoreGhsas must be a list ([] when empty)`);
    return;
  }
  const mirror = list.items.map((item) => (isScalar(item) ? item.value : String(item)));
  if (derived !== undefined && JSON.stringify(mirror) !== JSON.stringify(derived)) {
    problems.push(
      `${WORKSPACE}: auditConfig.ignoreGhsas is not the mirror of ${ALLOWLIST}; ` +
        `want ${JSON.stringify(derived)}, got ${JSON.stringify(mirror)}; run 'node scripts/scan-allowlist.mjs sync'`,
    );
  }
}

// pnpm 11.12.0 does not read audit settings from the `pnpm` field of
// package.json (verified), but a later pnpm could. Refused so it stays visible.
function checkPackageJson(root, problems) {
  const path = join(root, 'package.json');
  if (!existsSync(path)) return;
  let manifest;
  try {
    manifest = JSON.parse(readFileSync(path, 'utf8'));
  } catch (error) {
    problems.push(`package.json: cannot read (${error.message})`);
    return;
  }
  if (manifest !== null && typeof manifest === 'object' && Object.hasOwn(manifest, 'pnpm')) {
    problems.push('package.json: a "pnpm" field is not allowed; pnpm settings go in pnpm-workspace.yaml (ADR-081)');
  }
}

function staticCheck(root, options, problems) {
  for (const name of REFUSED_FILES) {
    if (existsSync(join(root, name))) {
      problems.push(`${name}: refused; it can suppress scan findings outside ${ALLOWLIST} (ADR-081)`);
    }
  }
  checkPackageJson(root, problems);
  const derived = checkAllowlist(root, options, problems);
  checkMirror(root, derived, problems);
  return derived;
}

// Returns the list of problems; an empty list means the repository is accepted.
// `expiry` turns on the two checks that read the clock `now`; without it, `now`
// is not read.
export function checkRepository(root, { now = new Date(), expiry = false } = {}) {
  const problems = [];
  staticCheck(root, { now, expiry }, problems);
  return problems;
}

// `pnpm config list --json` prints the configuration that pnpm builds, with
// expanded keys and pnpmfile hooks applied (verified with pnpm 11.12.0). `env` is
// a seam for tests, which must not read the developer's own pnpm configuration.
export function readEffectiveConfig(root, env = process.env) {
  const run = spawnSync('pnpm', ['config', 'list', '--json'], { cwd: root, encoding: 'utf8', env });
  if (run.error) throw run.error;
  if (run.status !== 0) throw new Error(`pnpm config list exited ${run.status}: ${run.stderr.trim()}`);
  return JSON.parse(run.stdout);
}

function normalise(key) {
  return key.replaceAll('-', '').toLowerCase();
}

// The static check, plus the configuration that pnpm builds for `pnpm audit`.
// `readConfig` is the seam for tests; it returns `pnpm config list --json`.
export function checkEffective(root, { readConfig = readEffectiveConfig } = {}) {
  const problems = [];
  const derived = staticCheck(root, { now: new Date(), expiry: false }, problems);
  let config;
  try {
    config = readConfig(root);
  } catch (error) {
    problems.push(`cannot read the effective pnpm configuration: ${error.message}`);
    return problems;
  }
  if (config === null || typeof config !== 'object' || Array.isArray(config)) {
    problems.push('cannot read the effective pnpm configuration: it is not an object');
    return problems;
  }
  const want = JSON.stringify({ ignoreGhsas: derived ?? null });
  const got = JSON.stringify(config.auditConfig ?? null);
  if (got !== want) {
    problems.push(`effective auditConfig is ${got}, want ${want}; pnpm changed or added to the committed mirror`);
  }
  const denied = new Set(EFFECTIVE_DENIED.map(normalise));
  for (const key of Object.keys(config)) {
    if (denied.has(normalise(key))) {
      problems.push(`effective setting "${key}" is set (${JSON.stringify(config[key])}); it changes what \`pnpm audit\` reports`);
    }
  }
  if (config.registry !== DEFAULT_REGISTRY) {
    problems.push(`effective registry is ${JSON.stringify(config.registry ?? null)}, want ${DEFAULT_REGISTRY}`);
  }
  return problems;
}

// Writes auditConfig.ignoreGhsas from the allowlist. Keeps the rest of the file.
export function syncWorkspace(root, { now = new Date() } = {}) {
  const problems = [];
  const derived = checkAllowlist(root, { now, expiry: false }, problems);
  if (problems.length > 0 || derived === undefined) throw new Refusal(problems.join('\n'));
  const path = join(root, WORKSPACE);
  const doc = parseDocument(readFileSync(path, 'utf8'));
  if (doc.errors.length > 0) throw new Refusal(`${WORKSPACE}: ${doc.errors[0].message}`);
  doc.setIn(['auditConfig', 'ignoreGhsas'], doc.createNode(derived));
  writeFileSync(path, doc.toString());
}

export const WORKFLOW = '.github/workflows/security.yaml';
export const PNPM_AUDIT_IF = "github.event_name != 'pull_request' || needs.changes.outputs.node == 'true'";
export const PNPM_AUDIT_TAIL = [
  'node scripts/scan-allowlist.mjs check --now',
  'node scripts/scan-allowlist.mjs check --effective',
  'pnpm audit --audit-level high',
];
const STEP_KEYS = ['name', 'id', 'uses', 'with', 'run'];
const TRIVY_INPUTS = {
  version: /^v\d+\.\d+\.\d+$/,
  'scan-type': 'fs',
  'scan-ref': '.',
  scanners: 'vuln,misconfig,secret',
  severity: 'HIGH,CRITICAL',
  'exit-code': '1',
  format: 'table',
  trivyignores: ALLOWLIST,
};

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function allowedKeys(where, object, allowed, problems) {
  for (const key of Object.keys(object)) {
    if (!allowed.includes(key)) problems.push(`${where} key "${key}" is not allowed`);
  }
}

// The wiring in the security workflow, read as YAML (not as lines), so text in a
// `name:` block or a quoted key reads as what it is. It is a tripwire against
// accidental or careless edits; a writer who can change the workflow can also
// change this script.
export function checkWiring(text) {
  const problems = [];
  const doc = parseStrict(WORKFLOW, text, problems, { wholeFile: true });
  if (!doc) return problems;
  const workflow = doc.toJS();
  if (!isObject(workflow) || !isObject(workflow.jobs)) return [...problems, `${WORKFLOW}: no jobs`];
  for (const key of ['env', 'defaults']) {
    if (Object.hasOwn(workflow, key)) problems.push(`workflow key "${key}" is not allowed; it changes every step`);
  }
  const { jobs } = workflow;

  // The node filter must re-run pnpm-audit when the allowlist changes, and the
  // runs that are not pull requests must scan everything.
  const filter = (jobs.changes?.steps ?? []).find((step) => isObject(step) && step.id === 'filter');
  const run = typeof filter?.run === 'string' ? filter.run : '';
  const nodeLine = run.split('\n').find((line) => line.trim().startsWith('echo "node=$(match ')) ?? '';
  if (!nodeLine.includes('\\.trivyignore\\.yaml') || !nodeLine.includes('pnpm-workspace\\.yaml')) {
    problems.push('changes: the node filter must match .trivyignore.yaml and pnpm-workspace.yaml');
  }
  if (!run.includes("printf 'go=true\\nnode=true\\n'")) {
    problems.push('changes: a run that is not a pull request must set node=true');
  }

  const audit = jobs['pnpm-audit'];
  if (!isObject(audit)) {
    problems.push('pnpm-audit: job missing');
  } else {
    allowedKeys('pnpm-audit: job', audit, ['name', 'needs', 'if', 'runs-on', 'steps'], problems);
    if (audit.needs !== 'changes') problems.push('pnpm-audit: "needs" must be changes');
    if (audit.if !== PNPM_AUDIT_IF) problems.push(`pnpm-audit: job "if" must be exactly: ${PNPM_AUDIT_IF}`);
    const steps = Array.isArray(audit.steps) ? audit.steps : [];
    for (const step of steps) {
      if (isObject(step)) allowedKeys('pnpm-audit: step', step, STEP_KEYS, problems);
      else problems.push('pnpm-audit: a step is not a map');
    }
    const tail = steps.slice(-PNPM_AUDIT_TAIL.length).map((step) => (isObject(step) ? step.run : undefined));
    const others = steps.slice(0, -PNPM_AUDIT_TAIL.length);
    const stray = others.some((step) => isObject(step) && typeof step.run === 'string' && /pnpm\s+audit|scan-allowlist/.test(step.run));
    if (JSON.stringify(tail) !== JSON.stringify(PNPM_AUDIT_TAIL) || stray) {
      problems.push(`pnpm-audit: the last steps must be exactly ${PNPM_AUDIT_TAIL.map((r) => `\`run: ${r}\``).join(', then ')}, with no other audit or allowlist step`);
    }
  }

  const scan = jobs['repository-scan'];
  if (!isObject(scan)) {
    problems.push('repository-scan: job missing');
  } else {
    allowedKeys('repository-scan: job', scan, ['name', 'runs-on', 'steps'], problems);
    const steps = Array.isArray(scan.steps) ? scan.steps : [];
    const trivy = steps.filter((step) => isObject(step) && String(step.uses ?? '').startsWith('aquasecurity/trivy-action@'));
    if (trivy.length !== 1) problems.push('repository-scan: want exactly one aquasecurity/trivy-action step');
    for (const step of steps) {
      if (!isObject(step)) continue;
      // A run step here could write a trivy.yaml or a .trivyignore before the scan.
      if (Object.hasOwn(step, 'run')) problems.push('repository-scan: a run step is not allowed');
    }
    if (trivy.length === 1) {
      const [step] = trivy;
      allowedKeys('repository-scan: Trivy step', step, ['name', 'id', 'uses', 'with'], problems);
      const inputs = isObject(step.with) ? step.with : {};
      for (const key of Object.keys(inputs)) {
        if (!Object.hasOwn(TRIVY_INPUTS, key)) problems.push(`repository-scan: Trivy input "${key}" is not allowed`);
      }
      for (const [key, want] of Object.entries(TRIVY_INPUTS)) {
        const value = inputs[key];
        const ok = want instanceof RegExp ? typeof value === 'string' && want.test(value) : value === want;
        if (!ok) problems.push(`repository-scan: Trivy input "${key}" must be ${want instanceof RegExp ? want : JSON.stringify(want)}, got ${JSON.stringify(value ?? null)}`);
      }
    }
  }
  return problems;
}

function usage(message) {
  console.error(`scan-allowlist: ${message}`);
  console.error(
    'usage: scan-allowlist.mjs check [--now] [--effective] [--root DIR] | sync [--root DIR] | wiring [--workflow FILE] [--root DIR]',
  );
  return 2;
}

export function main(argv) {
  const [command, ...rest] = argv;
  let root = process.cwd();
  let expiry = false;
  let effective = false;
  let workflow;
  for (let i = 0; i < rest.length; i += 1) {
    if (rest[i] === '--root' && i + 1 < rest.length) root = rest[++i];
    else if (rest[i] === '--now' && command === 'check') expiry = true;
    else if (rest[i] === '--effective' && command === 'check') effective = true;
    else if (rest[i] === '--workflow' && command === 'wiring' && i + 1 < rest.length) workflow = rest[++i];
    else return usage(`unknown option ${rest[i]}`);
  }
  if (command === 'check') {
    const problems = checkRepository(root, { now: new Date(), expiry });
    if (effective) problems.push(...checkEffective(root).filter((p) => !problems.includes(p)));
    if (problems.length > 0) {
      for (const problem of problems) console.error(`scan allowlist check failed: ${problem}`);
      return 1;
    }
    const extra = [expiry ? 'no entry expires within one hour' : '', effective ? 'the effective pnpm configuration matches' : '']
      .filter(Boolean)
      .join('; ');
    console.log(`scan allowlist: ${ALLOWLIST} and its pnpm mirror accepted${extra ? ` (${extra})` : ''}`);
    return 0;
  }
  if (command === 'wiring') {
    const path = workflow ?? join(root, WORKFLOW);
    let text;
    try {
      text = readFileSync(path, 'utf8');
    } catch (error) {
      console.error(`scan allowlist wiring failed: cannot read ${path} (${error.message})`);
      return 1;
    }
    const problems = checkWiring(text);
    if (problems.length > 0) {
      for (const problem of problems) console.error(`scan allowlist wiring failed: ${problem}`);
      return 1;
    }
    console.log(`scan allowlist wiring: ${path} runs the allowlist checks before pnpm audit and gives Trivy the allowlist`);
    return 0;
  }
  if (command === 'sync') {
    try {
      syncWorkspace(root);
    } catch (error) {
      if (!(error instanceof Refusal)) throw error;
      for (const line of error.message.split('\n')) console.error(`scan allowlist sync refused: ${line}`);
      return 1;
    }
    console.log(`scan allowlist: wrote auditConfig.ignoreGhsas in ${WORKSPACE}`);
    return 0;
  }
  return usage(`unknown command ${command ?? '(none)'}`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}
