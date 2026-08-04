import { describe, expect, it } from 'vitest';

import type { Read, OrderState, SafeTicket } from '../src/lib/api';
import { loadOrderConsole, parseLookup, parseRefund } from '../src/lib/order-console';

const ORDER = '11111111-1111-4111-8111-111111111111';
const REF = '22222222-2222-4222-8222-222222222222';
const KEY = 'abcdef01-2345-4678-89ab-cdef01234567';

const foundOrder: Read<OrderState> = { ok: true, value: { orderId: ORDER, status: 'completed' } };
const foundTickets: Read<SafeTicket[]> = {
  ok: true,
  value: [{ ticketId: 't1', issuedAt: '2026-08-01T10:00:00Z', history: [] }],
};
const missing = { ok: false, kind: 'not-found' } as const;
const broken = { ok: false, kind: 'unavailable' } as const;

/** Reads that answer from a script, so no live stack and no global fetch stub. */
const reads = (order: Read<OrderState>, tickets: Read<SafeTicket[]>) => ({
  order: async () => order,
  tickets: async () => tickets,
});

describe('what the console asks for (COS-2)', () => {
  it('refuses an empty submission rather than calling two services with nothing', () => {
    expect(parseLookup('', '')).toEqual({
      ok: false,
      message: 'Enter an order id, a ticket reference, or both.',
    });
  });

  // A1: BOTH are optional. A support agent has whatever the customer has, and
  // what the customer has is their ticket link — which carries the reference and
  // never the order id. Requiring both would make the page unusable in the case
  // it exists for.
  it('accepts either identifier alone', () => {
    expect(parseLookup(ORDER, '')).toEqual({ ok: true, value: { orderId: ORDER } });
    expect(parseLookup('', REF)).toEqual({ ok: true, value: { ref: REF } });
    expect(parseLookup(ORDER, REF)).toEqual({ ok: true, value: { orderId: ORDER, ref: REF } });
  });

  // A3: refuse locally instead of rendering an upstream 400. One fewer state in
  // the matrix, and a typo never reaches two services.
  it('refuses a malformed identifier before any request leaves', () => {
    expect(parseLookup('not-a-uuid', '')).toEqual({
      ok: false,
      message: 'That order id is not a valid identifier.',
    });
    expect(parseLookup('', 'nope')).toEqual({
      ok: false,
      message: 'That ticket reference is not a valid identifier.',
    });
  });

  it('ignores surrounding whitespace, which pasting always brings', () => {
    expect(parseLookup(`  ${ORDER} `, '')).toEqual({ ok: true, value: { orderId: ORDER } });
  });
});

describe('the two reads fail independently (COS-2)', () => {
  it('asks only for what it was given', async () => {
    const view = await loadOrderConsole({ orderId: ORDER }, reads(foundOrder, foundTickets));
    expect(view.order).toEqual(foundOrder);
    expect(view.tickets).toBeUndefined();
    expect(view.status).toBe(200);
  });

  it.each([
    ['both answer', foundOrder, foundTickets, 200],
    ['status found, tickets unknown', foundOrder, missing, 200],
    ['status unknown, tickets found', missing, foundTickets, 200],
    ['neither is known', missing, missing, 404],
    // An outage is never a 404: telling a support agent "no such order" when
    // commerce is merely down makes them tell the customer their order does not
    // exist.
    ['neither answers', broken, broken, 503],
    ['one is down, the other has nothing', broken, missing, 503],
    // A half that succeeded is still worth rendering — it is the half the agent
    // came for as often as not.
    ['one is down, the other answers', broken, foundTickets, 200],
  ])('%s → %i', async (_name, order, tickets, status) => {
    const view = await loadOrderConsole({ orderId: ORDER, ref: REF }, reads(order, tickets));
    expect(view.status).toBe(status);
    expect(view.order).toEqual(order);
    expect(view.tickets).toEqual(tickets);
  });
});

describe('what the refund form is allowed to carry (TKT-194)', () => {
  // COS-5. `actor` and `organizer_id` come from the session. A refund
  // attributable to a value the browser supplied is not attributable — and with
  // box office able to refund, attribution is the control that remains.
  it('ignores actor, organizer and amount if the browser sends them', () => {
    const form = new Map<string, string>([
      ['order_id', ORDER],
      ['quantity', '2'],
      ['reason', 'customer called'],
      ['idempotency_key', KEY],
      ['actor', 'someone-else'],
      ['organizer_id', 'another-org'],
      ['amount', '999999'],
    ]);
    expect(parseRefund((k) => form.get(k) ?? '')).toEqual({
      ok: true,
      value: { orderId: ORDER, quantity: 2, reason: 'customer called', idempotencyKey: KEY },
    });
  });

  it.each([
    ['no order id', { order_id: '', quantity: '1', reason: 'r', idempotency_key: KEY }, 'order id'],
    ['a malformed order id', { order_id: 'nope', quantity: '1', reason: 'r', idempotency_key: KEY }, 'order id'],
    ['quantity zero', { order_id: ORDER, quantity: '0', reason: 'r', idempotency_key: KEY }, 'between 1 and 50'],
    ['quantity above the contract maximum', { order_id: ORDER, quantity: '51', reason: 'r', idempotency_key: KEY }, 'between 1 and 50'],
    ['a fractional quantity', { order_id: ORDER, quantity: '1.5', reason: 'r', idempotency_key: KEY }, 'between 1 and 50'],
    ['a non-numeric quantity', { order_id: ORDER, quantity: 'two', reason: 'r', idempotency_key: KEY }, 'between 1 and 50'],
    // Number() accepts more than digits. '0x10' is 16 and '1e1' is 10 — both
    // whole numbers inside 1..50, so the range check alone admits them and an
    // operator refunding "0x10" tickets means something they did not type.
    // Found by mutation check M15, which survived until these existed.
    ['a hexadecimal quantity', { order_id: ORDER, quantity: '0x10', reason: 'r', idempotency_key: KEY }, 'between 1 and 50'],
    ['an exponent quantity', { order_id: ORDER, quantity: '1e1', reason: 'r', idempotency_key: KEY }, 'between 1 and 50'],
    ['no reason', { order_id: ORDER, quantity: '1', reason: '   ', idempotency_key: KEY }, 'reason'],
    // The key is minted server-side when the form renders. A missing or
    // malformed one means this submission cannot be made idempotent, and
    // refunding without that is how a double-click refunds twice.
    ['no idempotency key', { order_id: ORDER, quantity: '1', reason: 'r', idempotency_key: '' }, 'try the lookup again'],
    ['a malformed idempotency key', { order_id: ORDER, quantity: '1', reason: 'r', idempotency_key: 'nope' }, 'try the lookup again'],
  ])('refuses %s locally, before any request leaves', (_name, fields, want) => {
    const got = parseRefund((k) => (fields as Record<string, string>)[k] ?? '');
    expect(got.ok).toBe(false);
    if (!got.ok) expect(got.message).toContain(want);
  });

  it('trims the reason but keeps it whole', () => {
    const fields = { order_id: ORDER, quantity: '1', reason: '  duplicate charge  ', idempotency_key: KEY };
    const got = parseRefund((k) => (fields as Record<string, string>)[k] ?? '');
    expect(got.ok && got.value.reason).toBe('duplicate charge');
  });
});
