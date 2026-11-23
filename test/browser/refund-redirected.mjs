// Real-browser coverage for the staff refund form when the Back Office's Commerce
// call is redirected (TKT-602, ADR-088).
//
// The Back Office refuses every upstream redirect. For the order console this
// means: a redirected refund POST is an ambiguous answer, and a redirected staff
// order detail read does not confirm an abandonment. In both cases the page keeps
// the saved retry with its original key and terms, and offers no fresh refund
// form. The redirect target must receive no request.
//
// How the redirect is made. The stack's Back Office reaches Commerce directly in
// the Compose network, so a spec cannot put a redirect between them. This spec
// starts a SECOND Back Office process on the host, from the same built
// web/backoffice/dist that the stack runs, with the stack's own credentials. Its
// COMMERCE_URL is a loopback redirector that this spec controls: it passes
// requests through to the stack's published Commerce port, or answers 307 to a
// counting target on another port (a different origin). All other upstream
// URLs point at the stack's gateway and published service ports. The data is the
// stack's data, and Chrome signs in through the second process with a staff
// account provisioned in the stack's Catalog.
//
// The second process has no gateway in front of it. The Origin gate
// (src/lib/gate.ts) compares Origin with X-Forwarded-Proto and X-Forwarded-Host,
// which the gateway sets. Chrome sends both headers on every request of the
// context, with the values the gateway would set for this address.
//
// The counting target answers as a hostile host would: a 409 refund_abandoned to
// the refund POST, and a 200 that confirms an abandonment of the saved key to the
// detail read. A Back Office that follows the redirect therefore loses the saved
// key, and the checks below fail on the target count and on the rendered form.
//
// Run through ./scripts/browser.sh (make browser) with host Chrome.

import { execFileSync, spawn } from 'node:child_process';
import { createServer, request as httpRequest } from 'node:http';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';
import { seedPaidOrder } from './lib/refund-orders.mjs';
import { ORGANIZER, provisionAdmin, resultRecorder, signIn, sql, submitForm } from './lib/support.mjs';

const PG = process.env.POSTGRES_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
const BACKOFFICE = process.env.BACKOFFICE_CONTAINER;
const GATEWAY = process.env.BASE;
for (const [name, value] of Object.entries({
  POSTGRES_CONTAINER: PG,
  CATALOG_CONTAINER: CATALOG,
  BACKOFFICE_CONTAINER: BACKOFFICE,
  BASE: GATEWAY,
  CATALOG_PORT: process.env.CATALOG_PORT,
  INVENTORY_PORT: process.env.INVENTORY_PORT,
  COMMERCE_PORT: process.env.COMMERCE_PORT,
  ACCESS_PORT: process.env.ACCESS_PORT,
})) {
  if (!value) throw new Error(`${name} is unset; run through ./scripts/browser.sh`);
}

const START = fileURLToPath(new URL('../../web/backoffice/start.mjs', import.meta.url));
const LOOPBACK = '127.0.0.1';
const COMMERCE_PORT = Number(process.env.COMMERCE_PORT);
const READY_MS = 30_000;
const STOP_MS = 5_000;
const PATH = '/admin/orders';
const stamp = Date.now();
const identifier = `refund-redirected-${stamp}@example.test`;
const password = 'correct horse battery staple';
const AMBIGUOUS = 'Commerce could not be reached.';
const MAY_HAVE_APPLIED = 'This refund may or may not have been applied.';
const STAFF_TOKENS = [
  'CATALOG_STAFF_WRITE_TOKEN',
  'COMMERCE_STAFF_WRITE_TOKEN',
  'INVENTORY_STAFF_WRITE_TOKEN',
  'ACCESS_STAFF_WRITE_TOKEN',
];

// The stack Back Office's staff credentials, read from its container
// configuration. They are generated per stack, so this works for `make browser`
// and for `browser.sh up` followed by `browser.sh run`. No value goes on a
// command line or into the output.
function stackCredentials() {
  const env = JSON.parse(
    execFileSync('docker', ['inspect', '--format', '{{json .Config.Env}}', BACKOFFICE], { encoding: 'utf8' }),
  );
  const values = {};
  for (const entry of env) {
    const at = entry.indexOf('=');
    const name = entry.slice(0, at);
    if (STAFF_TOKENS.includes(name)) values[name] = entry.slice(at + 1);
  }
  for (const name of STAFF_TOKENS) {
    if (!values[name]) throw new Error(`the stack Back Office has no ${name}`);
  }
  return values;
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, LOOPBACK, () => resolve(server.address().port));
  });
}

function close(server) {
  server.closeAllConnections?.();
  return new Promise((resolve) => server.close(() => resolve()));
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

const REFUND_POST = /^\/internal\/orders\/([^/?]+)\/refunds$/;
const DETAIL_READ = /^\/internal\/orders\/([^/?]+)$/;

// The address a redirect sends a request to. Every request it receives is
// recorded with its method, path and the names (never the values) of the
// credential headers, and answered as a hostile host would answer.
function countingTarget(savedKeys) {
  const received = [];
  const server = createServer(async (req, res) => {
    await readBody(req).catch(() => Buffer.alloc(0));
    const path = new URL(req.url, 'http://target').pathname;
    received.push({
      method: req.method,
      path,
      credentials: Object.keys(req.headers).filter((h) => /token|assertion/i.test(h)),
    });
    let match;
    if (req.method === 'POST' && (match = REFUND_POST.exec(path))) {
      res.writeHead(409, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ error: 'refund abandoned', code: 'refund_abandoned' }));
    } else if (req.method === 'GET' && (match = DETAIL_READ.exec(path))) {
      const order = decodeURIComponent(match[1]).toLowerCase();
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(
        JSON.stringify({
          order_id: order,
          organizer_id: ORGANIZER,
          refunds: [{ idempotency_key: savedKeys.get(order) ?? '', abandoned: true }],
        }),
      );
    } else {
      res.writeHead(404).end();
    }
  });
  return { server, received };
}

// COMMERCE_URL of the second Back Office. It passes each request through to the
// stack's Commerce, or answers 307 to the counting target when the spec turns
// on a redirect for that call.
function commerceRedirector(targetPort) {
  const redirect = { refundPost: false, detailRead: false };
  const redirected = [];
  const passed = [];
  const server = createServer((req, res) => {
    const path = new URL(req.url, 'http://redirector').pathname;
    const isRefund = req.method === 'POST' && REFUND_POST.test(path);
    const isDetail = req.method === 'GET' && DETAIL_READ.test(path);
    if ((isRefund && redirect.refundPost) || (isDetail && redirect.detailRead)) {
      redirected.push(`${req.method} ${isRefund ? 'refund' : 'detail'}`);
      req.resume();
      res.writeHead(307, { location: `http://${LOOPBACK}:${targetPort}${req.url}` });
      res.end();
      return;
    }
    passed.push(`${req.method} ${isRefund ? 'refund' : isDetail ? 'detail' : path}`);
    const upstream = httpRequest(
      {
        host: LOOPBACK,
        port: COMMERCE_PORT,
        method: req.method,
        path: req.url,
        headers: { ...req.headers, host: `${LOOPBACK}:${COMMERCE_PORT}` },
      },
      (answer) => {
        res.writeHead(answer.statusCode ?? 502, answer.headers);
        answer.pipe(res);
      },
    );
    upstream.on('error', () => {
      if (!res.headersSent) res.writeHead(502);
      res.end();
    });
    req.pipe(upstream);
  });
  return { server, redirect, redirected, passed };
}

async function startBackOffice(port, commerceUrl) {
  const output = [];
  const child = spawn(process.execPath, [START], {
    env: {
      PATH: process.env.PATH ?? '',
      HOME: process.env.HOME ?? '',
      NODE_ENV: 'production',
      HOST: LOOPBACK,
      PORT: String(port),
      GATEWAY_URL: GATEWAY.replace(/\/$/, ''),
      COMMERCE_URL: commerceUrl,
      CATALOG_URL: `http://${LOOPBACK}:${process.env.CATALOG_PORT}`,
      INVENTORY_URL: `http://${LOOPBACK}:${process.env.INVENTORY_PORT}`,
      ACCESS_URL: `http://${LOOPBACK}:${process.env.ACCESS_PORT}`,
      ...stackCredentials(),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  child.stdout.on('data', (chunk) => output.push(String(chunk)));
  child.stderr.on('data', (chunk) => output.push(String(chunk)));
  const exited = new Promise((resolve) => child.once('exit', (code, signal) => resolve({ code, signal })));
  let gone = null;
  exited.then((result) => {
    gone = result;
  });

  const stop = async () => {
    if (gone) return;
    child.kill('SIGTERM');
    const timer = new Promise((resolve) => setTimeout(() => resolve('timeout'), STOP_MS));
    if ((await Promise.race([exited, timer])) === 'timeout') {
      child.kill('SIGKILL');
      await exited;
    }
  };

  // Bounded readiness: the health route answers 200, or the process exits, or
  // the deadline passes. On failure the process is stopped here, because the
  // caller has no handle on it yet.
  try {
    await waitUntilReady(port, () => gone, output);
  } catch (error) {
    await stop();
    throw error;
  }
  return { output, stop };
}

async function waitUntilReady(port, exitedWith, output) {
  const deadline = Date.now() + READY_MS;
  for (;;) {
    const gone = exitedWith();
    if (gone) throw new Error(`the second Back Office exited early (${JSON.stringify(gone)}):\n${output.join('')}`);
    try {
      const res = await fetch(`http://${LOOPBACK}:${port}/admin/healthz`, { signal: AbortSignal.timeout(1_000) });
      await res.body?.cancel();
      if (res.status === 200) break;
    } catch {
      // not listening yet
    }
    if (Date.now() > deadline) {
      throw new Error(`the second Back Office was not ready in ${READY_MS} ms:\n${output.join('')}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
}

async function freePort() {
  const probe = createServer();
  const port = await listen(probe);
  await close(probe);
  return port;
}

const refundRows = (order) =>
  sql(PG, 'commerce', `SELECT count(*) || '|' || string_agg(status, ',') FROM order_refunds WHERE order_id='${order}'`);

const { check, finish } = resultRecorder('refund-redirected browser spec');
provisionAdmin(CATALOG, identifier, password);
const postOrder = seedPaidOrder(PG, 'redirected-post', 2, 1250, stamp);
const readOrder = seedPaidOrder(PG, 'redirected-read', 2, 1250, stamp);

// order id -> saved key, so the hostile target can confirm exactly that key.
const savedKeys = new Map();
const target = countingTarget(savedKeys);
let redirector = null;
let backOffice = null;
let browser = null;

try {
  const targetPort = await listen(target.server);
  redirector = commerceRedirector(targetPort);
  const redirectorPort = await listen(redirector.server);
  const port = await freePort();
  backOffice = await startBackOffice(port, `http://${LOOPBACK}:${redirectorPort}`);
  const origin = `http://${LOOPBACK}:${port}`;

  browser = await chromium.launch({ channel: 'chrome' });
  const context = await browser.newContext({
    baseURL: origin,
    // What the gateway sets for this address (see the header comment).
    extraHTTPHeaders: { 'x-forwarded-proto': 'http', 'x-forwarded-host': `${LOOPBACK}:${port}` },
  });
  const page = await context.newPage();
  await signIn(page, identifier, password);

  const freshForm = () => page.locator('form.refund:has(h2:text("Refund this order"))');
  const retryForm = () => page.locator('form.refund:has(h2:text("Retry this refund"))');
  // Null when the page offers no single retry form, so a lost key fails a check
  // instead of waiting for a locator timeout.
  const retryValue = async (name) =>
    (await retryForm().count()) === 1 ? retryForm().locator(`input[name="${name}"]`).inputValue() : null;

  async function lookUp(o) {
    await page.goto(PATH, { waitUntil: 'domcontentloaded' });
    await page.fill('#order_id', o.order);
    await submitForm(page, page.getByRole('button', { name: 'Look it up' }));
  }

  // Through the redirector in pass-through mode: Commerce answers 502, so the
  // page saves the request as a retry under the submitted key and terms.
  async function saveUnresolvedRefund(o, label) {
    await lookUp(o);
    const form = freshForm();
    check(`${label}: the completed order renders the refund form`, (await form.count()) === 1);
    const key = await form.locator('input[name="idempotency_key"]').inputValue();
    const reason = `browser ${label} first attempt`;
    const passedBefore = redirector.passed.filter((r) => r === 'POST refund').length;
    await form.locator('#quantity').fill('1');
    await form.locator('#reason').fill(reason);
    await submitForm(page, form.getByRole('button', { name: 'Refund' }));
    check(`${label}: the refund is saved as a retry under the submitted key`, (await retryValue('idempotency_key')) === key);
    check(
      `${label}: the refund POST reached Commerce through the redirector`,
      redirector.passed.filter((r) => r === 'POST refund').length === passedBefore + 1,
    );
    check(`${label}: Commerce bound one pending refund`, refundRows(o.order) === '1|pending', refundRows(o.order));
    savedKeys.set(o.order, key);
    return { key, quantity: '1', reason };
  }

  // The saved retry with exactly these terms, and nothing else.
  async function checkOnlySavedRetry(label, saved) {
    check(`${label}: the saved retry is still offered`, (await retryForm().count()) === 1);
    check(`${label}: the saved retry keeps the original key`, (await retryValue('idempotency_key')) === saved.key);
    check(`${label}: the saved retry keeps the original quantity`, (await retryValue('quantity')) === saved.quantity);
    check(`${label}: the saved retry keeps the original reason`, (await retryValue('reason')) === saved.reason);
    check(`${label}: no fresh refund form is offered`, (await freshForm().count()) === 0);
    const body = await page.locator('body').innerText();
    check(`${label}: the page does not report an abandonment`, !body.includes('An operator recorded this refund as abandoned.'));
  }

  // ---- (a) The saved retry is submitted while the refund POST is redirected.
  const postSaved = await saveUnresolvedRefund(postOrder, 'post');
  redirector.redirect.refundPost = true;
  const submitted = await submitForm(page, retryForm().getByRole('button', { name: 'Retry the same refund' }));
  const posted = new URLSearchParams(submitted.postData() ?? '');
  check('post: the retry submits the saved key', posted.get('idempotency_key') === postSaved.key);
  check(
    'post: the redirector answered the refund POST with a redirect',
    redirector.redirected.filter((r) => r === 'POST refund').length === 1,
    redirector.redirected.join(','),
  );
  const alert = (await page.getByRole('alert').innerText()).trim();
  check('post: the page reports the ambiguous result', alert.includes(AMBIGUOUS) && alert.includes(MAY_HAVE_APPLIED), alert);
  await checkOnlySavedRetry('post', postSaved);
  check('post: Commerce still has one pending refund', refundRows(postOrder.order) === '1|pending', refundRows(postOrder.order));
  redirector.redirect.refundPost = false;

  // ---- (b) A plain lookup while the staff order detail read is redirected.
  const readSaved = await saveUnresolvedRefund(readOrder, 'read');
  redirector.redirect.detailRead = true;
  const before = redirector.redirected.filter((r) => r === 'GET detail').length;
  await lookUp(readOrder);
  check(
    'read: the redirector answered the detail read with a redirect',
    redirector.redirected.filter((r) => r === 'GET detail').length === before + 1,
    redirector.redirected.join(','),
  );
  await checkOnlySavedRetry('read', readSaved);
  redirector.redirect.detailRead = false;

} catch (error) {
  check('the spec ran to the end', false, error instanceof Error ? error.message : String(error));
} finally {
  await browser?.close().catch(() => {});
  await backOffice?.stop().catch(() => {});
  if (redirector?.server.listening) await close(redirector.server);
  if (target.server.listening) await close(target.server);
}

// ---- (c) The redirect target received nothing. Checked after the finally, so
// it is recorded also when the spec stopped early.
check('the redirect target received no request', target.received.length === 0, JSON.stringify(target.received));
if (!finish()) process.exit(1);
