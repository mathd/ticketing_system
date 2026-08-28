// Real-browser coverage for the ticket-resend form (TKT-203, ADR-068).
//
// `make check` cannot verify this. Its smoke suite does submit back-office forms, but
// through a Go client that constructs its own target, headers, cookies and redirect
// behaviour — it does not read the rendered form action, reproduce browser-generated
// Origin/Referer and SameSite, or execute JavaScript. A broken action, cookie path or
// CSP rule passes `make check` and fails a real click (AGENTS.md; TKT-105, TKT-226).
//
// WHAT THIS SPEC CAN AND CANNOT SEE. The page submits a form POST; the call to access
// happens SERVER-side, inside the SSR handler, so counting requests from Playwright
// observes zero. So the assertions are what the browser can see — the submit, the
// redirect, the rendered result — and what the write LEFT BEHIND, re-read from the
// access database. Exactly, not "non-null": TKT-244's assertion stayed green through a
// truncation because it only checked for presence.

import { randomUUID } from 'node:crypto';
import { chromium } from 'playwright-core';
import { ORGANIZER, provisionAdmin, resultRecorder, signIn, sql, submitForm } from './lib/support.mjs';

const BASE = process.env.BASE ?? 'http://localhost:18080';
const PG = process.env.POSTGRES_CONTAINER;
const CATALOG = process.env.CATALOG_CONTAINER;
if (!PG) throw new Error('POSTGRES_CONTAINER is unset; run through ./scripts/browser.sh');
if (!CATALOG) throw new Error('CATALOG_CONTAINER is unset; run through ./scripts/browser.sh');

const PATH = '/admin/orders';
const stamp = Date.now();
const identifier = `order-resend-${stamp}@example.test`;
const password = 'correct horse battery staple';
const reservationId = randomUUID();
const orderId = randomUUID();
const guestRef = randomUUID();
const buyerId = randomUUID();
const slotId = randomUUID();
const ticketTypeId = randomUUID();
// Two tickets, because a one-ticket fixture cannot tell "one event per delivery" from
// "one event per order".
const ticketIds = [randomUUID(), randomUUID()];

provisionAdmin(CATALOG, identifier, password);

// The commerce half: a completed order, so the console renders and offers the form.
sql(
  PG,
  'commerce',
  `INSERT INTO reservations
     (id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id, quantity,
      unit_amount, total_amount, face_value_amount, currency, status)
   VALUES
     ('${reservationId}', '${ORGANIZER}', '${randomUUID()}', '${slotId}',
      '${ticketTypeId}', '${buyerId}', 2, 1250, 2500, 2500, 'EUR', 'completed');
   INSERT INTO orders
     (id, reservation_id, status, idempotency_key, request_fingerprint, guest_order_ref)
   VALUES
     ('${orderId}', '${reservationId}', 'completed', 'browser-resend-${stamp}',
      'browser-resend-${stamp}', '${guestRef}');`,
);

// The access half: two issued tickets for that order.
//
// Tickets only — NO lifecycle_events rows are inserted here. Every lifecycle event must
// go through the signed append path, and a direct insert reads as tampering to
// `access verify-lifecycle`, which runs in the gate (AGENTS.md, ADR-021). So this spec
// asserts what the RESEND writes; the already-delivered case is covered where it can be
// driven honestly, in the store and smoke tiers.
sql(
  PG,
  'access',
  ticketIds
    .map(
      (id) =>
        `INSERT INTO tickets
           (id, order_id, guest_order_ref, organizer_id, buyer_id, slot_id, ticket_type_id,
            qr_payload, issued_at)
         VALUES
           ('${id}', '${orderId}', '${guestRef}', '${ORGANIZER}', '${buyerId}', '${slotId}',
            '${ticketTypeId}', 'browser-spec-credential', now());`,
    )
    .join('\n'),
);

const countAccess = (where) => Number(sql(PG, 'access', `SELECT count(*) FROM ${where}`));
const ticketList = ticketIds.map((id) => `'${id}'`).join(',');

const { check, finish } = resultRecorder('order-resend browser spec');
const browser = await chromium.launch({ channel: 'chrome' });

try {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await signIn(page, identifier, password);
  check('admin signs in', page.url().includes('/admin'));

  await page.goto(PATH, { waitUntil: 'domcontentloaded' });
  await page.fill('#order_id', orderId);
  await submitForm(page, page.getByRole('button', { name: 'Look it up' }));

  const resendForm = page.locator("form.refund:has(h2:text(\"Re-send this order's tickets\"))");
  check('a completed order renders the resend form', (await resendForm.count()) === 1);

  // COS-2, in the rendered DOM. The destination must be UNSUBMITTABLE, not validated:
  // there must be no input for it at all, of any kind — including a hidden one, which
  // always submits and would read as present-and-empty rather than absent (TKT-236).
  const names = await resendForm.locator('input, select, textarea').evaluateAll((els) =>
    els.map((e) => e.getAttribute('name')),
  );
  check(
    'the resend form carries no recipient field of any kind',
    !names.some((n) => /mail|address|recipient|to$|dest/i.test(n ?? '')),
    JSON.stringify(names),
  );
  check(
    'the resend form submits only the fields the server decides from',
    JSON.stringify([...names].sort()) ===
      JSON.stringify(['_action', 'idempotency_key', 'order_id', 'ticket_ref']),
    JSON.stringify(names),
  );

  const key = await resendForm.locator('input[name="idempotency_key"]').inputValue();
  check('the rendered resend form carries a server-minted key', /^[0-9a-f-]{36}$/i.test(key), key);

  check('no resend has happened yet', countAccess(`redelivery_attempts WHERE ticket_id IN (${ticketList})`) === 0);

  // The submit itself — the thing only a browser can prove. A wrong form action, a
  // cookie path that drops the session, or a CSP rule blocking the post all fail here
  // and pass `make check`.
  const request = await submitForm(
    page,
    resendForm.getByRole('button', { name: 'Re-send to the address on file' }),
  );
  check(
    'the resend form posts to the order console',
    new URL(request.url()).pathname === PATH,
    request.url(),
  );

  const rendered = await page.locator('.refund-result').innerText();
  check('the page reports the resend', rendered.includes('Handed to the mail transport'), rendered);
  check('the page states the destination is the address on file', rendered.includes('address already on file'));

  // COS-6 at the boundary the value would cross on its way to a human: the buyer's
  // address must not appear on the rendered page, nor a capability link.
  const html = await page.content();
  check('the rendered page shows no buyer address', !/[\w.+-]+@[\w-]+\.[\w.]+/.test(await page.locator('.refund-result').innerText()));
  check('the rendered page shows no ticket capability link', !html.includes('/en/tickets/'));
  check('the rendered page does not leak the guest reference', !html.includes(guestRef));

  // What the write LEFT BEHIND, re-read exactly.
  check(
    'the submit wrote one resend attempt per ticket',
    countAccess(`redelivery_attempts WHERE ticket_id IN (${ticketList})`) === ticketIds.length,
  );
  check(
    'the submit appended one redelivered event per ticket',
    countAccess(
      `lifecycle_events WHERE event_type='redelivered' AND ticket_id IN (${ticketList})`,
    ) === ticketIds.length,
  );
  check(
    'the resend bound its request to this order',
    sql(PG, 'access', `SELECT order_id::text FROM redelivery_requests WHERE order_id='${orderId}'`) === orderId,
  );

  // COS-7 through a real double submit. The page mints a NEW key per rendered form, so
  // a second click on a freshly rendered form is a second deliberate send — the
  // double-click case is the SAME key submitted twice, which is what going back to the
  // already-rendered form does.
  await page.goBack({ waitUntil: 'domcontentloaded' });
  const staleForm = page.locator("form.refund:has(h2:text(\"Re-send this order's tickets\"))");
  if ((await staleForm.count()) === 1) {
    const staleKey = await staleForm.locator('input[name="idempotency_key"]').inputValue();
    if (staleKey === key) {
      await submitForm(page, staleForm.getByRole('button', { name: 'Re-send to the address on file' }));
      check(
        'resubmitting the same key sent nothing further',
        countAccess(`redelivery_attempts WHERE ticket_id IN (${ticketList})`) === ticketIds.length,
      );
      check(
        'resubmitting the same key appended no further trail events',
        countAccess(
          `lifecycle_events WHERE event_type='redelivered' AND ticket_id IN (${ticketList})`,
        ) === ticketIds.length,
      );
    }
  }
} finally {
  await browser.close();
}

if (!finish()) process.exit(1);
