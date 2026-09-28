#!/usr/bin/env node
import assert from 'node:assert/strict';
import { createHash, createHmac, randomUUID } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { createServer } from 'node:net';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { chromium } from 'playwright-core';
import { fileURLToPath } from 'node:url';

const DIR = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(DIR, '../../..');
const PROFILE = JSON.parse(await readFile(path.join(DIR, 'profile.json'), 'utf8'));
const ORGANIZER = '00000000-0000-0000-0000-000000000001';
const STAFF = '00000000-0000-0000-0000-0000000000aa';
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const PORTS = ['GATEWAY_PORT', 'POSTGRES_PORT', 'NATS_PORT', 'GRAFANA_PORT', 'PROM_PORT', 'OTLP_PORT', 'CATALOG_PORT', 'INVENTORY_PORT', 'COMMERCE_PORT', 'PAYMENTS_PORT', 'ACCESS_PORT'];
const COMPOSE = ['compose.yaml', 'compose.direct-ports.yaml', 'compose.onsale-load.yaml', 'compose.smoke-cadence.yaml', 'compose.smoke.yaml'];
const REQUIRE = createRequire(path.join(ROOT, 'package.json'));
const PLAYWRIGHT_VERSION = REQUIRE('playwright-core/package.json').version;
const CONTROL_NAMES = PROFILE.controls;
const SAFE_ERROR_TYPES = new Set(['Error', 'AssertionError', 'TypeError', 'RangeError', 'SyntaxError', 'ReferenceError', 'URIError', 'EvalError']);

class CorrectnessAssertionError extends Error {
  constructor(code, category = 'database') {
    super(code);
    this.name = 'CorrectnessAssertionError';
    this.code = code;
    this.category = category;
  }
}

function correctnessAssert(condition, code, category = 'database') {
  if (!condition) throw new CorrectnessAssertionError(code, category);
}

const opts = parseArgs(process.argv.slice(2));
if (opts.mode === 'self-check') {
  selfCheckAssertions();
  selfCheckOwnedContainers();
  selfCheckExecSafe();
  console.log('runner static checks passed');
  process.exit(0);
}
const evidenceRoot = path.join(DIR, 'runs');
const runLabel = opts.mode === 'controls' ? 'controls' : `baseline-${opts.attempt}`;
const runID = `run-${new Date().toISOString().replaceAll(':', '').replaceAll('-', '').replaceAll('.', '')}-${randomUUID().slice(0, 8)}`;
const runDir = path.join(evidenceRoot, runLabel, runID);
await mkdir(runDir, { recursive: true, mode: 0o700 });
const metadataPath = path.join(runDir, 'metadata.json');
let evidenceStatus = 'aborted';
let browser;
let stackUp = false;
let project;
let runEnv;
let containerIds = {};
let startedAt = new Date().toISOString();
const healthRows = [];
const sampleRows = [];
const warmupRows = [];
const pageFaults = [];
const controls = [];
const correctness = { status: 'pending', sequences: [], reconciliation_diagnostics: [], lifecycle_verifier: 'pending', exact_assertions: false, failures: [] };
const fixtureAliases = Object.create(null);
const tickets = Object.create(null);
const devices = [];
const contexts = [];
const pages = [];
let catalogCredentials;
let baseURL;
let gatewayPort;
let requestedSignal = null;
let fixtureDeadlineMs = null;
process.on('SIGINT', () => handleSignal('SIGINT'));
process.on('SIGTERM', () => handleSignal('SIGTERM'));

try {
  const provenance = await verifyInputs();
  if (opts.mode === 'baseline') await requirePassedControls();
  project = `ticketing-tkt492-${randomUUID().replaceAll('-', '').slice(0, 18)}`;
  const slot = await stackSlot();
  const ports = derivePorts(slot);
  await assertPortsFree(Object.values(ports));
  const priorProject = execSafe('compose_project_name_collision', ['docker', 'ps', '--all', '--filter', `label=com.docker.compose.project=${project}`, '--format', '{{.ID}}'], { cwd: ROOT, encoding: 'utf8' }).trim();
  if (priorProject) throw new Error('compose_project_name_collision');
  runEnv = { ...process.env, SMOKE_COMPOSE_PROJECT: project };
  stackUp = true;
  await runBounded(() => execSafe('scripts/browser.sh up', [path.join(ROOT, 'scripts/browser.sh'), 'up'], {
    cwd: ROOT, env: runEnv, timeout: PROFILE.deadlines.startup_ms, maxBuffer: 2 * 1024 * 1024,
  }), PROFILE.deadlines.startup_ms, 'stack_startup');
  gatewayPort = ports.GATEWAY_PORT;
  baseURL = `http://localhost:${gatewayPort}`;
  containerIds = await ownedContainers(project);
  assert(Object.keys(containerIds).length >= 9, 'owned_stack_incomplete');
  const catalogID = containerIds.catalog?.id;
  const accessID = containerIds.access?.id;
  const postgresID = containerIds.postgres?.id;
  assert(catalogID && accessID && postgresID, 'required_owned_container_missing');
  catalogCredentials = await readCatalogCredentials(catalogID, project);
  healthRows.push(await stackHealth('setup'));
  await writeJSON(path.join(runDir, 'metadata.json'), {
    schema_version: 1,
    ticket: 'TKT-492',
    profile_id: PROFILE.profile_id,
    mode: opts.mode,
    attempt: opts.mode === 'baseline' ? opts.attempt : null,
    started_at: startedAt,
    status: 'running',
    source_head: provenance.head,
    source_head_expected: '036f776f32656cf978b8a397ff51787b7f834de6',
    source_dirty_paths: provenance.dirtyPaths,
    artifact_sha256: provenance.artifacts,
    profile_sha256: provenance.profileHash,
    harness_sha256: provenance.harnessHashes,
    root_gate_verdict_present: provenance.rootGateVerdictPresent,
    browser: { channel: 'chrome', playwright_core: PLAYWRIGHT_VERSION, node: process.version },
    host: { platform: process.platform, arch: process.arch, release: os.release(), cpu_model: os.cpus()[0]?.model ?? null, cpus: os.cpus().length, total_memory_bytes: os.totalmem() },
    topology: {
      compose_files: COMPOSE, project_alias: 'owned-tkt492-project', service_count: Object.keys(containerIds).length, port_slot: slot,
      worker_cadence: { access_lifecycle_checkpoint: '1s', access_event_retry_backoff: ['100ms', '200ms', '400ms', '800ms', '1s', '1s'], commerce_recovery: '2s', refund_reversal: '2s', healthcheck: '5s', otel_metric_export: '5s' },
      database_pool_defaults: { max_open_connections: 25, max_idle_connections: 10, connection_max_lifetime: '30m', connection_max_idle_time: '5m' },
    },
    images: await safeImageSummary(containerIds),
    docker_container_limits: await safeContainerLimits(containerIds),
    started_containers: Object.keys(containerIds).sort(),
    result: 'pending',
  });

  const fixtureDeadline = Date.now() + PROFILE.deadlines.fixture_setup_per_attempt_ms;
  fixtureDeadlineMs = fixtureDeadline;
  const offers = await createOffers();
  await createDevicePages(accessID);
  const fixture = opts.mode === 'baseline'
    ? await createBaselineFixture(offers, fixtureDeadline)
    : await createControlsFixture(offers, fixtureDeadline);
  assert(Date.now() <= fixtureDeadline, 'fixture_setup_deadline_exceeded');
  fixtureDeadlineMs = null;
  await writeJSON(path.join(runDir, 'fixtures.json'), safeFixtureSummary(fixture));
  healthRows.push(await stackHealth('warmup'));

  if (opts.mode === 'controls') {
    await runControls(fixture);
    await runShortCorrectness(fixture);
  } else {
    await runWarmups(fixture);
    healthRows.push(await stackHealth('measured_online'));
    await runMeasuredOnline(fixture);
    healthRows.push(await stackHealth('after_measured_online'));
    healthRows.push(await stackHealth('measured_offline'));
    await runMeasuredOffline(fixture);
    healthRows.push(await stackHealth('after_measured_offline'));
    await runCorrectness(fixture);
  }
  healthRows.push(await stackHealth('correctness'));
  await persistRows();
  if (opts.mode === 'controls') {
    await validateControlEvidence();
    evidenceStatus = controls.every((entry) => entry.detected === true) && correctness.status === 'passed' && correctness.sequences.length === PROFILE.correctness_sequences ? 'controls_passed' : 'controls_failed';
  } else {
    evidenceStatus = correctness.status === 'passed' ? 'complete' : 'correctness_failed';
  }
} catch (error) {
  evidenceStatus = 'aborted';
  const phase = safeErrorLabel(error);
  const diagnostics = safeErrorDiagnostics(error);
  const classifiedFailure = error instanceof CorrectnessAssertionError
    ? { outcome: 'correctness_failure', source: 'runner_assertion', category: error.category, code: error.code }
    : { outcome: 'inconclusive', source: 'runner_error' };
  if (error instanceof CorrectnessAssertionError) {
    correctness.status = 'failed';
    correctness.failures.push({ source: 'runner', category: error.category, code: error.code });
  }
  await writeJSON(path.join(runDir, 'failure.json'), {
    status: 'aborted', phase, classification: classifiedFailure, ...diagnostics, at: new Date().toISOString(),
  });
  if (!requestedSignal) process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const context of contexts) await context.close().catch(() => {});
  if (stackUp && project) {
    try {
      await assertProjectOwnership(project);
      execSafe('owned browser stack down', ['bash', path.join(ROOT, 'scripts/browser.sh'), 'down'], {
        cwd: ROOT, env: runEnv, stdio: 'pipe', timeout: 120000, maxBuffer: 1024 * 1024,
      });
    } catch {
      evidenceStatus = 'aborted_cleanup_failed';
      process.exitCode = 1;
    }
  }
  try { await persistRows(); }
  catch {
    evidenceStatus = 'aborted_evidence_write_failed';
    process.exitCode = 1;
  }
  if (opts.mode === 'baseline' && evidenceStatus === 'complete') {
    const calculated = classifyRawSamples(sampleRows);
    evidenceStatus = calculated.valid && pageFaults.length === 0 ? 'complete' : 'invalid';
  }
  const current = await readJSONSafe(metadataPath);
  if (opts.mode === 'controls') await writeJSON(path.join(runDir, 'controls.json'), { controls });
  await writeJSON(path.join(runDir, 'correctness.json'), correctness);
  const hashes = await evidenceHashes(runDir);
  await writeJSON(metadataPath, {
    ...(current ?? { schema_version: 1, ticket: 'TKT-492', profile_id: PROFILE.profile_id, mode: opts.mode, attempt: opts.attempt ?? null, started_at: startedAt }),
    completed_at: new Date().toISOString(),
    status: evidenceStatus,
    result: opts.mode === 'baseline' && evidenceStatus === 'complete'
      ? 'evidence_complete_pending_analysis'
      : evidenceStatus,
    evidence_sha256: hashes,
    aliases_present: Object.keys(fixtureAliases).length,
    page_faults: pageFaults,
  });
  if (!['complete', 'controls_passed'].includes(evidenceStatus) && !requestedSignal) process.exitCode = 1;
  console.log(`${opts.mode}: ${evidenceStatus}; evidence=${path.relative(ROOT, runDir)}`);
}

function parseArgs(argv) {
  if (argv.length === 1 && argv[0] === '--self-check') return { mode: 'self-check', attempt: null };
  const parsed = { mode: null, attempt: null };
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === '--mode') parsed.mode = argv[++i];
    else if (argv[i] === '--attempt') parsed.attempt = argv[++i];
    else usage();
  }
  if (!['controls', 'baseline'].includes(parsed.mode)) usage();
  if (parsed.mode === 'baseline' && !['01', '02'].includes(parsed.attempt)) usage();
  if (parsed.mode === 'controls' && parsed.attempt !== null) usage();
  return parsed;
}

function usage() {
  console.error('usage: node run.mjs --self-check | --mode controls | --mode baseline --attempt 01|02');
  process.exit(2);
}

function execSafe(label, command, options = {}) {
  try { return execFileSync(command[0], command.slice(1), { timeout: 30000, stdio: ['pipe', 'pipe', 'pipe'], ...options }); }
  catch { throw new Error(label); }
}

function execLifecycleVerifier() {
  const command = ['docker', 'exec', containerIds.access.id, '/app', 'verify-lifecycle'];
  try {
    return execFileSync(command[0], command.slice(1), { cwd: ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000 });
  } catch (error) {
    const stderr = typeof error?.stderr === 'string' ? error.stderr : Buffer.isBuffer(error?.stderr) ? error.stderr.toString('utf8') : '';
    if (Number.isInteger(error?.status) && lifecycleVerifierViolation(stderr)) {
      throw new CorrectnessAssertionError('lifecycle_verifier_integrity_mismatch');
    }
    throw new Error('lifecycle_verifier_failed');
  }
}

function lifecycleVerifierViolation(stderr) {
  const message = stderr.replace(/^access verify-lifecycle:\s*/m, '');
  return /(?:has no integrity row|declares canonical version|sequence gap on ticket|broken chain link on ticket|entry hash mismatch on ticket|has a head and no chained events|head mismatch on ticket|invalid lifecycle signature under key|references no matching lifecycle event|checkpoint gap for organizer|broken checkpoint link for organizer|claims \d+ leaves, has \d+|does not match its leaves|covers ticket .* which belongs to organizer|is not a signed head)/i.test(message);
}

function handleSignal(signal) {
  requestedSignal ??= signal;
  process.exitCode = signal === 'SIGINT' ? 130 : 143;
  if (browser) void browser.close().catch(() => {});
}

function assertNotInterrupted() {
  if (requestedSignal) throw new Error('interrupted');
}

async function runBounded(operation, ms, label) {
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(operation),
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(label)), ms); }),
    ]);
  } finally { clearTimeout(timer); }
}

async function verifyInputs() {
  const head = execSafe('source_provenance_unavailable', ['git', 'rev-parse', 'HEAD'], { cwd: ROOT, encoding: 'utf8' }).trim();
  if (head !== '036f776f32656cf978b8a397ff51787b7f834de6') throw new Error('source_head_mismatch');
  const { permitted, dirtyPaths } = parseSourceStatus(execSafe('source_status_unavailable', ['git', 'status', '--porcelain=v1', '--untracked-files=all'], { cwd: ROOT, encoding: 'utf8' }));
  assertAllowedSourceChanges(dirtyPaths);
  const required = ['catalog', 'inventory', 'commerce', 'payments', 'access', 'gateway'].map((name) => path.join(ROOT, 'bin/gate', name)).concat([
    path.join(ROOT, 'web/storefront/dist/server/entry.mjs'),
    path.join(ROOT, 'web/backoffice/dist/server/entry.mjs'),
  ]);
  const artifacts = {};
  for (const file of required) {
    const bytes = await readFile(file).catch(() => { throw new Error('required_build_artifact_missing'); });
    const name = path.relative(ROOT, file);
    artifacts[name] = createHash('sha256').update(bytes).digest('hex');
  }
  const scannerRoot = path.join(ROOT, 'web/scanner/dist');
  for (const file of await recursiveFiles(scannerRoot)) {
    const name = path.relative(ROOT, file);
    artifacts[name] = createHash('sha256').update(await readFile(file)).digest('hex');
  }
  if (!artifacts['web/scanner/dist/index.html'] || !Object.keys(artifacts).some((name) => /^web\/scanner\/dist\/.*\.(?:m?js)$/.test(name))) throw new Error('scanner_build_artifact_incomplete');
  const gate = await readFile(path.join(ROOT, '.gate.verdict')).catch(() => null);
  return {
    head,
    dirtyPaths: [...dirtyPaths, ...permitted.map((line) => line.slice(3))],
    artifacts,
    profileHash: (await import('node:crypto')).createHash('sha256').update(await readFile(path.join(DIR, 'profile.json'))).digest('hex'),
    harnessHashes: {
      runner: (await import('node:crypto')).createHash('sha256').update(await readFile(path.join(DIR, 'run.mjs'))).digest('hex'),
      analyzer: (await import('node:crypto')).createHash('sha256').update(await readFile(path.join(DIR, 'analyze.mjs'))).digest('hex'),
    },
    rootGateVerdictPresent: !!gate,
  };
}

async function recursiveFiles(directory) {
  const entries = await (await import('node:fs/promises')).readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const item = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...await recursiveFiles(item));
    else if (entry.isFile()) files.push(item);
  }
  return files.sort();
}

async function stackSlot() {
  const output = execSafe('stack_identity_failed', ['bash', path.join(ROOT, 'scripts/stack-env.sh'), '--identity', ROOT, 'browser'], { cwd: ROOT, encoding: 'utf8' }).trim();
  const slot = Number(output.split(/\s+/)[0]);
  if (!Number.isInteger(slot) || slot < 0 || slot > 39) throw new Error('stack_slot_invalid');
  return slot;
}

function derivePorts(slot) {
  return { GATEWAY_PORT: 18080 + slot, POSTGRES_PORT: 15432 + slot, NATS_PORT: 14222 + slot, GRAFANA_PORT: 13000 + slot, PROM_PORT: 19090 + slot, OTLP_PORT: 14318 + slot, CATALOG_PORT: 15080 + slot, INVENTORY_PORT: 16080 + slot, COMMERCE_PORT: 17080 + slot, PAYMENTS_PORT: 17580 + slot, ACCESS_PORT: 18580 + slot };
}

async function assertPortsFree(ports) {
  for (const port of ports) {
    const server = createServer();
    await new Promise((resolve, reject) => {
      server.once('error', () => reject(new Error('port_collision')));
      server.listen(port, '127.0.0.1', () => server.close(resolve));
    });
  }
}

async function ownedContainers(projectName) {
  const idsText = execSafe('owned_stack_lookup_failed', ['docker', 'ps', '--all', '--filter', `label=com.docker.compose.project=${projectName}`, '--format', '{{.ID}}'], { cwd: ROOT, encoding: 'utf8', timeout: 15000 });
  const ids = idsText.split(/\s+/).filter(Boolean);
  if (!ids.length) return {};
  const inspect = execSafe('owned_stack_inspection_failed', ['docker', 'inspect', ...ids], { cwd: ROOT, encoding: 'utf8', timeout: 15000 });
  return ownedContainerMap(inspect, projectName);
}

function ownedContainerMap(inspectText, projectName) {
  const containers = JSON.parse(inspectText);
  if (!Array.isArray(containers)) throw new Error('owned_stack_inspection_invalid');
  const result = {};
  for (const item of containers) {
    const labels = item.Config?.Labels ?? {};
    const service = labels['com.docker.compose.service'];
    if (labels['com.docker.compose.project'] !== projectName || !service || !item.Id) throw new Error('compose_ownership_mismatch');
    const limits = item.HostConfig ?? {};
    result[service] = {
      id: item.Id, running: item.State?.Running === true, oomKilled: item.State?.OOMKilled === true,
      restartCount: Number(item.RestartCount ?? 0), imageID: item.Image, imageReference: item.Config?.Image,
      nanoCpus: Number(limits.NanoCpus ?? 0), memoryLimitBytes: Number(limits.Memory ?? 0),
      cpuQuota: Number(limits.CpuQuota ?? 0), cpuPeriod: Number(limits.CpuPeriod ?? 0),
    };
  }
  return result;
}

function selfCheckOwnedContainers() {
  const valid = JSON.stringify([{
    Id: '0123456789abcdef', Image: 'sha256:image', RestartCount: 0,
    Config: { Image: 'catalog:test', Labels: { 'com.docker.compose.project': 'owned-project', 'com.docker.compose.service': 'catalog' } },
    State: { Running: true, OOMKilled: false },
    HostConfig: { NanoCpus: 1000000000, Memory: 1073741824, CpuQuota: 100000, CpuPeriod: 100000 },
  }]);
  const result = ownedContainerMap(valid, 'owned-project');
  if (result.catalog?.id !== '0123456789abcdef' || result.catalog.running !== true || result.catalog.imageReference !== 'catalog:test') throw new Error('runner_static_check_failed');
  let refused = false;
  try { ownedContainerMap(valid.replace('owned-project', 'other-project'), 'owned-project'); }
  catch { refused = true; }
  if (!refused) throw new Error('runner_static_ownership_check_failed');

  const allowed = parseSourceStatus(' M .claude/sdlc.config.json\n?? docs/evidence/TKT-492/fixture.json\n');
  if (allowed.dirtyPaths.length !== 1 || allowed.dirtyPaths[0] !== '.claude/sdlc.config.json'
    || allowed.permitted.length !== 1 || allowed.permitted[0] !== '?? docs/evidence/TKT-492/fixture.json') throw new Error('runner_static_source_status_parse_failed');
  assertAllowedSourceChanges(allowed.dirtyPaths);
  let unrelatedRefused = false;
  try { assertAllowedSourceChanges(parseSourceStatus(' M .claude/sdlc.config.json\n M docs/other.go\n').dirtyPaths); }
  catch { unrelatedRefused = true; }
  if (!unrelatedRefused) throw new Error('runner_static_source_status_allowlist_failed');
}

function selfCheckAssertions() {
  assert(true, 'runner_assertion_valid_check_failed');
  let failure;
  try { assert(false, 'runner_assertion_invalid_check_failed'); }
  catch (error) { failure = error; }
  assert(failure instanceof Error && failure.name === 'AssertionError'
    && failure.message === 'runner_assertion_invalid_check_failed', 'runner_assertion_error_check_failed');
}

function selfCheckExecSafe() {
  const input = 'SELECT 1\n';
  const child = "let value = ''; process.stdin.setEncoding('utf8'); process.stdin.on('data', (chunk) => value += chunk); process.stdin.on('end', () => { process.stdout.write(value); process.stderr.write('runner_exec_safe_child_stderr'); });";
  let forwardedStdout = '';
  let forwardedStderr = '';
  const stdoutWrite = process.stdout.write;
  const stderrWrite = process.stderr.write;
  let returned;
  try {
    process.stdout.write = (chunk) => { forwardedStdout += chunk; return true; };
    process.stderr.write = (chunk) => { forwardedStderr += chunk; return true; };
    returned = execSafe('runner_exec_safe_io_check_failed', [process.execPath, '-e', child], { encoding: 'utf8', input });
  } finally {
    process.stdout.write = stdoutWrite;
    process.stderr.write = stderrWrite;
  }
  assert.equal(returned, input, 'runner_exec_safe_stdin_stdout_check_failed');
  assert.equal(forwardedStdout, '', 'runner_exec_safe_stdout_forwarded');
  assert.equal(forwardedStderr, '', 'runner_exec_safe_stderr_forwarded');
}

function parseSourceStatus(output) {
  const changed = output.split('\n').filter(Boolean);
  const ownedPrefix = '?? docs/evidence/TKT-492/';
  return {
    permitted: changed.filter((line) => line.startsWith(ownedPrefix)),
    dirtyPaths: changed.filter((line) => !line.startsWith(ownedPrefix)).map((line) => line.slice(3)),
  };
}

function assertAllowedSourceChanges(dirtyPaths) {
  if (dirtyPaths.some((file) => file !== '.claude/sdlc.config.json')) throw new Error('unexpected_tracked_source_changes');
}

async function assertProjectOwnership(projectName) {
  const ids = execSafe('project_ownership_check_failed', ['docker', 'ps', '--all', '--filter', `label=com.docker.compose.project=${projectName}`, '--format', '{{.ID}}'], { cwd: ROOT, encoding: 'utf8', timeout: 15000 }).trim().split(/\s+/).filter(Boolean);
  if (!ids.length) return;
  const inspect = JSON.parse(execSafe('project_ownership_check_failed', ['docker', 'inspect', ...ids], { cwd: ROOT, encoding: 'utf8', timeout: 15000 }));
  if (inspect.length !== ids.length || inspect.some((item) => item.Config?.Labels?.['com.docker.compose.project'] !== projectName)) throw new Error('compose_ownership_mismatch');
}

async function safeImageSummary(containers) {
  return Object.fromEntries(Object.entries(containers).map(([service, info]) => [service, { image_id: info.imageID, reference: info.imageReference }]));
}

async function safeContainerLimits(containers) {
  return Object.fromEntries(Object.entries(containers).map(([service, info]) => [service, {
    nano_cpus: info.nanoCpus, memory_limit_bytes: info.memoryLimitBytes, cpu_quota: info.cpuQuota, cpu_period: info.cpuPeriod,
  }]));
}

async function readCatalogCredentials(catalogID, projectName) {
  if (!/^[0-9a-f]{12,64}$/i.test(catalogID)) throw new Error('catalog_container_id_invalid');
  await assertProjectOwnership(projectName);
  const inspected = JSON.parse(execSafe('owned_catalog_credential_read_failed', ['docker', 'inspect', catalogID], { cwd: ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }));
  if (inspected.length !== 1 || inspected[0].Id !== catalogID || inspected[0].Config?.Labels?.['com.docker.compose.project'] !== projectName) throw new Error('compose_ownership_mismatch');
  const environment = inspected[0].Config.Env ?? [];
  const values = new Map(environment.flatMap((entry) => {
    const separator = entry.indexOf('=');
    if (separator < 0) return [];
    const name = entry.slice(0, separator);
    return ['CATALOG_STAFF_WRITE_TOKEN', 'CATALOG_ORGANIZER_ASSERTION_KEY'].includes(name) ? [[name, entry.slice(separator + 1)]] : [];
  }));
  const staff = values.get('CATALOG_STAFF_WRITE_TOKEN');
  const key = values.get('CATALOG_ORGANIZER_ASSERTION_KEY');
  if (!staff || !key) throw new Error('owned_catalog_credentials_missing');
  return { staff, key };
}

function organizerAssertion() {
  const data = ['v1', STAFF, ORGANIZER, String(Math.floor(Date.now() / 1000) + 3600)].join('.');
  return `${data}.${createHmac('sha256', Buffer.from(catalogCredentials.key)).update(data).digest('base64url')}`;
}

async function requestJSON(url, { method = 'GET', body, catalog = false, idempotency = false } = {}) {
  assertNotInterrupted();
  if (fixtureDeadlineMs !== null && Date.now() >= fixtureDeadlineMs) throw new Error('fixture_setup_deadline_exceeded');
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (catalog) {
    headers['X-Catalog-Staff-Write-Token'] = catalogCredentials.staff;
    headers['X-Catalog-Organizer-Assertion'] = organizerAssertion();
  }
  if (idempotency) headers['Idempotency-Key'] = randomUUID();
  const response = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15000) });
  const status = response.status;
  let data = null;
  try { data = await response.json(); } catch { /* keep status only */ }
  return { status, data };
}

async function createOffers() {
  const venue = await catalogCreate('/venues', { name: `TKT492 ${randomUUID().slice(0, 8)}`, ga_capacity: 5000 }, 'catalog_venue_create');
  const single = await makeOffer(venue.id, 'single', null);
  const pass = await makeOffer(venue.id, 'operating_day', { mode: 'multi', requires_exit: false });
  const passExit = await makeOffer(venue.id, 'operating_day', { mode: 'multi', requires_exit: true });
  await waitAvailability(single.performanceId);
  await waitAvailability(pass.performanceId);
  await waitAvailability(passExit.performanceId);
  await waitPolicy(pass.performanceId, 'multi', false);
  await waitPolicy(passExit.performanceId, 'multi', true);
  return { single, pass, passExit };
}

async function makeOffer(venueID, kind, reentry) {
  const suffix = randomUUID().slice(0, 8);
  const event = await catalogCreate('/events', { name: { en: `TKT492 ${suffix}`, fr: `TKT492 ${suffix}` } }, 'catalog_event_create');
  const perfBody = kind === 'single'
    ? { event_id: event.id, venue_id: venueID, starts_at: new Date(Date.now() + 90 * 86400000).toISOString(), timezone: 'UTC' }
    : { event_id: event.id, venue_id: venueID, kind: 'operating_day', operating_date: new Date(Date.now() + 30 * 86400000).toISOString().slice(0, 10), opens_at: '10:00', closes_at: '22:00', timezone: 'UTC', re_entry: reentry };
  const performance = await catalogCreate('/performances', perfBody, 'catalog_performance_create');
  const ticketType = await catalogCreate('/ticket-types', { performance_id: performance.id, name: { en: 'General admission', fr: 'Admission générale' }, price: { amount: 1250, currency: 'EUR' } }, 'catalog_ticket_type_create');
  const published = await requestJSON(`${baseURL}/api/catalog/performances/${performance.id}/publish`, { method: 'POST', catalog: true });
  if (published.status !== 200) throw new Error('catalog_publish_status');
  return { performanceId: performance.id, ticketTypeId: ticketType.id, mode: kind === 'single' ? 'single' : reentry };
}

async function catalogCreate(route, body, operation) {
  const result = await requestJSON(`${baseURL}/api/catalog${route}`, { method: 'POST', catalog: true, idempotency: true, body });
  if (result.status !== 201 || !UUID.test(result.data?.id ?? '')) throw new Error(`${operation}_status_${result.status}`);
  return { id: result.data.id };
}

async function waitAvailability(performanceID) {
  await poll(async () => {
    const response = await requestJSON(`${baseURL}/api/inventory/slots/${performanceID}/availability?organizer_id=${ORGANIZER}`);
    return response.status === 200;
  }, 30000, 'inventory_projection_timeout');
}

async function waitPolicy(performanceID, mode, requiresExit) {
  await poll(async () => {
    const output = sql(`SELECT json_build_object('mode',mode,'requires_exit',requires_exit)::text FROM slot_re_entry_policies WHERE slot_id='${performanceID}'`);
    if (!output) return false;
    const row = JSON.parse(output);
    return row.mode === mode && row.requires_exit === requiresExit;
  }, 30000, 'access_policy_projection_timeout');
}

async function poll(check, timeout, label) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    try { if (await check()) return; } catch { /* retry until fixed deadline */ }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(label);
}

async function pollPageEvaluation(page, predicate, argument, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (true) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) throw pageEvaluationTimeout();
    let timer;
    let matched;
    try {
      matched = await Promise.race([
        page.evaluate(predicate, argument),
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(pageEvaluationTimeout()), remaining);
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
    if (Date.now() >= deadline) throw pageEvaluationTimeout();
    if (matched) return;
    const waitMs = deadline - Date.now();
    if (waitMs <= 0) throw pageEvaluationTimeout();
    await new Promise((resolve) => setTimeout(resolve, Math.min(100, waitMs)));
  }
}

function pageEvaluationTimeout() {
  const error = new Error('Timeout exceeded');
  error.name = 'TimeoutError';
  return error;
}

function sql(statement) {
  const id = containerIds.postgres.id;
  return execSafe('access_schema_query_failed', ['docker', 'exec', '-i', id, 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'access', '-tAq'], {
    cwd: ROOT, encoding: 'utf8', input: statement, maxBuffer: 4 * 1024 * 1024, timeout: 30000,
  }).trim();
}

async function checkoutBatch(offer, quantity, alias) {
  if (quantity > 50) throw new Error('checkout_quantity_limit');
  const reservation = await requestJSON(`${baseURL}/api/commerce/reservations`, {
    method: 'POST', idempotency: true,
    body: { organizer_id: ORGANIZER, ticket_type_id: offer.ticketTypeId, quantity },
  });
  if (reservation.status !== 201 || !reservation.data?.reservation_id) throw new Error(`reservation_status_${reservation.status}`);
  const order = await requestJSON(`${baseURL}/api/commerce/orders`, {
    method: 'POST', idempotency: true,
    body: { reservation_id: reservation.data.reservation_id, name: 'TKT-492 Measurement', email: `tkt492-${randomUUID()}@example.test`, payment_token: 'fake-ok' },
  });
  if (order.status !== 200 || !order.data?.guest_order_ref) throw new Error(`checkout_status_${order.status}`);
  const bundle = await waitTickets(order.data.guest_order_ref, quantity);
  const ids = [];
  for (let index = 0; index < bundle.tickets.length; index += 1) {
    const ticket = bundle.tickets[index];
    if (!ticket.qr_payload || !Array.isArray(ticket.history) || ticket.history.length !== 2 || ticket.history[0]?.type !== 'issued' || ticket.history[0]?.sequence !== 1 || ticket.history[1]?.type !== 'delivered' || ticket.history[1]?.sequence !== 2) throw new Error('issued_ticket_history_invalid');
    const claimsPart = ticket.qr_payload.split('.')[1];
    let ticketID;
    try { ticketID = JSON.parse(Buffer.from(claimsPart, 'base64url').toString('utf8')).tid; } catch { throw new Error('issued_ticket_claim_invalid'); }
    if (!UUID.test(ticketID ?? '')) throw new Error('issued_ticket_claim_invalid');
    const ticketAlias = `${alias}-${String(index + 1).padStart(4, '0')}`;
    tickets[ticketAlias] = { credential: ticket.qr_payload, ticketID, slotID: offer.performanceId };
    fixtureAliases[ticketAlias] = alias;
    ids.push(ticketAlias);
  }
  return ids;
}

async function waitTickets(guestRef, quantity) {
  const end = Date.now() + 30000;
  while (Date.now() < end) {
    let response;
    try {
      response = await requestJSON(`${baseURL}/api/access/orders/${guestRef}/tickets`);
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 250));
      continue;
    }
    if (response.status === 200) {
      const ticketsInBundle = response.data?.tickets;
      if (Array.isArray(ticketsInBundle) && ticketsInBundle.length > quantity) {
        throw new Error('issued_ticket_history_invalid');
      }
      if (Array.isArray(ticketsInBundle) && ticketsInBundle.length === quantity) {
        const state = ticketHistoryReadiness(response.data);
        if (state === 'invalid') throw new Error('issued_ticket_history_invalid');
        if (state === 'ready') return response.data;
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error('ticket_delivery_timeout');
}

function ticketHistoryReadiness(bundle) {
  if (!Array.isArray(bundle?.tickets) || bundle.tickets.length === 0) return 'invalid';
  let pending = false;
  for (const ticket of bundle.tickets) {
    if (!Array.isArray(ticket?.history)) return 'invalid';
    const history = ticket.history;
    if (history.length === 1 && history[0]?.type === 'issued' && history[0]?.sequence === 1) {
      pending = true;
      continue;
    }
    if (history.length === 2 && history[0]?.type === 'issued' && history[0]?.sequence === 1 && history[1]?.type === 'delivered' && history[1]?.sequence === 2) continue;
    return 'invalid';
  }
  return pending ? 'pending' : 'ready';
}

async function issueMany(offer, count, alias, deadline) {
  const ids = [];
  for (let offset = 0, batch = 0; offset < count; offset += 50, batch += 1) {
    assertNotInterrupted();
    if (Date.now() >= deadline) throw new Error('fixture_setup_deadline_exceeded');
    ids.push(...await checkoutBatch(offer, Math.min(50, count - offset), `${alias}-b${batch + 1}`));
  }
  return ids;
}

async function createDevicePages(accessID) {
  browser = await chromium.launch({ channel: 'chrome', headless: true, timeout: 30000 });
  const runtimeMetadata = await readJSONSafe(metadataPath);
  runtimeMetadata.browser.chrome_version = browser.version();
  await writeJSON(metadataPath, runtimeMetadata);
  for (const alias of ['A', 'B', 'C', 'D']) pages.push(await createDevicePage(accessID, alias));
}

async function createDevicePage(accessID, alias) {
  const token = await enrol(accessID, alias);
  devices.push({ alias, token });
  const context = await browser.newContext({ baseURL, viewport: { width: 1280, height: 800 } });
  contexts.push(context);
  await context.addInitScript(`(${observerInstall.toString()})(${JSON.stringify({ contextAlias: alias })})`);
  const page = await context.newPage();
  await page.goto('/scanner/', { waitUntil: 'domcontentloaded', timeout: 30000 });
  await pairPage(page, token);
  await page.getByRole('button', { name: 'Refresh revocation list' }).click();
  await page.waitForFunction(() => localStorage.getItem('scanner.device-token') !== null);
  page.on('crash', () => pageFaults.push({ context: alias, kind: 'crash' }));
  page.on('pageerror', () => pageFaults.push({ context: alias, kind: 'page_error' }));
  page.on('framenavigated', (frame) => {
    if (frame === page.mainFrame()) pageFaults.push({ context: alias, kind: 'unexpected_main_navigation' });
  });
  return page;
}

function enrol(accessID, alias) {
  try {
    const output = execFileSync('docker', ['exec', '-i', accessID, '/app', 'enrol-scanner', ORGANIZER, `TKT492 ${alias} ${randomUUID().slice(0, 8)}`], { cwd: ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'], timeout: 15000 });
    const token = output.match(/^  X-Scanner-Token: (.+)$/m)?.[1]?.trim();
    if (!token) throw new Error('scanner_enrollment_failed');
    return token;
  } catch { throw new Error('scanner_enrollment_failed'); }
}

async function createBaselineFixture(offers, deadline) {
  const onlineSingles = await issueMany(offers.single, 400, 'online-single', deadline);
  const onlineWarmupSingles = await issueMany(offers.single, 40, 'online-warmup', deadline);
  const offlineWarmupSingles = await issueMany(offers.single, 40, 'offline-warmup', deadline);
  const offlineSharedAB = await issueMany(offers.single, 50, 'offline-shared-ab', deadline);
  const offlineUniqueCD = await issueMany(offers.single, 100, 'offline-unique-cd', deadline);
  const passExit = await issueMany(offers.passExit, 4, 'pass-exit-refusal', deadline);
  for (let i = 0; i < passExit.length; i += 1) {
    const seed = await scanOne(pages[i], tickets[passExit[i]].credential, 'pass_accept', `exit-seed-${i}`);
    tickets[passExit[i]].preMeasuredEvents = [{ occurrence_id: seed.occurrence_id, ticket_id: tickets[passExit[i]].ticketID, event_type: 'entry', occurred_at: seed.scanned_at }];
  }
  const passWarmup = await issueMany(offers.pass, 4, 'pass-warmup', deadline);
  const passMeasured = await issueMany(offers.pass, 4, 'pass-measured', deadline);
  const correctnessPass = await issueMany(offers.pass, 1, 'correctness-pass', deadline);
  const offlinePassWarmup = await issueMany(offers.pass, 4, 'offline-pass-warmup', deadline);
  const offlinePassMeasured = await issueMany(offers.pass, 4, 'offline-pass-measured', deadline);
  const correctnessSingles = await issueMany(offers.single, 8, 'correctness-single', deadline);
  const raceTicket = await issueMany(offers.single, 1, 'correctness-race', deadline);
  const onlineByContext = {};
  for (let contextIndex = 0; contextIndex < 4; contextIndex += 1) onlineByContext[['A', 'B', 'C', 'D'][contextIndex]] = onlineSingles.slice(contextIndex * 100, (contextIndex + 1) * 100);
  const warmupByContext = {};
  for (let contextIndex = 0; contextIndex < 4; contextIndex += 1) warmupByContext[['A', 'B', 'C', 'D'][contextIndex]] = onlineWarmupSingles.slice(contextIndex * 10, (contextIndex + 1) * 10);
  const offlineByContext = { A: offlineSharedAB.slice(0, 50), B: offlineSharedAB.slice(0, 50), C: offlineUniqueCD.slice(0, 50), D: offlineUniqueCD.slice(50, 100) };
  return { offers, onlineByContext, warmupByContext, offlineWarmupSingles, offlineByContext, passExit, passWarmup, passMeasured, correctnessPass, offlinePassWarmup, offlinePassMeasured, correctnessSingles, raceTicket };
}

async function createControlsFixture(offers, deadline) {
  const singles = await issueMany(offers.single, 20, 'control-single', deadline);
  const passExit = await issueMany(offers.passExit, 1, 'control-pass-exit', deadline);
  await scanOne(pages[0], tickets[passExit[0]].credential, 'pass_accept', 'control-exit-seed');
  const pass = await issueMany(offers.pass, 2, 'control-pass', deadline);
  return { offers, singles, passExit, pass };
}

function observerInstall({ contextAlias }) {
  const state = { alias: contextAlias, active: null, records: [], requestRows: [], reconciliations: [], clearSeen: false, observer: null, resultGeneration: 0 };
  window.__tkt492 = state;
  const resultNode = () => document.querySelector('main.scanner .result');
  const readResult = () => {
    const node = resultNode();
    if (!node) return null;
    return { node, heading: node.querySelector('h2')?.textContent?.trim() ?? '', body: node.querySelector('p')?.textContent?.trim() ?? '' };
  };
  const finish = (active, appearance, rendered) => {
    if (active.done || active.occurrenceId == null || !active.response || !state.clearSeen) return;
    active.done = true;
    active.domAt = performance.now();
    requestAnimationFrame(() => requestAnimationFrame(() => {
      const latest = readResult();
      if (!latest || latest.node !== appearance.node) {
        active.renderError = 'result_changed_before_render_boundary';
        return;
      }
      active.renderAt = performance.now();
      active.renderedHeading = latest.heading;
      active.renderedClass = ['accepted', 'rejected', 'queued'].find((name) => latest.node.classList.contains(name)) ?? null;
      active.renderedBody = latest.body;
      active.resultGeneration = ++state.resultGeneration;
      active.replayNoticePresent = [...latest.node.querySelectorAll('p')].some((paragraph) => paragraph.textContent.includes('result replayed, entry counted once'));
      state.records.push({
        context: state.alias,
        fixtureAlias: active.fixtureAlias,
        occurrence_id: active.occurrenceId,
        result_generation: active.resultGeneration,
        scan_post_count: active.requests.length,
        request_started_at_epoch_ms: active.requests[0]?.startedAtEpochMs ?? null,
        response_at_epoch_ms: active.requests[0]?.responseAtEpochMs ?? null,
        scheduled_at_ms: active.scheduledAt,
        scheduled_at_epoch_ms: performance.timeOrigin + active.scheduledAt,
        wake_at_ms: active.wakeAt,
        click_at_ms: active.clickAt,
        dom_at_ms: active.domAt,
        render_at_ms: active.renderAt,
        schedule_lag_ms: Math.max(0, active.wakeAt - active.scheduledAt),
        latency_ms: active.renderAt - active.clickAt,
        status: active.response.status,
        decision: active.response.decision ?? null,
        reason: active.response.reason ?? null,
        replay: active.response.replay ?? null,
        scanned_at: active.response.scanned_at ?? null,
        original_scan_at: active.response.original_scan_at ?? null,
        rendered_heading: active.renderedHeading,
        rendered_class: active.renderedClass,
        rendered_body: active.renderedBody,
        replay_notice_present: active.replayNoticePresent,
        transport: active.transport ?? 'response',
        error_classification: active.errorClassification ?? null,
        class: active.className ?? null,
        fixture_alias: active.fixtureAlias ?? null,
        history_length: active.historyLength ?? null,
        offline_control: active.offlineControl ?? 'disabled',
        queue_state: active.queueState ?? null,
        visibility_at_click: active.visibilityAtClick,
        visibility_at_render: document.visibilityState,
      });
    }));
  };
  state.observer = new MutationObserver(() => {
    if (!state.active) return;
    const current = readResult();
    if (!current) {
      state.clearSeen = true;
      return;
    }
    const active = state.active;
    if (state.clearSeen && active.response && active.occurrenceId && active.requests.length === 1) finish(active, current, current);
  });
  state.observer.observe(document, { subtree: true, childList: true, characterData: true });
  document.addEventListener('click', (event) => {
    if (!(event.target instanceof Element) || !event.target.closest('button')) return;
    if (event.target.closest('button')?.textContent?.trim() !== 'Check ticket') return;
    const previous = resultNode();
    state.clearSeen = !previous;
    state.active = {
      className: window.__tkt492Next?.className ?? null,
      fixtureAlias: window.__tkt492Next?.fixtureAlias ?? null,
      historyLength: window.__tkt492Next?.historyLength ?? null,
      scheduledAt: window.__tkt492Next?.scheduledAt ?? performance.now(),
      wakeAt: window.__tkt492Next?.wakeAt ?? performance.now(),
      offlineControl: navigator.onLine ? 'disabled' : 'enabled',
      visibilityAtClick: document.visibilityState,
      clickAt: performance.now(), done: false, requests: [],
    };
    window.__tkt492Next = null;
    if (previous) queueMicrotask(() => { if (!resultNode()) state.clearSeen = true; });
  }, true);
  const originalFetch = window.fetch.bind(window);
  window.fetch = async (...args) => {
    const [input, init] = args;
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    const method = init?.method ?? (input instanceof Request ? input.method : 'GET');
    if (method.toUpperCase() === 'POST' && url.pathname === '/api/access/scans/reconciliations') {
      try {
        const response = await originalFetch(...args);
        let payload = null;
        try { payload = await response.clone().json(); } catch { /* status-only row below */ }
        state.reconciliations.push({
          status: response.status,
          results: Array.isArray(payload?.results) ? payload.results.map((entry) => ({
            occurrence_id: entry.occurrence_id ?? null,
            result: entry.result ?? null,
            occurred_at: entry.occurred_at ?? null,
          })) : [],
        });
        return response;
      } catch {
        state.reconciliations.push({ status: null, results: [], error: 'transport_failure' });
        throw new Error('reconciliation transport failed');
      }
    }
    if (method.toUpperCase() !== 'POST' || url.pathname !== '/api/access/scans') return originalFetch(...args);
    let body;
    try { body = JSON.parse(init?.body ?? (input instanceof Request ? await input.clone().text() : '{}')); } catch { body = {}; }
    const active = state.active;
    if (active) active.occurrenceId = body.occurrence_id ?? null;
    const row = { occurrence_id: body.occurrence_id ?? null, startedAtEpochMs: performance.timeOrigin + performance.now() };
    state.requestRows.push(row);
    if (active) active.requests.push(row);
    try {
      const response = await originalFetch(...args);
      let payload = null;
      try { payload = await response.clone().json(); } catch { if (active) active.errorClassification = 'unknown_transport'; }
      row.status = response.status;
      row.responseAtEpochMs = performance.timeOrigin + performance.now();
      if (active) {
        active.occurrenceId = body.occurrence_id ?? null;
        active.response = { status: response.status, decision: payload?.decision ?? null, reason: payload?.reason ?? null, replay: payload?.replay ?? null, scanned_at: payload?.scanned_at ?? null, original_scan_at: payload?.original_scan_at ?? null };
        active.transport = 'response';
        if (!payload || !response.ok && response.status !== 409) active.errorClassification = 'unknown_transport';
      }
      return response;
    } catch {
      if (active) {
        active.occurrenceId = body.occurrence_id ?? null;
        row.responseAtEpochMs = performance.timeOrigin + performance.now();
        active.errorClassification = navigator.onLine ? 'aborted_online' : 'intentional_offline';
        active.transport = navigator.onLine ? 'aborted' : 'failed_offline';
        active.status = null;
        active.response = { status: null, decision: null, reason: null, replay: null };
      }
      throw new Error('scan transport failed');
    }
  };
}

async function scanOne(page, credential, className, alias, { routeControl, offline = false, historyLength = 0 } = {}) {
  if (offline) await page.context().setOffline(true);
  await page.locator('#qr-payload').fill(credential);
  if (routeControl) await routeControl(page);
  await page.evaluate(async ({ className, alias, historyLength }) => {
    const scheduledAt = performance.now() + 200;
    window.__tkt492Next = { className, fixtureAlias: alias, historyLength, scheduledAt, wakeAt: performance.now() };
    await new Promise((resolve) => setTimeout(resolve, Math.max(0, scheduledAt - performance.now())));
    window.__tkt492Next.wakeAt = performance.now();
    [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click();
  }, { className, alias, historyLength });
  await page.waitForFunction((expectedAlias) => window.__tkt492.records.at(-1)?.fixtureAlias === expectedAlias, alias, { timeout: PROFILE.deadlines.scan_ms });
  const record = await page.evaluate(() => window.__tkt492.records.at(-1) ?? null);
  if (!record) throw new Error('scan_observation_missing');
  const local = await occurrenceRecord(page, record.occurrence_id);
  if (!local || local.occurrence_id !== record.occurrence_id || record.scan_post_count !== 1) throw new Error('scan_occurrence_correlation_failed');
  record.local_occurrence_id = local.occurrence_id;
  const persisted = await occurrenceRecord(page, record.occurrence_id);
  record.local_occurrence_id = persisted?.occurrence_id ?? null;
  if (!persisted || persisted.occurrence_id !== record.occurrence_id || record.scan_post_count !== 1) throw new Error('scan_occurrence_correlation_failed');
  if (offline) {
    const state = await occurrenceState(page, record.occurrence_id);
    record.queue_state = state;
    record.offline_control = 'enabled';
    if (state !== 'QUEUED') throw new Error('offline_occurrence_not_queued');
  }
  const normalized = normalizeRecord(record, className, alias, historyLength, offline);
  if (!offline) assertExpected(normalized, className);
  else if (normalized.rendered_class !== 'queued' || normalized.rendered_heading !== PROFILE.offline.heading || normalized.rendered_body_prefix !== PROFILE.offline.body_prefix || normalized.error_classification !== 'intentional_offline') throw new Error('offline_sample_invalid');
  return normalized;
}

function assertExpected(sample, className) {
  const expected = PROFILE.online_classes[className];
  const expectedClass = expected?.decision === 'accepted' ? 'accepted' : 'rejected';
  if (!expected || sample.status !== expected.status || sample.decision !== expected.decision || sample.reason !== expected.reason || sample.rendered_class !== expectedClass || sample.rendered_heading !== expected.heading || sample.rendered_body_prefix !== expected.body_prefix || sample.replay === true) throw new Error(`scan_correctness_${className}`);
}

function normalizeRecord(row, className, alias, historyLength, offline) {
  const expected = offline ? PROFILE.offline : PROFILE.online_classes[className];
  const result = {
    attempt: opts.attempt ?? 'controls', phase: opts.mode === 'baseline' ? 'measured' : 'control', context: row.context,
    fixture_alias: alias, occurrence_id: row.occurrence_id, class: className,
    local_occurrence_id: row.local_occurrence_id ?? null, result_generation: row.result_generation ?? null,
    scan_post_count: row.scan_post_count ?? null, request_started_at_epoch_ms: row.request_started_at_epoch_ms ?? null,
    response_at_epoch_ms: row.response_at_epoch_ms ?? null,
    history_observed_at_epoch_ms: null,
    history_length: historyLength, scheduled_at_ms: row.scheduled_at_ms, scheduled_at_epoch_ms: row.scheduled_at_epoch_ms, wake_at_ms: row.wake_at_ms,
    click_at_ms: row.click_at_ms, dom_at_ms: row.dom_at_ms, render_at_ms: row.render_at_ms,
    latency_ms: row.latency_ms, schedule_lag_ms: Math.max(0, row.wake_at_ms - row.scheduled_at_ms),
    status: offline ? null : row.status, decision: offline ? null : row.decision, reason: offline ? null : row.reason,
    replay: offline ? null : row.replay, rendered_heading: row.rendered_heading, rendered_class: row.rendered_class ?? null,
    replay_notice_present: row.replay_notice_present,
    scanned_at: offline ? null : row.scanned_at, original_scan_at: offline ? null : row.original_scan_at,
    rendered_body_prefix: typeof row.rendered_body === 'string' ? row.rendered_body.slice(0, expected.body_prefix.length) : null,
    offline_control: offline ? 'enabled' : 'disabled', transport: row.transport,
    queue_state: offline ? row.queue_state : null, error_classification: row.error_classification,
    visibility_at_click: row.visibility_at_click, visibility_at_render: row.visibility_at_render,
  };
  return result;
}

async function occurrenceState(page, occurrenceID) {
  return (await occurrenceRecord(page, occurrenceID))?.state ?? null;
}

async function occurrenceRecord(page, occurrenceID) {
  return page.evaluate(async (id) => {
    const request = indexedDB.open('gate-occurrences');
    const db = await new Promise((resolve, reject) => { request.onsuccess = () => resolve(request.result); request.onerror = () => reject(new Error('idb')); });
    const tx = db.transaction('occurrences', 'readonly');
    const get = tx.objectStore('occurrences').get(id);
    const row = await new Promise((resolve, reject) => { get.onsuccess = () => resolve(get.result); get.onerror = () => reject(new Error('idb')); });
    db.close();
    return row ? { occurrence_id: row.occurrenceId ?? null, state: row.state ?? null, occurred_at: row.occurredAt ?? null } : null;
  }, occurrenceID);
}

async function commonPageDeadline(leadMs) {
  const clocks = await Promise.all(pages.map((page) => page.evaluate(() => performance.timeOrigin + performance.now())));
  return Math.max(...clocks) + leadMs;
}

async function relativePageDeadline(page, epochDeadline, staggerMs) {
  return page.evaluate(({ epochDeadline, staggerMs }) => epochDeadline + staggerMs - performance.timeOrigin, { epochDeadline, staggerMs });
}

async function runWarmups(fixture) {
  const onlineStart = await commonPageDeadline(1000);
  const onlineWarmup = pages.map(async (page, contextIndex) => {
    const alias = ['A', 'B', 'C', 'D'][contextIndex];
    const due0 = await relativePageDeadline(page, onlineStart, contextIndex * PROFILE.browser.stagger_ms);
    for (let i = 0; i < 10; i += 1) {
      const single = fixture.warmupByContext[alias][i];
      const cycle = [
        ['single_accept', single, 0], ['single_duplicate', single, 1],
        ['pass_accept', fixture.passWarmup[contextIndex], i], ['pass_exit_required', fixture.passExit[contextIndex], 1],
      ];
      for (let step = 0; step < cycle.length; step += 1) {
        const [className, ticketAlias, historyLength] = cycle[step];
        const row = await scheduledScan(page, tickets[ticketAlias].credential, className, `warmup-${alias}-${className}-${i}`, historyLength, due0 + (i * 4 + step) * PROFILE.browser.interval_ms);
        warmupRows.push({ ...row, ticket_alias: ticketAlias, ticket_id: tickets[ticketAlias].ticketID, attempt: opts.attempt, phase: 'warmup' });
      }
    }
  });
  const onlineWarmupResults = await Promise.allSettled(onlineWarmup);
  if (onlineWarmupResults.some((result) => result.status === 'rejected')) throw new Error('online_warmup_failed');
  // Offline queue warm-up is per context. Sync each device before the measured
  // 100-occurrence reconciliation limit starts.
  const warm = fixture.offlineWarmupSingles;
  await Promise.all(pages.map((page) => page.context().setOffline(true)));
  const offlineStart = await commonPageDeadline(1000);
  const offlineWarmup = pages.map(async (page, i) => {
    const alias = ['A', 'B', 'C', 'D'][i];
    const due0 = await relativePageDeadline(page, offlineStart, i * PROFILE.browser.stagger_ms);
    for (let j = 0; j < 20; j += 1) {
      const isPass = j >= 10;
      const ticketAlias = isPass ? fixture.offlinePassWarmup[i] : warm[i * 10 + j];
      const row = await scheduledOfflineScan(page, tickets[ticketAlias].credential, `warmup-offline-${alias}-${j}`, due0 + j * PROFILE.browser.interval_ms, isPass ? 'offline_pass_queued' : 'offline_single_queued');
      warmupRows.push({ ...row, ticket_alias: ticketAlias, ticket_id: tickets[ticketAlias].ticketID, attempt: opts.attempt, phase: 'warmup' });
    }
  });
  const offlineWarmupResults = await Promise.allSettled(offlineWarmup);
  if (offlineWarmupResults.some((result) => result.status === 'rejected')) throw new Error('offline_warmup_failed');
  for (let i = 0; i < 4; i += 1) await reconcilePage(pages[i], 'warmup');
  healthRows.push(await stackHealth('warmup'));
}

async function runMeasuredOnline(fixture) {
  const phaseStart = await commonPageDeadline(1000);
  const tasks = pages.map(async (page, contextIndex) => {
    const contextAlias = ['A', 'B', 'C', 'D'][contextIndex];
    const rows = [];
    const due0 = await relativePageDeadline(page, phaseStart, contextIndex * PROFILE.browser.stagger_ms);
    for (let i = 0; i < 100; i += 1) {
      const singleAlias = fixture.onlineByContext[contextAlias][i];
      const passAlias = fixture.passMeasured[contextIndex];
      const exitAlias = fixture.passExit[contextIndex];
      const steps = [
        ['single_accept', singleAlias, 0],
        ['single_duplicate', singleAlias, 1],
        ['pass_accept', passAlias, null],
        ['pass_exit_required', exitAlias, 1],
      ];
      for (let step = 0; step < steps.length; step += 1) {
        const [className, ticketAlias, plannedHistoryLength] = steps[step];
        const ordinal = i * 4 + step;
        const scheduledAt = due0 + ordinal * PROFILE.browser.interval_ms;
        const historyLength = className === 'pass_accept' ? queryAcceptedPassCount(tickets[ticketAlias].ticketID) : plannedHistoryLength;
        const historyObservedAtEpochMs = className === 'pass_accept' ? Date.now() : null;
        const row = await scheduledScan(page, tickets[ticketAlias].credential, className, `measured-${contextAlias}-${className}-${i}`, historyLength, scheduledAt, historyObservedAtEpochMs);
        row.ticket_alias = ticketAlias;
        row.ticket_id = tickets[ticketAlias].ticketID;
        rows.push(row);
        sampleRows.push(row);
      }
    }
    return rows;
  });
  const results = await Promise.allSettled(tasks);
  if (results.some((result) => result.status === 'rejected')) throw new Error('measured_online_phase_failed');
}

async function scheduledScan(page, credential, className, alias, historyLength, scheduledAt, historyObservedAtEpochMs = null) {
  await page.locator('#qr-payload').fill(credential);
  const result = await page.evaluate(async ({ className, alias, historyLength, scheduledAt }) => {
    const now = performance.now();
    window.__tkt492Next = { className, fixtureAlias: alias, historyLength, scheduledAt, wakeAt: now };
    const delay = scheduledAt - now;
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    const wakeAt = performance.now();
    if (window.__tkt492.active && !window.__tkt492.active.done) return { missed: true, wakeAt, scheduledAt };
    window.__tkt492Next.wakeAt = wakeAt;
    window.__tkt492Next.scheduledAt = scheduledAt;
    [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click();
    return { missed: false, wakeAt, scheduledAt };
  }, { className, alias, historyLength, scheduledAt });
  if (result.missed) throw new Error('missed_arrival');
  await page.waitForFunction((expectedAlias) => {
    const last = window.__tkt492.records.at(-1);
    return last && last.fixtureAlias === expectedAlias;
  }, alias, { timeout: PROFILE.deadlines.scan_ms });
  const row = await page.evaluate(() => window.__tkt492.records.at(-1));
  const local = await occurrenceRecord(page, row.occurrence_id);
  if (!local || local.occurrence_id !== row.occurrence_id || row.scan_post_count !== 1) throw new Error('scheduled_occurrence_correlation_failed');
  const normalized = normalizeRecord({ ...row, local_occurrence_id: local.occurrence_id }, className, alias, historyLength, false);
  normalized.history_observed_at_epoch_ms = historyObservedAtEpochMs;
  return normalized;
}

function queryAcceptedPassCount(ticketID) {
  if (!UUID.test(ticketID)) throw new Error('pass_ticket_uuid_invalid');
  const value = sql(`SELECT count(*)::text FROM lifecycle_events WHERE ticket_id='${ticketID}'::uuid AND event_type='entry'`);
  if (!/^\d+$/.test(value)) throw new Error('pass_history_count_invalid');
  return Number(value);
}

function timestampMicros(value) {
  if (typeof value !== 'string') throw new Error('timestamp_invalid');
  const match = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!match) throw new Error('timestamp_invalid');
  const [, y, mo, d, h, mi, s, fraction = '', zone] = match;
  const year = Number(y), month = Number(mo), day = Number(d), hour = Number(h), minute = Number(mi), second = Number(s);
  if (month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || second > 59) throw new Error('timestamp_invalid');
  const date = new Date(0);
  date.setUTCFullYear(year, month - 1, day);
  date.setUTCHours(hour, minute, second, 0);
  if (date.getUTCFullYear() !== year || date.getUTCMonth() !== month - 1 || date.getUTCDate() !== day) throw new Error('timestamp_invalid');
  let offsetMinutes = 0;
  if (zone !== 'Z') {
    const offsetHour = Number(zone.slice(1, 3)), offsetMinute = Number(zone.slice(4, 6));
    if (offsetHour > 14 || offsetMinute > 59 || (offsetHour === 14 && offsetMinute !== 0)) throw new Error('timestamp_invalid');
    offsetMinutes = (offsetHour * 60 + offsetMinute) * (zone[0] === '+' ? 1 : -1);
  }
  const micros = BigInt((fraction + '000000').slice(0, 6));
  const firstDiscardedDigit = fraction.length > 6 ? Number(fraction[6]) : 0;
  const roundedMicros = micros + (firstDiscardedDigit >= 5 ? 1n : 0n);
  const secondsUTC = BigInt(date.getTime() / 1000 - offsetMinutes * 60);
  return secondsUTC * 1000000n + roundedMicros;
}

function sameTimestampMicros(left, right) {
  try { return timestampMicros(left) === timestampMicros(right); }
  catch { return false; }
}

async function runMeasuredOffline(fixture) {
  await Promise.all(pages.map((page) => page.context().setOffline(true)));
  const phaseStart = await commonPageDeadline(1000);
  const tasks = pages.map(async (page, contextIndex) => {
    const contextAlias = ['A', 'B', 'C', 'D'][contextIndex];
    const due0 = await relativePageDeadline(page, phaseStart, contextIndex * PROFILE.browser.stagger_ms);
    const rows = [];
    for (let i = 0; i < 100; i += 1) {
      let ticketAlias;
      if (contextAlias === 'A' || contextAlias === 'B') ticketAlias = fixture.offlineByContext[contextAlias][i % 50];
      else ticketAlias = fixture.offlineByContext[contextAlias][i];
      if (i < 50 && (contextAlias === 'A' || contextAlias === 'B')) {
        // Paired A/B rows share one underlying ticket, while each context has
        // its own occurrence id and saved timestamp.
      }
      const passAlias = fixture.offlinePassMeasured[contextIndex];
      const selected = i < 50 ? ticketAlias : passAlias;
      const type = i < 50 ? `offline-single-${contextAlias}-${i}` : `offline-pass-${contextAlias}-${i}`;
      const scheduledAt = due0 + i * PROFILE.browser.interval_ms;
      const row = await scheduledOfflineScan(page, tickets[selected].credential, type, scheduledAt, i < 50 ? 'offline_single_queued' : 'offline_pass_queued');
      row.ticket_alias = selected;
      row.ticket_id = tickets[selected].ticketID;
      rows.push(row);
      sampleRows.push(row);
    }
    return rows;
  });
  const results = await Promise.allSettled(tasks);
  if (results.some((result) => result.status === 'rejected')) throw new Error('measured_offline_phase_failed');
  // B must commit before A. Each context's 100-row batch stays within the API cap.
  for (const alias of ['B', 'A', 'C', 'D']) await reconcilePage(pages[['A', 'B', 'C', 'D'].indexOf(alias)], `measured-${alias}`);
}

async function scheduledOfflineScan(page, credential, alias, scheduledAt, ticketClass = 'offline_single_queued') {
  await page.locator('#qr-payload').fill(credential);
  await page.evaluate(async ({ alias, scheduledAt }) => {
    const now = performance.now();
    window.__tkt492Next = { className: 'offline_queued', fixtureAlias: alias, historyLength: 0, scheduledAt, wakeAt: now, offlineControl: 'enabled' };
    const delay = scheduledAt - now;
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    window.__tkt492Next.wakeAt = performance.now();
    [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click();
  }, { alias, scheduledAt });
  await page.waitForFunction((expectedAlias) => window.__tkt492.records.at(-1)?.fixtureAlias === expectedAlias, alias, { timeout: PROFILE.deadlines.scan_ms });
  const row = await page.evaluate(() => window.__tkt492.records.at(-1));
  const persisted = await occurrenceRecord(page, row.occurrence_id);
  if (!persisted || persisted.occurrence_id !== row.occurrence_id || row.scan_post_count !== 1) throw new Error('offline_occurrence_correlation_failed');
  row.local_occurrence_id = persisted.occurrence_id;
  row.queue_state = await occurrenceState(page, row.occurrence_id);
  row.transport = 'failed_offline';
  row.error_classification = 'intentional_offline';
  row.offline_control = 'enabled';
  const normalized = normalizeRecord(row, ticketClass, alias, 0, true);
  if (normalized.queue_state !== 'QUEUED') throw new Error('offline_occurrence_not_queued');
  return normalized;
}

async function reconcilePage(page, label) {
  const queuedBefore = await page.evaluate(async () => {
    const req = indexedDB.open('gate-occurrences');
    const db = await new Promise((resolve, reject) => { req.onsuccess = () => resolve(req.result); req.onerror = () => reject(new Error('idb')); });
    const tx = db.transaction('occurrences', 'readonly');
    const get = tx.objectStore('occurrences').getAll();
    const all = await new Promise((resolve, reject) => { get.onsuccess = () => resolve(get.result); get.onerror = () => reject(new Error('idb')); });
    db.close();
    return all.filter((row) => row.state === 'QUEUED').map((row) => ({ occurrence_id: row.occurrenceId, occurred_at: row.occurredAt }));
  });
  if (queuedBefore.length === 0 || queuedBefore.length > 100) throw new Error('offline_reconcile_batch_size_invalid');
  const before = await page.evaluate(() => window.__tkt492.reconciliations.length);
  await page.context().setOffline(false);
  await page.waitForFunction((count) => window.__tkt492.reconciliations.length > count, before, { timeout: PROFILE.deadlines.reconciliation_ms });
  try {
    await pollPageEvaluation(page, async ({ before, expectedIDs }) => {
      const response = window.__tkt492.reconciliations[before];
      if (response?.status !== 200 || !Array.isArray(response.results)) return false;
      const returned = new Map(response.results.map((row) => [row.occurrence_id, row]));
      if (returned.size !== expectedIDs.length || expectedIDs.some((id) => !returned.has(id))) return false;
      const req = indexedDB.open('gate-occurrences');
      const db = await new Promise((resolve, reject) => { req.onsuccess = () => resolve(req.result); req.onerror = () => reject(new Error('idb')); });
      const tx = db.transaction('occurrences', 'readonly');
      const get = tx.objectStore('occurrences').getAll();
      const rows = await new Promise((resolve, reject) => { get.onsuccess = () => resolve(get.result); get.onerror = () => reject(new Error('idb')); });
      db.close();
      const local = new Map(rows.map((row) => [row.occurrenceId, row]));
      return expectedIDs.every((id) => {
        const reply = returned.get(id);
        const row = local.get(id);
        return row && row.state === (reply.result === 'conflict' ? 'RESOLVED' : 'SYNCED') && row.result === reply.result;
      });
    }, { before, expectedIDs: queuedBefore.map((row) => row.occurrence_id) }, PROFILE.deadlines.reconciliation_ms);
  } catch (error) {
    const check = await page.evaluate(async ({ before, expectedIDs }) => {
      const response = window.__tkt492.reconciliations[before];
      if (response?.status !== 200 || !Array.isArray(response.results)) return null;
      const returned = new Map(response.results.map((row) => [row.occurrence_id, row]));
      if (returned.size !== expectedIDs.length || expectedIDs.some((id) => !returned.has(id))) return { mismatch: 'response_occurrence_set' };
      const req = indexedDB.open('gate-occurrences');
      const db = await new Promise((resolve, reject) => { req.onsuccess = () => resolve(req.result); req.onerror = () => reject(new Error('idb')); });
      const tx = db.transaction('occurrences', 'readonly');
      const get = tx.objectStore('occurrences').getAll();
      const rows = await new Promise((resolve, reject) => { get.onsuccess = () => resolve(get.result); get.onerror = () => reject(new Error('idb')); });
      db.close();
      const local = new Map(rows.map((row) => [row.occurrenceId, row]));
      return expectedIDs.some((id) => {
        const reply = returned.get(id);
        const row = local.get(id);
        return row && (row.state !== (reply.result === 'conflict' ? 'RESOLVED' : 'SYNCED') || row.result !== reply.result);
      }) ? { mismatch: 'saved_reconciliation_state' } : null;
    }, { before, expectedIDs: queuedBefore.map((row) => row.occurrence_id) }).catch(() => null);
    if (check?.mismatch === 'response_occurrence_set') correctnessAssert(false, `offline_reconcile_${label}_occurrence_mismatch`);
    if (check?.mismatch === 'saved_reconciliation_state') correctnessAssert(false, `offline_reconcile_${label}_local_state_mismatch`);
    throw error;
  }
  const result = await page.evaluate(async ({ before, label, expectedIDs }) => {
    const req = indexedDB.open('gate-occurrences');
    const db = await new Promise((resolve, reject) => { req.onsuccess = () => resolve(req.result); req.onerror = () => reject(new Error('idb')); });
    const tx = db.transaction('occurrences', 'readonly');
    const get = tx.objectStore('occurrences').getAll();
    const rows = await new Promise((resolve, reject) => { get.onsuccess = () => resolve(get.result); get.onerror = () => reject(new Error('idb')); });
    db.close();
    const response = window.__tkt492.reconciliations[before];
    const expected = new Set(expectedIDs);
    const pendingRows = rows.filter((row) => row.state === 'QUEUED');
    const localByID = new Map(rows.map((row) => [row.occurrenceId, row]));
    const localStateCounts = { QUEUED: 0, SYNCED: 0, RESOLVED: 0, missing: 0, other: 0 };
    for (const id of expectedIDs) {
      const row = localByID.get(id);
      if (!row) localStateCounts.missing += 1;
      else if (Object.hasOwn(localStateCounts, row.state)) localStateCounts[row.state] += 1;
      else localStateCounts.other += 1;
    }
    const hasResultsArray = Array.isArray(response?.results);
    const responseResults = hasResultsArray ? response.results : [];
    const returnedIDs = hasResultsArray
      ? new Set(responseResults.filter((row) => row && typeof row.occurrence_id === 'string').map((row) => row.occurrence_id))
      : null;
    return {
      pending: rows.filter((row) => row.state === 'QUEUED').length,
      local: rows.filter((row) => responseResults.some((item) => item?.occurrence_id === row.occurrenceId)).map((row) => ({ occurrence_id: row.occurrenceId, occurred_at: row.occurredAt, state: row.state, result: row.result ?? null })),
      response,
      diagnostics: {
        label,
        expected_occurrence_count: expectedIDs.length,
        response_status: response?.status ?? null,
        returned_total: hasResultsArray ? responseResults.length : null,
        returned_unique_count: returnedIDs?.size ?? null,
        pending_total: pendingRows.length,
        pending_inside_expected: pendingRows.filter((row) => expected.has(row.occurrenceId)).length,
        pending_outside_expected: pendingRows.filter((row) => !expected.has(row.occurrenceId)).length,
        local_state_counts: localStateCounts,
      },
    };
  }, { before, label, expectedIDs: queuedBefore.map((row) => row.occurrence_id) });
  correctness.reconciliation_diagnostics.push(result.diagnostics);
  correctnessAssert(result.response?.status === 200 && Array.isArray(result.response.results) && result.response.results.length === queuedBefore.length && result.pending === 0, `offline_reconcile_${label}_result_incomplete`);
  const returned = new Map(result.response.results.map((row) => [row.occurrence_id, row]));
  correctnessAssert(returned.size === queuedBefore.length && queuedBefore.every((row) => returned.has(row.occurrence_id)), `offline_reconcile_${label}_occurrence_mismatch`);
  const committedEvents = queryLifecycle(queuedBefore.map((row) => row.occurrence_id));
  const committedByID = new Map(committedEvents.map((row) => [row.id, row]));
  correctnessAssert(committedEvents.length === queuedBefore.length && committedByID.size === queuedBefore.length, `offline_reconcile_${label}_database_rows_incomplete`);
  const local = new Map(result.local.map((row) => [row.occurrence_id, row]));
  const annotations = [];
  for (const original of queuedBefore) {
    const reply = returned.get(original.occurrence_id);
    const saved = local.get(original.occurrence_id);
    const committed = committedByID.get(original.occurrence_id);
    const source = [...sampleRows, ...warmupRows].find((candidate) => candidate.occurrence_id === original.occurrence_id);
    const shouldConflict = label === 'measured-A' && source?.context === 'A' && source.class === 'offline_single_queued';
    correctnessAssert(reply.result === (shouldConflict ? 'conflict' : 'recorded'), `offline_reconcile_${label}_result_mismatch`);
    const wantType = reply.result === 'conflict' ? 'duplicate_admit' : source?.class === 'offline_pass_queued' ? 'entry' : 'redeemed';
    correctnessAssert(saved && committed && committed.event_type === wantType && sameTimestampMicros(saved.occurred_at, original.occurred_at) && sameTimestampMicros(reply.occurred_at, original.occurred_at) && sameTimestampMicros(committed.occurred_at, original.occurred_at), `offline_reconcile_${label}_timestamp_or_commit_mismatch`);
    const wantState = reply.result === 'conflict' ? 'RESOLVED' : 'SYNCED';
    correctnessAssert(saved.state === wantState && saved.result === reply.result, `offline_reconcile_${label}_local_state_mismatch`);
    annotations.push({ occurrence_id: original.occurrence_id, reconcile_result: reply.result, occurred_at: reply.occurred_at, device_occurred_at: original.occurred_at, persisted_state: saved.state, persisted_result: saved.result });
  }
  const byID = new Map(annotations.map((row) => [row.occurrence_id, row]));
  for (const row of [...sampleRows, ...warmupRows]) {
    const annotation = byID.get(row.occurrence_id);
    if (annotation) Object.assign(row, annotation);
  }
  return annotations;
}

async function runControls(fixture) {
  const accessID = containerIds.access.id;
  let page = await createDevicePage(accessID, 'control-delayed-response');
  const delayed = await scanControl(page, tickets[fixture.singles[0]].credential, 'single_accept', 'delayed_genuine_response', async (target) => {
    await target.route('**/api/access/scans', async (route) => {
      const response = await route.fetch();
      await new Promise((resolve) => setTimeout(resolve, 700));
      await route.fulfill({ response });
    });
  });
  controls.push({ name: 'delayed_genuine_response', observed_latency_ms: delayed.latency_ms, observed_status: delayed.status, observed_decision: delayed.decision, rendered_class: delayed.rendered_class, rendered_heading: delayed.rendered_heading, rendered_body_prefix: delayed.rendered_body_prefix, replay_notice_present: delayed.replay_notice_present, detected: delayed.latency_ms >= 500 && delayed.status === 200 && delayed.decision === 'accepted' && delayed.rendered_class === 'accepted' && delayed.rendered_heading === 'Accepted' && delayed.rendered_body_prefix === PROFILE.online_classes.single_accept.body_prefix && delayed.replay_notice_present === false });
  await page.unrouteAll({ behavior: 'wait' });

  page = await createDevicePage(accessID, 'control-online-abort');
  const aborted = await scanControl(page, tickets[fixture.singles[1]].credential, 'single_accept', 'online_abort_is_not_offline', async (target) => {
    await target.route('**/api/access/scans', (route) => route.abort('failed'));
  });
  controls.push({ name: 'online_abort_is_not_offline', online: true, observed_error: aborted.error_classification, detected: aborted.error_classification === 'aborted_online' });
  await page.unrouteAll({ behavior: 'wait' });

  page = await createDevicePage(accessID, 'control-wrong-wire');
  const wrongWire = await scanControl(page, tickets[fixture.singles[2]].credential, 'single_accept', 'wrong_wire_decision', async (target) => {
    await target.route('**/api/access/scans', async (route) => route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ decision: 'rejected', reason: 'already_redeemed', original_scan_at: new Date().toISOString() }) }));
  }, PROFILE.online_classes.single_duplicate.body_prefix);
  controls.push({ name: 'wrong_wire_decision', observed_status: wrongWire.status, observed_decision: wrongWire.decision, rendered_class: wrongWire.rendered_class, rendered_heading: wrongWire.rendered_heading, rendered_body_prefix: wrongWire.rendered_body_prefix, expected_rejected: true, detected: wrongWire.status === 409 && wrongWire.decision === 'rejected' && wrongWire.rendered_class === 'rejected' && wrongWire.rendered_heading === 'Rejected' && wrongWire.rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix });
  await page.unrouteAll({ behavior: 'wait' });

  async function createTamperedRenderPage(alias) {
    const target = await createDevicePage(accessID, alias);
    await target.addInitScript(() => {
      const observer = new MutationObserver(() => {
        const active = window.__tkt492?.active;
        const result = document.querySelector('main.scanner .result');
        if (!active || !result || active.className !== 'single_accept') return;
        if (active.fixtureAlias === 'wrong_render_heading' && result.querySelector('h2')?.textContent?.trim() === 'Accepted') result.querySelector('h2').textContent = 'Wrong result';
        if (active.fixtureAlias === 'wrong_render_class' && result.classList.contains('accepted')) {
          result.classList.remove('accepted');
          result.classList.add('rejected');
        }
      });
      window.__tkt492TamperObserver = observer;
      observer.observe(document, { subtree: true, childList: true, characterData: true });
    });
    await target.reload({ waitUntil: 'domcontentloaded' });
    return target;
  }
  page = await createTamperedRenderPage('control-wrong-heading');
  const wrongHeading = await scanControl(page, tickets[fixture.singles[3]].credential, 'single_accept', 'wrong_render_heading');
  const headingContext = page;
  page = await createTamperedRenderPage('control-wrong-class');
  const wrongClass = await scanControl(page, tickets[fixture.singles[6]].credential, 'single_accept', 'wrong_render_class');
  const expectedAcceptedBody = PROFILE.online_classes.single_accept.body_prefix;
  const wrongHeadingDetected = wrongHeading.status === 200 && wrongHeading.decision === 'accepted' && wrongHeading.rendered_class === 'accepted' && wrongHeading.rendered_heading !== 'Accepted' && wrongHeading.rendered_body_prefix === expectedAcceptedBody;
  const wrongClassDetected = wrongClass.status === 200 && wrongClass.decision === 'accepted' && wrongClass.rendered_class !== 'accepted' && wrongClass.rendered_heading === 'Accepted' && wrongClass.rendered_body_prefix === expectedAcceptedBody;
  controls.push({ name: 'correct_wire_wrong_render', subcases: [
    { name: 'wrong_heading', status: wrongHeading.status, decision: wrongHeading.decision, rendered_class: wrongHeading.rendered_class, rendered_heading: wrongHeading.rendered_heading, rendered_body_prefix: wrongHeading.rendered_body_prefix, detected: wrongHeadingDetected },
    { name: 'wrong_class', status: wrongClass.status, decision: wrongClass.decision, rendered_class: wrongClass.rendered_class, rendered_heading: wrongClass.rendered_heading, rendered_body_prefix: wrongClass.rendered_body_prefix, detected: wrongClassDetected },
  ], detected: wrongHeadingDetected && wrongClassDetected });
  await page.unrouteAll({ behavior: 'wait' });
  await page.evaluate(() => { window.__tkt492TamperObserver?.disconnect(); delete window.__tkt492TamperObserver; });
  await headingContext.evaluate(() => { window.__tkt492TamperObserver?.disconnect(); delete window.__tkt492TamperObserver; });

  page = await createDevicePage(accessID, 'control-identical-refusals');
  const repeatedCredential = tickets[fixture.singles[4]].credential;
  await scanControl(page, repeatedCredential, 'single_accept', 'identical-refusal-seed');
  const firstRefusal = await scanControl(page, repeatedCredential, 'single_duplicate', 'identical-refusal-1');
  const secondRefusal = await scanControl(page, repeatedCredential, 'single_duplicate', 'identical-refusal-2');
  controls.push({ name: 'identical_refusals_are_fresh', first_occurrence_id: firstRefusal.occurrence_id, second_occurrence_id: secondRefusal.occurrence_id, first_click_at_ms: firstRefusal.click_at_ms, second_click_at_ms: secondRefusal.click_at_ms, first_rendered_class: firstRefusal.rendered_class, first_rendered_heading: firstRefusal.rendered_heading, first_rendered_body_prefix: firstRefusal.rendered_body_prefix, second_rendered_class: secondRefusal.rendered_class, second_rendered_heading: secondRefusal.rendered_heading, second_rendered_body_prefix: secondRefusal.rendered_body_prefix, same_visible_class: firstRefusal.rendered_class === secondRefusal.rendered_class && firstRefusal.rendered_heading === secondRefusal.rendered_heading && firstRefusal.rendered_body_prefix === secondRefusal.rendered_body_prefix, detected: firstRefusal.occurrence_id !== secondRefusal.occurrence_id && firstRefusal.click_at_ms < secondRefusal.click_at_ms && firstRefusal.rendered_class === 'rejected' && firstRefusal.rendered_heading === 'Rejected' && firstRefusal.rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix && secondRefusal.rendered_class === 'rejected' && secondRefusal.rendered_heading === 'Rejected' && secondRefusal.rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix });

  page = await createDevicePage(accessID, 'control-missed-arrival');
  const missed = await missedArrivalControl(page, tickets[fixture.singles[5]].credential);
  controls.push({ name: 'missed_arrival_invalidates_generator', missed_arrivals: missed, detected: missed === 1 });
  if (controls.length !== CONTROL_NAMES.length || controls.some((entry, index) => entry.name !== CONTROL_NAMES[index] || !entry.detected)) throw new Error('control_suite_failed');
}

async function scanControl(page, credential, className, label, installRoute, renderedBodyPrefix = null) {
  if (installRoute) await installRoute(page);
  await page.locator('#qr-payload').fill(credential);
  await page.evaluate(({ className, label }) => { const at = performance.now() + 200; window.__tkt492Next = { className, fixtureAlias: label, historyLength: 0, scheduledAt: at, wakeAt: at }; }, { className, label });
  await page.waitForTimeout(220);
  await page.getByRole('button', { name: 'Check ticket' }).click();
  try { await page.waitForFunction((name) => window.__tkt492.records.at(-1)?.fixtureAlias === name, label, { timeout: PROFILE.deadlines.scan_ms }); } catch { /* aborted controls do not render a normal decision */ }
  const active = await page.evaluate((expectedAlias) => {
    const row = window.__tkt492.records.findLast((entry) => entry.fixture_alias === expectedAlias);
    const capture = window.__tkt492.active;
    return row?.fixture_alias ? row : capture ? {
      context: window.__tkt492.alias, occurrence_id: capture.occurrenceId ?? null,
      result_generation: capture.resultGeneration ?? null, scan_post_count: capture.requests.length,
      request_started_at_epoch_ms: capture.requests[0]?.startedAtEpochMs ?? null,
      response_at_epoch_ms: capture.requests[0]?.responseAtEpochMs ?? null,
      status: capture.response?.status ?? null, decision: capture.response?.decision ?? null,
      reason: capture.response?.reason ?? null, replay: capture.response?.replay ?? null,
      scanned_at: capture.response?.scanned_at ?? null, original_scan_at: capture.response?.original_scan_at ?? null,
      rendered_heading: document.querySelector('.result h2')?.textContent?.trim() ?? null,
      rendered_class: ['accepted', 'rejected', 'queued'].find((name) => document.querySelector('.result')?.classList.contains(name)) ?? null,
      rendered_body: document.querySelector('.result p')?.textContent?.trim() ?? null,
      click_at_ms: capture.clickAt, dom_at_ms: capture.domAt ?? null, render_at_ms: capture.renderAt ?? null,
      latency_ms: capture.renderAt ? capture.renderAt - capture.clickAt : null,
      error_classification: capture.errorClassification ?? null,
    } : null;
  }, label);
  if (!active) throw new Error('control_observation_missing');
  const local = await occurrenceRecord(page, active.occurrence_id);
  active.local_occurrence_id = local?.occurrence_id ?? null;
  if (!local || local.occurrence_id !== active.occurrence_id || active.scan_post_count !== 1) throw new Error('control_occurrence_correlation_failed');
  if (active.rendered_body) {
    const expected = renderedBodyPrefix ?? (className === 'offline_queued' ? PROFILE.offline.body_prefix : PROFILE.online_classes[className]?.body_prefix);
    active.rendered_body_prefix = expected ? active.rendered_body.slice(0, expected.length) : active.rendered_body;
  }
  if (active.render_at_ms && active.click_at_ms) active.latency_ms = active.render_at_ms - active.click_at_ms;
  return active;
}

async function missedArrivalControl(page, credentialOne) {
  await page.route('**/api/access/scans', async (route) => {
    const response = await route.fetch();
    await new Promise((resolve) => setTimeout(resolve, 1700));
    await route.fulfill({ response });
  });
  await page.locator('#qr-payload').fill(credentialOne);
  const result = await page.evaluate(async () => {
    const due = performance.now() + 200;
    window.__tkt492Next = { className: 'single_accept', fixtureAlias: 'missed-first', historyLength: 0, scheduledAt: due, wakeAt: due };
    await new Promise((resolve) => setTimeout(resolve, Math.max(0, due - performance.now())));
    [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click();
    const secondDue = due + 1000;
    await new Promise((resolve) => setTimeout(resolve, Math.max(0, secondDue - performance.now())));
    if (window.__tkt492.active && !window.__tkt492.active.done) return 1;
    return 0;
  });
  await page.unrouteAll({ behavior: 'wait' });
  return result;
}

async function runShortCorrectness(fixture) {
  const accessID = containerIds.access.id;
  const isolated = [];
  for (let index = 0; index < PROFILE.correctness_sequences; index += 1) isolated.push(await createDevicePage(accessID, `correctness-${index + 1}`));
  const first = await scanControl(isolated[0], tickets[fixture.singles[8]].credential, 'single_accept', 'short-single-accept');
  correctness.sequences.push(sequenceResult('single acceptance', first, 200, 'accepted', 'Accepted'));
  const duplicateCredential = tickets[fixture.singles[9]].credential;
  const seed = await scanControl(isolated[1], duplicateCredential, 'single_accept', 'short-duplicate-seed');
  const duplicate = await scanControl(isolated[1], duplicateCredential, 'single_duplicate', 'short-duplicate-refusal');
  const duplicateSequence = sequenceResult('duplicate refusal', duplicate, 409, 'rejected', 'Rejected', seed.status === 200);
  duplicateSequence.precondition = safeObservation(seed);
  correctness.sequences.push(duplicateSequence);
  const pass = await scanControl(isolated[2], tickets[fixture.pass[1]].credential, 'pass_accept', 'short-pass-entry');
  correctness.sequences.push(sequenceResult('multi-entry pass', pass, 200, 'accepted', 'Accepted'));
  const exit = await scanControl(isolated[3], tickets[fixture.passExit[0]].credential, 'pass_exit_required', 'short-exit-required');
  correctness.sequences.push(sequenceResult('exit-required refusal', exit, 409, 'rejected', 'Rejected'));
  const offlinePage = isolated[4];
  await offlinePage.context().setOffline(true);
  await offlinePage.locator('#qr-payload').fill(tickets[fixture.singles[10]].credential);
  await offlinePage.evaluate(() => { const at = performance.now() + 200; window.__tkt492Next = { className: 'offline_queued', fixtureAlias: 'short-offline', historyLength: 0, scheduledAt: at, wakeAt: performance.now() }; setTimeout(() => { window.__tkt492Next.wakeAt = performance.now(); [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click(); }, 200); });
  await offlinePage.waitForFunction((label) => window.__tkt492.records.at(-1)?.fixtureAlias === label, 'short-offline', { timeout: PROFILE.deadlines.scan_ms });
  const offlineRow = await offlinePage.evaluate(() => window.__tkt492.records.at(-1));
  const offlineState = await occurrenceState(offlinePage, offlineRow.occurrence_id);
  const reconciled = await reconcilePage(offlinePage, 'short-correctness');
  correctness.sequences.push({ name: 'offline queue and reconciliation', status: offlineState === 'QUEUED' && offlineRow.rendered_class === 'queued' && offlineRow.rendered_heading === PROFILE.offline.heading && offlineRow.rendered_body.startsWith(PROFILE.offline.body_prefix) && reconciled.length === 1 && reconciled[0].persisted_state === 'SYNCED' ? 'passed' : 'failed', observed: { occurrence_id: offlineRow.occurrence_id, rendered_class: offlineRow.rendered_class, rendered_heading: offlineRow.rendered_heading, rendered_body_prefix: offlineRow.rendered_body.slice(0, PROFILE.offline.body_prefix.length), queue_state_before_sync: offlineState, reconcile_result: reconciled[0]?.reconcile_result ?? null, persisted_state: reconciled[0]?.persisted_state ?? null } });
  const last = await scanControl(isolated[5], tickets[fixture.singles[11]].credential, 'single_accept', 'short-independent-accept');
  correctness.sequences.push(sequenceResult('second independent acceptance', last, 200, 'accepted', 'Accepted'));
  correctness.status = correctness.sequences.every((row) => row.status === 'passed') ? 'passed' : 'failed';
  correctness.lifecycle_verifier = 'not_run_controls_mode';
  correctness.exact_assertions = false;
}

async function runCorrectness(fixture) {
  const p = pages[0];
  const first = await scanControl(p, tickets[fixture.correctnessSingles[0]].credential, 'single_accept', 'correctness-single-accept');
  correctness.sequences.push(sequenceResult('single acceptance', first, 200, 'accepted', 'Accepted'));
  const duplicateCredential = tickets[fixture.correctnessSingles[1]].credential;
  const duplicateSeed = await scanControl(p, tickets[fixture.correctnessSingles[1]].credential, 'single_accept', 'correctness-duplicate-seed-proof');
  const duplicate = await scanControl(p, duplicateCredential, 'single_duplicate', 'correctness-duplicate-refusal');
  const duplicateSequence = sequenceResult('duplicate refusal', duplicate, 409, 'rejected', 'Rejected', duplicateSeed.status === 200);
  duplicateSequence.precondition = safeObservation(duplicateSeed);
  correctness.sequences.push(duplicateSequence);
  const pass = await scanControl(p, tickets[fixture.correctnessPass[0]].credential, 'pass_accept', 'correctness-pass-entry');
  correctness.sequences.push(sequenceResult('multi-entry pass', pass, 200, 'accepted', 'Accepted'));
  const exit = await scanControl(p, tickets[fixture.passExit[0]].credential, 'pass_exit_required', 'correctness-exit-required');
  correctness.sequences.push(sequenceResult('exit-required refusal', exit, 409, 'rejected', 'Rejected'));
  await p.context().setOffline(true);
  await p.locator('#qr-payload').fill(tickets[fixture.correctnessSingles[2]].credential);
  await p.evaluate(() => { const at = performance.now() + 200; window.__tkt492Next = { className: 'offline_queued', fixtureAlias: 'correctness-offline', historyLength: 0, scheduledAt: at, wakeAt: performance.now() }; setTimeout(() => { window.__tkt492Next.wakeAt = performance.now(); [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click(); }, 200); });
  await p.waitForFunction((label) => window.__tkt492.records.at(-1)?.fixtureAlias === label, 'correctness-offline', { timeout: PROFILE.deadlines.scan_ms });
  const offlineRow = await p.evaluate(() => window.__tkt492.records.at(-1));
  const offlineState = await occurrenceState(p, offlineRow.occurrence_id);
  const reconciled = await reconcilePage(p, 'correctness-offline');
  correctness.sequences.push({ name: 'offline queue and reconciliation', status: offlineState === 'QUEUED' && offlineRow.rendered_class === 'queued' && offlineRow.rendered_heading === PROFILE.offline.heading && offlineRow.rendered_body.startsWith(PROFILE.offline.body_prefix) && reconciled.length === 1 && reconciled[0].persisted_state === 'SYNCED' ? 'passed' : 'failed', observed: { occurrence_id: offlineRow.occurrence_id, rendered_class: offlineRow.rendered_class, rendered_heading: offlineRow.rendered_heading, rendered_body_prefix: offlineRow.rendered_body.slice(0, PROFILE.offline.body_prefix.length), queue_state_before_sync: offlineState, reconcile_result: reconciled[0]?.reconcile_result ?? null, persisted_state: reconciled[0]?.persisted_state ?? null } });
  const independent = await scanControl(pages[1], tickets[fixture.correctnessSingles[3]].credential, 'single_accept', 'correctness-independent-accept');
  correctness.sequences.push(sequenceResult('second independent acceptance', independent, 200, 'accepted', 'Accepted'));
  await runFourContextRace(fixture);
  await assertMeasuredLifecycle(fixture);
  const verifier = execLifecycleVerifier();
  correctness.lifecycle_verifier = verifier.includes('verified') ? 'passed' : 'inconclusive';
  if (correctness.lifecycle_verifier !== 'passed') throw new Error('lifecycle_verifier_inconclusive');
  correctness.exact_assertions = true;
  correctness.status = correctness.sequences.every((row) => row.status === 'passed') && correctness.lifecycle_verifier === 'passed' ? 'passed' : 'failed';
  if (correctness.status !== 'passed') throw new CorrectnessAssertionError('correctness_sequence_mismatch', 'scanner');
}

function sequenceResult(name, result, status, decision, heading, precondition = true) {
  const expectedBody = decision === 'accepted' ? PROFILE.online_classes.single_accept.body_prefix
    : result.reason === 'already_redeemed' ? PROFILE.online_classes.single_duplicate.body_prefix
      : PROFILE.online_classes.pass_exit_required.body_prefix;
  const expectedClass = decision === 'accepted' ? 'accepted' : 'rejected';
  const passed = precondition && result.status === status && result.decision === decision && result.rendered_class === expectedClass && result.rendered_heading === heading && result.rendered_body_prefix === expectedBody;
  return { name, status: passed ? 'passed' : 'failed', observed: safeObservation(result) };
}

function safeObservation(row) {
  return {
    occurrence_id: row.occurrence_id ?? null,
    status: row.status ?? null,
    decision: row.decision ?? null,
    reason: row.reason ?? null,
    rendered_class: row.rendered_class ?? null,
    rendered_heading: row.rendered_heading ?? null,
    rendered_body_prefix: row.rendered_body_prefix ?? (typeof row.rendered_body === 'string' ? row.rendered_body.slice(0, 45) : null),
    replay_notice_present: row.replay_notice_present ?? null,
    scanned_at: row.scanned_at ?? null,
    original_scan_at: row.original_scan_at ?? null,
    error_classification: row.error_classification ?? null,
  };
}

async function runFourContextRace(fixture) {
  const credential = tickets[fixture.raceTicket[0]].credential;
  const releaseAtEpochMs = await commonPageDeadline(1500);
  const labels = ['A', 'B', 'C', 'D'];
  await Promise.all(pages.map((page, index) => page.locator('#qr-payload').fill(credential)));
  await Promise.all(pages.map((page, index) => page.evaluate(async ({ label, releaseAtEpochMs }) => {
    const scheduledAt = releaseAtEpochMs - performance.timeOrigin;
    window.__tkt492Next = { className: 'single_accept', fixtureAlias: `four-context-race-${label}`, historyLength: 0, scheduledAt, wakeAt: null };
    await new Promise((resolve) => setTimeout(resolve, Math.max(0, scheduledAt - performance.now())));
    window.__tkt492Next.wakeAt = performance.now();
    [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Check ticket')?.click();
  }, { label: labels[index], releaseAtEpochMs })));
  const rows = await Promise.all(pages.map(async (page, index) => {
    const label = `four-context-race-${labels[index]}`;
    await page.waitForFunction((expected) => window.__tkt492.records.at(-1)?.fixtureAlias === expected, label, { timeout: PROFILE.deadlines.scan_ms });
    const raw = await page.evaluate(() => window.__tkt492.records.at(-1));
    const local = await occurrenceRecord(page, raw.occurrence_id);
    if (!local || local.occurrence_id !== raw.occurrence_id || raw.scan_post_count !== 1) throw new Error('race_occurrence_correlation_failed');
    const row = normalizeRecord({ ...raw, local_occurrence_id: local.occurrence_id }, 'single_accept', label, 0, false);
    return { ...row, context: labels[index], ticket_id: tickets[fixture.raceTicket[0]].ticketID };
  }));
  const starts = rows.map((row) => row.request_started_at_epoch_ms);
  const responses = rows.map((row) => row.response_at_epoch_ms);
  const intervalsOverlap = starts.length === 4 && starts.every(Number.isFinite) && responses.every(Number.isFinite) && Math.max(...starts) < Math.min(...responses);
  const sharedRelease = rows.every((row) => {
    const clickEpoch = row.scheduled_at_epoch_ms - row.scheduled_at_ms + row.click_at_ms;
    return Math.abs(row.scheduled_at_epoch_ms - releaseAtEpochMs) < 2 && clickEpoch >= releaseAtEpochMs - 2 && clickEpoch <= releaseAtEpochMs + PROFILE.deadlines.schedule_lag_ms_max;
  });
  const accepted = rows.filter((row) => row.status === 200 && row.decision === 'accepted').length;
  const refused = rows.filter((row) => row.status === 409 && row.decision === 'rejected' && row.reason === 'already_redeemed').length;
  correctness.single_entry_race = {
    accepted, duplicate_refusals: refused, shared_release_at_epoch_ms: releaseAtEpochMs,
    intervals_overlap: intervalsOverlap, passed: accepted === 1 && refused === 3 && intervalsOverlap && sharedRelease,
    observations: rows.map((row) => ({ context: row.context, ...safeObservation(row), scheduled_at_epoch_ms: row.scheduled_at_epoch_ms, click_at_epoch_ms: row.scheduled_at_epoch_ms - row.scheduled_at_ms + row.click_at_ms, dom_at_epoch_ms: row.scheduled_at_epoch_ms - row.scheduled_at_ms + row.dom_at_ms, request_started_at_epoch_ms: row.request_started_at_epoch_ms, response_at_epoch_ms: row.response_at_epoch_ms, scan_post_count: row.scan_post_count, local_occurrence_id: row.local_occurrence_id, result_generation: row.result_generation })),
  };
  correctnessAssert(correctness.single_entry_race.passed, 'four_context_race_failed', 'scanner');
  const expected = new Map([[rows.find((row) => row.status === 200).occurrence_id, {
    ticket_id: tickets[fixture.raceTicket[0]].ticketID, event_type: 'redeemed', occurred_at: rows.find((row) => row.status === 200).scanned_at,
  }]]);
  const events = queryLifecycle(rows.map((row) => row.occurrence_id));
  correctnessAssert(events.length === 1 && events[0].id === [...expected.keys()][0] && events[0].ticket_id === tickets[fixture.raceTicket[0]].ticketID && events[0].event_type === 'redeemed' && sameTimestampMicros(events[0].occurred_at, expected.values().next().value.occurred_at), 'four_context_race_lifecycle_mismatch');
  correctness.single_entry_race.event = events[0];
}

async function assertMeasuredLifecycle(fixture) {
  // The migrations were read before these queries: lifecycle_events and
  // lifecycle_event_integrity (0001/0003), occurrence-bound quarantine
  // (0005), pass conflicts/policy (0006), and alarm envelopes (0003).
  const online = sampleRows.filter((row) => !row.class.startsWith('offline_'));
  const offline = sampleRows.filter((row) => row.class.startsWith('offline_'));
  const expectedEvents = new Map();
  const deniedIDs = [];
  const passRefusalIDs = [];
  for (const row of online) {
    if (row.class === 'single_accept' || row.class === 'pass_accept') {
      expectedEvents.set(row.occurrence_id, {
        ticket_id: tickets[row.ticket_alias].ticketID,
        event_type: row.class === 'single_accept' ? 'redeemed' : 'entry',
        occurred_at: row.scanned_at,
      });
    } else {
      deniedIDs.push(row.occurrence_id);
      if (row.class === 'pass_exit_required') passRefusalIDs.push(row.occurrence_id);
    }
  }
  const offlineConflictIDs = [];
  for (const row of offline) {
    const conflict = row.class === 'offline_single_queued' && row.context === 'A';
    if (conflict) offlineConflictIDs.push(row.occurrence_id);
    expectedEvents.set(row.occurrence_id, {
      ticket_id: tickets[row.ticket_alias].ticketID,
      event_type: conflict ? 'duplicate_admit' : row.class === 'offline_single_queued' ? 'redeemed' : 'entry',
      occurred_at: row.device_occurred_at,
    });
  }
  const measuredTicketIDs = [...new Set([...online, ...offline].map((row) => row.ticket_id))];
  const measuredOccurrenceIDs = new Set(expectedEvents.keys());
  const preMeasuredEvents = Object.values(tickets).flatMap((ticket) => ticket.preMeasuredEvents ?? []);
  for (const event of preMeasuredEvents) expectedEvents.set(event.occurrence_id, event);
  const ticketLifecycleEvents = queryLifecycleForTickets(measuredTicketIDs);
  const scanEvents = ticketLifecycleEvents.filter((event) => expectedEvents.has(event.id));
  const remaining = new Map(expectedEvents);
  for (const event of ticketLifecycleEvents) {
    const expected = remaining.get(event.id);
    if (expected) {
      correctnessAssert(event.ticket_id === expected.ticket_id && event.event_type === expected.event_type && sameTimestampMicros(event.occurred_at, expected.occurred_at) && Number.isSafeInteger(event.sequence), 'exact_lifecycle_event_mismatch');
      remaining.delete(event.id);
    } else if (!['issued', 'delivered'].includes(event.event_type)) {
      throw new CorrectnessAssertionError('unexpected_measured_ticket_lifecycle_event');
    }
  }
  correctnessAssert(remaining.size === 0 && ticketLifecycleEvents.length === measuredTicketIDs.length * 2 + expectedEvents.size, 'full_measured_ticket_lifecycle_set_mismatch');
  for (const ticketID of measuredTicketIDs) {
    const rows = ticketLifecycleEvents.filter((event) => event.ticket_id === ticketID).toSorted((a, b) => a.sequence - b.sequence);
    correctnessAssert(rows.length >= 2 && rows[0].event_type === 'issued' && rows[0].sequence === 1 && rows[1].event_type === 'delivered' && rows[1].sequence === 2, 'measured_ticket_initial_lifecycle_mismatch');
  }
  const eventRows = scanEvents.filter((event) => measuredOccurrenceIDs.has(event.id));
  const refusedEvents = queryLifecycle(deniedIDs);
  correctnessAssert(refusedEvents.length === 0, 'online_refusal_appended_lifecycle_event');

  const passConflicts = JSON.parse(sql(`SELECT COALESCE(json_agg(json_build_object('occurrence_id',occurrence_id::text,'rule',rule,'status',status,'version',version) ORDER BY occurrence_id),'[]'::json)::text FROM pass_policy_conflicts WHERE occurrence_id = ANY(${uuidArray(passRefusalIDs)})`));
  correctnessAssert(passConflicts.length === 0, 'live_pass_refusal_changed_conflict_projection');
  const passAlarms = alarmOccurrences(passRefusalIDs, 'platform.access.admission-policy-conflict.alarm');
  correctnessAssert(passAlarms.length === 0, 'live_pass_refusal_raised_policy_alarm');
  const conflictAlarms = alarmOccurrences(offlineConflictIDs, 'platform.access.admission-conflict.alarm');
  assertSameIDs(conflictAlarms, offlineConflictIDs, 'offline_duplicate_alarm_mismatch');
  correctness.assertion_counts = {
    online_accepted: online.filter((row) => row.class === 'single_accept' || row.class === 'pass_accept').length,
    online_refused: deniedIDs.length,
    offline_recorded: offline.length - offlineConflictIDs.length,
    offline_conflicts: offlineConflictIDs.length,
    pass_policy_conflicts: passConflicts.length,
    admission_conflict_alarms: conflictAlarms.length,
  };
  correctness.database_assertions = {
    measured_events: eventRows,
    pre_measured_events: preMeasuredEvents,
    measured_ticket_ids: measuredTicketIDs,
    ticket_lifecycle_events: ticketLifecycleEvents,
    online_refusal_event_ids: refusedEvents.map((row) => row.id),
    live_pass_conflicts: passConflicts,
    live_pass_alarms: passAlarms,
    offline_conflict_alarm_occurrence_ids: conflictAlarms,
  };

  if (offlineConflictIDs.length) {
    const before = conflictAlarms.length;
    const replay = await replayConflictOccurrences(pages[0], offline.filter((row) => offlineConflictIDs.includes(row.occurrence_id)));
    correctnessAssert(replay.length === offlineConflictIDs.length && replay.every((row) => row.result === 'synced'), 'offline_conflict_replay_mismatch');
    const after = alarmOccurrences(offlineConflictIDs, 'platform.access.admission-conflict.alarm');
    correctnessAssert(after.length === before, 'offline_replay_added_alarm');
    correctness.database_assertions.replay_results = replay;
    correctness.database_assertions.offline_alarms_before_replay = before;
    correctness.database_assertions.offline_alarms_after_replay = after.length;
  }
  const policy = JSON.parse(sql(`SELECT COALESCE(json_agg(json_build_object('mode',mode,'requires_exit',requires_exit) ORDER BY slot_id),'[]'::json)::text FROM slot_re_entry_policies WHERE slot_id IN ('${fixture.offers.pass.performanceId}','${fixture.offers.passExit.performanceId}')`));
  correctnessAssert(policy.some((row) => row.mode === 'multi' && row.requires_exit === false) && policy.some((row) => row.mode === 'multi' && row.requires_exit === true), 'pass_policy_projection_mismatch');
}

function uuidArray(ids) {
  for (const id of ids) if (!UUID.test(id)) throw new Error('occurrence_uuid_invalid');
  return `ARRAY[${ids.map((id) => `'${id}'::uuid`).join(',')}]`;
}

function queryLifecycle(ids) {
  if (!ids.length) return [];
  return JSON.parse(sql(`SELECT COALESCE(json_agg(json_build_object('id',e.id::text,'ticket_id',e.ticket_id::text,'event_type',e.event_type,'occurred_at',to_char(e.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'sequence',i.sequence) ORDER BY e.id),'[]'::json)::text FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i ON i.event_id=e.id WHERE e.id = ANY(${uuidArray(ids)})`));
}

function queryLifecycleForTickets(ticketIDs) {
  if (!ticketIDs.length) return [];
  return JSON.parse(sql(`SELECT COALESCE(json_agg(json_build_object('id',e.id::text,'ticket_id',e.ticket_id::text,'event_type',e.event_type,'occurred_at',to_char(e.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'sequence',i.sequence) ORDER BY e.ticket_id,i.sequence,e.id),'[]'::json)::text FROM lifecycle_events e LEFT JOIN lifecycle_event_integrity i ON i.event_id=e.id WHERE e.ticket_id = ANY(${uuidArray(ticketIDs)})`));
}

function alarmOccurrences(ids, subject) {
  if (!ids.length) return [];
  const safeSubject = ['platform.access.admission-policy-conflict.alarm', 'platform.access.admission-conflict.alarm'].includes(subject) ? subject : null;
  if (!safeSubject) throw new Error('alarm_subject_invalid');
  return JSON.parse(sql(`SELECT COALESCE(json_agg((envelope::jsonb #>> '{data,occurrence_id}')::uuid ORDER BY event_id),'[]'::json)::text FROM lifecycle_integrity_alarm_outbox WHERE subject='${safeSubject}' AND (envelope::jsonb #>> '{data,occurrence_id}')::uuid = ANY(${uuidArray(ids)})`));
}

function assertSameIDs(actualValues, expectedValues, errorCode) {
  const actual = actualValues.toSorted();
  const expected = [...expectedValues].toSorted();
  correctnessAssert(JSON.stringify(actual) === JSON.stringify(expected), errorCode);
}

async function requirePassedControls() {
  const controlDir = await latestRunDir('controls');
  const metadata = await readJSONSafe(path.join(controlDir, 'metadata.json'));
  const controlFile = await readJSONSafe(path.join(controlDir, 'controls.json'));
  const checks = controlFile?.controls ?? [];
  const expectedHarness = {
    runner: (await import('node:crypto')).createHash('sha256').update(await readFile(path.join(DIR, 'run.mjs'))).digest('hex'),
    analyzer: (await import('node:crypto')).createHash('sha256').update(await readFile(path.join(DIR, 'analyze.mjs'))).digest('hex'),
  };
  const profileHash = (await import('node:crypto')).createHash('sha256').update(await readFile(path.join(DIR, 'profile.json'))).digest('hex');
  if (metadata?.status !== 'controls_passed' || metadata.profile_sha256 !== profileHash || metadata.source_head !== '036f776f32656cf978b8a397ff51787b7f834de6' || JSON.stringify(metadata.harness_sha256) !== JSON.stringify(expectedHarness) || checks.length !== CONTROL_NAMES.length || checks.some((row, index) => row.name !== CONTROL_NAMES[index] || !controlDetected(row))) throw new Error('required_controls_not_passed');
}

async function latestRunDir(label) {
  const root = path.join(evidenceRoot, label);
  const children = await (await import('node:fs/promises')).readdir(root, { withFileTypes: true }).catch(() => []);
  const candidates = [];
  for (const child of children.filter((entry) => entry.isDirectory())) {
    const directory = path.join(root, child.name);
    const metadata = await readJSONSafe(path.join(directory, 'metadata.json'));
    if (metadata) candidates.push({ directory, at: metadata.completed_at ?? metadata.started_at ?? '' });
  }
  candidates.sort((a, b) => a.at.localeCompare(b.at));
  if (!candidates.length) throw new Error(`missing_${label}_evidence`);
  return candidates.at(-1).directory;
}

function controlDetected(row) {
  switch (row.name) {
    case 'delayed_genuine_response': return row.observed_latency_ms >= 500 && row.observed_status === 200 && row.observed_decision === 'accepted' && row.rendered_class === 'accepted' && row.rendered_heading === 'Accepted' && row.rendered_body_prefix === PROFILE.online_classes.single_accept.body_prefix;
    case 'online_abort_is_not_offline': return row.online === true && row.observed_error === 'aborted_online';
    case 'wrong_wire_decision': return row.observed_status === 409 && row.observed_decision === 'rejected' && row.rendered_class === 'rejected' && row.rendered_heading === 'Rejected' && row.rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix;
    case 'correct_wire_wrong_render': return Array.isArray(row.subcases) && row.subcases.length === 2 && row.subcases[0].name === 'wrong_heading' && row.subcases[1].name === 'wrong_class' && row.subcases.every((item) => item.detected === true && item.status === 200 && item.decision === 'accepted' && item.rendered_body_prefix === PROFILE.online_classes.single_accept.body_prefix);
    case 'identical_refusals_are_fresh': return row.first_occurrence_id !== row.second_occurrence_id && row.same_visible_class === true && row.first_click_at_ms < row.second_click_at_ms && row.first_rendered_class === 'rejected' && row.first_rendered_heading === 'Rejected' && row.first_rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix && row.second_rendered_class === 'rejected' && row.second_rendered_heading === 'Rejected' && row.second_rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix;
    case 'missed_arrival_invalidates_generator': return row.missed_arrivals === 1;
    default: return false;
  }
}

async function replayConflictOccurrences(page, conflictRows) {
  const ids = conflictRows.map((row) => row.occurrence_id);
  return page.evaluate(async (occurrenceIDs) => {
    const token = localStorage.getItem('scanner.device-token');
    const req = indexedDB.open('gate-occurrences');
    const db = await new Promise((resolve, reject) => { req.onsuccess = () => resolve(req.result); req.onerror = () => reject(new Error('idb')); });
    const tx = db.transaction('occurrences', 'readonly');
    const get = tx.objectStore('occurrences').getAll();
    const records = await new Promise((resolve, reject) => { get.onsuccess = () => resolve(get.result); get.onerror = () => reject(new Error('idb')); });
    db.close();
    const occurrences = records.filter((row) => occurrenceIDs.includes(row.occurrenceId)).map((row) => ({ qr_payload: row.qrPayload, occurrence_id: row.occurrenceId, occurred_at: row.occurredAt }));
    const response = await fetch('/api/access/scans/reconciliations', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Scanner-Token': token }, body: JSON.stringify({ occurrences }) });
    const body = await response.json();
    return response.status === 200 && Array.isArray(body.results) ? body.results.map((row) => ({ occurrence_id: row.occurrence_id, result: row.result })) : [];
  }, ids);
}

function classifyRawSamples(rows) {
  const expected = PROFILE.samples_per_attempt.measured_total;
  const expectedCounts = { ...Object.fromEntries(Object.keys(PROFILE.online_classes).map((name) => [name, 400])), offline_single_queued: 200, offline_pass_queued: 200 };
  const counts = rows.reduce((result, row) => { result[row.class] = (result[row.class] ?? 0) + 1; return result; }, {});
  const complete = rows.length === expected && Object.entries(expectedCounts).every(([name, count]) => counts[name] === count);
  const validRows = rows.every((row) => {
    const offline = row.class === 'offline_single_queued' || row.class === 'offline_pass_queued';
    return row.phase === 'measured' && row.visibility_at_click === 'visible' && row.visibility_at_render === 'visible' && Number.isFinite(row.latency_ms) && Number.isFinite(row.schedule_lag_ms) && row.schedule_lag_ms <= PROFILE.deadlines.schedule_lag_ms_max &&
      (offline ? row.error_classification === 'intentional_offline' && row.transport === 'failed_offline' : row.error_classification === null && row.transport === 'response');
  });
  const lagValues = rows.map((row) => row.schedule_lag_ms).toSorted((a, b) => a - b);
  return { valid: complete && validRows && lagValues.length > 0 && lagValues[Math.ceil(0.99 * lagValues.length) - 1] <= PROFILE.deadlines.schedule_lag_p99_ms_max };
}

async function stackHealth(phase) {
  const current = await ownedContainers(project);
  const values = Object.values(current);
  const required = ['postgres', 'nats', 'catalog', 'inventory', 'commerce', 'payments', 'access', 'gateway', 'scanner', 'storefront', 'backoffice'];
  const requiredRunning = required.filter((service) => current[service]?.running).length;
  const oomKills = values.filter((item) => item.oomKilled).length;
  const restartCount = values.reduce((sum, item) => sum + item.restartCount, 0);
  const liveIDs = required.map((service) => current[service]?.id).filter(Boolean);
  const statText = execSafe('docker_resource_sample_failed', ['docker', 'stats', '--no-stream', '--format', '{{.CPUPerc}} {{.MemUsage}}', ...liveIDs], { cwd: ROOT, encoding: 'utf8', timeout: 15000 });
  const stats = statText.trim().split('\n').map((line) => {
    const [cpuText, used, slash, limit, ...rest] = line.trim().split(/\s+/);
    if (!/^\d+(?:\.\d+)?%$/.test(cpuText ?? '') || slash !== '/' || rest.length) throw new Error('docker_resource_sample_invalid');
    const memoryUsedBytes = parseBytes(used);
    const memoryLimitBytes = parseBytes(limit);
    return { cpu_percent: Number(cpuText.slice(0, -1)), memory_used_bytes: memoryUsedBytes, memory_limit_bytes: memoryLimitBytes };
  });
  if (stats.length !== required.length) throw new Error('docker_resource_sample_incomplete');
  return {
    at: new Date().toISOString(), phase, healthy: requiredRunning === required.length && oomKills === 0 && restartCount === 0,
    running_containers: requiredRunning, expected_running_containers: required.length,
    oom_kills: oomKills, restart_count: restartCount,
    cpu_percent_total: Number(stats.reduce((sum, row) => sum + row.cpu_percent, 0).toFixed(3)),
    memory_used_bytes_total: stats.reduce((sum, row) => sum + row.memory_used_bytes, 0),
    memory_limit_bytes_total: stats.reduce((sum, row) => sum + row.memory_limit_bytes, 0),
  };
}

function parseBytes(value) {
  const match = /^(\d+(?:\.\d+)?)(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)$/.exec(value ?? '');
  if (!match) throw new Error('docker_resource_sample_invalid');
  const powers = { B: 1, kB: 1000, MB: 1000 ** 2, GB: 1000 ** 3, TB: 1000 ** 4, KiB: 1024, MiB: 1024 ** 2, GiB: 1024 ** 3, TiB: 1024 ** 4 };
  return Math.round(Number(match[1]) * powers[match[2]]);
}

async function persistRows() {
  await writeFile(path.join(runDir, 'samples.jsonl'), sampleRows.map((row) => JSON.stringify(row)).join('\n') + (sampleRows.length ? '\n' : ''), { mode: 0o600 });
  await writeFile(path.join(runDir, 'warmups.jsonl'), warmupRows.map((row) => JSON.stringify(row)).join('\n') + (warmupRows.length ? '\n' : ''), { mode: 0o600 });
  await writeFile(path.join(runDir, 'health.jsonl'), healthRows.map((row) => JSON.stringify(row)).join('\n') + (healthRows.length ? '\n' : ''), { mode: 0o600 });
}

async function evidenceHashes(directory) {
  const { createHash } = await import('node:crypto');
  const results = {};
  for (const name of ['samples.jsonl', 'warmups.jsonl', 'health.jsonl', 'fixtures.json', 'correctness.json', 'controls.json', 'failure.json']) {
    const bytes = await readFile(path.join(directory, name)).catch(() => null);
    if (bytes) results[name] = createHash('sha256').update(bytes).digest('hex');
  }
  return results;
}

async function writeJSON(file, data) {
  await writeFile(file, `${JSON.stringify(data, null, 2)}\n`, { mode: 0o600 });
}

async function readJSONSafe(file) {
  try { return JSON.parse(await readFile(file, 'utf8')); } catch { return null; }
}

function safeErrorLabel(error) {
  if (requestedSignal) return 'interrupted';
  const message = error instanceof Error ? error.message : 'unknown';
  return /^[a-z0-9_-]+(?:_[a-z0-9_-]+)*$/i.test(message) ? message : 'setup_or_run_failed';
}

function safeErrorDiagnostics(error) {
  const errorType = error instanceof Error && SAFE_ERROR_TYPES.has(error.name) ? error.name : null;
  const stack = error instanceof Error && typeof error.stack === 'string' ? error.stack : '';
  const match = /^\s*at (?:async )?file:\/\/\/[A-Za-z0-9_./-]+\/run\.mjs:(\d+):(\d+)\s*$/m.exec(stack);
  return {
    error_type: errorType,
    error_location: match ? `run.mjs:${match[1]}:${match[2]}` : null,
  };
}

function safeFixtureSummary(fixture) {
  const entries = Object.keys(tickets);
  return {
    alias_count: entries.length,
    class_counts: entries.reduce((result, alias) => { const key = fixtureAliases[alias]; result[key] = (result[key] ?? 0) + 1; return result; }, {}),
    offer_classes: Object.fromEntries(Object.entries(fixture.offers).map(([name, offer]) => [name, { offer_alias: name, policy: offer.mode }])),
    credentials_written: false,
    guest_references_written: false,
  };
}

async function validateControlEvidence() {
  if (controls.length !== CONTROL_NAMES.length || controls.some((row, i) => row.name !== CONTROL_NAMES[i] || row.detected !== true)) throw new Error('control_suite_failed');
}

async function pairPage(page, token) {
  await page.locator('#pairing-token').fill(token);
  await page.locator('button[type="submit"]').click();
  await page.locator('#qr-payload').waitFor({ state: 'visible', timeout: 15000 });
}
