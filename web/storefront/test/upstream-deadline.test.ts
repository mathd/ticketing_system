import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  authenticateCustomer,
  claimGuestOrder,
  completePasswordReset,
  listCustomerOrders,
  registerCustomer,
  requestPasswordReset,
} from '../src/lib/customer-api';
import { getTicketBundle, ISSUANCE_TOTAL_BUDGET_MS } from '../src/lib/ticket-api';
import { UPSTREAM_DEADLINE_MS } from '../src/lib/upstream';

type CapturedCall = { signal?: AbortSignal | null };

function stallUpstream(): CapturedCall[] {
  const calls: CapturedCall[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((_input: RequestInfo | URL, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        const signal = init?.signal;
        calls.push({ signal });
        if (!signal) return;
        const abort = () => reject(new Error('upstream aborted'));
        if (signal.aborted) abort();
        else signal.addEventListener('abort', abort, { once: true });
      }),
    ),
  );
  return calls;
}

beforeEach(() => vi.useFakeTimers());

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('storefront upstream operation deadlines', () => {
  it.each([
    [
      'registration',
      () => registerCustomer('buyer@example.test', 'correct horse battery'),
      { ok: false, reason: 'unavailable' },
      UPSTREAM_DEADLINE_MS,
    ],
    [
      'authentication',
      () => authenticateCustomer('buyer@example.test', 'correct horse battery'),
      { ok: false, reason: 'unavailable' },
      UPSTREAM_DEADLINE_MS,
    ],
    [
      'wallet read',
      () => listCustomerOrders('customer-1', 'assertion', 'en'),
      undefined,
      UPSTREAM_DEADLINE_MS,
    ],
    [
      'guest-order claim',
      () => claimGuestOrder('11111111-1111-1111-1111-111111111111', 'assertion'),
      { ok: false, reason: 'unavailable' },
      UPSTREAM_DEADLINE_MS,
    ],
    [
      'password-reset request',
      () => requestPasswordReset('buyer@example.test'),
      { ok: false },
      UPSTREAM_DEADLINE_MS,
    ],
    [
      'password-reset completion',
      () => completePasswordReset('reset-token', 'a new password'),
      { ok: false, reason: 'unavailable' },
      UPSTREAM_DEADLINE_MS,
    ],
    [
      'ticket-bundle read',
      () => getTicketBundle('guest-order-ref'),
      { ok: false, status: 503 },
      // A read that retries carries its own budget. Advancing the shared default
      // here would assert that it aborts EARLIER than it is allowed to.
      ISSUANCE_TOTAL_BUDGET_MS,
    ],
  ])('aborts stalled %s and returns its unavailable result', async (_name, operation, expected, budgetMs) => {
    const calls = stallUpstream();
    const result = operation();

    expect(calls[0]?.signal).toBeInstanceOf(AbortSignal);
    await vi.advanceTimersByTimeAsync(budgetMs);

    expect(calls[0]?.signal?.aborted).toBe(true);
    await expect(result).resolves.toEqual(expected);
  });

  it('keeps the deadline active while decoding an account response', async () => {
    let signal: AbortSignal | null | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        signal = init?.signal;
        return {
          status: 201,
          json: () =>
            new Promise((_resolve, reject) => {
              signal?.addEventListener('abort', () => reject(new Error('body aborted')), { once: true });
            }),
        } as Response;
      }),
    );

    const result = registerCustomer('buyer@example.test', 'correct horse battery');
    await vi.advanceTimersByTimeAsync(UPSTREAM_DEADLINE_MS);

    expect(signal?.aborted).toBe(true);
    await expect(result).resolves.toEqual({ ok: false, reason: 'unavailable' });
  });

  it('clears the deadline after an operation settles', async () => {
    let signal: AbortSignal | null | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        signal = init?.signal;
        return new Response('{}', { status: 409 });
      }),
    );

    await expect(registerCustomer('buyer@example.test', 'correct horse battery')).resolves.toEqual({
      ok: false,
      reason: 'taken',
    });
    await vi.advanceTimersByTimeAsync(UPSTREAM_DEADLINE_MS);

    expect(signal?.aborted).toBe(false);
  });

  it('keeps the ticket issuance retry inside one deadline', async () => {
    const signals: Array<AbortSignal | null | undefined> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        signals.push(init?.signal);
        if (signals.length < 3) return new Response('{}', { status: 404 });
        return new Response(JSON.stringify({ tickets: [] }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        });
      }),
    );

    const result = getTicketBundle('guest-order-ref');
    await vi.advanceTimersByTimeAsync(500);

    await expect(result).resolves.toEqual({ ok: true, value: { tickets: [] } });
    expect(signals).toHaveLength(3);
    expect(signals[0]).toBe(signals[1]);
    expect(signals[1]).toBe(signals[2]);
  });

  // The retry budget must survive REAL attempts, not instant ones. The test above
  // settles on attempt 3 after 500ms of fake time, so it cannot observe the
  // deadline expiring mid-loop: with each attempt costing 200ms of transport the
  // twelve reads and eleven waits run past a 5s bound, and the buyer whose
  // issuance landed just in time was handed a 503. Success on the LAST attempt,
  // at a latency a real access service reaches, is the case that goes red.
  it('still delivers tickets when issuance catches up on the final attempt', async () => {
    const PER_ATTEMPT_MS = 200;
    let attempts = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        attempts += 1;
        const mine = attempts;
        const signal = init?.signal;
        await new Promise<void>((resolve, reject) => {
          const timer = setTimeout(resolve, PER_ATTEMPT_MS);
          signal?.addEventListener(
            'abort',
            () => {
              clearTimeout(timer);
              reject(new Error('upstream aborted'));
            },
            { once: true },
          );
        });
        if (mine < 12) return new Response('{}', { status: 404 });
        return new Response(JSON.stringify({ tickets: [{ ticket_id: 't-1' }] }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        });
      }),
    );

    const result = getTicketBundle('guest-order-ref');
    await vi.advanceTimersByTimeAsync(30_000);

    await expect(result).resolves.toEqual({
      ok: true,
      value: { tickets: [{ ticket_id: 't-1' }] },
    });
    expect(attempts).toBe(12);
  });

  // The budget is a ceiling, not licence to retry for ever: an issuance that
  // never lands must still give the SSR request back rather than spending every
  // attempt against a service that is simply down.
  it('gives up once the issuance budget is spent', async () => {
    let attempts = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        attempts += 1;
        const signal = init?.signal;
        return new Promise<Response>((_resolve, reject) => {
          signal?.addEventListener('abort', () => reject(new Error('upstream aborted')), {
            once: true,
          });
        });
      }),
    );

    const result = getTicketBundle('guest-order-ref');
    await vi.advanceTimersByTimeAsync(60_000);

    await expect(result).resolves.toEqual({ ok: false, status: 503 });
    expect(attempts).toBe(1);
  });
});
