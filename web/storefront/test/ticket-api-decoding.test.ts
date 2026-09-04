import { afterEach, describe, expect, it, vi } from 'vitest';

import { getTicketBundle } from '../src/lib/ticket-api';

const ORDER_REF = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1';
const OTHER_ORDER_REF = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2';
const TICKET = 'cccccccc-cccc-4ccc-8ccc-ccccccccccc3';
const EVENT = 'dddddddd-dddd-4ddd-8ddd-ddddddddddd4';

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
  }],
};

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
      }],
    }],
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
