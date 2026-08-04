import { describe, expect, it } from 'vitest';

import type { Read, OrderState, SafeTicket } from '../src/lib/api';
import { loadOrderConsole, parseLookup } from '../src/lib/order-console';

const ORDER = '11111111-1111-4111-8111-111111111111';
const REF = '22222222-2222-4222-8222-222222222222';

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
