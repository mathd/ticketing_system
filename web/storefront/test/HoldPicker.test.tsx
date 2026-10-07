// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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

// TKT-289. A reservation can arrive with its TTL already elapsed: inventory admitted it on
// transaction-start time while the request queued (ADR-024), and commerce passes through an
// advancing server_time, so remaining time is ZERO on arrival. Two defects followed: the page
// labelled it "Held for" with no timer and no way to pay, and pressing Reserve again REPLAYED
// the same dead hold, because the key rotated only when the terms changed. The same replay
// trap followed an ORDINARY expiry. The rule now: once a hold is known dead, the next
// Reserve mints a fresh key; while it is live, or while an outcome is unknown, the key stays.
describe('a dead hold is never replayed', () => {
  const T0 = '2026-08-03T12:00:00.000Z';
  const LIVE_RESERVATION = '00000000-0000-0000-0000-000000000021';
  const LIVE_HOLD = '00000000-0000-0000-0000-000000000022';

  function deadOnArrival() {
    return heldReservation({ server_time: T0, expires_at: T0 });
  }

  function liveFor(ms: number) {
    return heldReservation({
      reservation_id: LIVE_RESERVATION, hold_id: LIVE_HOLD,
      server_time: T0, expires_at: new Date(Date.parse(T0) + ms).toISOString(),
    });
  }

  function reservationsReturning(...bodies: unknown[]): ReturnType<typeof vi.fn> {
    let call = 0;
    return vi.fn(async (input: RequestInfo | URL) => {
      if (!String(input).includes('/reservations')) throw new Error(`unexpected fetch ${String(input)}`);
      const body = bodies[Math.min(call, bodies.length - 1)];
      call += 1;
      return new Response(JSON.stringify(body), { status: 200 });
    });
  }

  function bodiesFor(stub: ReturnType<typeof vi.fn>, path: string): string[] {
    return stub.mock.calls
      .filter(([url]) => String(url).includes(path))
      .map(([, init]) => String((init as RequestInit).body));
  }

  afterEach(() => {
    vi.useRealTimers();
  });

  // COS1: zero remaining time means expired NOW. Interval timers are frozen, so no tick can
  // rescue the assertion: the expired state must come from the response handler itself.
  it('renders a dead-on-arrival hold as expired synchronously, never as held', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const stub = reservationsReturning(deadOnArrival());
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByText('Hold expired');
    expect(screen.queryByText(/Held for/)).toBeNull();
    expect(screen.queryByRole('button', { name: /Pay/ })).toBeNull();
    expect((screen.getByRole('button', { name: 'Reserve' }) as HTMLButtonElement).disabled).toBe(false);
  });

  // COS2, the deliverable: unchanged terms after a dead hold send a DIFFERENT key, so commerce
  // takes out a fresh hold instead of replaying the dead one.
  it('re-reserves a dead-on-arrival hold under a NEW key for unchanged terms', async () => {
    const stub = reservationsReturning(deadOnArrival(), liveFor(10 * 60_000));
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByText('Hold expired');
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });

    const [first, second] = keysFor(stub, '/reservations');
    const [firstBody, secondBody] = bodiesFor(stub, '/reservations');
    expect(keysFor(stub, '/reservations')).toHaveLength(2);
    expect(first).not.toBe('');
    expect(second).not.toBe('');
    expect(secondBody).toBe(firstBody); // unchanged terms: the rotation is not the terms rule
    expect(second).not.toBe(first);
  });

  // COS3: the same trap after an ORDINARY expiry, reached by the countdown. performance.now
  // is faked with the intervals, so advancing time moves the deadline the tick reads.
  it('re-reserves under a NEW key after the countdown expires a live hold', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const stub = reservationsReturning(liveFor(1000), liveFor(10 * 60_000));
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    await countdownRunning();

    act(() => { vi.advanceTimersByTime(750); });
    expect(screen.queryByRole('button', { name: /Pay/ })).not.toBeNull(); // still live
    act(() => { vi.advanceTimersByTime(500); });
    await screen.findByText('Hold expired');
    expect(screen.queryByRole('button', { name: /Pay/ })).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    const [first, second] = keysFor(stub, '/reservations');
    const [firstBody, secondBody] = bodiesFor(stub, '/reservations');
    expect(secondBody).toBe(firstBody);
    expect(second).not.toBe(first);
  });

  // A fetch stub whose responses are queued per path, each either a body or a deferred
  // promise the test settles later, so a test can hold a request open across a tick.
  type Reply = { status: number; body: unknown } | Promise<Response>;
  function queuedFetch(queues: Record<string, Reply[]>): ReturnType<typeof vi.fn> {
    return vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      const path = Object.keys(queues).find((p) => url.includes(p));
      if (!path) throw new Error(`unexpected fetch ${url}`);
      const next = queues[path].shift();
      if (!next) throw new Error(`no reply queued for ${url}`);
      if (next instanceof Promise) return next;
      return new Response(JSON.stringify(next.body), { status: next.status });
    });
  }

  function deferred() {
    let settle!: (r: Response | Error) => void;
    const promise = new Promise<Response>((resolve, reject) => {
      settle = (r) => (r instanceof Error ? reject(r) : resolve(r));
    });
    return { promise, settle };
  }

  // A BARRIER, not a sleep: the countdown interval is registered by a React passive effect,
  // which can run after the Pay button is already in the DOM. Advancing fake time before it
  // exists ticks nothing, and the test either flakes or — worse — passes vacuously under a
  // mutation (TKT-289: 3 of 18 parallel runs failed this way before the barrier).
  async function countdownRunning() {
    await waitFor(() => expect(vi.getTimerCount()).toBeGreaterThan(0));
  }

  // Review pass 1 [high]: an old dead hold must not retire a NEWER request's key. Reserve is
  // enabled right after a dead-on-arrival response, so a second request can be in flight
  // when an old timer would fire; if its key were retired and its response lost, the retry
  // would take a second hold. The pending request's replay must keep its key.
  it('never lets a dead hold retire the key of a newer request in flight', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const pending = deferred();
    const stub = queuedFetch({ '/reservations': [{ status: 200, body: deadOnArrival() }, pending.promise, { status: 200, body: liveFor(60_000) }] });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByText('Hold expired');
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    act(() => { vi.advanceTimersByTime(1000); }); // where an old countdown would have fired
    await act(async () => { pending.settle(new TypeError('network')); });
    await screen.findByText('Service unavailable');

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(3));
    const [, second, third] = keysFor(stub, '/reservations');
    expect(third).toBe(second); // the unknown outcome replays; it does not take a second hold
  });

  // Review pass 1 [high]: once a checkout was attempted, the hold may be finalizing — live past
  // its TTL in inventory (ADR-024). If the countdown ends while the outcome is unknown, Reserve
  // must REPLAY the same reservation, whose checkout key replays the payment, not mint a key
  // that would take a second reservation the buyer could pay for twice.
  it('keeps the reserve key when the countdown ends during an unresolved checkout', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const payment = deferred();
    // The replay returns the SAME reservation, now with no time left: inventory keeps a
    // finalizing hold past its TTL and commerce passes the advancing server_time through.
    const expiredReplay = heldReservation({
      reservation_id: LIVE_RESERVATION, hold_id: LIVE_HOLD, server_time: T0, expires_at: T0,
    });
    const stub = queuedFetch({
      '/reservations': [{ status: 200, body: liveFor(1000) }, { status: 200, body: expiredReplay }, { status: 200, body: expiredReplay }],
      '/checkout': [payment.promise],
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    await countdownRunning();
    fireEvent.click(screen.getByRole('button', { name: /Pay/ }));
    act(() => { vi.advanceTimersByTime(1250); });
    await screen.findByText('Hold expired');
    await act(async () => { payment.settle(new Response('{}', { status: 500 })); });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Reserve' }) as HTMLButtonElement).disabled).toBe(false));

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    // Review pass 2 [high]: the replay comes back DEAD, and that must not retire the key
    // either — the reservation is still depended on by the unresolved checkout.
    await screen.findByText('Hold expired');
    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(3));
    const [first, second, third] = keysFor(stub, '/reservations');
    expect(second).toBe(first);
    expect(third).toBe(first);
  });

  // Review pass 2 [medium]: a FIRST checkout refused 401 created nothing (commerce checks the
  // assertion before any order exists), so the hold is free again and expiry retires its key.
  it('re-reserves under a NEW key after expiry when the only checkout was refused 401', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const stub = queuedFetch({
      '/reservations': [{ status: 200, body: liveFor(1000) }, { status: 200, body: liveFor(60_000) }],
      '/checkout': [{ status: 401, body: { error: 'unauthorized' } }],
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    await countdownRunning();
    fireEvent.click(screen.getByRole('button', { name: /Pay/ }));
    await waitFor(() => expect(keysFor(stub, '/checkout')).toHaveLength(1));
    await screen.findByText(/sign in/i);
    act(() => { vi.advanceTimersByTime(1250); });
    await screen.findByText('Hold expired');

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    const [first, second] = keysFor(stub, '/reservations');
    expect(second).not.toBe(first);
  });

  // The countdown can end while that refused checkout is still in flight: once it settles
  // with nothing created, the dead hold's key is retired then.
  it('retires the key when a 401 settles after the countdown ended mid-checkout', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const payment = deferred();
    const stub = queuedFetch({
      '/reservations': [{ status: 200, body: liveFor(1000) }, { status: 200, body: liveFor(60_000) }],
      '/checkout': [payment.promise],
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    await countdownRunning();
    fireEvent.click(screen.getByRole('button', { name: /Pay/ }));
    act(() => { vi.advanceTimersByTime(1250); });
    await screen.findByText('Hold expired');
    await act(async () => { payment.settle(new Response('{"error":"unauthorized"}', { status: 401 })); });
    await screen.findByText(/sign in/i);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    const [first, second] = keysFor(stub, '/reservations');
    expect(second).not.toBe(first);
  });

  // Decision audit [high]: a COMPLETED checkout is not a dead hold. Past the original deadline
  // the page still says confirmed, and Reserve replays the completed reservation's key rather
  // than buying again.
  it('keeps the confirmation and the key after a completed checkout passes its deadline', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const stub = queuedFetch({
      '/reservations': [{ status: 200, body: liveFor(1000) }, { status: 200, body: liveFor(60_000) }],
      '/checkout': [{ status: 200, body: { order_id: ORDER, guest_order_ref: GUEST_ORDER_REF, status: 'completed' } }],
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    await countdownRunning();
    fireEvent.click(screen.getByRole('button', { name: /Pay/ }));
    const confirmed = await screen.findByText(/confirmed/i);
    const confirmation = confirmed.textContent;
    act(() => { vi.advanceTimersByTime(1250); });
    expect(screen.queryByText('Hold expired')).toBeNull();
    expect(screen.getByText(/confirmed/i).textContent).toBe(confirmation);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    const [first, second] = keysFor(stub, '/reservations');
    expect(second).toBe(first);
  });

  // ...but a 401 AFTER an uncertain attempt proves nothing: that order may exist.
  it('keeps the key when a 401 follows an uncertain checkout', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'performance'] });
    const stub = queuedFetch({
      '/reservations': [{ status: 200, body: liveFor(1000) }, { status: 200, body: liveFor(60_000) }],
      '/checkout': [{ status: 500, body: {} }, { status: 401, body: { error: 'unauthorized' } }],
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    await countdownRunning();
    fireEvent.click(screen.getByRole('button', { name: /Pay/ }));
    await waitFor(() => expect(keysFor(stub, '/checkout')).toHaveLength(1));
    await waitFor(() => expect((screen.getByRole('button', { name: /Pay/ }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole('button', { name: /Pay/ }));
    await waitFor(() => expect(keysFor(stub, '/checkout')).toHaveLength(2));
    await screen.findByText(/sign in/i);
    act(() => { vi.advanceTimersByTime(1250); });
    await screen.findByText('Hold expired');

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    const [first, second] = keysFor(stub, '/reservations');
    expect(second).toBe(first);
  });

  // Review pass 1 [medium]: a terminal decline (402) means commerce RELEASED the hold, so it is
  // dead and nothing depends on it — a retry for the same terms takes a fresh hold.
  it('re-reserves under a NEW key after a terminal payment decline', async () => {
    const stub = queuedFetch({
      '/reservations': [{ status: 200, body: liveFor(60_000) }, { status: 200, body: liveFor(60_000) }],
      '/checkout': [{ status: 402, body: { error: 'payment declined' } }],
    });
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    fireEvent.click(await screen.findByRole('button', { name: /Pay/ }));
    await waitFor(() => expect((screen.getByRole('button', { name: 'Reserve' }) as HTMLButtonElement).disabled).toBe(false));

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await waitFor(() => expect(keysFor(stub, '/reservations')).toHaveLength(2));
    const [first, second] = keysFor(stub, '/reservations');
    expect(second).not.toBe(first);
  });

  // COS4, the invariant the terms binding exists for: a LIVE hold is not re-reserved. It
  // cannot be, from this UI — Reserve is disabled while the hold counts down — and an
  // unknown outcome still replays under the same key ('replays a failed reserve…' above).
  it('keeps Reserve disabled while a live hold counts down', async () => {
    const stub = reservationsReturning(liveFor(10 * 60_000));
    mountGA(stub);

    fireEvent.click(screen.getByRole('button', { name: 'Reserve' }));
    await screen.findByRole('button', { name: /Pay/ });
    expect((screen.getByRole('button', { name: 'Reserve' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/Held for/)).not.toBeNull();
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
