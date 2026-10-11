#!/usr/bin/env node
// Checks the vulnerability scan allowlist and its pnpm mirror (TKT-335, ADR-081).
//
//   node scripts/scan-allowlist.mjs check [--now] [--root DIR]
//   node scripts/scan-allowlist.mjs sync [--root DIR]
//
// `check` is deterministic: it refuses a bad schema, a bad date, a lifetime that
// is not 1 to 30 days, an approval date in the future, a mirror that is not the
// derived list, and the other files that can suppress a finding. `check --now`
// also refuses an entry whose expiry date has started (UTC). `make check` runs
// `check`. The pnpm-audit job runs `check --now` before `pnpm audit` (ADR-081 D2).
// Trivy drops an expired entry by itself; pnpm does not, so this check is what
// stops an expired GHSA from staying in `auditConfig.ignoreGhsas`.
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
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { isAlias, isMap, isScalar, isSeq, parseAllDocuments, parseDocument, visit } from 'yaml';

export const ALLOWLIST = '.trivyignore.yaml';
export const WORKSPACE = 'pnpm-workspace.yaml';
// Each of these can suppress a Trivy finding without this check. The action
// sets TRIVY_IGNOREFILE, so a `.trivyignore` is not read today, but it is read
// again the moment the `trivyignores` input goes. Trivy reads `trivy.yaml` from
// the working directory by default, and `scan.skip-files` in it hides findings.
// It also reads `trivy-secret.yaml` by default, and `disable-rules` in it turns
// secret rules off (both verified with Trivy 0.72.0 in TKT-335).
export const REFUSED_FILES = ['trivy.yaml', 'trivy.yml', 'trivy-secret.yaml', '.trivyignore'];
export const ALLOWED_OWNERS = ['mathd'];
export const MAX_LIFETIME_DAYS = 30;

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
  if (approved !== undefined && approved > startOfDay(now)) {
    problems.push(`${label}: "approved_at" is in the future`);
  }
  if (approved !== undefined && expires !== undefined) {
    const days = (expires - approved) / DAY;
    if (days < 1 || days > MAX_LIFETIME_DAYS) {
      problems.push(`${label}: lifetime is ${days} days; it must be 1 to ${MAX_LIFETIME_DAYS} days from "approved_at"`);
    }
  }
  if (expiry && expires !== undefined && expires <= now.getTime()) {
    problems.push(`${label}: expired at the start of ${new Date(expires).toISOString().slice(0, 10)} UTC; remove it or renew it with a new review`);
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
  const doc = parseStrict(WORKSPACE, text, problems, { wholeFile: false });
  if (!doc) return;
  const audit = isMap(doc.contents) ? doc.contents.items.find((pair) => keyName(pair) === 'auditConfig') : undefined;
  if (!audit) {
    problems.push(`${WORKSPACE}: missing auditConfig.ignoreGhsas; run 'node scripts/scan-allowlist.mjs sync'`);
    return;
  }
  refuseFeatures(`${WORKSPACE} auditConfig`, audit, problems);
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

// Returns the list of problems; an empty list means the repository is accepted.
// `now` is the clock. `expiry` turns on the clock-dependent expiry check.
export function checkRepository(root, { now = new Date(), expiry = false } = {}) {
  const problems = [];
  for (const name of REFUSED_FILES) {
    if (existsSync(join(root, name))) {
      problems.push(`${name}: refused; it can suppress scan findings outside ${ALLOWLIST} (ADR-081)`);
    }
  }
  const derived = checkAllowlist(root, { now, expiry }, problems);
  checkMirror(root, derived, problems);
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

function usage(message) {
  console.error(`scan-allowlist: ${message}`);
  console.error('usage: scan-allowlist.mjs check [--now] [--root DIR] | sync [--root DIR]');
  return 2;
}

export function main(argv) {
  const [command, ...rest] = argv;
  let root = process.cwd();
  let expiry = false;
  for (let i = 0; i < rest.length; i += 1) {
    if (rest[i] === '--root' && i + 1 < rest.length) root = rest[++i];
    else if (rest[i] === '--now' && command === 'check') expiry = true;
    else return usage(`unknown option ${rest[i]}`);
  }
  if (command === 'check') {
    const problems = checkRepository(root, { now: new Date(), expiry });
    if (problems.length > 0) {
      for (const problem of problems) console.error(`scan allowlist check failed: ${problem}`);
      return 1;
    }
    console.log(`scan allowlist: ${ALLOWLIST} and its pnpm mirror accepted${expiry ? ' (no entry has expired)' : ''}`);
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
