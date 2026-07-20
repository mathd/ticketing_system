import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  addSeatMapRow,
  addSeatMapSeat,
  addSeatMapSection,
  createSeatMap,
  DEFAULT_ORGANIZER_ID,
  getSeatMapGeometry,
  getVenues,
  listVenueSeatMaps,
} from '../src/lib/api';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('getVenues', () => {
  it('reads the organizer-scoped venue list through the gateway', async () => {
    let requestedUrl = '';
    const fetchSpy = vi.fn(async (input: RequestInfo | URL) => {
      requestedUrl = String(input);
      return jsonResponse({
        venues: [
          {
            id: '00000000-0000-0000-0000-0000000000a2',
            organizer_id: DEFAULT_ORGANIZER_ID,
            name: 'Le Petit Théâtre',
            ga_capacity: 350,
            created_at: '2026-07-20T00:00:00Z',
          },
        ],
      });
    });
    vi.stubGlobal('fetch', fetchSpy);

    const venues = await getVenues();

    // Consumes the public contract through the gateway, scoped by organizer_id.
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(requestedUrl).toContain('/api/catalog/public/venues');
    expect(requestedUrl).toContain(`organizer_id=${DEFAULT_ORGANIZER_ID}`);
    // Unwraps the { venues: [...] } envelope; capacity is present for the list.
    expect(venues).toHaveLength(1);
    expect(venues[0].name).toBe('Le Petit Théâtre');
    expect(venues[0].ga_capacity).toBe(350);
  });

  it('throws when the catalog read fails, so the page does not render stale/empty silently', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('nope', { status: 502 })),
    );
    await expect(getVenues()).rejects.toThrow(/venue read failed: 502/);
  });
});

// Captures method, url, and parsed body of the last fetch call.
function spyFetch(responseBody: unknown, status = 201) {
  const calls: { url: string; method: string; body: unknown }[] = [];
  const fetchSpy = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({
      url: String(input),
      method: init?.method ?? 'GET',
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    });
    return new Response(JSON.stringify(responseBody), {
      status,
      headers: { 'content-type': 'application/json' },
    });
  });
  vi.stubGlobal('fetch', fetchSpy);
  return calls;
}

describe('seat-map authoring client', () => {
  it('creates a draft map under a venue, organizer-scoped, through the gateway', async () => {
    const calls = spyFetch({
      id: 'm1',
      organizer_id: DEFAULT_ORGANIZER_ID,
      venue_id: 'v1',
      name: 'Floor',
      version: 1,
      status: 'draft',
      created_at: '2026-07-20T00:00:00Z',
    });
    const m = await createSeatMap('v1', 'Floor');
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/venues/v1/seat-maps');
    expect(calls[0].body).toMatchObject({ organizer_id: DEFAULT_ORGANIZER_ID, name: 'Floor' });
    expect(m.status).toBe('draft');
    expect(m.version).toBe(1);
  });

  it('adds a section, row, and seat to the right nested endpoints', async () => {
    const secCalls = spyFetch({ id: 's1', name: 'Orchestra', position: 1 });
    await addSeatMapSection('m1', { name: 'Orchestra', position: 1 });
    expect(secCalls[0].url).toContain('/api/catalog/seat-maps/m1/sections');
    expect(secCalls[0].body).toMatchObject({ organizer_id: DEFAULT_ORGANIZER_ID, name: 'Orchestra', position: 1 });

    const rowCalls = spyFetch({ id: 'r1', label: 'A', position: 1 });
    await addSeatMapRow('m1', { section_id: 's1', label: 'A', position: 1 });
    expect(rowCalls[0].url).toContain('/api/catalog/seat-maps/m1/rows');
    expect(rowCalls[0].body).toMatchObject({ section_id: 's1', label: 'A', position: 1 });

    const seatCalls = spyFetch({ id: 'x1', seat_identity: 'Orchestra/A/1', label: '1', position: 1 });
    const seat = await addSeatMapSeat('m1', { row_id: 'r1', label: '1', position: 1 });
    expect(seatCalls[0].url).toContain('/api/catalog/seat-maps/m1/seats');
    expect(seatCalls[0].body).toMatchObject({ row_id: 'r1', label: '1', position: 1 });
    // Identity is composed server-side and returned; the client does not mint it.
    expect(seat.seat_identity).toBe('Orchestra/A/1');
  });

  it('reads a venue seat-map list and unwraps the envelope', async () => {
    const calls = spyFetch(
      { seat_maps: [{ id: 'm1', organizer_id: DEFAULT_ORGANIZER_ID, venue_id: 'v1', name: 'Floor', version: 1, status: 'draft', created_at: '2026-07-20T00:00:00Z' }] },
      200,
    );
    const maps = await listVenueSeatMaps('v1');
    expect(calls[0].method).toBe('GET');
    expect(calls[0].url).toContain('/api/catalog/public/venues/v1/seat-maps');
    expect(maps).toHaveLength(1);
    expect(maps[0].name).toBe('Floor');
  });

  it('reads full geometry through the public read', async () => {
    const calls = spyFetch(
      {
        map: { id: 'm1', organizer_id: DEFAULT_ORGANIZER_ID, venue_id: 'v1', name: 'Floor', version: 1, status: 'draft', created_at: '2026-07-20T00:00:00Z' },
        sections: [{ id: 's1', name: 'Orchestra', position: 1, rows: [{ id: 'r1', label: 'A', position: 1, seats: [{ id: 'x1', seat_identity: 'Orchestra/A/1', label: '1', position: 1 }] }] }],
      },
      200,
    );
    const g = await getSeatMapGeometry('m1');
    expect(calls[0].url).toContain('/api/catalog/public/seat-maps/m1');
    expect(g.sections[0].rows?.[0].seats?.[0].seat_identity).toBe('Orchestra/A/1');
  });

  it('throws on a write failure so the page surfaces it, not a silent success', async () => {
    spyFetch({ error: 'conflict' }, 409);
    await expect(addSeatMapSeat('m1', { row_id: 'r1', label: '1', position: 1 })).rejects.toThrow(/catalog write failed: 409/);
  });
});
