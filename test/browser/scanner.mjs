// Real-browser submission coverage for the Scanner write path.
// The unit suite controls storage failures. This spec proves that Chrome can
// pair a device and send the rendered form through the gateway.

import { chromium } from 'playwright-core';
import { enrolScanner, resultRecorder } from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const ACCESS = process.env.ACCESS_CONTAINER;
if (!ACCESS) throw new Error('ACCESS_CONTAINER is unset. Run this spec through ./scripts/browser.sh');

const recorder = resultRecorder('scanner');
const { check } = recorder;
const token = enrolScanner(ACCESS, `browser gate ${Date.now()}`);

const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  const scanRequests = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans') {
      scanRequests.push(request);
    }
  });

  await page.goto('/scanner/', { waitUntil: 'domcontentloaded' });
  await page.fill('#pairing-token', token);
  await page.click('button[type=submit]');
  await page.getByLabel('Ticket credential').waitFor();
  check('an enrolled device can pair through the rendered form', await page.getByLabel('Ticket credential').isVisible());

  await page.getByLabel('Ticket credential').fill('not-a-ticket');
  const requestPromise = page.waitForRequest((request) =>
    request.method() === 'POST' && new URL(request.url()).pathname === '/api/access/scans');
  await page.getByRole('button', { name: 'Check ticket' }).click();
  const request = await requestPromise;
  await page.getByRole('heading', { name: 'Rejected' }).waitFor();

  const body = request.postDataJSON();
  check('the form submits one scan request', scanRequests.length === 1, `observed ${scanRequests.length}`);
  check('the request carries the paired device token', request.headers()['x-scanner-token'] === token);
  check('the request carries the entered credential', body.qr_payload === 'not-a-ticket');
  check(
    'the request carries a scanner-minted occurrence id',
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(body.occurrence_id),
  );
  check('the request carries a valid occurrence time', !Number.isNaN(Date.parse(body.occurred_at)));
  check('the rejected result is rendered', await page.getByRole('heading', { name: 'Rejected' }).isVisible());
  check(
    'the form is usable after the request finishes',
    !(await page.getByRole('button', { name: 'Check ticket' }).isDisabled()),
  );
} finally {
  await browser.close();
  if (!recorder.finish()) process.exitCode = 1;
}
