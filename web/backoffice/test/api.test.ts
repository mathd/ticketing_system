import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  addSeatMapRow,
  addSeatMapSeat,
  addSeatMapSection,
  authenticateStaff,
  CatalogApiError,
  createSeatMap,
  DEFAULT_ORGANIZER_ID,
  editSeatMap,
  getOrderState,
  getOrderTickets,
  getSeatMapGeometry,
  getVenues,
  listSeatMapVersions,
  listVenueSeatMaps,
  publishSeatMap,
  updateVenueGaCapacity,
} from '../src/lib/api';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

// Every catalog write carries the staff-write credential since TKT-191, so the
// client refuses to fetch without one. Supplied here for the whole suite; the
// one test that asserts the refusal deletes it deliberately.
beforeEach(() => {
  process.env.CATALOG_STAFF_WRITE_TOKEN = 'test-credential';
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
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
  const calls: { url: string; method: string; body: unknown; headers: Record<string, string> }[] = [];
  const fetchSpy = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({
      url: String(input),
      method: init?.method ?? 'GET',
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
      headers: (init?.headers ?? {}) as Record<string, string>,
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
    await expect(addSeatMapSeat('m1', { row_id: 'r1', label: '1', position: 1 })).rejects.toThrow(/conflict/);
  });
});

// TKT-105: editing, version history, GA config, and error-body surfacing.
describe('seat-map edit + versioning client (TKT-105)', () => {
  const publishedMap = {
    id: 'm2',
    organizer_id: DEFAULT_ORGANIZER_ID,
    venue_id: 'v1',
    name: 'Floor',
    version: 2,
    status: 'published',
    published_at: '2026-07-20T10:00:00Z',
    created_at: '2026-07-20T10:00:00Z',
  };

  it('surfaces the server {error} body on a rejected edit (409 orphan), not just the status', async () => {
    spyFetch({ error: 'edit would orphan a seat identity pinned by a sale or hold' }, 409);
    // The actionable message reaches the UI — a bare "409" would be useless.
    await expect(
      editSeatMap('m1', { organizer_id: DEFAULT_ORGANIZER_ID, sections: [] }),
    ).rejects.toThrow(/orphan a seat identity pinned/);
  });

  it('throws a typed CatalogApiError carrying the status', async () => {
    spyFetch({ error: 'nope' }, 409);
    try {
      await editSeatMap('m1', { organizer_id: DEFAULT_ORGANIZER_ID, sections: [] });
      throw new Error('should have thrown');
    } catch (e) {
      expect(e).toBeInstanceOf(CatalogApiError);
      expect((e as CatalogApiError).status).toBe(409);
    }
  });

  it('posts the full replacement geometry to the edit endpoint and returns the new version', async () => {
    const calls = spyFetch(publishedMap, 201);
    const body = {
      organizer_id: DEFAULT_ORGANIZER_ID,
      sections: [{ name: 'Orchestra', position: 1, rows: [{ label: 'A', position: 1, seats: [{ label: '1', position: 1 }] }] }],
    };
    const nv = await editSeatMap('m1', body);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/seat-maps/m1/edit');
    expect(calls[0].body).toMatchObject(body);
    expect(nv.version).toBe(2);
  });

  it('publishes a draft map through the publish endpoint', async () => {
    const calls = spyFetch(publishedMap, 200);
    await publishSeatMap('m1');
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/seat-maps/m1/publish');
  });

  it('reads version history and unwraps current_version + versions', async () => {
    const calls = spyFetch({ current_version: 2, versions: [publishedMap, { ...publishedMap, id: 'm1', version: 1 }] }, 200);
    const h = await listSeatMapVersions('m1');
    expect(calls[0].method).toBe('GET');
    expect(calls[0].url).toContain('/api/catalog/public/seat-maps/m1/versions');
    expect(h.current_version).toBe(2);
    expect(h.versions).toHaveLength(2);
  });

  it('updates a venue GA capacity through the GA endpoint', async () => {
    const calls = spyFetch({ id: 'v1', organizer_id: DEFAULT_ORGANIZER_ID, name: 'Hall', ga_capacity: 250, created_at: '2026-07-20T00:00:00Z' }, 200);
    const v = await updateVenueGaCapacity('v1', 250);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/venues/v1/ga-capacity');
    expect(calls[0].body).toMatchObject({ organizer_id: DEFAULT_ORGANIZER_ID, ga_capacity: 250 });
    expect(v.ga_capacity).toBe(250);
  });
});

describe('staff authentication client (TKT-190)', () => {
  it('posts the credential to the catalog through the gateway, like every other call', async () => {
    const calls = spyFetch({ staff_id: 's1', organizer_id: 'o1', role: 'admin' }, 200);
    const principal = await authenticateStaff('ada@example.test', 'correct horse');
    expect(calls[0].method).toBe('POST');
    // Through the gateway, not straight at the catalog container: one network
    // path means the back office never needs the shared internal credential.
    expect(calls[0].url).toContain('/api/catalog/staff/authenticate');
    expect(calls[0].body).toEqual({ identifier: 'ada@example.test', password: 'correct horse' });
    expect(principal).toEqual({ staffId: 's1', organizerId: 'o1', role: 'admin' });
  });

  it('reports invalid credentials as null, not as an exception', async () => {
    // A 401 is an expected answer to a sign-in attempt. Throwing would push the
    // page into its error path and tempt it into rendering the upstream message.
    spyFetch({ error: 'invalid credentials' }, 401);
    await expect(authenticateStaff('ada@example.test', 'wrong')).resolves.toBeNull();
  });

  it('does not translate an outage into a credential verdict', async () => {
    spyFetch({ error: 'authentication unavailable' }, 500);
    await expect(authenticateStaff('ada@example.test', 'correct horse')).rejects.toThrow();
  });

  it('never puts the password in the URL', async () => {
    const calls = spyFetch({ staff_id: 's1', organizer_id: 'o1', role: 'admin' }, 200);
    await authenticateStaff('ada@example.test', 'correct horse');
    expect(calls[0].url).not.toContain('correct horse');
  });
});


describe('catalog write credential (TKT-191)', () => {
  const HEADER = 'X-Catalog-Staff-Write-Token';

  it('attaches the credential to every catalog write', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ id: 'm1', organizer_id: 'o', venue_id: 'v', name: 'M', version: 1, status: 'draft', created_at: 'x' });
    await createSeatMap('v1', 'Main');
    expect(calls[0].headers[HEADER]).toBe('the-credential');
  });

  // Guarded like every other unsafe operation: leaving it out would mean an
  // exception list inside a fail-closed scheme.
  it('attaches it to the sign-in call too', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ staff_id: 's1', organizer_id: 'o1', role: 'admin' }, 200);
    await authenticateStaff('ada@example.test', 'pw');
    expect(calls[0].headers[HEADER]).toBe('the-credential');
  });

  it('does NOT attach it to reads — they are public and must stay so', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ venues: [] }, 200);
    await getVenues();
    expect(calls[0].headers[HEADER]).toBeUndefined();
  });

  // Fail locally with a message naming the variable, rather than sending
  // `undefined` and receiving catalog's deliberately uninformative 401 — which
  // on the sign-in path is indistinguishable from a wrong password.
  it('throws before fetching when the credential is not configured', async () => {
    delete process.env.CATALOG_STAFF_WRITE_TOKEN;
    const calls = spyFetch({}, 200);
    await expect(createSeatMap('v1', 'Main')).rejects.toThrow(/CATALOG_STAFF_WRITE_TOKEN/);
    expect(calls).toHaveLength(0);
  });
});


describe('staff role propagation (TKT-197)', () => {
  it('carries the role from the catalog response into the principal', async () => {
    for (const role of ['admin', 'box_office', 'finance']) {
      spyFetch({ staff_id: 's1', organizer_id: 'o1', role }, 200);
      await expect(authenticateStaff('ada@example.test', 'pw')).resolves.toMatchObject({ role });
    }
  });

  // Catalog validates the stored role too, so reaching this means the contract
  // and this client disagree about the vocabulary. Refuse rather than mint a
  // session carrying a role the route matrix cannot classify — an unclassifiable
  // role in a session is the fail-open this ticket exists to prevent.
  it('refuses a response whose role is outside the vocabulary', async () => {
    spyFetch({ staff_id: 's1', organizer_id: 'o1', role: 'superuser' }, 200);
    await expect(authenticateStaff('ada@example.test', 'pw')).rejects.toThrow(/unrecognised staff role/);
  });

  it('refuses a response with no role at all', async () => {
    spyFetch({ staff_id: 's1', organizer_id: 'o1' }, 200);
    await expect(authenticateStaff('ada@example.test', 'pw')).rejects.toThrow(/unrecognised staff role/);
  });
});


describe('the order console reads (TKT-193)', () => {
  const ORDER = '11111111-1111-4111-8111-111111111111';
  const REF = '22222222-2222-4222-8222-222222222222';

  it('reads order status from commerce through the gateway', async () => {
    const calls = spyFetch({ order_id: ORDER, status: 'completed' }, 200);
    await expect(getOrderState(ORDER)).resolves.toEqual({
      ok: true,
      value: { orderId: ORDER, status: 'completed' },
    });
    expect(calls).toHaveLength(1);
    expect(calls[0].method).toBe('GET');
    expect(calls[0].url).toBe(`http://localhost:8080/api/commerce/orders/${ORDER}`);
  });

  it('reads the ticket bundle from access through the gateway', async () => {
    const calls = spyFetch({ order_ref: REF, tickets: [] }, 200);
    await getOrderTickets(REF);
    expect(calls[0].method).toBe('GET');
    expect(calls[0].url).toBe(`http://localhost:8080/api/access/orders/${REF}/tickets`);
  });

  // The two reads fail INDEPENDENTLY and the page renders each half on its own,
  // so the client must distinguish "this reference is unknown" from "the service
  // could not answer". Collapsing an outage into not-found tells a support agent
  // the customer's order does not exist.
  it.each([
    [404, 'not-found'],
    [500, 'unavailable'],
    [503, 'unavailable'],
    // We validate the shape before calling, so a 400 means our understanding of
    // the contract is wrong — which is a failure to answer, not an absence.
    [400, 'unavailable'],
  ])('turns %i into %s', async (status, kind) => {
    spyFetch({ error: 'nope' }, status);
    await expect(getOrderState(ORDER)).resolves.toEqual({ ok: false, kind });
    spyFetch({ error: 'nope' }, status);
    await expect(getOrderTickets(REF)).resolves.toEqual({ ok: false, kind });
  });

  // ai-review pass 1. A 200 carrying the wrong shape is a failure to answer, not
  // a successful read: without runtime validation, commerce answering `{}` would
  // render "Commerce reports this order as **undefined**" — a claim about an
  // order, sourced from nothing, at HTTP 200.
  it.each([
    ['an empty commerce body', {}],
    ['a commerce body missing status', { order_id: 'o1' }],
    ['a commerce status that is not a string', { order_id: 'o1', status: 7 }],
  ])('treats %s as unavailable, not as a status', async (_name, body) => {
    spyFetch(body, 200);
    await expect(getOrderState(ORDER)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  it.each([
    ['no tickets key', {}],
    ['a ticket with no id', { tickets: [{ issued_at: 'x', history: [] }] }],
    ['a history entry with no type', { tickets: [{ ticket_id: 't', issued_at: 'x', history: [{ id: 'e', occurred_at: 'x' }] }] }],
    ['a non-numeric sequence', { tickets: [{ ticket_id: 't', issued_at: 'x', history: [{ id: 'e', type: 'issued', sequence: 'two', occurred_at: 'x' }] }] }],
  ])('treats %s as unavailable, not as tickets', async (_name, body) => {
    spyFetch(body, 200);
    await expect(getOrderTickets(REF)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  // COS-7. qr_payload is the credential that admits at the gate, and qr_url
  // points at an UNAUTHENTICATED endpoint that renders it as an image. Either
  // one on a staff console is a working ticket for someone else's order, in
  // every screenshot and support transcript thereafter. Dropped here, at the
  // client boundary, so no page can render what it never receives.
  it('drops the QR credential before the page can see it', async () => {
    const raw = {
      order_ref: REF,
      tickets: [
        {
          ticket_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          qr_payload: 'SENTINEL-QR-PAYLOAD-VALUE',
          qr_url: '/SENTINEL-QR-URL-VALUE.png',
          issued_at: '2026-08-01T10:00:00Z',
          history: [
            { id: 'e1', type: 'issued', sequence: 1, occurred_at: '2026-08-01T10:00:00Z' },
            { id: 'e2', type: 'delivered', occurred_at: '2026-08-01T10:05:00Z' },
          ],
        },
      ],
    };
    spyFetch(raw, 200);
    const got = await getOrderTickets(REF);

    expect(got).toEqual({
      ok: true,
      value: [
        {
          ticketId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          issuedAt: '2026-08-01T10:00:00Z',
          history: [
            { id: 'e1', type: 'issued', sequence: 1, occurredAt: '2026-08-01T10:00:00Z' },
            { id: 'e2', type: 'delivered', sequence: undefined, occurredAt: '2026-08-01T10:05:00Z' },
          ],
        },
      ],
    });

    // Deep equality above already pins the shape; these pin the VALUES, which is
    // what actually leaks. A future field carrying the payload under another
    // name passes the shape check and fails this one.
    const serialized = JSON.stringify(got);
    expect(serialized).not.toContain('SENTINEL-QR-PAYLOAD-VALUE');
    expect(serialized).not.toContain('SENTINEL-QR-URL-VALUE');
    expect(serialized).not.toContain('qr_payload');
    expect(serialized).not.toContain('qr_url');
  });
});
