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
const HOLD = '00000000-0000-0000-0000-000000000004';
const BUYER = '00000000-0000-0000-0000-000000000005';
const ORDER = '00000000-0000-0000-0000-000000000006';
const GUEST_ORDER_REF = '00000000-0000-0000-0000-000000000007';
const SLOT = '00000000-0000-0000-0000-000000000008';
const MAP = '00000000-0000-0000-0000-000000000009';
const VENUE = '00000000-0000-0000-0000-000000000010';
const SECTION = '00000000-0000-0000-0000-000000000011';
const ROW = '00000000-0000-0000-0000-000000000012';
const SEAT = '00000000-0000-0000-0000-000000000013';
const SECOND_SEAT = '00000000-0000-0000-0000-000000000014';
const SEAT_IDENTITY = 'Stalls/A/1';
const SECOND_SEAT_IDENTITY = 'Stalls/A/2';

function heldReservation(extra: Record<string, unknown> = {}) {
  const now = new Date('2026-08-03T12:00:00Z');
  return {
    hold_id: HOLD, reservation_id: RESERVATION, buyer_id: BUYER,
    status: 'held', amount: 2500, currency: 'EUR',
    server_time: now.toISOString(),
    expires_at: new Date(now.getTime() + 10 * 60_000).toISOString(),
    ...extra,
  };
}

function seatGeometry(seatIdentity = SEAT_IDENTITY) {
  return {
    map: {
      id: MAP,
      organizer_id: ORG,
      venue_id: VENUE,
      name: 'Stalls',
      status: 'published',
      version: 1,
      published_at: '2026-08-03T12:00:00Z',
      orphan_prevention_enabled: false,
      created_at: '2026-08-03T11:00:00Z',
    },
    sections: [{
      id: SECTION,
      name: 'Stalls',
      position: 1,
      rows: [{
        id: ROW,
        label: 'A',
        position: 1,
        seats: [
          { id: SEAT, seat_identity: seatIdentity, label: '1', position: 1 },
          { id: SECOND_SEAT, seat_identity: SECOND_SEAT_IDENTITY, label: '2', position: 2 },
        ],
      }],
    }],
  };
}

function seatOccupancy() {
  return {
    slot_id: SLOT,
    seat_map_id: MAP,
    offering_status: 'open',
    remaining_capacity: 2,
    unavailable_seat_identities: [],
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

function mountSeated(stub: ReturnType<typeof vi.fn>) {
  vi.stubGlobal('fetch', stub);
  render(
    <HoldPicker
      organizerId={ORG}
      ticketTypeId={TT}
      locale="en"
      slotId={SLOT}
      seatMapId={MAP}
    />,
  );
}

function seatedFetch(
  reservationBody: unknown,
  status = 200,
  seatIdentity = SEAT_IDENTITY,
): ReturnType<typeof vi.fn> {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/api/catalog/public/seat-maps/')) {
      return new Response(JSON.stringify(seatGeometry(seatIdentity)), { status: 200 });
    }
    if (url.includes('/seat-occupancy')) {
      return new Response(JSON.stringify(seatOccupancy()), { status: 200 });
    }
    if (url.includes('/reservations')) {
      return new Response(JSON.stringify(reservationBody), { status });
    }
    throw new Error(`unexpected fetch ${url}`);
  });
}

async function selectAndReserve(
  stub: ReturnType<typeof vi.fn>,
  selectSecondSeat = false,
): Promise<void> {
  mountSeated(stub);
  fireEvent.click(await screen.findByRole('button', { name: /Stalls, row A, seat 1, Available/ }));
  if (selectSecondSeat) {
    fireEvent.click(screen.getByRole('button', { name: /Stalls, row A, seat 2, Available/ }));
  }
  const reserve = screen.getByRole('button', { name: 'Reserve seats' });
  await waitFor(() => expect((reserve as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(reserve);
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

describe('commerce response decoding', () => {
  it.each([
    ['an absent identity', () => {
      const { hold_id: _holdId, ...body } = heldReservation();
      return body;
    }],
    ['a malformed identity', () => ({ ...heldReservation(), buyer_id: 'buyer-1' })],
    ['a non-integer money amount', () => ({ ...heldReservation(), amount: 12.5 })],
    ['a negative money amount', () => ({ ...heldReservation(), amount: -1 })],
    ['a malformed currency', () => ({ ...heldReservation(), currency: 'eur' })],
    ['seats on a general-admission hold', () => heldReservation({ seats: [SEAT_IDENTITY] })],
    ['an unsafe amount', () => ({ ...heldReservation(), amount: Number.MAX_SAFE_INTEGER + 1 })],
    ['an unsafe face value', () => heldReservation({ face_value: Number.MAX_SAFE_INTEGER + 1 })],
    ['an unsafe passed-on fee total', () => heldReservation({ passed_on_fees: Number.MAX_SAFE_INTEGER + 1 })],
    ['a negative face value', () => heldReservation({ face_value: -1 })],
    ['a negative passed-on fee total', () => heldReservation({ passed_on_fees: -1 })],
    ['an unsafe fee amount', () => heldReservation({
      fee_breakdown: [{
        fee_code: 'booking',
        basis: 'per_order_fixed',
        incidence: 'passed_on',
        amount: Number.MAX_SAFE_INTEGER + 1,
        currency: 'EUR',
      }],
    })],
    ['a negative fee amount', () => heldReservation({
      fee_breakdown: [{
        fee_code: 'booking',
        basis: 'per_order_fixed',
        incidence: 'passed_on',
        amount: -1,
        currency: 'EUR',
      }],
    })],
    ['an empty fee code', () => heldReservation({
      fee_breakdown: [{
        fee_code: '',
        basis: 'per_order_fixed',
        incidence: 'passed_on',
        amount: 100,
        currency: 'EUR',
      }],
    })],
    ['an overlong fee code', () => heldReservation({
      fee_breakdown: [{
        fee_code: 'F'.repeat(65),
        basis: 'per_order_fixed',
        incidence: 'passed_on',
        amount: 100,
        currency: 'EUR',
      }],
    })],
    ['an invalid expiry', () => ({ ...heldReservation(), expires_at: 'later' })],
    ['an invalid fee discriminant', () => ({
      ...heldReservation(),
      fee_breakdown: [{
        fee_code: 'booking',
        basis: 'sometimes',
        incidence: 'passed_on',
        amount: 100,
        currency: 'EUR',
      }],
    })],
  ])('does not expose checkout when a 200 reservation contains %s', async (_name, body) => {
    const stub = vi.fn(async () => new Response(JSON.stringify(body()), { status: 200 }));
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByText('Service unavailable');
    expect(screen.queryByRole('button', { name: /^Pay/ })).toBe(null);
  });

  it('accepts the exact JavaScript-safe money ceiling and 64-character fee-code limit', async () => {
    const amount = Number.MAX_SAFE_INTEGER;
    const body = heldReservation({
      amount,
      face_value: 0,
      passed_on_fees: amount,
      fee_breakdown: [{
        fee_code: 'F'.repeat(64),
        basis: 'per_order_fixed',
        incidence: 'passed_on',
        amount,
        currency: 'EUR',
      }],
    });
    const stub = vi.fn(async () => new Response(JSON.stringify(body), { status: 200 }));
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    expect(await screen.findByRole('button', { name: /^Pay/ })).toBeTruthy();
  });

  it.each([
    ['omits seats', heldReservation()],
    ['returns an empty seat set', heldReservation({ seats: [] })],
    ['returns a different seat', heldReservation({ seats: ['Stalls/A/2'] })],
  ])('fails a seated reservation closed when its 200 response %s', async (_name, body) => {
    await selectAndReserve(seatedFetch(body));

    await screen.findByText('Service unavailable');
    expect(screen.queryByRole('button', { name: /^Pay/ })).toBeNull();
  });

  it('accepts a seated reservation only when its returned seat set matches the request', async () => {
    await selectAndReserve(seatedFetch(heldReservation({ seats: [SEAT_IDENTITY] })));

    expect(await screen.findByRole('button', { name: /^Pay/ })).toBeTruthy();
  });

  it('accepts the same multi-seat set in commerce canonical order', async () => {
    await selectAndReserve(seatedFetch(heldReservation({
      seats: [SECOND_SEAT_IDENTITY, SEAT_IDENTITY],
    })), true);

    expect(await screen.findByRole('button', { name: /^Pay/ })).toBeTruthy();
  });

  it('rejects a nonempty strict subset of the submitted seat set', async () => {
    await selectAndReserve(seatedFetch(heldReservation({ seats: [SEAT_IDENTITY] })), true);

    await screen.findByText('Service unavailable');
    expect(screen.queryByRole('button', { name: /^Pay/ })).toBeNull();
  });

  it('accepts a 200-character seat identity on both sides of a seated reservation', async () => {
    const seatIdentity = '🎟'.repeat(200);
    await selectAndReserve(seatedFetch(
      heldReservation({ seats: [seatIdentity] }),
      200,
      seatIdentity,
    ));

    expect(await screen.findByRole('button', { name: /^Pay/ })).toBeTruthy();
  });

  it.each([
    ['omits the refused seats', undefined],
    ['returns an empty refused-seat set', []],
    ['names a seat outside the submitted selection', ['Stalls/A/2']],
  ])('does not apply a seat_taken refusal that %s', async (_name, seatIdentities) => {
    const body = {
      error: 'seat unavailable',
      code: 'seat_taken',
      ...(seatIdentities === undefined ? {} : { seat_identities: seatIdentities }),
    };
    await selectAndReserve(seatedFetch(body, 409));

    await screen.findByText('Quantity unavailable');
    expect(screen.queryByText(/No longer available/)).toBeNull();
  });

  it('applies a seat_taken refusal only to a seat in the submitted selection', async () => {
    await selectAndReserve(seatedFetch({
      error: 'seat unavailable',
      code: 'seat_taken',
      seat_identities: [SEAT_IDENTITY],
    }, 409));

    await screen.findByText(`No longer available: ${SEAT_IDENTITY}`);
  });

  it('accepts a nonempty seat_taken subset without discarding the other submitted seat', async () => {
    await selectAndReserve(seatedFetch({
      error: 'seat unavailable',
      code: 'seat_taken',
      seat_identities: [SEAT_IDENTITY],
    }, 409), true);

    await screen.findByText(`No longer available: ${SEAT_IDENTITY}`);
    expect(screen.getByRole('button', { name: /Stalls, row A, seat 2, Selected/ })).toBeTruthy();
  });

  it.each([
    ['an absent order identity', { guest_order_ref: GUEST_ORDER_REF, status: 'completed' }],
    ['a malformed guest reference', { order_id: ORDER, guest_order_ref: 'ref-1', status: 'completed' }],
    ['an invalid status discriminant', { order_id: ORDER, guest_order_ref: GUEST_ORDER_REF, status: 'pending' }],
  ])('does not confirm an order when a 200 checkout contains %s', async (_name, checkoutBody) => {
    const stub = vi.fn(async (url: RequestInfo | URL) => {
      const body = String(url).includes('/reservations') ? heldReservation() : checkoutBody;
      return new Response(JSON.stringify(body), { status: 200 });
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    fireEvent.click(await screen.findByRole('button', { name: /^Pay/ }));

    await screen.findByText(/Payment status is being checked/);
    expect(screen.queryByText('Order confirmed')).toBe(null);
    expect(screen.queryByRole('link', { name: 'View my tickets' })).toBe(null);
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
        JSON.stringify({ order_id: ORDER, guest_order_ref: GUEST_ORDER_REF, status: 'completed' }),
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

    const keys = keysFor(stub, '/checkout');
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
        JSON.stringify({ order_id: ORDER, guest_order_ref: GUEST_ORDER_REF, status: 'completed' }),
        { status: 200 },
      );
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    fireEvent.click(await screen.findByRole('button', { name: /^Pay/ }));
    await screen.findByText('Order confirmed');

    // Distinct keyspaces: commerce derives the reservation id from one and the order's
    // stored key from the other, so sharing a value would collide two identities.
    expect(keysFor(stub, '/checkout')[0]).not.toBe(keysFor(stub, '/reservations')[0]);
  });
});
