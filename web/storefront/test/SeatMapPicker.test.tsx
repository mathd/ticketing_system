// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SeatMapPicker, {
  applySeatConflict,
  orderByPosition,
  selectionCeiling,
} from '../src/components/SeatMapPicker';
import type { SeatMapGeometry, SeatMapRow, SeatMapSeat, SeatMapSection } from '../src/lib/api';

const ORG = '00000000-0000-0000-0000-000000000001';
const SLOT = '00000000-0000-0000-0000-000000000002';
const MAP = '00000000-0000-0000-0000-000000000003';

// Deliberately out of position order at every level: the picker must sort, not
// trust the response's array order.
function geometry(): SeatMapGeometry {
  return {
    map: { id: MAP, venue_id: 'v', name: 'Stalls', status: 'published', version: 1 },
    sections: [
      {
        id: 's2', name: 'Balcony', position: 2,
        rows: [
          { id: 'r2', label: 'B', position: 1, seats: [
            { id: 'x', seat_identity: 'Balcony/B/2', label: '2', position: 2 },
            { id: 'y', seat_identity: 'Balcony/B/1', label: '1', position: 1 },
          ] },
        ],
      },
      {
        id: 's1', name: 'Stalls', position: 1,
        rows: [
          { id: 'r1b', label: 'A2', position: 2, seats: [
            { id: 'z', seat_identity: 'Stalls/A2/1', label: '1', position: 1 },
          ] },
          { id: 'r1a', label: 'A1', position: 1, seats: [
            { id: 'w', seat_identity: 'Stalls/A1/1', label: '1', position: 1 },
          ] },
        ],
      },
    ],
  } as SeatMapGeometry;
}

function occupancy(unavailable: string[], extra: Record<string, unknown> = {}) {
  return {
    slot_id: SLOT, seat_map_id: MAP, offering_status: 'open',
    remaining_capacity: 100, unavailable_seat_identities: unavailable, ...extra,
  };
}

// stubFetch answers the two reads the picker makes, by URL.
function stubFetch(geo: unknown, occ: unknown, opts: { geoStatus?: number; occStatus?: number } = {}) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/api/catalog/public/seat-maps/')) {
      return new Response(JSON.stringify(geo), { status: opts.geoStatus ?? 200 });
    }
    if (url.includes('/seat-occupancy')) {
      return new Response(JSON.stringify(occ), { status: opts.occStatus ?? 200 });
    }
    throw new Error('unexpected fetch ' + url);
  });
}

function mount(fetchStub: ReturnType<typeof stubFetch>, onChange = vi.fn()) {
  vi.stubGlobal('fetch', fetchStub);
  render(
    <SeatMapPicker organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />,
  );
  return onChange;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('pure helpers', () => {
  it('orderByPosition sorts every level, not just the top', () => {
    const sections = orderByPosition(geometry().sections ?? []);
    expect(sections.map((s: SeatMapSection) => s.name)).toEqual(['Stalls', 'Balcony']);
    expect(sections[0].rows?.map((r: SeatMapRow) => r.label)).toEqual(['A1', 'A2']);
    expect(sections[1].rows?.[0].seats?.map((s: SeatMapSeat) => s.label)).toEqual(['1', '2']);
  });

  it('selectionCeiling takes the smallest of the band, the pool ceiling and the free seats', () => {
    expect(selectionCeiling(100, 80)).toBe(50); // the 1..50 band the claim path enforces
    expect(selectionCeiling(3, 80)).toBe(3); // a draining capacity cut binds
    expect(selectionCeiling(100, 2)).toBe(2); // a small map binds
    expect(selectionCeiling(0, 80)).toBe(0); // no headroom: nothing is selectable
  });

  it('applySeatConflict keeps the seats that were NOT contended', () => {
    const next = applySeatConflict(
      { selected: ['A/1/1', 'A/1/2', 'A/1/3'], unavailable: ['A/1/9'] },
      ['A/1/2'],
    );
    expect(next.selected).toEqual(['A/1/1', 'A/1/3']);
    expect(next.unavailable).toEqual(['A/1/2', 'A/1/9']);
  });
});

describe('SeatMapPicker', () => {
  it('renders seats in position order regardless of response order', async () => {
    mount(stubFetch(geometry(), occupancy([])));
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1/ });
    const labels = screen.getAllByRole('button').map((b) => b.getAttribute('aria-label'));
    expect(labels).toEqual([
      'Stalls, row A1, seat 1, Available',
      'Stalls, row A2, seat 1, Available',
      'Balcony, row B, seat 1, Available',
      'Balcony, row B, seat 2, Available',
    ]);
  });

  it('toggles a free seat and reports the selection upward', async () => {
    const onChange = mount(stubFetch(geometry(), occupancy([])));
    const seat = await screen.findByRole('button', { name: /Stalls, row A1, seat 1/ });

    fireEvent.click(seat);
    expect(seat.getAttribute('aria-pressed')).toBe('true');
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith(['Stalls/A1/1']));

    fireEvent.click(seat);
    expect(seat.getAttribute('aria-pressed')).toBe('false');
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith([]));
  });

  it('marks a taken seat disabled and says so in its label — never colour alone', async () => {
    mount(stubFetch(geometry(), occupancy(['Stalls/A1/1'])));
    const taken = await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Unavailable/ });
    expect((taken as HTMLButtonElement).disabled).toBe(true);
    // A taken seat is not "pressed": disabled and pressed are different states and
    // conflating them is invisible without a screen reader.
    expect(taken.hasAttribute('aria-pressed')).toBe(false);
    expect(taken.textContent).toMatch(/×/);
  });

  it('refuses to select past the ceiling', async () => {
    mount(stubFetch(geometry(), occupancy([], { remaining_capacity: 2 })));
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1/ });
    const seats = screen.getAllByRole('button');
    fireEvent.click(seats[0]);
    fireEvent.click(seats[1]);
    fireEvent.click(seats[2]);
    expect(seats[2].getAttribute('aria-pressed')).toBe('false');
    expect(screen.getByRole('status').textContent).toMatch(/2/);
  });

  it('a closed offering makes nothing selectable but still tells taken from free', async () => {
    mount(stubFetch(geometry(), occupancy(['Balcony/B/1'], { offering_status: 'closed' })));
    const free = await screen.findByRole('button', { name: /Stalls, row A1, seat 1/ });
    expect((free as HTMLButtonElement).disabled).toBe(true);
    expect(free.getAttribute('aria-label')).not.toMatch(/Unavailable/);
    expect((screen.getByRole('button', { name: /Balcony, row B, seat 1/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  // TKT-172 predicted this window: catalog can advertise a seated slot before
  // inventory has provisioned its pool, so occupancy 404s for a moment.
  it('fails closed when a read fails — and never offers a quantity fallback', async () => {
    mount(stubFetch(geometry(), {}, { occStatus: 404 }));
    await screen.findByText(/Seat selection is temporarily unavailable/);
    // No purchase control at all. Falling back to the quantity picker would be
    // worse than nothing: a quantity hold on a seated pool is refused by
    // ErrPoolKindMismatch every single time.
    expect(screen.queryAllByRole('button').length).toBe(0);
    expect(screen.queryByRole('spinbutton')).toBe(null);
  });

  it('keeps the last good map when a later poll fails', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (occCall === 1) return new Response(JSON.stringify(occupancy([])), { status: 200 });
      return new Response('{}', { status: 503 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button')).toHaveLength(4));

    await vi.advanceTimersByTimeAsync(5000);
    await vi.waitFor(() => expect(occCall).toBeGreaterThan(1));
    // The map is still there — blanking it would read as a sold-out house.
    expect(screen.getAllByRole('button').length).toBe(4);
  });

  it('polls occupancy at the contract tier and no faster', async () => {
    vi.useFakeTimers();
    const stub = stubFetch(geometry(), occupancy([]));
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button')).toHaveLength(4));
    const occCalls = () => stub.mock.calls.filter(([u]) => String(u).includes('seat-occupancy')).length;
    expect(occCalls()).toBe(1);

    await vi.advanceTimersByTimeAsync(4000);
    expect(occCalls()).toBe(1); // ADR-004's seconds tier is max-age=5; faster would violate it
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => expect(occCalls()).toBe(2));
  });

  it('fails closed when the two services name different map versions', async () => {
    mount(stubFetch(geometry(), occupancy([], { seat_map_id: 'a-different-version' })));
    await screen.findByText(/Seat selection is temporarily unavailable/);
  });
});
