import { beforeEach, describe, expect, it } from 'vitest';

import {
  MAX_UNRESOLVED,
  UNRESOLVED_TTL_MS,
  clearUnresolvedRefund,
  noteUnresolvedRefund,
  resetUnresolvedForTest,
  unresolvedRefundFor,
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

beforeEach(resetUnresolvedForTest);

describe('an outstanding refund outlives the request that produced it (TKT-194)', () => {
  // The pass-4 finding. The simplest recovery an operator reaches for after a
  // failed refund is looking the order up again — and commerce leaves the order
  // `completed` with quantity refundable, so a page consulting only request
  // state would offer a freshly keyed refund form. This is what makes the
  // lookup path safe.
  it('is still there on a later, unrelated request', () => {
    noteUnresolvedRefund(ORG, req(ORDER, K1));
    expect(unresolvedRefundFor(ORG, ORDER)).toMatchObject({ idempotencyKey: K1 });
  });

  it('is scoped to the order, not to whoever submitted it', () => {
    noteUnresolvedRefund(ORG, req(ORDER, K1));
    // A different operator on the same order sees it — the shift-change case
    // that made a 409 look like a settled answer in pass 3.
    expect(unresolvedRefundFor(ORG, ORDER)).toBeDefined();
    expect(unresolvedRefundFor(ORG, OTHER_ORDER)).toBeUndefined();
    expect(unresolvedRefundFor('another-org', ORDER)).toBeUndefined();
  });

  // The forged/stale-provenance defect, from the other side: the store is the
  // only source of the key, so a later request naming a different key cannot
  // displace the one that actually became ambiguous.
  it('keeps the FIRST key, and a later attempt does not replace it', () => {
    noteUnresolvedRefund(ORG, req(ORDER, K1));
    noteUnresolvedRefund(ORG, req(ORDER, K2));
    expect(unresolvedRefundFor(ORG, ORDER)).toMatchObject({ idempotencyKey: K1 });
  });

  it('is gone once commerce has settled it', () => {
    noteUnresolvedRefund(ORG, req(ORDER, K1));
    clearUnresolvedRefund(ORG, ORDER);
    expect(unresolvedRefundFor(ORG, ORDER)).toBeUndefined();
  });

  it('expires, so one outage does not block an order forever', () => {
    noteUnresolvedRefund(ORG, req(ORDER, K1), 1_000);
    expect(unresolvedRefundFor(ORG, ORDER, 1_000 + UNRESOLVED_TTL_MS)).toBeDefined();
    expect(unresolvedRefundFor(ORG, ORDER, 1_000 + UNRESOLVED_TTL_MS + 1)).toBeUndefined();
  });

  // Bounded like the session map, and bounded in the direction that keeps the
  // newest entries: dropping a record is what costs money, so the eviction must
  // not throw away what just happened.
  it('stays bounded, evicting the oldest', () => {
    for (let i = 0; i < MAX_UNRESOLVED; i++) {
      noteUnresolvedRefund(ORG, req(`order-${i}`, `key-${i}`));
    }
    noteUnresolvedRefund(ORG, req('one-more', K2));
    expect(unresolvedRefundFor(ORG, 'one-more')).toBeDefined();
    expect(unresolvedRefundFor(ORG, 'order-0')).toBeUndefined();
    expect(unresolvedRefundFor(ORG, `order-${MAX_UNRESOLVED - 1}`)).toBeDefined();
  });
});
