// TKT-563 combined verification probe. Run it only through run-probe.sh.
//
// The runner owns the preflight, the lock, the probe-only stack, the credentials
// and the receipts. This file owns the browser journeys, the named checks and
// the verdict. It never writes a credential, a device token or a signing key to
// its output, and every detail string passes the scrubber first.
//
// Exit codes: 0 PASS (candidate) or DETECTED (diagnostic), 1 FAIL or
// NOT_DETECTED, 2 setup error. A journey exception is a named failure, not a
// setup error, so a diagnostic cannot pass on an exception.

import { createRequire } from 'node:module';
import { readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { pathToFileURL, fileURLToPath } from 'node:url';
import { createChecks, createScrubber } from './lib/checks.mjs';
import { checkStockAssets, installDiagnosticAssets, treeDigests } from './lib/assets.mjs';
import { createFixtures, createReadbacks } from './lib/fixtures.mjs';
import { installSyncObserver } from './lib/kit.mjs';
import { denialJourney } from './lib/denial.mjs';
import { mixedJourney } from './lib/mixed.mjs';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const inputs = JSON.parse(readFileSync(join(HERE, 'inputs.json'), 'utf8'));
const env = process.env;

const required = (name) => {
  if (!env[name]) throw new Error(`${name} is unset`);
  return env[name];
};

const variant = required('PROBE_VARIANT');
const spec = inputs.variants[variant];
if (!spec) throw new Error(`unknown variant ${variant}`);
const repeat = Number(required('PROBE_REPEAT'));
if (repeat !== spec.repeat) throw new Error(`variant ${variant} takes repeat ${spec.repeat}`);
if (env.SCANNER_MUTATION !== undefined) throw new Error('SCANNER_MUTATION is set; refusing to run');

const REPO = required('PROBE_REPO');
const OUTPUT = required('PROBE_OUTPUT');
const BASE = required('BASE');
const PG = required('POSTGRES_CONTAINER');
const ACCESS = required('ACCESS_CONTAINER');
const ACCESS_PORT = required('ACCESS_PORT');
const QR_SEED = required('ACCESS_QR_PRIVATE_KEY');
const QR_KID = required('ACCESS_QR_KID');
const INTERNAL_SERVICE_TOKEN = required('INTERNAL_SERVICE_TOKEN');
const STOCK_DIST = required('PROBE_STOCK_DIST');
const DIAG_DIST = spec.assets === 'diagnostic' ? required('PROBE_DIAG_DIST') : null;

const scrubber = createScrubber();
scrubber.add(QR_SEED);
scrubber.add(INTERNAL_SERVICE_TOKEN);

const result = {
  schema: 'tkt-563-probe-result/1',
  variant,
  kind: spec.kind,
  repeat,
  runId: randomUUID(),
  startedAt: new Date().toISOString(),
  endedAt: null,
  head: env.PROBE_HEAD ?? null,
  tree: env.PROBE_TREE ?? null,
  baseline: env.PROBE_BASELINE ?? null,
  inputsSchema: inputs.schema,
  browserVersion: null,
  playwrightVersion: null,
  nodeVersion: process.version,
  invocation: { checks: [], stockAssets: null },
  repetitions: [],
  diagnostic: null,
  setupError: null,
  failedIds: [],
  verdict: null,
  exitCode: 2,
};

// Writes through a temporary file, so a reader never sees a torn result.
function writeResult() {
  const target = join(OUTPUT, 'probe-result.json');
  writeFileSync(`${target}.tmp`, `${JSON.stringify(result, null, 2)}\n`);
  renameSync(`${target}.tmp`, target);
}

const log = (line) => console.log(scrubber.scrub(line));
const fileUrl = (path) => pathToFileURL(join(REPO, path)).href;

function verdictOf() {
  const checks = result.repetitions.flatMap((item) => item.checks);
  const all = [...checks, ...result.invocation.checks];
  const failed = [...new Set(all.filter((item) => !item.ok).map((item) => item.id))];
  const passed = (id) => all.some((item) => item.id === id && item.ok);
  if (result.setupError) return { verdict: 'SETUP_ERROR', exitCode: 2, failed };
  if (spec.kind === 'candidate') {
    const pass = result.repetitions.length === repeat && checks.length > 0 && failed.length === 0;
    return { verdict: pass ? 'PASS' : 'FAIL', exitCode: pass ? 0 : 1, failed };
  }
  // A diagnostic is detected only when the mutation reached the system, every
  // intended named check failed, and nothing outside the allowed list failed.
  const mutationChecks = spec.kind === 'presenter' ? ['DIAG-MUTANT-SERVED'] : ['MIX-MUTATION-BOOKKEEPING'];
  const mutationOk = mutationChecks.every(passed);
  const intendedFailed = spec.intended.filter((id) => failed.includes(id));
  const unexpected = failed.filter((id) => !spec.allowed.includes(id));
  const detected = result.repetitions.length === 1 && mutationOk && intendedFailed.length === spec.intended.length && unexpected.length === 0;
  result.diagnostic = {
    variant,
    mutation: spec.mutation,
    layer: spec.mutationLayer,
    intended: spec.intended,
    allowed: spec.allowed,
    intendedFailed,
    unexpectedFailedIds: unexpected,
    mutationChecksPassed: mutationOk,
  };
  return { verdict: detected ? 'DETECTED' : 'NOT_DETECTED', exitCode: detected ? 0 : 1, failed };
}

async function main() {
  const requireRepo = createRequire(join(REPO, 'package.json'));
  const { chromium } = requireRepo('playwright-core');
  result.playwrightVersion = JSON.parse(readFileSync(join(REPO, 'node_modules', 'playwright-core', 'package.json'), 'utf8')).version;

  // Helpers come from the supplied repository, so the probe runs the code under test.
  const controls = await import(fileUrl('test/browser/lib/scanner-controls.mjs'));
  const support = await import(fileUrl('test/browser/lib/support.mjs'));
  const entry = await import(fileUrl('test/browser/lib/scanner-entry-core.mjs'));
  const H = { ...controls, entryCore: entry.entryCore, ORGANIZER: support.ORGANIZER, sql: support.sql, enrolScanner: support.enrolScanner, resultRecorder: support.resultRecorder };

  const token = H.enrolScanner(ACCESS, `TKT-563 probe ${result.runId}`);
  scrubber.add(token);
  const fx = createFixtures({ PG, QR_SEED, QR_KID, ORGANIZER: H.ORGANIZER, sql: H.sql, scrubber });
  const rb = createReadbacks({ PG, sql: H.sql });
  const recorder = H.resultRecorder(`tkt-563 probe ${variant}`);

  const browser = await chromium.launch({ channel: 'chrome' });
  result.browserVersion = browser.version();
  const newContext = browser.newContext.bind(browser);
  browser.newContext = async (...args) => {
    const context = await newContext(...args);
    context.setDefaultTimeout(10000);
    context.setDefaultNavigationTimeout(15000);
    return context;
  };

  try {
    // Stock served assets are compared with the candidate build, once per invocation.
    const invocation = createChecks(scrubber);
    result.invocation.checks = invocation.list;
    const stockContext = await browser.newContext({ baseURL: BASE });
    try {
      const stock = await checkStockAssets(stockContext.request, BASE, STOCK_DIST);
      result.invocation.stockAssets = stock;
      invocation.check('SERVED-STOCK-INDEX', stock.indexMatches, `status ${stock.index.status}`);
      invocation.check('SERVED-STOCK-ASSETS', stock.assetsMatch, `assets ${stock.assets.length}`);
    } finally {
      await stockContext.close();
    }

    const diagHashes = DIAG_DIST ? treeDigests(DIAG_DIST) : null;
    const stockHashes = DIAG_DIST ? treeDigests(STOCK_DIST) : null;

    for (let index = 1; index <= repeat; index += 1) {
      const checks = createChecks(scrubber);
      const served = [];
      const record = { index, checks: checks.list, journeys: {}, served };
      result.repetitions.push(record);

      // The existing Entry self-checks. SCANNER_MUTATION is absent here by construction.
      let entryIndex = 0;
      const entryCheck = (name, ok, detail) => {
        entryIndex += 1;
        recorder.check(name, ok, detail);
        checks.check(`ENTRYCORE-${String(entryIndex).padStart(2, '0')}`, ok, name);
      };
      try {
        const entryHelpers = await H.entryCore({ browser, BASE, PG, QR_SEED, QR_KID, token, check: entryCheck });
        // The returned helpers are kept for the repetition. The journeys build their own fixtures.
        checks.check('ENTRYCORE-COMPLETE', true);
        checks.check('ENTRYCORE-HELPERS', ['fixture', 'facts', 'quarantine'].every((name) => typeof entryHelpers?.[name] === 'function'));
      } catch (error) {
        result.setupError = scrubber.scrub(error?.stack ?? String(error)).slice(0, 4000);
        checks.check('ENTRYCORE-COMPLETE', false, result.setupError);
        writeResult();
        break;
      }

      // Page errors and console errors from the journey pages, scrubbed, kept as evidence.
      const browserErrors = [];
      record.browserErrors = browserErrors;
      const newJourneyContext = async ({ assetsDir }) => {
        const context = await browser.newContext({ baseURL: BASE, acceptDownloads: false });
        context.on('page', (page) => {
          page.on('pageerror', (error) => browserErrors.push({ kind: 'pageerror', message: scrubber.scrub(error.message).slice(0, 500) }));
          page.on('console', (message) => {
            if (message.type() === 'error') browserErrors.push({ kind: 'console', text: scrubber.scrub(message.text()).slice(0, 500) });
          });
        });
        await H.installProbe(context);
        await installSyncObserver(context);
        if (assetsDir) await installDiagnosticAssets(context, assetsDir, served);
        return context;
      };
      const ctx = {
        H,
        token,
        fx,
        rb,
        checks,
        BASE,
        ACCESS_PORT,
        INTERNAL_SERVICE_TOKEN,
        ORGANIZER: H.ORGANIZER,
        assetsDir: DIAG_DIST,
        newJourneyContext,
      };
      for (const journey of spec.journeys) {
        // The journey fills this object in place, so a journey exception keeps every readback taken before it.
        const evidence = {};
        record.journeys[journey] = evidence;
        try {
          if (journey === 'denial') await denialJourney(ctx, evidence);
          else if (journey === 'mixed') await mixedJourney(ctx, spec.kind === 'request' ? spec.mutation : null, evidence);
          else throw new Error(`unknown journey ${journey}`);
        } catch (error) {
          checks.check(`JOURNEY-EXCEPTION-${journey}`, false, scrubber.scrub(error?.stack ?? String(error)).slice(0, 2000));
        }
      }

      // An uncaught page exception is an unexpected outcome. Console errors are kept as evidence only.
      const uncaught = browserErrors.filter((item) => item.kind === 'pageerror');
      checks.check('BROWSER-PAGEERRORS-NONE', uncaught.length === 0, JSON.stringify(uncaught.slice(0, 5)));

      if (spec.kind === 'presenter') {
        // Every served asset must belong to the mutant build. At least one script must
        // differ from every stock script. A stylesheet can match stock bytes, so it is not required to differ.
        const assetRequests = served.filter((item) => item.path.startsWith('/scanner/assets/'));
        const mutantScripts = assetRequests.filter((item) => item.path.endsWith('.js'));
        const servedOk =
          served.some((item) => item.path === '/scanner/') &&
          assetRequests.length > 0 &&
          assetRequests.every((item) => diagHashes.has(item.sha256)) &&
          mutantScripts.some((item) => !stockHashes.has(item.sha256));
        checks.check('DIAG-MUTANT-SERVED', servedOk, JSON.stringify({ requests: served.length, assets: assetRequests.length }));
      }
      writeResult();
      log(`repetition ${index}: ${checks.failedIds().length === 0 ? 'named checks passed' : `failed ${checks.failedIds().join(', ')}`}`);
    }
  } finally {
    await browser.close();
    recorder.finish();
  }
}

try {
  await main();
} catch (error) {
  result.setupError = scrubber.scrub(error?.stack ?? String(error)).slice(0, 4000);
} finally {
  result.endedAt = new Date().toISOString();
  const { verdict, exitCode, failed } = verdictOf();
  result.verdict = verdict;
  result.exitCode = exitCode;
  result.failedIds = failed;
  writeResult();
  log(`verdict ${verdict}; failing check ids: ${failed.length === 0 ? 'none' : failed.join(', ')}`);
  process.exitCode = exitCode;
}
