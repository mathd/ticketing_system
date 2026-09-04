import { beforeEach, describe, expect, it } from 'vitest';

import {
  MAX_UNRESOLVED_REDELIVERIES,
  UNRESOLVED_REDELIVERY_TTL_MS,
  createUnresolvedRedeliveryTracker,
} from '../src/lib/unresolved-redeliveries';

// After a resend whose
// outcome is unknown, the order keeps offering the key that may have partially executed,
// because only that key can resume it.
describe('an unsettled resend keeps its original key', () => {
  const ORG = 'org-1';
  const ORDER = 'abcdef01-2345-4678-89ab-cdef01234567';
  const request = { orderId: ORDER, idempotencyKey: 'the-original-key' };
  let now = 1_000_000;
  let redeliveries = createUnresolvedRedeliveryTracker(() => now);

  beforeEach(() => {
    now = 1_000_000;
    redeliveries = createUnresolvedRedeliveryTracker(() => now);
  });

  it('keeps independent tracker instances isolated', () => {
    const other = createUnresolvedRedeliveryTracker(() => now);
    redeliveries.note(ORG, request);

    expect(other.find(ORG, ORDER)).toBeUndefined();
    expect(redeliveries.find(ORG, ORDER)).toEqual(request);
  });

  it('offers the recorded key back for that organizer and order', () => {
    redeliveries.note(ORG, request);
    expect(redeliveries.find(ORG, ORDER)).toEqual(request);
  });

  // The whole point: a LATER attempt must not replace the first. The first key is the
  // only one access can resume; a later one starts a fresh request and strands the
  // partial work.
  it('keeps the FIRST key when a later attempt is also unsettled', () => {
    redeliveries.note(ORG, request);
    redeliveries.note(ORG, { orderId: ORDER, idempotencyKey: 'a-later-key' });
    expect(redeliveries.find(ORG, ORDER)?.idempotencyKey).toBe('the-original-key');
  });

  // Scoped by organizer AND order, because the risk belongs to the order. Another
  // tenant's identical order id must not see this key.
  it('does not leak an outstanding key to another organizer', () => {
    redeliveries.note(ORG, request);
    expect(redeliveries.find('org-2', ORDER)).toBeUndefined();
  });

  it('does not offer one order key for a different order', () => {
    redeliveries.note(ORG, request);
    expect(redeliveries.find(ORG, 'deadbeef-1234-4567-89ab-cdef01234567')).toBeUndefined();
  });

  it('stops offering the key once access has settled it', () => {
    redeliveries.note(ORG, request);
    redeliveries.clear(ORG, ORDER);
    expect(redeliveries.find(ORG, ORDER)).toBeUndefined();
  });

  // Both sides of the TTL boundary. A test that only checked a very old entry would pass
  // just as happily against a TTL of one second.
  it('expires the key after the TTL and not before', () => {
    redeliveries.note(ORG, request);
    now += UNRESOLVED_REDELIVERY_TTL_MS;
    expect(redeliveries.find(ORG, ORDER)).toEqual(request);
    now++;
    expect(redeliveries.find(ORG, ORDER)).toBeUndefined();
  });

  // The eviction DIRECTION is the half worth testing: dropping the newest instead of the
  // oldest would silently discard the entry most likely to still be unsettled.
  it('evicts the oldest entry, not the newest, when full', () => {
    // Keep every write and read inside one TTL window so expiry cannot hide the
    // eviction direction.
    for (let i = 0; i < MAX_UNRESOLVED_REDELIVERIES; i++) {
      redeliveries.note(ORG, { orderId: `order-${i}`, idempotencyKey: `key-${i}` });
      now++;
    }
    redeliveries.note(ORG, { orderId: 'newest', idempotencyKey: 'newest-key' });

    expect(redeliveries.find(ORG, 'newest')?.idempotencyKey).toBe('newest-key');
    expect(redeliveries.find(ORG, 'order-0')).toBeUndefined();
    expect(redeliveries.find(ORG, 'order-1')?.idempotencyKey).toBe('key-1');
  });
});
