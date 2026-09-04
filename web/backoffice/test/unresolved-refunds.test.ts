import { beforeEach, describe, expect, it } from 'vitest';

import {
  MAX_UNRESOLVED,
  UNRESOLVED_TTL_MS,
  createUnresolvedRefundTracker,
} from '../src/lib/unresolved-refunds';

const ORG = 'org-1';
const ORDER = '11111111-1111-4111-8111-111111111111';
const OTHER_ORDER = '22222222-2222-4222-8222-222222222222';
const K1 = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
const K2 = 'ffffffff-1111-4222-8333-444444444444';

const req = (orderId: string, idempotencyKey: string) => ({
  orderId,
  idempotencyKey,
  quantity: 1,
  reason: 'customer called',
});

let now = 1_000_000;
let refunds = createUnresolvedRefundTracker(() => now);

beforeEach(() => {
  now = 1_000_000;
  refunds = createUnresolvedRefundTracker(() => now);
});

describe('an outstanding refund outlives the request that produced it (TKT-194)', () => {
  it('keeps independent tracker instances isolated', () => {
    const other = createUnresolvedRefundTracker(() => now);
    refunds.note(ORG, req(ORDER, K1));

    expect(other.find(ORG, ORDER)).toBeUndefined();
    expect(refunds.find(ORG, ORDER)).toMatchObject({ idempotencyKey: K1 });
  });

  // Commerce leaves the order `completed` with quantity still refundable. A
  // later lookup must retain the unsettled key instead of offering a fresh one.
  it('is still there on a later, unrelated request', () => {
    refunds.note(ORG, req(ORDER, K1));
    expect(refunds.find(ORG, ORDER)).toMatchObject({ idempotencyKey: K1 });
  });

  it('is scoped to the order, not to whoever submitted it', () => {
    refunds.note(ORG, req(ORDER, K1));
    // A different operator on the same order sees it across a shift change.
    expect(refunds.find(ORG, ORDER)).toBeDefined();
    expect(refunds.find(ORG, OTHER_ORDER)).toBeUndefined();
    expect(refunds.find('another-org', ORDER)).toBeUndefined();
  });

  // The forged/stale-provenance defect, from the other side: the store is the
  // only source of the key, so a later request naming a different key cannot
  // displace the one that actually became ambiguous.
  it('keeps the FIRST key, and a later attempt does not replace it', () => {
    refunds.note(ORG, req(ORDER, K1));
    refunds.note(ORG, req(ORDER, K2));
    expect(refunds.find(ORG, ORDER)).toMatchObject({ idempotencyKey: K1 });
  });

  it('is gone once commerce has settled it', () => {
    refunds.note(ORG, req(ORDER, K1));
    expect(refunds.clear(ORG, ORDER, K1)).toBe(true);
    expect(refunds.find(ORG, ORDER)).toBeUndefined();
  });

  it('does not clear an outstanding request when another key settles', () => {
    refunds.note(ORG, req(ORDER, K1));

    expect(refunds.clear(ORG, ORDER, K2)).toBe(false);
    expect(refunds.find(ORG, ORDER)).toMatchObject({ idempotencyKey: K1 });
  });

  it('expires, so one outage does not block an order forever', () => {
    now = 1_000;
    refunds.note(ORG, req(ORDER, K1));
    now = 1_000 + UNRESOLVED_TTL_MS;
    expect(refunds.find(ORG, ORDER)).toBeDefined();
    now++;
    expect(refunds.find(ORG, ORDER)).toBeUndefined();
  });

  // Bounded like the session map, and bounded in the direction that keeps the
  // newest entries: dropping a record is what costs money, so the eviction must
  // not throw away what just happened.
  it('stays bounded, evicting the oldest', () => {
    for (let i = 0; i < MAX_UNRESOLVED; i++) {
      refunds.note(ORG, req(`order-${i}`, `key-${i}`));
    }
    refunds.note(ORG, req('one-more', K2));
    expect(refunds.find(ORG, 'one-more')).toBeDefined();
    expect(refunds.find(ORG, 'order-0')).toBeUndefined();
    expect(refunds.find(ORG, `order-${MAX_UNRESOLVED - 1}`)).toBeDefined();
  });
});
