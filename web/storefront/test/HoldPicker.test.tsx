// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import HoldPicker, { reservationTerms } from '../src/components/HoldPicker';

// TKT-184. Both commerce endpoints key off `Idempotency-Key`, and the storefront used to
// mint a fresh uuid on every attempt — which is the same as sending none.
//
// The two failures are different and both silent:
//  - reserve: commerce DERIVES the reservation id from the key, so a retry under a new
//    key takes out a SECOND hold. Nothing errors; the seats are just held twice.
//  - checkout: commerce compares the incoming key against the one stored on the order
//    and answers 409 when they differ, forever. The buyer cannot finish paying.
//
// These assert on the header the component sends, because that IS the contract — a test
// that only checked "a request was made" passes against the bug.

const ORG = '00000000-0000-0000-0000-000000000001';
const TT = '00000000-0000-0000-0000-000000000002';
const RESERVATION = '00000000-0000-0000-0000-000000000003';

function heldReservation() {
  const now = new Date('2026-08-03T12:00:00Z');
  return {
    hold_id: 'hold-1', reservation_id: RESERVATION, buyer_id: 'buyer-1',
    status: 'held', amount: 2500, currency: 'EUR',
    server_time: now.toISOString(),
    expires_at: new Date(now.getTime() + 10 * 60_000).toISOString(),
  };
}

/** Every Idempotency-Key sent to a given path, in order. */
function keysFor(stub: ReturnType<typeof vi.fn>, path: string): string[] {
  return stub.mock.calls
    .filter(([url]) => String(url).includes(path))
    .map(([, init]) => new Headers((init as RequestInit).headers).get('Idempotency-Key') ?? '');
}

function mountGA(stub: ReturnType<typeof vi.fn>) {
  vi.stubGlobal('fetch', stub);
  // No slotId/seatMapId: the GA path, so the seat map never mounts and the only thing
  // under test is the reservation/checkout pair.
  render(<HoldPicker organizerId={ORG} ticketTypeId={TT} locale="en" />);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('reservationTerms', () => {
  it('is order-independent for seats, because commerce compares the SET', () => {
    expect(reservationTerms(true, ['B/2', 'A/1'], 1)).toBe(reservationTerms(true, ['A/1', 'B/2'], 1));
  });

  it('does not reorder the caller\'s selection as a side effect of naming it', () => {
    const seats = ['B/2', 'A/1'];
    reservationTerms(true, seats, 1);
    expect(seats).toEqual(['B/2', 'A/1']);
  });

  it('separates a GA quantity from a seat list, and distinguishes different terms', () => {
    expect(reservationTerms(false, [], 2)).not.toBe(reservationTerms(false, [], 3));
    expect(reservationTerms(false, [], 1)).not.toBe(reservationTerms(true, ['A/1'], 1));
  });
});

describe('reserve idempotency', () => {
  it('replays a failed reserve under the SAME key rather than taking a second hold', async () => {
    const stub = vi.fn(async () => { throw new TypeError('network'); });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByText('Service unavailable');
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations').length).toBe(2));

    const [first, second] = keysFor(stub, '/reservations');
    expect(first).not.toBe('');
    expect(second).toBe(first);
  });

  it('mints a NEW key when the terms change, which commerce would otherwise refuse', async () => {
    const stub = vi.fn(async () => { throw new TypeError('network'); });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations').length).toBe(1));

    fireEvent.change(screen.getByLabelText('Quantity'), { target: { value: '3' } });
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations').length).toBe(2));

    const [first, second] = keysFor(stub, '/reservations');
    expect(second).not.toBe(first);
  });
});

describe('checkout idempotency', () => {
  // The dead end this closes: first attempt binds the order under key K, the response is
  // lost, the retry sends a new key, commerce answers 409 on the mismatch — and every
  // later attempt does the same. A stable key makes the retry a replay instead.
  it('retries checkout under the SAME key, and says the 409 is temporary', async () => {
    let checkouts = 0;
    const stub = vi.fn(async (url: RequestInfo | URL) => {
      if (String(url).includes('/reservations')) {
        return new Response(JSON.stringify(heldReservation()), { status: 200 });
      }
      checkouts += 1;
      // First: commerce is holding the order under a recovery lease. Then it clears.
      if (checkouts === 1) {
        return new Response(JSON.stringify({ error: 'this order is being recovered; retry shortly' }), { status: 409 });
      }
      return new Response(
        JSON.stringify({ order_id: 'order-1', guest_order_ref: 'ref-1', status: 'completed' }),
        { status: 200 },
      );
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    const pay = await screen.findByRole('button', { name: /^Pay/ });

    fireEvent.click(pay);
    // A 409 must NOT read as the ambiguous "being checked": it clears on its own, and
    // leaving the buyer on a message that implies nothing to do is how they abandon.
    // Substring, not exact: the status span also carries the live countdown while the
    // hold is alive, which is exactly the state a retryable 409 leaves the buyer in.
    await screen.findByText(/This order is being finalised/);

    fireEvent.click(screen.getByRole('button', { name: /^Pay/ }));
    await screen.findByText('Order confirmed');

    const keys = keysFor(stub, '/orders');
    expect(keys.length).toBe(2);
    expect(keys[0]).not.toBe('');
    expect(keys[1]).toBe(keys[0]);
  });

  it('uses a different key from the reserve that produced the reservation', async () => {
    const stub = vi.fn(async (url: RequestInfo | URL) => {
      if (String(url).includes('/reservations')) {
        return new Response(JSON.stringify(heldReservation()), { status: 200 });
      }
      return new Response(
        JSON.stringify({ order_id: 'order-1', guest_order_ref: 'ref-1', status: 'completed' }),
        { status: 200 },
      );
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    fireEvent.click(await screen.findByRole('button', { name: /^Pay/ }));
    await screen.findByText('Order confirmed');

    // Distinct keyspaces: commerce derives the reservation id from one and the order's
    // stored key from the other, so sharing a value would collide two identities.
    expect(keysFor(stub, '/orders')[0]).not.toBe(keysFor(stub, '/reservations')[0]);
  });
});
