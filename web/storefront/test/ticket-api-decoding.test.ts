import { afterEach, describe, expect, it, vi } from 'vitest';

import { UI_STRINGS } from '../src/lib/locales';
import { getTicketBundle, ticketsForDisplay } from '../src/lib/ticket-api';

const ORDER_REF = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1';
const OTHER_ORDER_REF = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2';
const TICKET = 'cccccccc-cccc-4ccc-8ccc-ccccccccccc3';
const EVENT = 'dddddddd-dddd-4ddd-8ddd-ddddddddddd4';
const OTHER_TICKET = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee5';
const THIRD_TICKET = 'ffffffff-ffff-4fff-8fff-fffffffffff6';

const bundle = {
  order_ref: ORDER_REF,
  tickets: [{
    ticket_id: TICKET,
    qr_payload: 'signed-ticket-payload',
    issued_at: '2026-08-03T12:00:00Z',
    qr_url: '/api/access/tickets/qr/signed-ticket-payload',
    history: [{
      id: EVENT,
      type: 'issued',
      sequence: 1,
      occurred_at: '2026-08-03T12:00:00Z',
    }],
    validity: { status: 'valid' },
    performance_timezone: 'Asia/Kolkata',
  }],
};

// TKT-355: one bundle with all three verdicts, each on its own ticket. The histories are
// all issued-only on purpose: the verdict is Access's, and the decoder must carry it
// as sent, not recompute it.
const mixed = {
  ...bundle,
  tickets: [
    { ...bundle.tickets[0], validity: { status: 'voided', reason: 'refunded' } },
    { ...bundle.tickets[0], ticket_id: OTHER_TICKET, validity: { status: 'valid' } },
    { ...bundle.tickets[0], ticket_id: THIRD_TICKET, validity: { status: 'voided', reason: 'exchanged' } },
  ],
};

function withZone(zone: unknown) {
  return { ...bundle, tickets: [{ ...bundle.tickets[0], performance_timezone: zone }] };
}

function withoutZone() {
  const { performance_timezone: _zone, ...ticket } = bundle.tickets[0];
  return { ...bundle, tickets: [ticket] };
}

function withValidity(validity: unknown) {
  return { ...bundle, tickets: [{ ...bundle.tickets[0], validity }] };
}

function respond(body: unknown): void {
  vi.stubGlobal('fetch', async () =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    }));
}

afterEach(() => vi.unstubAllGlobals());

describe('ticket bundle response decoding', () => {
  it('returns the generated contract shape for a valid bundle', async () => {
    respond(bundle);
    await expect(getTicketBundle(ORDER_REF)).resolves.toEqual({ ok: true, value: bundle });
  });

  it('keeps each ticket\'s verdict exactly as Access sent it', async () => {
    respond(mixed);
    await expect(getTicketBundle(ORDER_REF)).resolves.toEqual({ ok: true, value: mixed });
  });

  it('projects each ticket\'s verdict for display, in bundle order', async () => {
    respond(mixed);
    const read = await getTicketBundle(ORDER_REF);
    if (!read.ok) throw new Error('bundle did not decode');
    expect(ticketsForDisplay(read.value, 'en').map((ticket) => ticket.validity)).toEqual([
      { kind: 'voided', reason: 'refunded' },
      { kind: 'valid' },
      { kind: 'voided', reason: 'exchanged' },
    ]);
  });

  // TKT-365 (ADR-083): null is Access saying "Catalog did not answer". It is a valid
  // bundle; the page then shows labelled UTC.
  it('accepts a null performance zone', async () => {
    respond(withZone(null));
    await expect(getTicketBundle(ORDER_REF)).resolves.toEqual({ ok: true, value: withZone(null) });
  });

  it('projects each ticket\'s history with its own zone and a localized label', async () => {
    const twoZones = {
      ...bundle,
      tickets: [
        { ...bundle.tickets[0], history: [{ ...bundle.tickets[0].history[0], occurred_at: '2026-07-14T22:30:05Z' }] },
        {
          ...bundle.tickets[0],
          ticket_id: OTHER_TICKET,
          performance_timezone: null,
          history: [{ ...bundle.tickets[0].history[0], type: 'future_kind', occurred_at: '2026-07-14T22:30:05Z' }],
        },
      ],
    };
    respond(twoZones);
    const read = await getTicketBundle(ORDER_REF);
    if (!read.ok) throw new Error('bundle did not decode');
    const [kolkata, unknownZone] = ticketsForDisplay(read.value, 'fr');
    expect(kolkata.history).toHaveLength(1);
    expect(kolkata.history[0].label).toBe(UI_STRINGS.fr.historyIssued);
    expect(kolkata.history[0].occurredAt).toBe('2026-07-14T22:30:05Z');
    expect(kolkata.history[0].time).toContain('04:00:05');
    expect(kolkata.history[0].time).toContain('UTC+5:30');
    expect(unknownZone.history[0].label).toBe(UI_STRINGS.fr.historyOther);
    expect(unknownZone.history[0].time).toContain('22:30:05');
    expect(unknownZone.history[0].time).toContain('UTC');
  });

  it('accepts and canonicalises an order identity that differs from the request only by case', async () => {
    respond(bundle);
    await expect(getTicketBundle(ORDER_REF.toUpperCase())).resolves.toMatchObject({
      ok: true,
      value: { order_ref: ORDER_REF },
    });
  });

  it('accepts the exact JavaScript-safe lifecycle sequence ceiling', async () => {
    const atCeiling = {
      ...bundle,
      tickets: [{
        ...bundle.tickets[0],
        history: [{ ...bundle.tickets[0].history[0], sequence: Number.MAX_SAFE_INTEGER }],
      }],
    };
    respond(atCeiling);

    await expect(getTicketBundle(ORDER_REF)).resolves.toEqual({ ok: true, value: atCeiling });
  });

  it.each([
    ['an absent tickets array', { order_ref: ORDER_REF }],
    ['a non-array tickets field', { order_ref: ORDER_REF, tickets: {} }],
    ['a different order identity', { ...bundle, order_ref: OTHER_ORDER_REF }],
    ['a malformed ticket identity', {
      ...bundle,
      tickets: [{ ...bundle.tickets[0], ticket_id: 'ticket-1' }],
    }],
    ['an invalid issue date', {
      ...bundle,
      tickets: [{ ...bundle.tickets[0], issued_at: 'today' }],
    }],
    ['an absent history array', {
      ...bundle,
      tickets: [{
        ticket_id: TICKET,
        qr_payload: 'signed-ticket-payload',
        issued_at: '2026-08-03T12:00:00Z',
        qr_url: '/api/access/tickets/qr/signed-ticket-payload',
        validity: { status: 'valid' },
        performance_timezone: 'Asia/Kolkata',
      }],
    }],
    // TKT-355: one row per malformation CLASS of the verdict. A missing verdict must
    // never read as valid: that is the default a decoder reaches for, and it would show
    // a dead QR.
    ['an absent validity', {
      ...bundle,
      tickets: [{
        ticket_id: TICKET,
        qr_payload: 'signed-ticket-payload',
        issued_at: '2026-08-03T12:00:00Z',
        qr_url: '/api/access/tickets/qr/signed-ticket-payload',
        history: [],
        performance_timezone: 'Asia/Kolkata',
      }],
    }],
    // TKT-365: the zone is required and nullable. Absent is a contract break, not null.
    ['an absent performance zone', withoutZone()],
    ['an empty performance zone', withZone('')],
    ['a non-string performance zone', withZone(5)],
    ['an object performance zone', withZone({ name: 'Asia/Kolkata' })],
    ['a null validity', withValidity(null)],
    ['a string validity', withValidity('valid')],
    ['an array validity', withValidity([{ status: 'valid' }])],
    ['an empty validity', withValidity({})],
    ['a non-string status', withValidity({ status: 1 })],
    ['an unknown status', withValidity({ status: 'suspended' })],
    ['an unknown reason', withValidity({ status: 'voided', reason: 'stolen' })],
    ['a non-string reason', withValidity({ status: 'voided', reason: true })],
    ['a valid verdict with a reason', withValidity({ status: 'valid', reason: 'refunded' })],
    ['a voided verdict without a reason', withValidity({ status: 'voided' })],
    ['an extra validity field', withValidity({ status: 'valid', note: 'x' })],
    ['an impossible lifecycle sequence', {
      ...bundle,
      tickets: [{
        ...bundle.tickets[0],
        history: [{ ...bundle.tickets[0].history[0], sequence: -1 }],
      }],
    }],
    ['a zero lifecycle sequence', {
      ...bundle,
      tickets: [{
        ...bundle.tickets[0],
        history: [{ ...bundle.tickets[0].history[0], sequence: 0 }],
      }],
    }],
    ['a fractional lifecycle sequence', {
      ...bundle,
      tickets: [{
        ...bundle.tickets[0],
        history: [{ ...bundle.tickets[0].history[0], sequence: 1.5 }],
      }],
    }],
    ['an unsafe lifecycle sequence', {
      ...bundle,
      tickets: [{
        ...bundle.tickets[0],
        history: [{ ...bundle.tickets[0].history[0], sequence: Number.MAX_SAFE_INTEGER + 1 }],
      }],
    }],
  ])('fails a 200 response closed when it contains %s', async (_name, body) => {
    respond(body);
    await expect(getTicketBundle(ORDER_REF)).resolves.toEqual({ ok: false, status: 503 });
  });
});
