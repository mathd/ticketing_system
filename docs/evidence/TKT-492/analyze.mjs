#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const DIR = path.dirname(fileURLToPath(import.meta.url));
const PROFILE = JSON.parse(await readFile(path.join(DIR, 'profile.json'), 'utf8'));
const SAMPLE_KEYS = new Set([
  'attempt', 'phase', 'context', 'fixture_alias', 'occurrence_id', 'class', 'ticket_id',
  'history_length', 'scheduled_at_ms', 'scheduled_at_epoch_ms', 'wake_at_ms', 'click_at_ms', 'dom_at_ms',
  'render_at_ms', 'latency_ms', 'schedule_lag_ms', 'status', 'decision', 'reason',
  'replay', 'rendered_class', 'rendered_heading', 'rendered_body_prefix', 'offline_control',
  'local_occurrence_id', 'result_generation', 'scan_post_count', 'request_started_at_epoch_ms', 'response_at_epoch_ms', 'history_observed_at_epoch_ms',
  'transport', 'queue_state', 'error_classification', 'expected',
  'reconcile_result', 'occurred_at', 'device_occurred_at', 'persisted_state', 'persisted_result',
  'scanned_at', 'original_scan_at', 'ticket_alias', 'visibility_at_click', 'visibility_at_render', 'replay_notice_present',
]);
const SAFE_ID = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,100}$/;

class DatabaseCorrectnessError extends Error {
  constructor(code) {
    super(code);
    this.name = 'DatabaseCorrectnessError';
    this.code = code;
  }
}

function databaseAssert(condition, code) {
  if (!condition) throw new DatabaseCorrectnessError(code);
}

const args = process.argv.slice(2);
const allowedArgs = new Set(['--all-attempts', '--attempt', '--self-check']);
if (args.length === 1 && args[0] === '--self-check') {
  runSelfChecks();
  console.log('analyzer synthetic checks passed');
  process.exit(0);
}
if (args.some((arg) => !allowedArgs.has(arg) && !/^\d{2}$/.test(arg))) usage();
const all = args.includes('--all-attempts');
const attemptFlag = args.indexOf('--attempt');
const attempt = attemptFlag < 0 ? null : args[attemptFlag + 1];
if (attemptFlag >= 0 && (!attempt || !/^\d{2}$/.test(attempt))) usage();
if (all === !!attempt || (all && args.length !== 1) || (attempt && args.length !== 2)) usage();

function usage() {
  console.error('usage: node analyze.mjs --all-attempts | --attempt 01');
  process.exit(2);
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function parseJsonl(source, label) {
  const lines = source.split('\n').filter((line) => line.trim() !== '');
  return lines.map((line, index) => {
    try { return JSON.parse(line); } catch { throw new Error(`${label}:${index + 1} is not JSON`); }
  });
}

async function readJson(file) {
  try { return JSON.parse(await readFile(file, 'utf8')); }
  catch { throw new Error(`${path.basename(file)} is missing or invalid JSON`); }
}

function quantile(sorted, q) {
  if (!sorted.length) return null;
  return sorted[Math.ceil(q * sorted.length) - 1];
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
  return BigInt(date.getTime() / 1000 - offsetMinutes * 60) * 1000000n + roundedMicros;
}

function sameTimestampMicros(left, right) {
  try { return timestampMicros(left) === timestampMicros(right); }
  catch { return false; }
}

function validTimestamp(value) {
  try { timestampMicros(value); return true; }
  catch { return false; }
}

function exactPrefix(actual, expected) { return actual === expected; }

function requestFitsClickResult(click, start, response, dom) {
  return [click, start, response, dom].every(Number.isFinite) && click <= start && start <= response && response <= dom;
}

function scheduleLagMatches(scheduled, wake, reportedLag) {
  return [scheduled, wake, reportedLag].every(Number.isFinite) && reportedLag >= 0 &&
    Math.abs(Math.max(0, wake - scheduled) - reportedLag) < 0.01;
}

function exactIdentifierSet(actual, expected) {
  return Array.isArray(actual) && Array.isArray(expected) && actual.length === expected.length && new Set(actual).size === actual.length && [...expected].every((id) => actual.includes(id));
}

function runSelfChecks() {
  assert(exactPrefix('Already redeemed at', 'Already redeemed at'), 'prefix positive check failed');
  assert(!exactPrefix('Already redeemed a', 'Already redeemed at'), 'prefix truncation was not rejected');
  assert(!exactPrefix('Credential is invalid', 'Credential is invalid or cannot be redeemed'), 'prefix mismatch was not rejected');
  assert(requestFitsClickResult(100, 101, 120, 121), 'request interval positive check failed');
  assert(!requestFitsClickResult(100, 99, 120, 121), 'request before click was not rejected');
  assert(!requestFitsClickResult(100, 101, 122, 121), 'response after fresh DOM result was not rejected');
  assert(scheduleLagMatches(10, 9.1, 0), 'early wake should report zero scheduler lag');
  assert(scheduleLagMatches(10, 10, 0), 'on-time wake should report zero scheduler lag');
  assert(scheduleLagMatches(10, 11, 1), 'late wake should report positive scheduler lag');
  assert(!scheduleLagMatches(10, 9.1, -0.9), 'negative reported scheduler lag was accepted');
  assert(!scheduleLagMatches(10, 11, 0), 'incorrect reported scheduler lag was accepted');
  assert(!scheduleLagMatches(10, Number.NaN, 0), 'non-finite wake timestamp was accepted');
  assert(!scheduleLagMatches(10, 11, Number.POSITIVE_INFINITY), 'non-finite reported scheduler lag was accepted');
  assert(sameTimestampMicros('2026-09-27T12:00:00.123456Z', '2026-09-27T14:00:00.123456+02:00'), 'equivalent offset timestamp was rejected');
  assert(!sameTimestampMicros('2026-09-27T12:00:00.123456Z', '2026-09-27T12:00:00.123457Z'), 'microsecond timestamp difference was missed');
  assert(sameTimestampMicros('2026-09-27T12:00:00.123457Z', '2026-09-27T12:00:00.123456789Z'), 'database microsecond rounding was not preserved');
  assert(exactIdentifierSet(['a', 'b'], ['b', 'a']), 'set order should not matter');
  assert(!exactIdentifierSet(['a', 'b', 'unexpected'], ['a', 'b']), 'unexpected lifecycle event was not rejected');
  assert(!exactIdentifierSet(['a'], ['a', 'b']), 'missing lifecycle event was not rejected');
  const expected = { status: 200, decision: 'accepted', reason: null, rendered_class: 'accepted', rendered_heading: 'Accepted', rendered_body_prefix: 'Entry recorded at' };
  const sample = (overrides = {}) => ({ ...expected, class: 'single_accept', latency_ms: 100, schedule_lag_ms: 5, transport: 'response', error_classification: null, visibility_at_click: 'visible', visibility_at_render: 'visible', ...overrides });
  const expectedCounts = { single_accept: 1 };
  assert(classifyOutcome({ rows: [sample()], plannedCount: 1, expectedCounts, completeEvidence: true }).outcome === 'profile_meets_target', 'healthy fast classification failed');
  const slow = classifyOutcome({ rows: [sample({ latency_ms: 500 })], plannedCount: 1, expectedCounts, completeEvidence: true });
  assert(slow.generator === 'valid' && slow.correctness === 'pass' && slow.latency === 'fail' && slow.outcome === 'latency_miss', 'healthy slow classification failed');
  const invalid = classifyOutcome({ rows: [sample({ schedule_lag_ms: 101 })], plannedCount: 1, expectedCounts, completeEvidence: true });
  assert(invalid.generator === 'invalid' && invalid.latency === 'inconclusive' && invalid.outcome === 'inconclusive_generator', 'invalid generator classification failed');
  const unknown = classifyOutcome({ rows: [sample({ transport: 'unknown', error_classification: 'unknown_transport' })], plannedCount: 1, expectedCounts, completeEvidence: true });
  assert(unknown.generator === 'invalid' && unknown.outcome === 'inconclusive_generator', 'unknown transport classification failed');
  const wrong = classifyOutcome({ rows: [sample({ status: 409, decision: 'rejected', rendered_class: 'rejected', rendered_heading: 'Rejected', rendered_body_prefix: 'Already redeemed at', schedule_lag_ms: 101 })], plannedCount: 1, expectedCounts, completeEvidence: false });
  assert(wrong.generator === 'invalid' && wrong.correctness === 'fail' && wrong.outcome === 'correctness_failure', 'delivered wrong result was hidden by invalid generator');
  const runnerFailure = { outcome: 'correctness_failure', source: 'runner_assertion', category: 'database', code: 'full_measured_ticket_lifecycle_set_mismatch' };
  const invalidWithDatabaseFailure = classifyOutcome({ rows: [sample({ schedule_lag_ms: 101 })], plannedCount: 2, expectedCounts, completeEvidence: false, correctnessFailures: [runnerFailure] });
  assert(invalidWithDatabaseFailure.generator === 'invalid' && invalidWithDatabaseFailure.correctness === 'fail' && invalidWithDatabaseFailure.outcome === 'correctness_failure', 'runner database failure was hidden by invalid generator and partial samples');
  const partialDatabaseFailure = classifyOutcome({ rows: [sample()], plannedCount: 2, expectedCounts, completeEvidence: false, correctnessFailures: [{ source: 'analyzer', category: 'database', code: 'measured_lifecycle_event_mismatch' }] });
  assert(partialDatabaseFailure.generator === 'incomplete' && partialDatabaseFailure.correctness === 'fail' && partialDatabaseFailure.outcome === 'correctness_failure', 'analyzer database failure was hidden by partial evidence');
  const partialOffline = { class: 'offline_single_queued', context: 'A', occurrence_id: 'offline-a', reconcile_result: 'recorded', persisted_state: 'SYNCED', persisted_result: 'recorded' };
  let partialViolation;
  try { verifyPartialDatabaseEvidence([partialOffline], {}); }
  catch (error) { if (error instanceof DatabaseCorrectnessError) partialViolation = { source: 'analyzer', category: 'database', code: error.code }; }
  assert(partialViolation?.code === 'offline_reconcile_result_mismatch', 'partial reconciliation evidence violation was not detected');
  const partialAnalyzerOutcome = classifyOutcome({ rows: [sample({ schedule_lag_ms: 101 })], plannedCount: 2, expectedCounts, completeEvidence: false, correctnessFailures: [partialViolation] });
  assert(partialAnalyzerOutcome.correctness === 'fail' && partialAnalyzerOutcome.outcome === 'correctness_failure', 'partial analyzer-detected reconciliation failure was hidden');
  verifyPartialDatabaseEvidence([{ class: 'offline_single_queued', context: 'A', occurrence_id: 'offline-a' }], {});
  let partialLifecycleViolation;
  try {
    verifyPartialDatabaseEvidence(
      [{ class: 'single_accept', occurrence_id: 'accept-a', ticket_id: 'ticket-a', scanned_at: '2026-09-27T12:00:00Z' }],
      { database_assertions: { measured_events: [{ id: 'accept-a', ticket_id: 'ticket-a', event_type: 'entry', sequence: 4, occurred_at: '2026-09-27T12:00:00Z' }] } },
    );
  } catch (error) { if (error instanceof DatabaseCorrectnessError) partialLifecycleViolation = error.code; }
  assert(partialLifecycleViolation === 'measured_lifecycle_event_mismatch', 'partial lifecycle evidence violation was not detected');
  let detectedDatabaseFailure;
  try { databaseAssert(false, 'measured_lifecycle_event_mismatch'); }
  catch (error) { if (error instanceof DatabaseCorrectnessError) detectedDatabaseFailure = { source: 'analyzer', category: 'database', code: error.code }; }
  assert(detectedDatabaseFailure?.code === 'measured_lifecycle_event_mismatch', 'database assertion did not produce a structured analyzer failure');
  const analyzerDetected = classifyOutcome({ rows: [sample({ schedule_lag_ms: 101 })], plannedCount: 2, expectedCounts, completeEvidence: false, correctnessFailures: [detectedDatabaseFailure] });
  assert(analyzerDetected.correctness === 'fail' && analyzerDetected.outcome === 'correctness_failure', 'analyzer-detected database failure was not retained');
  const runnerEvidence = runnerCorrectnessFailures(
    { failures: [{ source: 'runner', category: 'database', code: 'full_measured_ticket_lifecycle_set_mismatch' }] },
    { classification: runnerFailure },
  );
  assert(runnerEvidence.length === 1 && runnerEvidence[0].category === 'database', 'runner database assertion evidence was not recognized');
  assert(runnerCorrectnessFailures({ failures: [] }, { classification: { outcome: 'correctness_failure', source: 'runner_error' } }).length === 0, 'unclassified runner error was treated as a correctness assertion');
  const incomplete = classifyOutcome({ rows: [sample()], plannedCount: 2, expectedCounts, completeEvidence: false, correctnessFailures: [] });
  assert(incomplete.correctness === 'inconclusive' && incomplete.outcome === 'inconclusive_generator', 'missing correctness evidence was reported as a failure');
  const unfinished = classifyOutcome({ rows: [], plannedCount: 2, expectedCounts, completeEvidence: false, correctnessFailures: [] });
  assert(unfinished.correctness === 'inconclusive' && unfinished.outcome === 'inconclusive_generator', 'unfinished work was reported as a correctness failure');
}

function classifyOutcome({ rows, plannedCount, expectedCounts = null, completeEvidence, correctnessFailures = [] }) {
  const deliveredWrong = rows.some((row) => {
    const expected = PROFILE.online_classes[row.class];
    if (expected) return row.transport === 'response' && row.status !== null && (row.status !== expected.status || row.decision !== expected.decision || row.reason !== expected.reason || row.rendered_class !== (expected.decision === 'accepted' ? 'accepted' : 'rejected') || row.rendered_heading !== expected.heading || !exactPrefix(row.rendered_body_prefix, expected.body_prefix));
    if (row.class === 'offline_single_queued' || row.class === 'offline_pass_queued') return row.status !== null || row.decision !== null || row.rendered_class !== 'queued' || row.rendered_heading !== PROFILE.offline.heading || !exactPrefix(row.rendered_body_prefix, PROFILE.offline.body_prefix);
    return false;
  });
  const lag = rows.map((row) => row.schedule_lag_ms).filter(Number.isFinite).toSorted((a, b) => a - b);
  const lastClick = new Map();
  let overlaps = false;
  for (const row of rows) {
    const prior = lastClick.get(row.context);
    if (prior !== undefined && Number.isFinite(row.click_at_ms) && row.click_at_ms < prior) overlaps = true;
    if (row.context && Number.isFinite(row.render_at_ms)) lastClick.set(row.context, row.render_at_ms);
  }
  const invalidRow = rows.some((row) => !Number.isFinite(row.latency_ms) || !Number.isFinite(row.schedule_lag_ms) || row.schedule_lag_ms > PROFILE.deadlines.schedule_lag_ms_max || row.visibility_at_click !== 'visible' || row.visibility_at_render !== 'visible' || row.error_classification === 'unknown_transport' || row.error_classification === 'aborted_online' || row.transport === 'unknown');
  const counts = rows.reduce((result, row) => { result[row.class] = (result[row.class] ?? 0) + 1; return result; }, {});
  const countsMatch = !expectedCounts || Object.keys(expectedCounts).length === Object.keys(counts).length && Object.entries(expectedCounts).every(([name, count]) => counts[name] === count);
  const generatorFault = overlaps || invalidRow || !countsMatch || lag.length > 0 && lag[Math.ceil(0.99 * lag.length) - 1] > PROFILE.deadlines.schedule_lag_p99_ms_max;
  const enoughRows = rows.length === plannedCount;
  const generator = generatorFault ? 'invalid' : enoughRows ? 'valid' : 'incomplete';
  const correctness = deliveredWrong || correctnessFailures.length > 0 ? 'fail' : completeEvidence && enoughRows ? 'pass' : 'inconclusive';
  const latency = generator === 'valid' && correctness === 'pass'
    ? rows.some((row) => row.latency_ms >= PROFILE.target_ms_exclusive) ? 'fail' : 'pass'
    : 'inconclusive';
  const outcome = correctness === 'fail' ? 'correctness_failure'
    : generator !== 'valid' ? 'inconclusive_generator'
      : latency === 'fail' ? 'latency_miss'
        : latency === 'pass' ? 'profile_meets_target' : 'incomplete';
  return { generator, correctness, latency, outcome };
}

function runnerCorrectnessFailures(correctness, failure) {
  if (!Array.isArray(correctness?.failures) || failure?.classification?.outcome !== 'correctness_failure' || failure.classification.source !== 'runner_assertion') return [];
  const classified = failure.classification;
  if (!['database', 'scanner'].includes(classified.category) || !SAFE_ID.test(classified.code ?? '')) return [];
  const recorded = correctness.failures.some((item) => item?.source === 'runner' && item.category === classified.category && item.code === classified.code);
  return recorded ? [{ source: 'runner', category: classified.category, code: classified.code }] : [];
}

async function trustedEvidenceFiles(dir, metadata, names) {
  if (!metadata?.evidence_sha256) return false;
  for (const name of names) {
    const expected = metadata.evidence_sha256[name];
    if (typeof expected !== 'string') return false;
    const bytes = await readFile(path.join(dir, name)).catch(() => null);
    if (!bytes || createHash('sha256').update(bytes).digest('hex') !== expected) return false;
  }
  return true;
}

async function summarizeAttempt(id, records, dir) {
  const metadata = await readJson(path.join(dir, 'metadata.json')).catch(() => null);
  const correctnessEvidence = await readJson(path.join(dir, 'correctness.json')).catch(() => null);
  const failureEvidence = await readJson(path.join(dir, 'failure.json')).catch(() => null);
  let summary = null;
  let diagnostic = null;
  try { summary = await summarizeCompleteAttempt(id, records, dir); }
  catch (error) { diagnostic = error instanceof Error ? error.message : 'analysis_failed'; }
  const correctnessFailures = await trustedEvidenceFiles(dir, metadata, ['correctness.json', 'failure.json'])
    ? runnerCorrectnessFailures(correctnessEvidence, failureEvidence)
    : [];
  if (correctnessEvidence && await trustedEvidenceFiles(dir, metadata, ['samples.jsonl', 'correctness.json'])) {
    try {
      verifyPartialDatabaseEvidence(records, correctnessEvidence);
      if (records.length === PROFILE.samples_per_attempt.measured_total && correctnessEvidence.database_assertions) verifyDatabaseEvidence(id, records, correctnessEvidence);
    }
    catch (error) {
      if (error instanceof DatabaseCorrectnessError) correctnessFailures.push({ source: 'analyzer', category: 'database', code: error.code });
    }
  }
  const expectedCounts = { ...Object.fromEntries(Object.keys(PROFILE.online_classes).map((name) => [name, PROFILE.samples_per_attempt.measured_online_per_class_per_context * PROFILE.browser.contexts])), offline_single_queued: PROFILE.samples_per_attempt.measured_offline_total / 2, offline_pass_queued: PROFILE.samples_per_attempt.measured_offline_total / 2 };
  const classification = classifyOutcome({ rows: records, plannedCount: PROFILE.samples_per_attempt.measured_total, expectedCounts, completeEvidence: summary !== null, correctnessFailures });
  const latencies = records.map((row) => row.latency_ms).filter(Number.isFinite).toSorted((a, b) => a - b);
  const lags = records.map((row) => row.schedule_lag_ms).filter(Number.isFinite).toSorted((a, b) => a - b);
  const counts = records.reduce((result, row) => { result[row.class] = (result[row.class] ?? 0) + 1; return result; }, {});
  const evidenceHashes = {};
  for (const name of ['samples.jsonl', 'warmups.jsonl', 'health.jsonl', 'fixtures.json', 'correctness.json', 'failure.json']) {
    try { evidenceHashes[name] = createHash('sha256').update(await readFile(path.join(dir, name))).digest('hex'); } catch { /* preserve hashes for files that exist */ }
  }
  if (summary) return {
    ...summary,
    run_status: metadata?.status ?? 'missing_metadata',
    result: classification.outcome,
    classification,
    correctness_failures: correctnessFailures,
  };
  return {
    attempt: id,
    run_status: metadata?.status ?? 'missing_metadata',
    result: classification.outcome,
    classification,
    correctness_failures: correctnessFailures,
    diagnostic,
    counts,
    latency_ms: { count: latencies.length, p50: quantile(latencies, 0.5), p95: quantile(latencies, 0.95), p99: quantile(latencies, 0.99), maximum: latencies.at(-1) ?? null, violations_at_or_above_500: latencies.filter((value) => value >= PROFILE.target_ms_exclusive).length },
    scheduler_lag_ms: { count: lags.length, p50: quantile(lags, 0.5), p95: quantile(lags, 0.95), p99: quantile(lags, 0.99), maximum: lags.at(-1) ?? null },
    validity: { missed_arrivals: records.filter((row) => row.schedule_lag_ms > PROFILE.deadlines.schedule_lag_ms_max).length, overlap_count: null, unknown_transport_failures: records.filter((row) => ['unknown_transport', 'aborted_online'].includes(row.error_classification)).length },
    evidence_hashes: evidenceHashes,
  };
}

async function summarizeCompleteAttempt(id, records, dir) {
  const metadata = await readJson(path.join(dir, 'metadata.json'));
  assert(metadata.ticket === 'TKT-492' && metadata.profile_id === PROFILE.profile_id && metadata.mode === 'baseline' && metadata.attempt === id && metadata.status === 'complete', `attempt ${id}: run metadata is not complete`);
  const profileHash = createHash('sha256').update(await readFile(path.join(DIR, 'profile.json'))).digest('hex');
  assert(metadata.profile_sha256 === profileHash, `attempt ${id}: profile changed after the run`);
  assert(metadata.source_head === '036f776f32656cf978b8a397ff51787b7f834de6', `attempt ${id}: source revision mismatch`);
  await verifyHarnessHashes(metadata, `attempt ${id}`);
  await verifyArtifactHashes(metadata);
  await verifyControls();
  const expectedTotal = PROFILE.samples_per_attempt.measured_total;
  assert(records.length === expectedTotal, `attempt ${id}: expected ${expectedTotal} measured records, got ${records.length}`);
  const counts = Object.create(null);
  const latencies = [];
  const lags = [];
  let violations = 0;
  let missed = 0;
  let overlaps = 0;
  let unknown = 0;
  const seenOccurrences = new Set();
  const lastClickByContext = new Map();
  for (const row of records) {
    assert(row && Object.keys(row).every((key) => SAMPLE_KEYS.has(key)), `attempt ${id}: sample has a non-allowlisted field`);
    assert(row.attempt === id && row.phase === 'measured', `attempt ${id}: unexpected sample attempt or phase`);
    assert(row.visibility_at_click === 'visible' && row.visibility_at_render === 'visible', `attempt ${id}: hidden page invalidates the sample`);
    assert(['A', 'B', 'C', 'D'].includes(row.context), `attempt ${id}: invalid context alias`);
    assert(SAFE_ID.test(row.fixture_alias) && SAFE_ID.test(row.occurrence_id), `attempt ${id}: unsafe fixture or occurrence identifier`);
    assert(row.local_occurrence_id === row.occurrence_id && Number.isSafeInteger(row.result_generation) && row.result_generation > 0 && row.scan_post_count === 1, `attempt ${id}: scan request is not tied to one durable occurrence and fresh result`);
    assert(Number.isFinite(row.request_started_at_epoch_ms) && Number.isFinite(row.response_at_epoch_ms) && row.response_at_epoch_ms >= row.request_started_at_epoch_ms, `attempt ${id}: request interval is incomplete`);
    assert(SAFE_ID.test(row.ticket_alias) && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(row.ticket_id), `attempt ${id}: invalid ticket correlation`);
    assert(Number.isFinite(row.latency_ms) && row.latency_ms >= 0, `attempt ${id}: missing latency`);
    assert(Number.isFinite(row.schedule_lag_ms) && row.schedule_lag_ms >= 0, `attempt ${id}: missing scheduler lag`);
    assert(Number.isFinite(row.scheduled_at_ms) && Number.isFinite(row.wake_at_ms) && Number.isFinite(row.click_at_ms) && Number.isFinite(row.dom_at_ms) && Number.isFinite(row.render_at_ms), `attempt ${id}: missing page timestamps`);
    assert(Number.isFinite(row.scheduled_at_epoch_ms), `attempt ${id}: missing shared-clock schedule timestamp`);
    assert(row.render_at_ms >= row.dom_at_ms && row.dom_at_ms >= row.click_at_ms, `attempt ${id}: timestamp order is invalid`);
    const clickEpoch = row.scheduled_at_epoch_ms - row.scheduled_at_ms + row.click_at_ms;
    const domEpoch = row.scheduled_at_epoch_ms - row.scheduled_at_ms + row.dom_at_ms;
    assert(requestFitsClickResult(clickEpoch, row.request_started_at_epoch_ms, row.response_at_epoch_ms, domEpoch), `attempt ${id}: scan POST interval is not between this click and result`);
    assert(Math.abs((row.render_at_ms - row.click_at_ms) - row.latency_ms) < 0.01, `attempt ${id}: latency does not match page timestamps`);
    assert(scheduleLagMatches(row.scheduled_at_ms, row.wake_at_ms, row.schedule_lag_ms), `attempt ${id}: scheduler lag does not match page timestamps`);
    assert(SAFE_ID.test(row.ticket_alias), `attempt ${id}: invalid ticket alias`);
    assert(!seenOccurrences.has(row.occurrence_id), `attempt ${id}: repeated occurrence identifier`);
    seenOccurrences.add(row.occurrence_id);
    const prior = lastClickByContext.get(row.context);
    if (prior !== undefined && row.click_at_ms < prior) overlaps += 1;
    lastClickByContext.set(row.context, row.render_at_ms);
    if (row.wake_at_ms > row.scheduled_at_ms + PROFILE.deadlines.schedule_lag_ms_max) missed += 1;
    if (row.error_classification === 'unknown_transport' || row.error_classification === 'aborted_online') unknown += 1;
    if (row.latency_ms >= PROFILE.target_ms_exclusive) violations += 1;
    latencies.push(row.latency_ms);
    lags.push(row.schedule_lag_ms);
    counts[row.class] = (counts[row.class] ?? 0) + 1;

    if (row.class === 'offline_single_queued' || row.class === 'offline_pass_queued') {
      assert(row.offline_control === 'enabled' && row.transport === 'failed_offline', `attempt ${id}: offline sample lacks explicit offline control`);
      assert(row.status === null && row.decision === null && row.reason === null, `attempt ${id}: offline sample has a delivered response`);
      assert(row.rendered_class === 'queued' && row.rendered_heading === PROFILE.offline.heading && exactPrefix(row.rendered_body_prefix, PROFILE.offline.body_prefix), `attempt ${id}: wrong offline render`);
      assert(row.queue_state === PROFILE.offline.queue_state && row.error_classification === 'intentional_offline', `attempt ${id}: offline sample was not durably queued`);
      assert(['recorded', 'conflict'].includes(row.reconcile_result), `attempt ${id}: offline result is missing`);
      assert(row.occurred_at && row.device_occurred_at && sameTimestampMicros(row.occurred_at, row.device_occurred_at), `attempt ${id}: offline timestamp changed`);
      if (row.class === 'offline_single_queued' && row.context === 'A') assert(row.reconcile_result === 'conflict', `attempt ${id}: A should reconcile as conflict`);
      else assert(row.reconcile_result === 'recorded', `attempt ${id}: offline sample should reconcile as recorded`);
      assert(row.persisted_state === (row.reconcile_result === 'conflict' ? 'RESOLVED' : 'SYNCED') && row.persisted_result === row.reconcile_result, `attempt ${id}: local state does not match reconciliation`);
    } else {
      const expected = PROFILE.online_classes[row.class];
      assert(expected, `attempt ${id}: unknown class ${row.class}`);
      assert(row.offline_control === 'disabled' && row.transport === 'response', `attempt ${id}: online sample has invalid transport classification`);
    assert(row.status === expected.status && row.decision === expected.decision && row.reason === expected.reason, `attempt ${id}: wire result mismatch for ${row.class}`);
    assert(row.rendered_class === (expected.decision === 'accepted' ? 'accepted' : 'rejected') && row.rendered_heading === expected.heading && exactPrefix(row.rendered_body_prefix, expected.body_prefix), `attempt ${id}: rendered result mismatch for ${row.class}`);
      if (row.class === 'single_accept') assert(row.history_length === 0, `attempt ${id}: fresh single ticket has prior history`);
      if (row.class === 'single_duplicate' || row.class === 'pass_exit_required') assert(row.history_length === 1, `attempt ${id}: refusal fixture history is not exactly one`);
      assert(row.replay === false || row.replay === null, `attempt ${id}: measured occurrence was replayed`);
      if (row.decision === 'accepted') assert(row.replay_notice_present === false, `attempt ${id}: rendered replay notice appeared for a fresh acceptance`);
      if (row.decision === 'accepted') assert(typeof row.scanned_at === 'string' && validTimestamp(row.scanned_at), `attempt ${id}: accepted response has no scan timestamp`);
      if (row.reason === 'already_redeemed') assert(typeof row.original_scan_at === 'string' && validTimestamp(row.original_scan_at), `attempt ${id}: duplicate response has no original timestamp`);
      if (row.class === 'pass_accept') assert(Number.isFinite(row.history_observed_at_epoch_ms) && row.history_observed_at_epoch_ms <= row.scheduled_at_epoch_ms - row.scheduled_at_ms + row.click_at_ms, `attempt ${id}: pass history was not read before its scan`);
      assert(row.error_classification === null, `attempt ${id}: online sample has an error classification`);
    }
  }
  for (const name of Object.keys(PROFILE.online_classes)) {
    const expected = PROFILE.samples_per_attempt.measured_online_per_class_per_context * PROFILE.browser.contexts;
    assert(counts[name] === expected, `attempt ${id}: ${name} count ${counts[name] ?? 0}, expected ${expected}`);
  }
  assert(counts.offline_single_queued === PROFILE.samples_per_attempt.measured_offline_total / 2, `attempt ${id}: offline single count mismatch`);
  assert(counts.offline_pass_queued === PROFILE.samples_per_attempt.measured_offline_total / 2, `attempt ${id}: offline pass count mismatch`);
  await verifyWarmups(id, dir);
  for (const context of ['A', 'B', 'C', 'D']) {
    assert(records.filter((row) => row.context === context && row.class === 'offline_single_queued').length === 50, `attempt ${id}: ${context} offline single count mismatch`);
    assert(records.filter((row) => row.context === context && row.class === 'offline_pass_queued').length === 50, `attempt ${id}: ${context} offline pass count mismatch`);
    for (const name of Object.keys(PROFILE.online_classes)) assert(records.filter((row) => row.context === context && row.class === name).length === 100, `attempt ${id}: ${context} ${name} count mismatch`);
    const contextRows = records.filter((row) => row.context === context);
    assert(contextRows.every((row, index) => index === 0 || row.result_generation > contextRows[index - 1].result_generation), `attempt ${id}: result generation did not advance for each fresh scan`);
    const onlineRows = contextRows.filter((row) => !row.class.startsWith('offline_'));
    const offlineRows = contextRows.filter((row) => row.class.startsWith('offline_'));
    const onlineOrder = Object.keys(PROFILE.online_classes);
    assert(onlineRows.every((row, index) => row.class === onlineOrder[index % onlineOrder.length]), `attempt ${id}: online class order changed for ${context}`);
    assert(offlineRows.every((row, index) => row.class === (index < 50 ? 'offline_single_queued' : 'offline_pass_queued')), `attempt ${id}: offline class order changed for ${context}`);
    for (const rows of [onlineRows, offlineRows]) for (let index = 1; index < rows.length; index += 1) assert(Math.abs(rows[index].scheduled_at_ms - rows[index - 1].scheduled_at_ms - PROFILE.browser.interval_ms) < 1, `attempt ${id}: schedule interval changed for ${context}`);
    const passHistory = onlineRows.filter((row) => row.class === 'pass_accept').map((row) => row.history_length);
    assert(passHistory.every((value, index) => value === index), `attempt ${id}: ${context} pass history did not increase from 0 through 99`);
  }
  const firstOnlineByContext = ['A', 'B', 'C', 'D'].map((context) => records.find((row) => row.context === context && !row.class.startsWith('offline_')));
  const firstOfflineByContext = ['A', 'B', 'C', 'D'].map((context) => records.find((row) => row.context === context && row.class.startsWith('offline_')));
  for (const firstRows of [firstOnlineByContext, firstOfflineByContext]) for (let index = 1; index < firstRows.length; index += 1) assert(Math.abs(firstRows[index].scheduled_at_epoch_ms - firstRows[index - 1].scheduled_at_epoch_ms - PROFILE.browser.stagger_ms) < 2, `attempt ${id}: context start stagger changed`);

  const health = parseJsonl(await readFile(path.join(dir, 'health.jsonl'), 'utf8'), 'health.jsonl');
  assert(health.length >= 8, `attempt ${id}: missing phase health samples`);
  for (const phase of ['setup', 'warmup', 'measured_online', 'after_measured_online', 'measured_offline', 'after_measured_offline', 'correctness']) assert(health.some((row) => row.phase === phase), `attempt ${id}: missing ${phase} health sample`);
  for (const row of health) {
    assert(row && ['setup', 'warmup', 'measured_online', 'after_measured_online', 'measured_offline', 'after_measured_offline', 'correctness'].includes(row.phase), `attempt ${id}: invalid health phase`);
    assert(Number.isFinite(row.running_containers) && row.running_containers === row.expected_running_containers && Number.isFinite(row.expected_running_containers) && row.expected_running_containers === 11, `attempt ${id}: required stack service is down`);
    assert(Number.isFinite(row.oom_kills) && row.oom_kills === 0, `attempt ${id}: OOM evidence missing or nonzero`);
    assert(Number.isFinite(row.restart_count) && row.restart_count === 0, `attempt ${id}: service restart evidence missing or nonzero`);
    assert(row.healthy === (row.running_containers === row.expected_running_containers && row.oom_kills === 0 && row.restart_count === 0), `attempt ${id}: health verdict does not match raw counts`);
    assert(Number.isFinite(row.cpu_percent_total) && row.cpu_percent_total >= 0 && Number.isFinite(row.memory_used_bytes_total) && row.memory_used_bytes_total >= 0 && Number.isFinite(row.memory_limit_bytes_total), `attempt ${id}: Docker resource sample is incomplete`);
  }
  const correctness = await readJson(path.join(dir, 'correctness.json'));
  assert(Array.isArray(metadata.page_faults) && metadata.page_faults.length === 0, `attempt ${id}: page crash, error, or unexpected navigation observed`);
  assert(correctness.status === 'passed' && correctness.sequences?.length === 6, `attempt ${id}: six correctness sequences are incomplete`);
  assert(correctness.sequences.every((entry) => entry.status === 'passed' && typeof entry.name === 'string'), `attempt ${id}: correctness sequence failed`);
  assert(correctness.single_entry_race?.passed === true && correctness.single_entry_race.accepted === 1 && correctness.single_entry_race.duplicate_refusals === 3, `attempt ${id}: four-context single-entry race failed`);
  assert(correctness.lifecycle_verifier === 'passed', `attempt ${id}: lifecycle verifier did not pass`);
  assert(correctness.exact_assertions === true, `attempt ${id}: exact occurrence assertions are incomplete`);
  verifyDatabaseEvidence(id, records, correctness);
  const actualHashes = await hashEvidence(dir);
  assert(Object.keys(metadata.evidence_sha256 ?? {}).length === Object.keys(actualHashes).length, `attempt ${id}: evidence hash list is incomplete`);
  for (const [file, hash] of Object.entries(metadata.evidence_sha256 ?? {})) assert(actualHashes[file] === hash, `attempt ${id}: evidence hash mismatch for ${file}`);

  const sorted = latencies.toSorted((a, b) => a - b);
  const lagSorted = lags.toSorted((a, b) => a - b);
  const validity = missed === 0 && overlaps === 0 && unknown === 0 && violations === 0 &&
    quantile(lagSorted, 0.99) <= PROFILE.deadlines.schedule_lag_p99_ms_max &&
    Math.max(...lagSorted) <= PROFILE.deadlines.schedule_lag_ms_max;
  const result = validity ? 'profile_meets_target' : 'profile_misses_target_or_is_invalid';
  return {
    attempt: id,
    result,
    counts,
    latency_ms: { count: sorted.length, p50: quantile(sorted, 0.50), p95: quantile(sorted, 0.95), p99: quantile(sorted, 0.99), maximum: Math.max(...sorted), violations_at_or_above_500: violations },
    scheduler_lag_ms: { p50: quantile(lagSorted, 0.50), p95: quantile(lagSorted, 0.95), p99: quantile(lagSorted, 0.99), maximum: Math.max(...lagSorted) },
    validity: { missed_arrivals: missed, overlap_count: overlaps, unknown_transport_failures: unknown, latency_violations: violations },
    evidence_hashes: await hashEvidence(dir),
  };
}

async function verifyWarmups(id, dir) {
  const rows = parseJsonl(await readFile(path.join(dir, 'warmups.jsonl'), 'utf8'), 'warmups.jsonl');
  assert(rows.length === 240, `attempt ${id}: warm-up count is incomplete`);
  for (const context of ['A', 'B', 'C', 'D']) {
    const current = rows.filter((row) => row.context === context);
    assert(current.length === 60 && current.every((row) => row.attempt === id && row.phase === 'warmup'), `attempt ${id}: ${context} warm-up records are incomplete`);
    assert(current.every((row, index) => row.local_occurrence_id === row.occurrence_id && row.scan_post_count === 1 && Number.isSafeInteger(row.result_generation) && row.result_generation > 0 && Number.isFinite(row.request_started_at_epoch_ms) && Number.isFinite(row.response_at_epoch_ms) && (index === 0 || row.result_generation > current[index - 1].result_generation)), `attempt ${id}: ${context} warm-up request/result correlation is incomplete`);
    for (const name of Object.keys(PROFILE.online_classes)) {
      const selected = current.filter((row) => row.class === name);
      const expected = PROFILE.online_classes[name];
      assert(selected.length === 10, `attempt ${id}: ${context} ${name} warm-up count mismatch`);
      assert(selected.every((row) => row.status === expected.status && row.decision === expected.decision && row.reason === expected.reason && row.rendered_class === (expected.decision === 'accepted' ? 'accepted' : 'rejected') && row.rendered_heading === expected.heading && row.rendered_body_prefix === expected.body_prefix && row.error_classification === null), `attempt ${id}: ${context} ${name} warm-up result mismatch`);
    }
    for (const name of ['offline_single_queued', 'offline_pass_queued']) {
      const selected = current.filter((row) => row.class === name);
      assert(selected.length === 10 && selected.every((row) => row.offline_control === 'enabled' && row.transport === 'failed_offline' && row.rendered_class === 'queued' && row.rendered_heading === PROFILE.offline.heading && row.rendered_body_prefix === PROFILE.offline.body_prefix && row.queue_state === PROFILE.offline.queue_state && row.error_classification === 'intentional_offline' && row.reconcile_result === 'recorded' && row.persisted_state === 'SYNCED'), `attempt ${id}: ${context} ${name} warm-up result mismatch`);
    }
  }
}

function verifyDatabaseEvidence(id, records, correctness) {
  const evidence = correctness.database_assertions;
  assert(evidence && Array.isArray(evidence.measured_events), `attempt ${id}: database event evidence is missing`);
  const expected = new Map();
  const refused = [];
  const offlineConflicts = [];
  for (const row of records) {
    assert(row && SAFE_ID.test(row.occurrence_id ?? '') && SAFE_ID.test(row.class ?? '') && /^[0-9a-f-]{36}$/i.test(row.ticket_id ?? ''), `attempt ${id}: sample identity evidence is incomplete`);
    assert(['A', 'B', 'C', 'D'].includes(row.context), `attempt ${id}: sample context evidence is invalid`);
    if (row.class === 'single_accept' || row.class === 'pass_accept') {
      expected.set(row.occurrence_id, {
        ticket_id: row.ticket_id,
        event_type: row.class === 'single_accept' ? 'redeemed' : 'entry',
        occurred_at: row.scanned_at,
      });
    } else if (row.class === 'single_duplicate' || row.class === 'pass_exit_required') refused.push(row.occurrence_id);
    else if (row.class === 'offline_single_queued' || row.class === 'offline_pass_queued') {
      const conflict = row.class === 'offline_single_queued' && row.context === 'A';
      const expectedResult = conflict ? 'conflict' : 'recorded';
      const expectedState = conflict ? 'RESOLVED' : 'SYNCED';
      assert(typeof row.reconcile_result === 'string' && typeof row.persisted_state === 'string' && typeof row.persisted_result === 'string', `attempt ${id}: offline reconciliation evidence is missing`);
      databaseAssert(row.reconcile_result === expectedResult, 'offline_reconcile_result_mismatch');
      databaseAssert(row.persisted_state === expectedState && row.persisted_result === expectedResult, 'offline_reconcile_saved_state_mismatch');
      expected.set(row.occurrence_id, {
        ticket_id: row.ticket_id,
        event_type: conflict ? 'duplicate_admit' : row.class === 'offline_single_queued' ? 'redeemed' : 'entry',
        occurred_at: row.device_occurred_at,
      });
      if (conflict) offlineConflicts.push(row.occurrence_id);
    } else assert(false, `attempt ${id}: sample class evidence is invalid`);
  }
  const actual = new Map(evidence.measured_events.map((event) => [event.id, event]));
  databaseAssert(actual.size === expected.size && expected.size === evidence.measured_events.length, 'measured_lifecycle_event_count_mismatch');
  for (const [occurrenceID, fact] of expected) {
    const event = actual.get(occurrenceID);
    databaseAssert(event && event.ticket_id === fact.ticket_id && event.event_type === fact.event_type && Number.isSafeInteger(event.sequence) && event.sequence > 0, 'measured_lifecycle_event_mismatch');
    databaseAssert(sameTimestampMicros(event.occurred_at, fact.occurred_at), 'measured_lifecycle_timestamp_mismatch');
  }
  assert(Array.isArray(evidence.pre_measured_events), `attempt ${id}: setup scan event evidence is missing`);
  const allExpectedScans = new Map(expected);
  for (const event of evidence.pre_measured_events) {
    assert(event && SAFE_ID.test(event.occurrence_id) && /^[0-9a-f-]{36}$/i.test(event.ticket_id) && event.event_type === 'entry' && validTimestamp(event.occurred_at), `attempt ${id}: setup scan event evidence is invalid`);
    databaseAssert(!allExpectedScans.has(event.occurrence_id), 'setup_lifecycle_event_duplicates_measured_occurrence');
    allExpectedScans.set(event.occurrence_id, { ticket_id: event.ticket_id, event_type: event.event_type, occurred_at: event.occurred_at });
  }
  const measuredTicketIDs = [...new Set(records.map((row) => row.ticket_id))];
  databaseAssert(exactIdentifierSet(evidence.measured_ticket_ids, measuredTicketIDs), 'measured_ticket_id_set_mismatch');
  const allEvents = evidence.ticket_lifecycle_events;
  assert(Array.isArray(allEvents), `attempt ${id}: full measured ticket lifecycle evidence is missing`);
  const baseEvents = allEvents.filter((event) => event.event_type === 'issued' || event.event_type === 'delivered');
  databaseAssert(baseEvents.length === measuredTicketIDs.length * 2, 'measured_ticket_issue_delivery_event_count_mismatch');
  for (const ticketID of measuredTicketIDs) {
    const initial = baseEvents.filter((event) => event.ticket_id === ticketID).toSorted((a, b) => a.sequence - b.sequence);
    databaseAssert(initial.length === 2 && initial[0].event_type === 'issued' && initial[0].sequence === 1 && initial[1].event_type === 'delivered' && initial[1].sequence === 2, 'measured_ticket_initial_lifecycle_mismatch');
  }
  databaseAssert(allEvents.every((event) => event.sequence !== null && Number.isSafeInteger(event.sequence)), 'measured_lifecycle_integrity_sequence_missing');
  const allEventIDs = allEvents.map((event) => event.id);
  const expectedAllIDs = [...allExpectedScans.keys(), ...baseEvents.map((event) => event.id)];
  databaseAssert(exactIdentifierSet(allEventIDs, expectedAllIDs), 'full_measured_ticket_lifecycle_set_mismatch');
  for (const [occurrenceID, fact] of allExpectedScans) {
    const event = allEvents.find((row) => row.id === occurrenceID);
    databaseAssert(event && event.ticket_id === fact.ticket_id && event.event_type === fact.event_type && sameTimestampMicros(event.occurred_at, fact.occurred_at), 'full_lifecycle_scan_event_mismatch');
  }
  assert(Array.isArray(evidence.online_refusal_event_ids), `attempt ${id}: refusal lifecycle evidence is missing`);
  databaseAssert(evidence.online_refusal_event_ids.length === 0, 'online_refusal_lifecycle_event_present');
  assert(Array.isArray(evidence.live_pass_conflicts), `attempt ${id}: pass conflict evidence is missing`);
  databaseAssert(evidence.live_pass_conflicts.length === 0, 'live_pass_refusal_conflict_present');
  assert(Array.isArray(evidence.live_pass_alarms), `attempt ${id}: pass alarm evidence is missing`);
  databaseAssert(evidence.live_pass_alarms.length === 0, 'live_pass_refusal_alarm_present');
  assert(Array.isArray(evidence.offline_conflict_alarm_occurrence_ids), `attempt ${id}: offline alarm evidence is missing`);
  databaseAssert(JSON.stringify(evidence.offline_conflict_alarm_occurrence_ids.toSorted()) === JSON.stringify(offlineConflicts.toSorted()), 'offline_conflict_alarm_mapping_mismatch');
  assert(Array.isArray(evidence.replay_results), `attempt ${id}: replay evidence is missing`);
  databaseAssert(evidence.replay_results.length === offlineConflicts.length && evidence.replay_results.every((row) => row.result === 'synced' && offlineConflicts.includes(row.occurrence_id)), 'offline_conflict_replay_result_mismatch');
  assert(Number.isSafeInteger(evidence.offline_alarms_before_replay) && Number.isSafeInteger(evidence.offline_alarms_after_replay), `attempt ${id}: replay alarm evidence is missing`);
  databaseAssert(evidence.offline_alarms_before_replay === offlineConflicts.length && evidence.offline_alarms_after_replay === offlineConflicts.length, 'offline_replay_alarm_count_changed');
  const race = correctness.single_entry_race;
  const accepted = race.observations.filter((row) => row.status === 200 && row.decision === 'accepted');
  const refusedRace = race.observations.filter((row) => row.status === 409 && row.decision === 'rejected' && row.reason === 'already_redeemed');
  const raceStarts = race.observations.map((row) => row.request_started_at_epoch_ms);
  const raceResponses = race.observations.map((row) => row.response_at_epoch_ms);
  assert(race.observations.length === 4 && accepted.length === 1 && refusedRace.length === 3 && race.intervals_overlap === true && Math.max(...raceStarts) < Math.min(...raceResponses), `attempt ${id}: four request intervals do not overlap`);
  assert(race.observations.every((row) => row.scan_post_count === 1 && row.local_occurrence_id === row.occurrence_id && Number.isSafeInteger(row.result_generation) && row.result_generation > 0 && Math.abs(row.scheduled_at_epoch_ms - race.shared_release_at_epoch_ms) < 2 && row.click_at_epoch_ms >= race.shared_release_at_epoch_ms - 2 && row.click_at_epoch_ms <= race.shared_release_at_epoch_ms + PROFILE.deadlines.schedule_lag_ms_max && requestFitsClickResult(row.click_at_epoch_ms, row.request_started_at_epoch_ms, row.response_at_epoch_ms, row.dom_at_epoch_ms)), `attempt ${id}: race requests do not share one release and durable occurrence`);
  databaseAssert(race.event?.id === accepted[0].occurrence_id && race.event.event_type === 'redeemed' && race.event.ticket_id && Number.isSafeInteger(race.event.sequence), 'race_lifecycle_event_mismatch');
  databaseAssert(sameTimestampMicros(race.event.occurred_at, accepted[0].scanned_at), 'race_lifecycle_timestamp_mismatch');
}

function verifyPartialDatabaseEvidence(records, correctness) {
  const evidence = correctness?.database_assertions;
  if (!Array.isArray(records)) return;
  const events = Array.isArray(evidence?.measured_events) ? evidence.measured_events : null;
  const refusals = Array.isArray(evidence?.online_refusal_event_ids) ? evidence.online_refusal_event_ids : null;
  for (const row of records) {
    if (row.class === 'single_accept' || row.class === 'pass_accept') {
      if (events && SAFE_ID.test(row.occurrence_id ?? '')) {
        const matches = events.filter((event) => event?.id === row.occurrence_id);
        if (matches.length > 1) databaseAssert(false, 'measured_lifecycle_event_duplicated');
        const event = matches[0];
        if (event && event.ticket_id && event.event_type && Number.isSafeInteger(event.sequence) && event.occurred_at && row.ticket_id && row.scanned_at) {
          databaseAssert(event.ticket_id === row.ticket_id && event.event_type === (row.class === 'single_accept' ? 'redeemed' : 'entry') && sameTimestampMicros(event.occurred_at, row.scanned_at), 'measured_lifecycle_event_mismatch');
        }
      }
    } else if (row.class === 'single_duplicate' || row.class === 'pass_exit_required') {
      if (refusals?.includes(row.occurrence_id)) databaseAssert(false, 'online_refusal_lifecycle_event_present');
    } else if (row.class === 'offline_single_queued' || row.class === 'offline_pass_queued') {
      const conflict = row.class === 'offline_single_queued' && row.context === 'A';
      const expectedResult = conflict ? 'conflict' : 'recorded';
      const expectedState = conflict ? 'RESOLVED' : 'SYNCED';
      if (typeof row.reconcile_result === 'string' && typeof row.persisted_state === 'string' && typeof row.persisted_result === 'string') {
        databaseAssert(row.reconcile_result === expectedResult && row.persisted_state === expectedState && row.persisted_result === expectedResult, 'offline_reconcile_result_mismatch');
      }
      if (events && SAFE_ID.test(row.occurrence_id ?? '')) {
        const matches = events.filter((event) => event?.id === row.occurrence_id);
        if (matches.length > 1) databaseAssert(false, 'measured_lifecycle_event_duplicated');
        const event = matches[0];
        if (event && event.ticket_id && event.event_type && Number.isSafeInteger(event.sequence) && event.occurred_at && row.ticket_id && row.device_occurred_at) {
          databaseAssert(event.ticket_id === row.ticket_id && event.event_type === (conflict ? 'duplicate_admit' : row.class === 'offline_single_queued' ? 'redeemed' : 'entry') && sameTimestampMicros(event.occurred_at, row.device_occurred_at), 'measured_lifecycle_event_mismatch');
        }
      }
    }
  }
}

async function verifyControls() {
  const dir = await latestRunDir('controls');
  const metadata = await readJson(path.join(dir, 'metadata.json'));
  const data = await readJson(path.join(dir, 'controls.json'));
  const correctness = await readJson(path.join(dir, 'correctness.json'));
  const health = parseJsonl(await readFile(path.join(dir, 'health.jsonl'), 'utf8'), 'controls health');
  assert(health.length >= 3 && health.some((row) => row.phase === 'setup') && health.some((row) => row.phase === 'correctness'), 'controls: phase health evidence is incomplete');
  for (const row of health) {
    assert(row.running_containers === 11 && row.expected_running_containers === 11 && row.oom_kills === 0 && row.restart_count === 0, 'controls: stack health evidence failed');
    assert(row.healthy === true, 'controls: stack health verdict is false');
  }
  assert(metadata.mode === 'controls' && metadata.status === 'controls_passed', 'controls run is absent or incomplete');
  assert(metadata.profile_sha256 === createHash('sha256').update(await readFile(path.join(DIR, 'profile.json'))).digest('hex'), 'controls: profile changed after the run');
  assert(metadata.source_head === '036f776f32656cf978b8a397ff51787b7f834de6', 'controls: source revision mismatch');
  await verifyHarnessHashes(metadata, 'controls');
  await verifyArtifactHashes(metadata);
  const recordedHashes = metadata.evidence_sha256 ?? {};
  const actualHashes = await hashEvidence(dir);
  assert(Object.keys(recordedHashes).length === Object.keys(actualHashes).length, 'controls: evidence hash list is incomplete');
  for (const [file, hash] of Object.entries(recordedHashes)) assert(actualHashes[file] === hash, `controls: evidence hash mismatch for ${file}`);
  const rows = data.controls;
  assert(Array.isArray(rows) && rows.length === PROFILE.controls.length, 'control evidence is incomplete');
  for (let index = 0; index < PROFILE.controls.length; index += 1) {
    const row = rows[index];
    assert(row.name === PROFILE.controls[index], 'control evidence order is invalid');
    switch (row.name) {
      case 'delayed_genuine_response': assert(row.observed_latency_ms >= 500 && row.observed_status === 200 && row.observed_decision === 'accepted' && row.rendered_class === 'accepted' && row.rendered_heading === 'Accepted' && row.rendered_body_prefix === PROFILE.online_classes.single_accept.body_prefix && row.replay_notice_present === false, 'delayed genuine response control did not cross threshold'); break;
      case 'online_abort_is_not_offline': assert(row.online === true && row.observed_error === 'aborted_online', 'online abort was misclassified'); break;
      case 'wrong_wire_decision': assert(row.observed_status === 409 && row.observed_decision === 'rejected' && row.rendered_class === 'rejected' && row.rendered_heading === 'Rejected' && row.rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix, 'wrong wire control was missed'); break;
      case 'correct_wire_wrong_render': assert(Array.isArray(row.subcases) && row.subcases.length === 2, 'wrong render subcases are missing');
        assert(row.subcases[0].name === 'wrong_heading' && row.subcases[0].status === 200 && row.subcases[0].decision === 'accepted' && row.subcases[0].rendered_class === 'accepted' && row.subcases[0].rendered_heading !== 'Accepted' && row.subcases[0].rendered_body_prefix === PROFILE.online_classes.single_accept.body_prefix, 'wrong heading control was missed');
        assert(row.subcases[1].name === 'wrong_class' && row.subcases[1].status === 200 && row.subcases[1].decision === 'accepted' && row.subcases[1].rendered_class !== 'accepted' && row.subcases[1].rendered_heading === 'Accepted' && row.subcases[1].rendered_body_prefix === PROFILE.online_classes.single_accept.body_prefix, 'wrong class control was missed'); break;
      case 'identical_refusals_are_fresh': assert(row.first_occurrence_id !== row.second_occurrence_id && row.same_visible_class === true && row.first_click_at_ms < row.second_click_at_ms && row.first_rendered_class === 'rejected' && row.first_rendered_heading === 'Rejected' && row.first_rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix && row.second_rendered_class === 'rejected' && row.second_rendered_heading === 'Rejected' && row.second_rendered_body_prefix === PROFILE.online_classes.single_duplicate.body_prefix, 'identical refusal control was missed'); break;
      case 'missed_arrival_invalidates_generator': assert(row.missed_arrivals === 1, 'missed arrival control was missed'); break;
      default: throw new Error('unknown control');
    }
  }
  assert(correctness.status === 'passed' && correctness.sequences?.length === PROFILE.correctness_sequences && correctness.sequences.every((row) => row.status === 'passed'), 'six control-mode correctness sequences did not pass');
  verifyCorrectnessSequences('controls', correctness.sequences);
}

async function verifyArtifactHashes(metadata) {
  const artifacts = metadata.artifact_sha256;
  assert(artifacts && typeof artifacts === 'object' && !Array.isArray(artifacts), 'artifact hash list is missing');
  const root = path.resolve(DIR, '../../..');
  const expectedPaths = [
    ...['catalog', 'inventory', 'commerce', 'payments', 'access', 'gateway'].map((name) => `bin/gate/${name}`),
    'web/storefront/dist/server/entry.mjs', 'web/backoffice/dist/server/entry.mjs',
    ...(await recursiveFiles(path.join(root, 'web/scanner/dist'))).map((file) => path.relative(root, file)),
  ].toSorted();
  assert(Object.keys(artifacts).toSorted().join('\n') === expectedPaths.join('\n'), 'artifact hash set is incomplete or changed');
  assert(artifacts['web/scanner/dist/index.html'] && expectedPaths.some((name) => /^web\/scanner\/dist\/.*\.(?:m?js)$/.test(name)), 'scanner JavaScript artifact hash is missing');
  for (const [name, expected] of Object.entries(artifacts)) {
    assert(!name.startsWith('/') && !name.split(path.sep).includes('..'), 'artifact path is invalid');
    const actual = createHash('sha256').update(await readFile(path.join(root, name))).digest('hex');
    assert(actual === expected, `artifact hash mismatch: ${name}`);
  }
}

async function recursiveFiles(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const item = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...await recursiveFiles(item));
    else if (entry.isFile()) files.push(item);
  }
  return files.sort();
}

async function verifyHarnessHashes(metadata, label) {
  const expected = {
    runner: createHash('sha256').update(await readFile(path.join(DIR, 'run.mjs'))).digest('hex'),
    analyzer: createHash('sha256').update(await readFile(path.join(DIR, 'analyze.mjs'))).digest('hex'),
  };
  assert(JSON.stringify(metadata.harness_sha256) === JSON.stringify(expected), `${label}: harness changed after the run`);
}

function verifyCorrectnessSequences(label, sequences) {
  const byName = new Map(sequences.map((row) => [row.name, row]));
  const single = byName.get('single acceptance')?.observed;
  const duplicate = byName.get('duplicate refusal');
  const pass = byName.get('multi-entry pass')?.observed;
  const exit = byName.get('exit-required refusal')?.observed;
  const offline = byName.get('offline queue and reconciliation')?.observed;
  const second = byName.get('second independent acceptance')?.observed;
  const accepted = (row) => row?.status === 200 && row.decision === 'accepted' && row.rendered_class === 'accepted' && row.rendered_heading === 'Accepted' && row.rendered_body_prefix === 'Entry recorded at' && row.replay_notice_present === false;
  assert(accepted(single) && accepted(pass) && accepted(second), `${label}: accepted correctness sequence does not match raw result`);
  assert(duplicate?.observed?.status === 409 && duplicate.observed.decision === 'rejected' && duplicate.observed.reason === 'already_redeemed' && duplicate.observed.rendered_class === 'rejected' && duplicate.observed.rendered_heading === 'Rejected' && duplicate.observed.rendered_body_prefix === 'Already redeemed at', `${label}: duplicate correctness sequence does not match raw result`);
  assert(accepted(duplicate.precondition), `${label}: duplicate sequence did not prove the ticket was first redeemed`);
  assert(exit?.status === 409 && exit.decision === 'rejected' && exit.reason === 'exit_required' && exit.rendered_class === 'rejected' && exit.rendered_heading === 'Rejected' && exit.rendered_body_prefix === 'Credential is invalid or cannot be redeemed', `${label}: exit-required sequence does not match raw result`);
  assert(offline?.rendered_class === 'queued' && offline.rendered_heading === PROFILE.offline.heading && offline.rendered_body_prefix === PROFILE.offline.body_prefix && offline.queue_state_before_sync === 'QUEUED' && offline.reconcile_result === 'recorded' && offline.persisted_state === 'SYNCED', `${label}: offline sequence does not match rendered queue and reconciliation evidence`);
}

async function latestRunDir(label) {
  const root = path.join(DIR, 'runs', label);
  const entries = await readdir(root, { withFileTypes: true }).catch(() => []);
  const candidates = [];
  for (const entry of entries.filter((item) => item.isDirectory())) {
    const directory = path.join(root, entry.name);
    const metadata = await readJson(path.join(directory, 'metadata.json')).catch(() => null);
    if (metadata) candidates.push({ directory, at: metadata.completed_at ?? metadata.started_at ?? '' });
  }
  candidates.sort((a, b) => a.at.localeCompare(b.at));
  assert(candidates.length > 0, `${label}: no run evidence exists`);
  return candidates.at(-1).directory;
}

async function hashEvidence(dir) {
  const files = ['samples.jsonl', 'warmups.jsonl', 'health.jsonl', 'fixtures.json', 'correctness.json'];
  const hashes = {};
  for (const file of files) {
    const content = await readFile(path.join(dir, file));
    hashes[file] = createHash('sha256').update(content).digest('hex');
  }
  const controls = await readFile(path.join(dir, 'controls.json')).catch(() => null);
  if (controls) hashes['controls.json'] = createHash('sha256').update(controls).digest('hex');
  return hashes;
}

function render(report) {
  const lines = ['# TKT-492 analysis', '', `Profile: \`${PROFILE.profile_id}\``, ''];
  for (const item of report.attempts) {
    lines.push(`## Attempt ${item.attempt}`, '', `Result: **${item.result}**`, `Generator: ${item.classification.generator}. Correctness: ${item.classification.correctness}. Latency: ${item.classification.latency}.`, '');
    lines.push(`Measured samples: ${item.latency_ms.count}. Maximum: ${item.latency_ms.maximum ?? 'n/a'} ms. Samples at or above 500 ms: ${item.latency_ms.violations_at_or_above_500}.`);
    lines.push(`Latency p50/p95/p99: ${item.latency_ms.p50}/${item.latency_ms.p95}/${item.latency_ms.p99} ms.`);
    lines.push(`Scheduler lag p99/max: ${item.scheduler_lag_ms.p99}/${item.scheduler_lag_ms.maximum} ms.`);
    lines.push(`Validity: missed=${item.validity.missed_arrivals}, overlap=${item.validity.overlap_count}, unknown transport=${item.validity.unknown_transport_failures}.`, '');
    lines.push('Counts:', '', '```json', JSON.stringify(item.counts, null, 2), '```', '');
    lines.push('SHA-256:', '', '```json', JSON.stringify(item.evidence_hashes, null, 2), '```', '');
  }
  lines.push(`Combined result: **${report.combined}**`, '');
  return lines.join('\n');
}

const ids = all ? ['01', '02'] : [attempt];
const results = [];
for (const id of ids) {
  const dir = await latestRunDir(`baseline-${id}`);
  const sampleText = await readFile(path.join(dir, 'samples.jsonl'), 'utf8').catch(() => '');
  const records = parseJsonl(sampleText, 'samples.jsonl');
  results.push(await summarizeAttempt(id, records, dir));
}
const combined = results.length === 1
  ? results[0].result
  : results.every((r) => r.result === 'profile_meets_target')
    ? 'both_repetitions_meet_target'
    : results.some((r) => r.result === 'correctness_failure')
      ? 'at_least_one_correctness_failure'
      : results.some((r) => r.result === 'latency_miss')
        ? 'at_least_one_latency_miss'
        : results.some((r) => r.result === 'inconclusive_generator')
          ? 'at_least_one_inconclusive_generator'
          : 'incomplete';
const report = { profile_id: PROFILE.profile_id, attempts: results, combined };
const output = render(report);
if (all) {
  await writeFile(path.join(DIR, 'runs', 'analysis.json'), `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 });
  await writeFile(path.join(DIR, 'runs', 'analysis.md'), output, { mode: 0o600 });
} else {
  const dir = await latestRunDir(`baseline-${attempt}`);
  await writeFile(path.join(dir, 'analysis.json'), `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 });
  await writeFile(path.join(dir, 'analysis.md'), output, { mode: 0o600 });
}
console.log(output);
process.exitCode = combined === 'both_repetitions_meet_target' || (!all && results[0].result === 'profile_meets_target') ? 0 : 1;
