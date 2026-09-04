// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SeatMapPicker, {
  type SeatMapHandle,
  applySeatConflict,
  orderByPosition,
  reconcileSelection,
  selectionCeiling,
} from '../src/components/SeatMapPicker';
import type { SeatMapGeometry, SeatMapRow, SeatMapSeat, SeatMapSection } from '../src/lib/api';

const ORG = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1';
const SLOT = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2';
const MAP = 'cccccccc-cccc-4ccc-8ccc-ccccccccccc3';
const OTHER_MAP = 'dddddddd-dddd-4ddd-8ddd-ddddddddddd4';
const VENUE = '00000000-0000-0000-0000-000000000005';
const SECTION_BALCONY = '00000000-0000-0000-0000-000000000006';
const SECTION_STALLS = '00000000-0000-0000-0000-000000000007';
const ROW_BALCONY = '00000000-0000-0000-0000-000000000008';
const ROW_STALLS_TWO = '00000000-0000-0000-0000-000000000009';
const ROW_STALLS_ONE = '00000000-0000-0000-0000-000000000010';
const SEAT_BALCONY_TWO = '00000000-0000-0000-0000-000000000011';
const SEAT_BALCONY_ONE = '00000000-0000-0000-0000-000000000012';
const SEAT_STALLS_TWO = '00000000-0000-0000-0000-000000000013';
const SEAT_STALLS_ONE = '00000000-0000-0000-0000-000000000014';
const POLL = 5000;

// Deliberately out of position order at every level: the picker must sort, not
// trust the response's array order.
function geometry(): SeatMapGeometry {
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
    sections: [
      {
        id: SECTION_BALCONY, name: 'Balcony', position: 2,
        rows: [
          { id: ROW_BALCONY, label: 'B', position: 1, seats: [
            { id: SEAT_BALCONY_TWO, seat_identity: 'Balcony/B/2', label: '2', position: 2 },
            { id: SEAT_BALCONY_ONE, seat_identity: 'Balcony/B/1', label: '1', position: 1 },
          ] },
        ],
      },
      {
        id: SECTION_STALLS, name: 'Stalls', position: 1,
        rows: [
          { id: ROW_STALLS_TWO, label: 'A2', position: 2, seats: [
            { id: SEAT_STALLS_TWO, seat_identity: 'Stalls/A2/1', label: '1', position: 1 },
          ] },
          { id: ROW_STALLS_ONE, label: 'A1', position: 1, seats: [
            { id: SEAT_STALLS_ONE, seat_identity: 'Stalls/A1/1', label: '1', position: 1 },
          ] },
        ],
      },
    ],
  };
}

function occupancy(unavailable: string[], extra: Record<string, unknown> = {}) {
  return {
    slot_id: SLOT, seat_map_id: MAP, offering_status: 'open',
    remaining_capacity: 100, unavailable_seat_identities: unavailable, ...extra,
  };
}

function geometryWithPosition(
  level: 'section' | 'row' | 'seat',
  position: number,
): SeatMapGeometry {
  const result = structuredClone(geometry());
  const section = result.sections?.[0];
  const row = section?.rows?.[0];
  const seat = row?.seats?.[0];
  if (!section || !row || !seat) throw new Error('geometry fixture is incomplete');
  if (level === 'section') section.position = position;
  if (level === 'row') row.position = position;
  if (level === 'seat') seat.position = position;
  return result;
}

function geometryWithFirstSeatIdentity(seatIdentity: string): SeatMapGeometry {
  const result = structuredClone(geometry());
  const seat = result.sections?.[0]?.rows?.[0]?.seats?.[0];
  if (!seat) throw new Error('geometry fixture is incomplete');
  seat.seat_identity = seatIdentity;
  return result;
}

// stubFetch answers the two reads the picker makes, by URL.
function stubFetch(geo: unknown, occ: unknown, opts: { geoStatus?: number; occStatus?: number } = {}) {
  // The init parameter is declared even though only some cases read it: without it the
  // mock's call tuple is typed as length 1 and indexing [1] is a compile error.
  return vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
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

// The imperative handle HoldPicker holds (TKT-184). Capturing it here is what makes
// these tests drive the SAME channel production drives — the previous `window`
// CustomEvent could be dispatched by a test whether or not anything still listened.
const handle: { current: SeatMapHandle | null } = { current: null };

function mount(fetchStub: ReturnType<typeof stubFetch>, onChange = vi.fn()) {
  vi.stubGlobal('fetch', fetchStub);
  render(
    <SeatMapPicker
      ref={handle}
      organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />,
  );
  return onChange;
}

// applyConflict updates state outside React's event system, so it needs act() — without
// it the assertions race the re-render rather than the behaviour.
function conflict(lost: string[]) {
  // Throw rather than optional-chain: a helper that silently does nothing when the
  // handle is missing is the exact failure mode the window CustomEvent had, and it
  // would let every assertion below pass against a component that never heard.
  if (handle.current === null) throw new Error('no SeatMapPicker mounted');
  const h = handle.current;
  act(() => h.applyConflict(lost));
}

afterEach(() => {
  cleanup();
  handle.current = null;
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

  it('accepts geometry and occupancy UUIDs that differ from the requested IDs only by case', async () => {
    vi.stubGlobal('fetch', stubFetch(geometry(), occupancy([])));
    render(
      <SeatMapPicker
        ref={handle}
        organizerId={ORG.toUpperCase()}
        slotId={SLOT.toUpperCase()}
        seatMapId={MAP.toUpperCase()}
        locale="en"
        onSelectionChange={vi.fn()}
      />,
    );

    await screen.findByRole('button', { name: /Stalls, row A1, seat 1/ });
    expect(screen.queryByText(/Seat selection is temporarily unavailable/)).toBeNull();
  });

  it('toggles a free seat and reports the selection upward', async () => {
    const onChange = mount(stubFetch(geometry(), occupancy([])));
    const seat = await screen.findByRole('button', { name: /Stalls, row A1, seat 1/ });

    fireEvent.click(seat);
    expect(seat.getAttribute('aria-pressed')).toBe('true');
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: ['Stalls/A1/1'], claimable: true }));

    fireEvent.click(seat);
    expect(seat.getAttribute('aria-pressed')).toBe('false');
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: [], claimable: false }));
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
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
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
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button')).toHaveLength(4));
    const occCalls = () => stub.mock.calls.filter(([u]) => String(u).includes('seat-occupancy')).length;
    expect(occCalls()).toBe(1);

    await vi.advanceTimersByTimeAsync(4000);
    expect(occCalls()).toBe(1); // ADR-004's seconds tier is max-age=5; faster would violate it
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => expect(occCalls()).toBe(2));
  });

  it('fails closed when the two services name different map versions', async () => {
    mount(stubFetch(geometry(), occupancy([], { seat_map_id: OTHER_MAP })));
    await screen.findByText(/Seat selection is temporarily unavailable/);
  });

  it('rejects a composed geometry seat identity longer than 200 characters', async () => {
    mount(stubFetch(geometryWithFirstSeatIdentity('S'.repeat(201)), occupancy([])));
    await screen.findByText(/Seat selection is temporarily unavailable/);
  });

  it('accepts a 200-character Unicode seat identity', async () => {
    mount(stubFetch(geometryWithFirstSeatIdentity('🎟'.repeat(200)), occupancy([])));
    await screen.findByRole('button', { name: /Balcony, row B, seat 2, Available/ });
    expect(screen.queryByText(/Seat selection is temporarily unavailable/)).toBeNull();
  });

  it('accepts an occupancy identity at the 200-character contract limit', async () => {
    mount(stubFetch(geometry(), occupancy(['S'.repeat(200)])));
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });
    expect(screen.queryByText(/Seat selection is temporarily unavailable/)).toBeNull();
  });

  it('accepts the exact JavaScript-safe remaining-capacity ceiling', async () => {
    mount(stubFetch(geometry(), occupancy([], { remaining_capacity: Number.MAX_SAFE_INTEGER })));
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });
    expect(screen.queryByText(/Seat selection is temporarily unavailable/)).toBeNull();
  });

  it.each([
    ['an absent occupancy identity', () => {
      const { slot_id: _slotId, ...body } = occupancy([]);
      return [geometry(), body];
    }],
    ['a non-array unavailable set', () => [
      geometry(),
      occupancy([], { unavailable_seat_identities: {} }),
    ]],
    ['a negative remaining capacity', () => [
      geometry(),
      occupancy([], { remaining_capacity: -1 }),
    ]],
    ['an unsafe remaining capacity', () => [
      geometry(),
      occupancy([], { remaining_capacity: Number.MAX_SAFE_INTEGER + 1 }),
    ]],
    ['an overlong occupancy identity', () => [
      geometry(),
      occupancy(['S'.repeat(201)]),
    ]],
    ['an unknown offering status', () => [
      geometry(),
      occupancy([], { offering_status: 'paused' }),
    ]],
    ['a malformed seat identity', () => {
      const geo = geometry();
      return [
        {
          ...geo,
          sections: geo.sections.map((section, sectionIndex) => sectionIndex === 0
            ? {
                ...section,
                rows: section.rows?.map((row, rowIndex) => rowIndex === 0
                  ? {
                      ...row,
                      seats: row.seats?.map((seat, seatIndex) => seatIndex === 0
                        ? { ...seat, id: 'seat-1' }
                        : seat),
                    }
                  : row),
              }
            : section),
        },
        occupancy([]),
      ];
    }],
    ['a zero section position', () => [geometryWithPosition('section', 0), occupancy([])]],
    ['a zero row position', () => [geometryWithPosition('row', 0), occupancy([])]],
    ['a zero seat position', () => [geometryWithPosition('seat', 0), occupancy([])]],
    ['an empty geometry seat identity', () => [
      geometryWithFirstSeatIdentity(''),
      occupancy([]),
    ]],
    ['an invalid geometry date', () => {
      const geo = geometry();
      return [{ ...geo, map: { ...geo.map, created_at: 'yesterday' } }, occupancy([])];
    }],
  ])('fails closed when a successful seat read contains %s', async (_name, response) => {
    const [geo, occ] = response();
    mount(stubFetch(geo, occ));
    await screen.findByText(/Seat selection is temporarily unavailable/);
    expect(screen.queryAllByRole('button')).toHaveLength(0);
  });
});

describe('ai-review findings', () => {
  it('reconcileSelection drops seats that became taken and trims to the new ceiling', () => {
    expect(reconcileSelection(['a', 'b', 'c'], new Set(['b']), 5)).toEqual(['a', 'c']);
    expect(reconcileSelection(['a', 'b', 'c'], new Set(), 2)).toEqual(['a', 'b']);
    expect(reconcileSelection(['a'], new Set(['a']), 5)).toEqual([]);
  });

  // A poll that marks a selected seat taken must not leave it counted and submittable:
  // the UI would show it unavailable while still sending it, and inventory would refuse
  // a request the buyer was invited to make.
  it('drops a selected seat that a later poll marks taken', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      return new Response(JSON.stringify(occupancy(occCall === 1 ? [] : ['Stalls/A1/1'])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    const onChange = vi.fn();
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));

    fireEvent.click(screen.getByRole('button', { name: /Stalls, row A1, seat 1, Available/ }));
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: ['Stalls/A1/1'], claimable: true }));

    await vi.advanceTimersByTimeAsync(POLL);
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: [], claimable: false }));
    expect((screen.getByRole('button', { name: /row A1, seat 1, Unavailable/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  // Two forces meet here and the resolution is the point.
  //
  // The occupancy read is cacheable for 5s, so the refresh that follows a 409 can
  // legitimately answer with a body that still shows the seat free — treating that as
  // truth resurrects a seat the server has already refused. But an overlay that only
  // ever grows is equally wrong: a seat that caused one conflict would stay dark for
  // the life of the tab, even after its hold is released.
  //
  // So: the 409 applies immediately, the refresh bypasses the HTTP cache, and THAT
  // read — being authoritative by construction — is what the overlay defers to.
  it('marks a conflicted seat taken at once and refreshes past the cache', async () => {
    const stub = stubFetch(geometry(), occupancy([]));
    mount(stub);
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });

    conflict(['Stalls/A1/1']);

    // The refresh bypasses the HTTP cache: that is what makes its answer authoritative,
    // and it is the only part of the sequence a fast stub can observe — the overlay is
    // applied synchronously but the read here resolves before the DOM can be sampled.
    await waitFor(() => {
      const occ = stub.mock.calls.filter(([u]) => String(u).includes('seat-occupancy'));
      expect(occ.length).toBeGreaterThan(1);
      expect((occ[occ.length - 1][1] as RequestInit | undefined)?.cache).toBe('no-store');
    });
  });

  it('keeps a conflicted seat taken when the authoritative read agrees', async () => {
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      return new Response(JSON.stringify(occupancy(occCall === 1 ? [] : ['Stalls/A1/1'])), { status: 200 });
    });
    mount(stub);
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });

    conflict(['Stalls/A1/1']);

    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Unavailable/ });
    // And it stays taken once the authoritative read lands.
    await waitFor(() => expect(occCall).toBeGreaterThan(1));
    expect(screen.getByRole('button', { name: /Stalls, row A1, seat 1, Unavailable/ })).toBeTruthy();
  });

  // The other half: a released hold must become buyable again. An overlay that
  // outlived the authoritative read would quietly retire inventory.
  it('releases a conflicted seat when the authoritative read says it is free', async () => {
    // The authoritative read is delayed on purpose, so the overlay is observable
    // BEFORE it is superseded. Without the delay the two states collapse into one
    // render and the assertion would pass whether or not the overlay ever cleared.
    let release: (() => void) | null = null;
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (occCall > 1) {
        await new Promise<void>((resolve) => { release = resolve; });
      }
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    mount(stub);
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });

    conflict(['Stalls/A1/1']);
    // The 409 is honoured immediately, before any network answer.
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Unavailable/ });

    // Now let the authoritative read land. It reports the seat free — the hold was
    // released — so the overlay must not outlive it. An overlay that only grew would
    // quietly retire inventory for the life of the tab.
    await waitFor(() => expect(release).not.toBe(null));
    release!();
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });
  });

  it('reports the selection as unclaimable once the offering closes', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      return new Response(JSON.stringify(
        occupancy([], occCall === 1 ? {} : { offering_status: 'closed' })), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    const onChange = vi.fn();
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));
    fireEvent.click(screen.getByRole("button", { name: /row A1, seat 1, Available/ }));
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: ['Stalls/A1/1'], claimable: true }));

    await vi.advanceTimersByTimeAsync(POLL);
    // The seat stays selected — the buyer's work is not discarded — but nothing may be
    // claimed, and the PARENT is the thing that has to know.
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ claimable: false })));
  });

  // A fixed interval against slow responses accumulates overlapping requests that each
  // supersede the last: the map never finishes loading while the load keeps growing.
  it('never has two occupancy reads in flight at once', async () => {
    vi.useFakeTimers();
    let inFlight = 0;
    let maxInFlight = 0;
    const stub = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      inFlight += 1;
      maxInFlight = Math.max(maxInFlight, inFlight);
      await new Promise((r) => setTimeout(r, POLL * 3)); // slower than the poll period
      inFlight -= 1;
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.advanceTimersByTimeAsync(POLL * 6);
    expect(maxInFlight).toBe(1);
  });

  it('recovers from a transient failure instead of staying broken', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (occCall === 2) return new Response('{}', { status: 503 });
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));

    await vi.advanceTimersByTimeAsync(POLL);        // the failing read: degraded, map kept
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));
    await vi.advanceTimersByTimeAsync(POLL);        // a good read: the notice clears
    await vi.waitFor(() => expect(screen.queryByText(/last known seat availability/)).toBe(null));
  });
});

describe('ai-review pass 2 findings', () => {
  it('a degraded read makes the selection unclaimable even though the map stays up', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (occCall === 1) return new Response(JSON.stringify(occupancy([])), { status: 200 });
      return new Response('{}', { status: 503 });
    });
    vi.stubGlobal('fetch', stub);
    const onChange = vi.fn();
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));
    fireEvent.click(screen.getByRole("button", { name: /row A1, seat 1, Available/ }));
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: ['Stalls/A1/1'], claimable: true }));

    await vi.advanceTimersByTimeAsync(POLL);
    // The map is still there (blanking it would read as sold out) but nothing may be
    // claimed against occupancy the component has declared unreadable.
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ claimable: false })));
    expect(screen.getAllByRole('button').length).toBe(4);
  });

  it('a hung read times out instead of stopping the poll forever', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (occCall === 2) {
        // Never resolves on its own — only the deadline can end it.
        return new Promise((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
        }) as Promise<Response>;
      }
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));

    await vi.advanceTimersByTimeAsync(POLL);          // the hung read starts
    await vi.advanceTimersByTimeAsync(8000 + POLL);   // its deadline fires, then the next tick
    // Polling survived: a third read happened.
    await vi.waitFor(() => expect(occCall).toBeGreaterThan(2));
  });

  // Geometry is read once and cannot degrade; occupancy is read forever. Sharing one
  // field let a successful poll clear a terminal geometry failure, leaving the picker
  // rendering "loading" for the rest of the session.
  it('a geometry failure is not cleared by successful occupancy polls', async () => {
    vi.useFakeTimers();
    const stub = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) return new Response('{}', { status: 503 });
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);

    await vi.waitFor(() => expect(screen.getByText(/Seat selection is temporarily unavailable/)).toBeTruthy());
    await vi.advanceTimersByTimeAsync(POLL * 3);
    // Still the failure message, never an endless "loading".
    expect(screen.getByText(/Seat selection is temporarily unavailable/)).toBeTruthy();
  });
});

describe('ai-review pass 3 findings', () => {
  // A hung GEOMETRY read leaves sections null forever, and no amount of successful
  // polling can move the render past "loading" — the permanent-loading failure the
  // split read states were meant to end, reachable by the other door.
  it('a hung geometry read fails rather than loading forever', async () => {
    vi.useFakeTimers();
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Promise((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () =>
            reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
        }) as Promise<Response>;
      }
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);

    await vi.advanceTimersByTimeAsync(8000 + 100);
    await vi.waitFor(() => expect(screen.getByText(/Seat selection is temporarily unavailable/)).toBeTruthy());
  });

  // The routine poll must DEFER to an in-flight authoritative read, not abort it.
  // Only a successful authoritative read clears the overlay, so a poll landing in the
  // 5-8s window would supersede it and strand the overlay for the life of the picker.
  it('a slow authoritative refresh is not preempted by the next poll', async () => {
    vi.useFakeTimers();
    let release: (() => void) | null = null;
    let authoritativeAborted = false;
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      if (init?.cache === 'no-store') {
        init.signal?.addEventListener('abort', () => { authoritativeAborted = true; });
        await new Promise<void>((resolve) => { release = resolve; });
      }
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));

    conflict(['Stalls/A1/1']);
    await vi.waitFor(() => expect(release).not.toBe(null));

    // Push past a poll boundary while the authoritative read is still outstanding.
    await vi.advanceTimersByTimeAsync(POLL + 500);
    expect(authoritativeAborted).toBe(false);

    release!();
    // And having survived, it clears the overlay: the seat is buyable again.
    await vi.waitFor(() =>
      expect(screen.getByRole('button', { name: /Stalls, row A1, seat 1, Available/ })).toBeTruthy());
  });

  // An ordinary abort (unmount, supersession) must NOT be reported as a failure.
  // DOMException is an Error, so inferring "timeout" from the abort reason's type
  // cannot tell the two apart — the flag has to be set by the deadline itself.
  it('an unmount abort is not reported as a read failure', async () => {
    const onChange = vi.fn();
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      return new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () =>
          reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
      }) as Promise<Response>;
    });
    vi.stubGlobal('fetch', stub);
    const view = render(
      <SeatMapPicker organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />,
    );
    await screen.findByText(/Loading the seat map/);
    view.unmount();
    // Nothing after the unmount: no state update, no failure reported.
    await new Promise((r) => setTimeout(r, 50));
    expect(onChange).not.toHaveBeenCalledWith(expect.objectContaining({ claimable: true }));
  });

  it('a timed-out read degrades and makes the selection unclaimable', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (occCall === 2) {
        return new Promise((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () =>
            reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
        }) as Promise<Response>;
      }
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    const onChange = vi.fn();
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={onChange} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));
    fireEvent.click(screen.getByRole("button", { name: /row A1, seat 1, Available/ }));
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith({ seats: ['Stalls/A1/1'], claimable: true }));

    await vi.advanceTimersByTimeAsync(POLL);        // the hung read starts
    await vi.advanceTimersByTimeAsync(8000 + 100);  // its deadline fires
    // The stale notice appears AND the selection stops being claimable — the timeout
    // test previously only proved that polling continued, which the chain does anyway.
    await vi.waitFor(() => expect(screen.getByText(/last known seat availability/)).toBeTruthy());
    await vi.waitFor(() => expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ claimable: false })));
  });
});

describe('ai-review pass 4 finding', () => {
  // fetch() resolves when HEADERS arrive, not when the body has been read. A deadline
  // cleared on the response therefore leaves a stalled body unbounded — and a stalled
  // body is worse than a stalled connection: readOccupancy never reaches its `finally`,
  // so the authoritative-read guard is never released and every later poll is skipped
  // for good.
  // A hand-built Response is not wired to the AbortController the way a real fetch's
  // is, so the signal has to be connected explicitly — otherwise the test proves
  // nothing about aborting, only about stalling.
  function stalledBody(signal?: AbortSignal | null): Response {
    return new Response(
      new ReadableStream({
        start(controller) {
          signal?.addEventListener('abort', () =>
            controller.error(Object.assign(new Error('aborted'), { name: 'AbortError' })));
        },
      }),
      { status: 200, headers: { 'Content-Type': 'application/json' } },
    );
  }

  it('a stalled response body fails the geometry read rather than hanging it', async () => {
    vi.useFakeTimers();
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes('/api/catalog/public/seat-maps/')) return stalledBody(init?.signal);
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);

    await vi.advanceTimersByTimeAsync(8000 + 200);
    await vi.waitFor(() => expect(screen.getByText(/Seat selection is temporarily unavailable/)).toBeTruthy());
  });

  it('a stalled authoritative body does not suppress polling for ever', async () => {
    vi.useFakeTimers();
    let occCall = 0;
    const stub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes('/api/catalog/public/seat-maps/')) {
        return new Response(JSON.stringify(geometry()), { status: 200 });
      }
      occCall += 1;
      if (init?.cache === 'no-store') return stalledBody(init?.signal);
      return new Response(JSON.stringify(occupancy([])), { status: 200 });
    });
    vi.stubGlobal('fetch', stub);
    render(<SeatMapPicker ref={handle} organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="en" onSelectionChange={vi.fn()} />);
    await vi.waitFor(() => expect(screen.getAllByRole('button').length).toBe(4));

    conflict(['Stalls/A1/1']);
    const before = occCall;
    // Past the deadline: the guard must have been released and polling resumed.
    await vi.advanceTimersByTimeAsync(8000 + POLL * 2);
    await vi.waitFor(() => expect(occCall).toBeGreaterThan(before + 1));
  });
});

describe('orphan refusal (TKT-182)', () => {
  // The seats an orphan refusal names are FREE and unrequested. Routing them through
  // the conflict channel would mark them unavailable and remove the buyer's only
  // repair — adding one. The distinction is the whole reason the wire code differs.
  it('a conflict marks seats taken; an orphan refusal must not', async () => {
    const stub = stubFetch(geometry(), occupancy([]));
    mount(stub);
    await screen.findByRole('button', { name: /Stalls, row A1, seat 1, Available/ });

    // Only the seat_taken path dispatches the conflict event.
    conflict(['Stalls/A1/1']);
    await waitFor(() => {
      const occ = stub.mock.calls.filter(([u]) => String(u).includes('seat-occupancy'));
      expect((occ[occ.length - 1][1] as RequestInit | undefined)?.cache).toBe('no-store');
    });

    // An orphan refusal is a message only: the map keeps every free seat selectable.
    const free = screen.getAllByRole('button').filter((b) => !(b as HTMLButtonElement).disabled);
    expect(free.length).toBeGreaterThan(0);
  });
});
