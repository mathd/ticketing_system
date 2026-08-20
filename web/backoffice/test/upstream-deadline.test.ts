import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getVenues, listChannelsForOperator, publishPerformance, updateChannel } from '../src/lib/catalog';
import { getOrderState, refundOrder, type RefundRequest } from '../src/lib/commerce';
import { getStaffAvailability, replaceChannelAllocations } from '../src/lib/inventory';
import { unresolvedRefund } from '../src/lib/order-console';
import { UPSTREAM_DEADLINE_MS } from '../src/lib/upstream';

const ASSERTION =
  'v1.11111111-1111-1111-1111-111111111111.00000000-0000-0000-0000-000000000001.99999999999.testmac';
const ORDER = '11111111-1111-4111-8111-111111111111';
const SLOT = '22222222-2222-4222-8222-222222222222';

type CapturedCall = { signal?: AbortSignal | null; headers: Headers };

function stallUpstream(): CapturedCall[] {
  const calls: CapturedCall[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((_input: RequestInfo | URL, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        const signal = init?.signal;
        calls.push({ signal, headers: new Headers(init?.headers) });
        if (!signal) return;
        const abort = () => reject(new Error('upstream aborted'));
        if (signal.aborted) abort();
        else signal.addEventListener('abort', abort, { once: true });
      }),
    ),
  );
  return calls;
}

beforeEach(() => {
  vi.useFakeTimers();
  process.env.CATALOG_STAFF_WRITE_TOKEN = 'catalog-test-credential';
  process.env.COMMERCE_STAFF_WRITE_TOKEN = 'commerce-test-credential';
  process.env.INVENTORY_STAFF_WRITE_TOKEN = 'inventory-test-credential';
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
  delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
});

describe('back-office upstream operation deadlines', () => {
  it('turns a stalled shared read into unavailable', async () => {
    const calls = stallUpstream();
    const result = getOrderState(ORDER);

    expect(calls[0]?.signal).toBeInstanceOf(AbortSignal);
    await vi.advanceTimersByTimeAsync(UPSTREAM_DEADLINE_MS);

    expect(calls[0]?.signal?.aborted).toBe(true);
    await expect(result).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  it.each([
    ['catalog read', () => getVenues('org-1')],
    ['catalog write', () => publishPerformance(SLOT, ASSERTION)],
    ['operator catalog read', () => listChannelsForOperator(ASSERTION)],
    [
      'operator catalog write',
      () =>
        updateChannel(ASSERTION, 'channel-1', {
          code: 'box-office',
          displayName: 'Box office',
          kind: 'pos',
          enabled: true,
        }),
    ],
    ['inventory read', () => getStaffAvailability(SLOT, 'org-1')],
    [
      'inventory write',
      () => replaceChannelAllocations(SLOT, { organizer_id: 'org-1', allocations: [] }),
    ],
  ])('aborts a stalled %s', async (_name, operation) => {
    const calls = stallUpstream();
    const result = operation();
    const rejection = expect(result).rejects.toThrow('upstream aborted');

    expect(calls[0]?.signal).toBeInstanceOf(AbortSignal);
    await vi.advanceTimersByTimeAsync(UPSTREAM_DEADLINE_MS);

    expect(calls[0]?.signal?.aborted).toBe(true);
    await rejection;
  });

  it('keeps the deadline active while decoding the response body', async () => {
    let signal: AbortSignal | null | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        signal = init?.signal;
        return {
          ok: true,
          status: 200,
          json: () =>
            new Promise((_resolve, reject) => {
              signal?.addEventListener('abort', () => reject(new Error('body aborted')), { once: true });
            }),
        } as Response;
      }),
    );

    const result = getVenues('org-1');
    const rejection = expect(result).rejects.toThrow('body aborted');
    await vi.advanceTimersByTimeAsync(UPSTREAM_DEADLINE_MS);

    expect(signal?.aborted).toBe(true);
    await rejection;
  });

  it('classifies a refund timeout as ambiguous and retains its idempotency key', async () => {
    const calls = stallUpstream();
    const request: RefundRequest = {
      orderId: ORDER,
      quantity: 1,
      reason: 'customer called',
      actor: 'staff-42',
      organizerId: 'org-1',
      idempotencyKey: 'refund-key-1',
    };
    const result = refundOrder(request);

    expect(calls[0]?.signal).toBeInstanceOf(AbortSignal);
    expect(calls[0]?.headers.get('Idempotency-Key')).toBe(request.idempotencyKey);
    await vi.advanceTimersByTimeAsync(UPSTREAM_DEADLINE_MS);

    const outcome = await result;
    expect(outcome).toEqual({
      ok: false,
      kind: 'ambiguous',
      message: 'Commerce could not be reached.',
    });
    expect(unresolvedRefund(outcome, request, null)?.idempotencyKey).toBe(request.idempotencyKey);
  });
});
