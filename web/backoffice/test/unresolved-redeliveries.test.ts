import { afterEach, describe, expect, it } from 'vitest';

import {
  MAX_UNRESOLVED_REDELIVERIES,
  UNRESOLVED_REDELIVERY_TTL_MS,
  clearUnresolvedRedelivery,
  noteUnresolvedRedelivery,
  resetUnresolvedRedeliveriesForTest,
  unresolvedRedeliveryFor,
} from '../src/lib/unresolved-redeliveries';

// TKT-203, ai-review F1. The page told the operator that submitting again would replay
// the same request, while minting a NEW key on every render — so the button under that
// sentence sent a second time and burned another slot of the per-order bound.
//
// The invariant these pin, without naming the implementation: after a resend whose
// outcome is unknown, the order keeps offering the key that may have partially executed,
// because only that key can resume it.
describe('an unsettled resend keeps its original key', () => {
  afterEach(resetUnresolvedRedeliveriesForTest);

  const ORG = 'org-1';
  const ORDER = 'abcdef01-2345-4678-89ab-cdef01234567';
  const request = { orderId: ORDER, idempotencyKey: 'the-original-key' };

  it('offers the recorded key back for that organizer and order', () => {
    noteUnresolvedRedelivery(ORG, request);
    expect(unresolvedRedeliveryFor(ORG, ORDER)).toEqual(request);
  });

  // The whole point: a LATER attempt must not replace the first. The first key is the
  // only one access can resume; a later one starts a fresh request and strands the
  // partial work.
  it('keeps the FIRST key when a later attempt is also unsettled', () => {
    noteUnresolvedRedelivery(ORG, request);
    noteUnresolvedRedelivery(ORG, { orderId: ORDER, idempotencyKey: 'a-later-key' });
    expect(unresolvedRedeliveryFor(ORG, ORDER)?.idempotencyKey).toBe('the-original-key');
  });

  // Scoped by organizer AND order, because the risk belongs to the order. Another
  // tenant's identical order id must not see this key.
  it('does not leak an outstanding key to another organizer', () => {
    noteUnresolvedRedelivery(ORG, request);
    expect(unresolvedRedeliveryFor('org-2', ORDER)).toBeUndefined();
  });

  it('does not offer one order key for a different order', () => {
    noteUnresolvedRedelivery(ORG, request);
    expect(unresolvedRedeliveryFor(ORG, 'deadbeef-1234-4567-89ab-cdef01234567')).toBeUndefined();
  });

  it('stops offering the key once access has settled it', () => {
    noteUnresolvedRedelivery(ORG, request);
    clearUnresolvedRedelivery(ORG, ORDER);
    expect(unresolvedRedeliveryFor(ORG, ORDER)).toBeUndefined();
  });

  // Both sides of the TTL boundary. A test that only checked a very old entry would pass
  // just as happily against a TTL of one second.
  it('expires the key after the TTL and not before', () => {
    const t0 = 1_000_000;
    noteUnresolvedRedelivery(ORG, request, t0);
    expect(unresolvedRedeliveryFor(ORG, ORDER, t0 + UNRESOLVED_REDELIVERY_TTL_MS)).toEqual(request);
    expect(
      unresolvedRedeliveryFor(ORG, ORDER, t0 + UNRESOLVED_REDELIVERY_TTL_MS + 1),
    ).toBeUndefined();
  });

  // The eviction DIRECTION is the half worth testing: dropping the newest instead of the
  // oldest would silently discard the entry most likely to still be unsettled.
  it('evicts the oldest entry, not the newest, when full', () => {
    // Every timestamp inside one TTL window, and every read at the same instant the
    // last write happened. The first version of this test spread the writes across
    // milliseconds 1..1000 and read at t=9,000,000 — so the survivor it expected had
    // EXPIRED rather than survived eviction, and the test failed while the eviction
    // direction was correct. Isolate the mechanism under test from the other one.
    const t0 = 1_000_000;
    for (let i = 0; i < MAX_UNRESOLVED_REDELIVERIES; i++) {
      noteUnresolvedRedelivery(ORG, { orderId: `order-${i}`, idempotencyKey: `key-${i}` }, t0 + i);
    }
    const last = t0 + MAX_UNRESOLVED_REDELIVERIES;
    noteUnresolvedRedelivery(ORG, { orderId: 'newest', idempotencyKey: 'newest-key' }, last);

    expect(unresolvedRedeliveryFor(ORG, 'newest', last)?.idempotencyKey).toBe('newest-key');
    expect(unresolvedRedeliveryFor(ORG, 'order-0', last)).toBeUndefined();
    expect(unresolvedRedeliveryFor(ORG, 'order-1', last)?.idempotencyKey).toBe('key-1');
  });
});
