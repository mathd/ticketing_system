import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// One file for all three service clients (catalog.ts, commerce.ts, access.ts):
// they share these fixtures, and the response-validation rules they exercise
// live in one place (upstream.ts).
import { getOrderTickets, redeliverOrderTickets } from '../src/lib/access';
import {
  addSeatMapRow,
  addSeatMapSeat,
  addSeatMapSection,
  authenticateStaff,
  CatalogApiError,
  createChannel,
  createEvent,
  createPerformance,
  createTicketType,
  listChannelsForOperator,
  createSeatMap,
  editSeatMap,
  getSeatMapGeometry,
  getVenues,
  listSeatMapVersions,
  listVenueSeatMaps,
  publishPerformance,
  publishSeatMap,
  updateChannel,
  updateVenueGaCapacity,
} from '../src/lib/catalog';

// TKT-245: the organizer is no longer a module constant with a hardcoded default.
// Reads take it as an explicit parameter; writes take a signed assertion and take
// no organizer at all. These stand in for both.
const TEST_ORGANIZER_ID = '00000000-0000-0000-0000-000000000001';
const TEST_ASSERTION = 'v1.11111111-1111-1111-1111-111111111111.00000000-0000-0000-0000-000000000001.99999999999.testmac';
import { getOrderState, refundOrder } from '../src/lib/commerce';
import {
  getStaffAvailability,
  InventoryApiError,
  replaceChannelAllocations,
} from '../src/lib/inventory';

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
  // TKT-194. Deliberately a DIFFERENT value from the catalog one: the client
  // refuses equal credentials, so a suite that set both to the same string
  // would fail every refund test for a reason unrelated to what it is testing.
  process.env.COMMERCE_STAFF_WRITE_TOKEN = 'commerce-test-credential';
  // TKT-244. A THIRD distinct value, for the same reason.
  process.env.INVENTORY_STAFF_WRITE_TOKEN = 'inventory-test-credential';
  process.env.ACCESS_STAFF_WRITE_TOKEN = 'access-test-credential';
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
  delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
  delete process.env.ACCESS_STAFF_WRITE_TOKEN;
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
            organizer_id: TEST_ORGANIZER_ID,
            name: 'Le Petit Théâtre',
            ga_capacity: 350,
            created_at: '2026-07-20T00:00:00Z',
          },
        ],
      });
    });
    vi.stubGlobal('fetch', fetchSpy);

    const venues = await getVenues(TEST_ORGANIZER_ID);

    // Consumes the public contract through the gateway, scoped by organizer_id.
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(requestedUrl).toContain('/api/catalog/public/venues');
    expect(requestedUrl).toContain(`organizer_id=${TEST_ORGANIZER_ID}`);
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
    await expect(getVenues(TEST_ORGANIZER_ID)).rejects.toThrow(/venue read failed: 502/);
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
      organizer_id: TEST_ORGANIZER_ID,
      venue_id: 'v1',
      name: 'Floor',
      version: 1,
      status: 'draft',
      created_at: '2026-07-20T00:00:00Z',
    });
    const m = await createSeatMap('v1', 'Floor', TEST_ASSERTION);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/venues/v1/seat-maps');
    expect(calls[0].body).toMatchObject({ name: 'Floor' });
    expect(calls[0].body).not.toHaveProperty('organizer_id');
    expect(m.status).toBe('draft');
    expect(m.version).toBe(1);
  });

  it('adds a section, row, and seat to the right nested endpoints', async () => {
    const secCalls = spyFetch({ id: 's1', name: 'Orchestra', position: 1 });
    await addSeatMapSection('m1', { name: 'Orchestra', position: 1 }, TEST_ASSERTION);
    expect(secCalls[0].url).toContain('/api/catalog/seat-maps/m1/sections');
    expect(secCalls[0].body).toMatchObject({ name: 'Orchestra', position: 1 });
    expect(secCalls[0].body).not.toHaveProperty('organizer_id');

    const rowCalls = spyFetch({ id: 'r1', label: 'A', position: 1 });
    await addSeatMapRow('m1', { section_id: 's1', label: 'A', position: 1 }, TEST_ASSERTION);
    expect(rowCalls[0].url).toContain('/api/catalog/seat-maps/m1/rows');
    expect(rowCalls[0].body).toMatchObject({ section_id: 's1', label: 'A', position: 1 });

    const seatCalls = spyFetch({ id: 'x1', seat_identity: 'Orchestra/A/1', label: '1', position: 1 });
    const seat = await addSeatMapSeat('m1', { row_id: 'r1', label: '1', position: 1 }, TEST_ASSERTION);
    expect(seatCalls[0].url).toContain('/api/catalog/seat-maps/m1/seats');
    expect(seatCalls[0].body).toMatchObject({ row_id: 'r1', label: '1', position: 1 });
    // Identity is composed server-side and returned; the client does not mint it.
    expect(seat.seat_identity).toBe('Orchestra/A/1');
  });

  it('reads a venue seat-map list and unwraps the envelope', async () => {
    const calls = spyFetch(
      { seat_maps: [{ id: 'm1', organizer_id: TEST_ORGANIZER_ID, venue_id: 'v1', name: 'Floor', version: 1, status: 'draft', created_at: '2026-07-20T00:00:00Z' }] },
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
        map: { id: 'm1', organizer_id: TEST_ORGANIZER_ID, venue_id: 'v1', name: 'Floor', version: 1, status: 'draft', created_at: '2026-07-20T00:00:00Z' },
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
    await expect(
      addSeatMapSeat('m1', { row_id: 'r1', label: '1', position: 1 }, TEST_ASSERTION),
    ).rejects.toThrow(/conflict/);
  });
});

// TKT-105: editing, version history, GA config, and error-body surfacing.
describe('seat-map edit + versioning client (TKT-105)', () => {
  const publishedMap = {
    id: 'm2',
    organizer_id: TEST_ORGANIZER_ID,
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
      editSeatMap('m1', { sections: [] }, TEST_ASSERTION),
    ).rejects.toThrow(/orphan a seat identity pinned/);
  });

  it('throws a typed CatalogApiError carrying the status', async () => {
    spyFetch({ error: 'nope' }, 409);
    try {
      await editSeatMap('m1', { sections: [] }, TEST_ASSERTION);
      throw new Error('should have thrown');
    } catch (e) {
      expect(e).toBeInstanceOf(CatalogApiError);
      expect((e as CatalogApiError).status).toBe(409);
    }
  });

  it('posts the full replacement geometry to the edit endpoint and returns the new version', async () => {
    const calls = spyFetch(publishedMap, 201);
    const body = {
      sections: [{ name: 'Orchestra', position: 1, rows: [{ label: 'A', position: 1, seats: [{ label: '1', position: 1 }] }] }],
    };
    const nv = await editSeatMap('m1', body, TEST_ASSERTION);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/seat-maps/m1/edit');
    expect(calls[0].body).toMatchObject(body);
    expect(nv.version).toBe(2);
  });

  it('publishes a draft map through the publish endpoint, carrying the assertion', async () => {
    const calls = spyFetch(publishedMap, 200);
    await publishSeatMap('m1', TEST_ASSERTION);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/seat-maps/m1/publish');
    // TKT-251: the transition is organizer-scoped server-side, so the assertion
    // has to reach it. Asserted HERE rather than in a browser spec: the page
    // submits a form POST and this call happens server-side, so Playwright
    // observes nothing of it.
    expect(calls[0].headers['X-Catalog-Organizer-Assertion']).toBe(TEST_ASSERTION);
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
    const calls = spyFetch({ id: 'v1', organizer_id: TEST_ORGANIZER_ID, name: 'Hall', ga_capacity: 250, created_at: '2026-07-20T00:00:00Z' }, 200);
    const v = await updateVenueGaCapacity('v1', 250, TEST_ASSERTION);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].url).toContain('/api/catalog/venues/v1/ga-capacity');
    expect(calls[0].body).toMatchObject({ ga_capacity: 250 });
    expect(calls[0].body).not.toHaveProperty('organizer_id');
    expect(v.ga_capacity).toBe(250);
  });
});

describe('staff authentication client (TKT-190)', () => {
  it('posts the credential to the catalog through the gateway, like every other call', async () => {
    const calls = spyFetch({ staff_id: 's1', organizer_id: 'o1', role: 'admin', organizer_assertion: TEST_ASSERTION }, 200);
    const principal = await authenticateStaff('ada@example.test', 'correct horse');
    expect(calls[0].method).toBe('POST');
    // Through the gateway, not straight at the catalog container: one network
    // path means the back office never needs the shared internal credential.
    expect(calls[0].url).toContain('/api/catalog/staff/authenticate');
    expect(calls[0].body).toEqual({ identifier: 'ada@example.test', password: 'correct horse' });
    expect(principal).toEqual({
      staffId: 's1',
      organizerId: 'o1',
      role: 'admin',
      // The credential the session will forward on every write (TKT-245).
      organizerAssertion: TEST_ASSERTION,
    });
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
    const calls = spyFetch({ staff_id: 's1', organizer_id: 'o1', role: 'admin', organizer_assertion: TEST_ASSERTION }, 200);
    await authenticateStaff('ada@example.test', 'correct horse');
    expect(calls[0].url).not.toContain('correct horse');
  });
});


describe('catalog write credential (TKT-191)', () => {
  const HEADER = 'X-Catalog-Staff-Write-Token';

  it('attaches the credential to every catalog write', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ id: 'm1', organizer_id: 'o', venue_id: 'v', name: 'M', version: 1, status: 'draft', created_at: 'x' });
    await createSeatMap('v1', 'Main', TEST_ASSERTION);
    expect(calls[0].headers[HEADER]).toBe('the-credential');
    // And the assertion rides alongside it: the credential says WHO is calling,
    // the assertion says which organizer for (TKT-245).
    expect(calls[0].headers['X-Catalog-Organizer-Assertion']).toBe(TEST_ASSERTION);
  });

  // TKT-251: the path-id transitions used to send the credential and NO
  // assertion, which is how a staff token reached another tenant's slot. Both
  // headers now, on the two transitions the back office actually calls.
  it('attaches both headers to the path-id transitions', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch(
      { id: 'p1', organizer_id: 'o1', event_id: 'e1', venue_id: 'v1', kind: 'performance', status: 'published', timezone: 'America/Toronto', created_at: 'x' },
      200,
    );
    await publishPerformance('p1', TEST_ASSERTION);
    expect(calls[0].url).toContain('/api/catalog/performances/p1/publish');
    expect(calls[0].headers[HEADER]).toBe('the-credential');
    expect(calls[0].headers['X-Catalog-Organizer-Assertion']).toBe(TEST_ASSERTION);
  });

  // Guarded like every other unsafe operation: leaving it out would mean an
  // exception list inside a fail-closed scheme.
  it('attaches it to the sign-in call too', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch(
      { staff_id: 's1', organizer_id: 'o1', role: 'admin', organizer_assertion: TEST_ASSERTION },
      200,
    );
    await authenticateStaff('ada@example.test', 'pw');
    expect(calls[0].headers[HEADER]).toBe('the-credential');
    // But NOT an assertion: this is the call that issues one. Requiring it here
    // would be a chicken-and-egg 401 nobody could ever satisfy.
    expect(calls[0].headers['X-Catalog-Organizer-Assertion']).toBeUndefined();
  });

  it('does NOT attach it to reads — they are public and must stay so', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ venues: [] }, 200);
    await getVenues(TEST_ORGANIZER_ID);
    expect(calls[0].headers[HEADER]).toBeUndefined();
  });

  // Fail locally with a message naming the variable, rather than sending
  // `undefined` and receiving catalog's deliberately uninformative 401 — which
  // on the sign-in path is indistinguishable from a wrong password.
  it('throws before fetching when the credential is not configured', async () => {
    delete process.env.CATALOG_STAFF_WRITE_TOKEN;
    const calls = spyFetch({}, 200);
    await expect(createSeatMap('v1', 'Main', TEST_ASSERTION)).rejects.toThrow(/CATALOG_STAFF_WRITE_TOKEN/);
    expect(calls).toHaveLength(0);
  });
});


describe('staff role propagation (TKT-197)', () => {
  it('carries the role from the catalog response into the principal', async () => {
    for (const role of ['admin', 'box_office', 'finance']) {
      spyFetch({ staff_id: 's1', organizer_id: 'o1', role, organizer_assertion: TEST_ASSERTION }, 200);
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
  // These carry hex LETTERS on purpose. An all-digit uuid makes
  // `.toUpperCase()` a no-op, so the case-insensitivity test below would pass
  // against a case-SENSITIVE comparison — a fixture that cannot express the
  // negative it claims to prove (AGENTS.md; caught by mutation check M14).
  const ORDER = 'abcdef01-2345-4678-89ab-cdef01234567';
  const REF = 'fedcba98-7654-4321-8ba9-876543210fed';
  const OTHER = 'deadbeef-1234-4567-89ab-cdef01234567';

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
    // order_id: ORDER, not a placeholder. sameIdentity runs FIRST, so a fixture
    // naming a different order is rejected before the status check is ever
    // reached — and the test would stay green with that check deleted
    // (ai-review pass 3).
    ['a commerce body missing status', { order_id: ORDER }],
    ['a commerce status that is not a string', { order_id: ORDER, status: 7 }],
    ['a commerce status that is empty', { order_id: ORDER, status: '' }],
  ])('treats %s as unavailable, not as a status', async (_name, body) => {
    spyFetch(body, 200);
    await expect(getOrderState(ORDER)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  it.each([
    ['no tickets key', { order_ref: REF }],
    ['a ticket with no id', { order_ref: REF, tickets: [{ issued_at: 'x', history: [] }] }],
    ['a history entry with no type', { order_ref: REF, tickets: [{ ticket_id: 't', issued_at: 'x', history: [{ id: 'e', occurred_at: 'x' }] }] }],
    ['a non-numeric sequence', { order_ref: REF, tickets: [{ ticket_id: 't', issued_at: 'x', history: [{ id: 'e', type: 'issued', sequence: 'two', occurred_at: 'x' }] }] }],
    // The access contract makes history required; defaulting it to [] would
    // make the page say "no lifecycle events recorded yet" about a ticket,
    // when what happened is that access did not answer properly.
    ['a ticket with no history at all', { order_ref: REF, tickets: [{ ticket_id: 't', issued_at: 'x' }] }],
    // A chain position is an integer >= 1 (openapi.yaml). "#0" rendered beside
    // an event would read as a gap in the integrity chain (ADR-025 §D5).
    ['a zero sequence', { order_ref: REF, tickets: [{ ticket_id: 't', issued_at: 'x', history: [{ id: 'e', type: 'issued', sequence: 0, occurred_at: 'x' }] }] }],
    ['a fractional sequence', { order_ref: REF, tickets: [{ ticket_id: 't', issued_at: 'x', history: [{ id: 'e', type: 'issued', sequence: 1.5, occurred_at: 'x' }] }] }],
  ])('treats %s as unavailable, not as tickets', async (_name, body) => {
    spyFetch(body, 200);
    await expect(getOrderTickets(REF)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  // ai-review pass 2. The page labels each half with the identifier the OPERATOR
  // typed, so a response about a DIFFERENT order — misrouted, stale, or served
  // from a cache that ignored no-store — would appear under the wrong heading.
  // That is the misreading the page's caveat exists to prevent, arriving by the
  // back door, so the client refuses it rather than the page annotating it.
  it('refuses a commerce response about a different order', async () => {
    spyFetch({ order_id: OTHER, status: 'completed' }, 200);
    await expect(getOrderState(ORDER)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  it('refuses a ticket bundle about a different reference', async () => {
    spyFetch({ order_ref: OTHER, tickets: [] }, 200);
    await expect(getOrderTickets(REF)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  // Both sides are UUIDs, so case is a formatting choice and not a different
  // order — refusing on it would be a self-inflicted outage.
  it('accepts the same identifier in a different case', async () => {
    spyFetch({ order_id: ORDER.toUpperCase(), status: 'completed' }, 200);
    await expect(getOrderState(ORDER)).resolves.toMatchObject({ ok: true });
  });

  // TKT-194. The refund goes DIRECT to commerce, not through the gateway: the
  // gateway edge-denies /internal/ by construction and adding an exception would
  // publish a money-moving endpoint to the internet, while granting nothing the
  // in-network call does not. Access already reaches commerce this way.
  it('sends the refund direct to commerce with the staff credential', async () => {
    const calls = spyFetch(
      {
        refund_id: 'r1', order_id: ORDER, quantity: 1, amount: 1250, currency: 'EUR',
        refund_status: 'partial', refunded_quantity: 1, refunded_amount: 1250,
        replay: false, tickets_voided: true, capacity_returned: true,
      },
      200,
    );
    const got = await refundOrder({
      orderId: ORDER, quantity: 1, reason: 'customer called',
      actor: 'staff-42', organizerId: 'org-1', idempotencyKey: 'key-1',
    });

    expect(got).toMatchObject({ ok: true });
    // COMMERCE_URL is unset in the suite, so this is the client's own default —
    // which is what a developer running `pnpm dev` outside compose gets.
    expect(calls[0].url).toBe(`http://localhost:8082/internal/orders/${ORDER}/refunds`);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].headers['X-Commerce-Staff-Write-Token']).toBe('commerce-test-credential');
    expect(calls[0].headers['Idempotency-Key']).toBe('key-1');
    // Never the shared internal token, which the back office does not hold.
    expect(calls[0].headers['X-Internal-Token']).toBeUndefined();
  });

  // COS-5. `actor` is the attribution for an operation that moves money. Taking
  // it from the form would make a refund attributable to whatever the client
  // typed, which is not attributable at all — and it matters more now that box
  // office can refund, because attribution is the control that remains.
  it('sends only the four contract fields, with actor and organizer from the caller', async () => {
    const calls = spyFetch({ refund_id: 'r1', order_id: ORDER, quantity: 1, amount: 1, currency: 'EUR', refund_status: 'partial', refunded_quantity: 1, refunded_amount: 1, replay: false, tickets_voided: true, capacity_returned: true }, 200);
    await refundOrder({ orderId: ORDER, quantity: 2, reason: 'why', actor: 'staff-42', organizerId: 'org-1', idempotencyKey: 'k' });
    expect(calls[0].body).toEqual({
      organizer_id: 'org-1', quantity: 2, actor: 'staff-42', reason: 'why',
    });
  });

  it.each([
    [404, 'not-found'],
    [409, 'refused'],
    [400, 'refused'],
    [500, 'ambiguous'],
    [502, 'ambiguous'],
    [503, 'ambiguous'],
  ])('maps a %i to %s without inventing a refund', async (status, kind) => {
    spyFetch({ error: 'commerce says no' }, status);
    const got = await refundOrder({ orderId: ORDER, quantity: 1, reason: 'r', actor: 'a', organizerId: 'o', idempotencyKey: 'k' });
    expect(got).toMatchObject({ ok: false, kind, message: 'commerce says no' });
  });

  // A 200 whose body is not a Refund is not a refund. Rendering a success
  // section from it would claim money moved on the strength of a shape we did
  // not check — and after an ambiguous response the page must say retry, not
  // that nothing happened.
  it.each([
    ['an empty body', {}],
    ['a refund for a different order', { refund_id: 'r1', order_id: OTHER, quantity: 1, amount: 1, currency: 'EUR', refund_status: 'partial', refunded_quantity: 1, refunded_amount: 1, replay: false, tickets_voided: true, capacity_returned: true }],
    ['a fractional amount', { refund_id: 'r1', order_id: ORDER, quantity: 1, amount: 12.5, currency: 'EUR', refund_status: 'partial', refunded_quantity: 1, refunded_amount: 1, replay: false, tickets_voided: true, capacity_returned: true }],
    // Commerce declares these int64. Past 2^53 a JSON number no longer maps
    // one-to-one onto a JS number, so a larger int64 arrives already rounded and
    // an isInteger check passes it — presenting a figure that is not the one
    // commerce sent as the exact refund amount.
    //
    // MAX_SAFE_INTEGER + 1 rather than a literal: the literal that demonstrates
    // this is itself unrepresentable, so writing it out is a lint error (and
    // would silently become a different number than the one intended).
    ['an amount past what JS can represent exactly', { refund_id: 'r1', order_id: ORDER, quantity: 1, amount: Number.MAX_SAFE_INTEGER + 1, currency: 'EUR', refund_status: 'partial', refunded_quantity: 1, refunded_amount: 1, replay: false, tickets_voided: true, capacity_returned: true }],
    ['a refund_status outside the enum', { refund_id: 'r1', order_id: ORDER, quantity: 1, amount: 1, currency: 'EUR', refund_status: 'reversed', refunded_quantity: 1, refunded_amount: 1, replay: false, tickets_voided: true, capacity_returned: true }],
    ['a non-boolean tickets_voided', { refund_id: 'r1', order_id: ORDER, quantity: 1, amount: 1, currency: 'EUR', refund_status: 'partial', refunded_quantity: 1, refunded_amount: 1, replay: false, tickets_voided: 'yes', capacity_returned: true }],
  ])('treats %s as ambiguous rather than a refund', async (_name, body) => {
    spyFetch(body, 200);
    const got = await refundOrder({ orderId: ORDER, quantity: 1, reason: 'r', actor: 'a', organizerId: 'o', idempotencyKey: 'k' });
    expect(got).toMatchObject({ ok: false, kind: 'ambiguous' });
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

describe('the channel registry client (TKT-236)', () => {
  const STAFF = 'X-Catalog-Staff-Write-Token';
  const INTERNAL = 'X-Internal-Token';

  it('reads the operator list DIRECT from catalog, not through the gateway', async () => {
    const calls = spyFetch({ channels: [] }, 200);
    await listChannelsForOperator(TEST_ASSERTION);
    // CATALOG_URL is unset in the suite, so this is the client's own default —
    // what a developer running `pnpm dev` outside compose gets. Same shape as
    // the commerce refund's assertion above: the env var is read at module load,
    // so setting it inside a test would not take effect, and a test that
    // pretended otherwise would assert nothing.
    expect(calls[0].url).toBe('http://localhost:8081/internal/channels');
    // The gateway edge-denies every /api/<svc>/internal/ route by construction
    // (ADR-002), so routing this through the gateway would 404 in production
    // while passing any test that only checked the path.
    expect(calls[0].url).not.toContain('/api/catalog');
  });

  it('authenticates the operator read with the STAFF credential, never the internal token', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ channels: [] }, 200);
    await listChannelsForOperator(TEST_ASSERTION);
    expect(calls[0].headers[STAFF]).toBe('the-credential');
    // The posture the whole ticket rests on: this process does not hold the
    // shared internal token and must never send one (compose.yaml).
    expect(calls[0].headers[INTERNAL]).toBeUndefined();
  });

  // TKT-245 replaced the query parameter with the assertion header. There is no
  // longer an organizer in the URL to encode -- which is the point: this read
  // returns a tenant's whole channel configuration, and naming the tenant in a
  // query string is the enumeration ADR-053 recorded.
  it('names no organizer in the URL and sends the assertion instead', async () => {
    const calls = spyFetch({ channels: [] }, 200);
    await listChannelsForOperator(TEST_ASSERTION);
    expect(calls[0].url).not.toContain('organizer_id');
    expect(calls[0].headers['X-Catalog-Organizer-Assertion']).toBe(TEST_ASSERTION);
  });

  // A hand-mounted route is outside catalog's response validation (ADR-009), so
  // nothing upstream guarantees the key is present. `undefined` here would crash
  // the page's .map() with a stack trace instead of rendering an empty table.
  it('survives a body with no channels array', async () => {
    spyFetch({}, 200);
    await expect(listChannelsForOperator(TEST_ASSERTION)).resolves.toEqual([]);
  });

  it('surfaces a refusal as CatalogApiError with its status', async () => {
    spyFetch({ error: 'unauthorized' }, 401);
    await expect(listChannelsForOperator(TEST_ASSERTION)).rejects.toMatchObject({ status: 401 });
  });

  it('creates through the gateway with the write credential', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ id: 'c1', code: 'pos' }, 201);
    await createChannel(TEST_ASSERTION, { code: 'pos', displayName: 'Box office', kind: 'pos' });
    expect(calls[0].url).toContain('/api/catalog/channels');
    expect(calls[0].headers[STAFF]).toBe('the-credential');
    expect(calls[0].headers[INTERNAL]).toBeUndefined();
  });

  // The contract's `default: true` only applies when the key is ABSENT. Sending
  // `enabled: false` explicitly is a different request, and conflating the two
  // is how a channel is created invisible to the storefront.
  it('omits enabled when the channel is created available, and sends false when it is not', async () => {
    let calls = spyFetch({ id: 'c1' }, 201);
    await createChannel(TEST_ASSERTION, { code: 'pos', displayName: 'Box office', kind: 'pos' });
    expect(calls[0].body).not.toHaveProperty('enabled');

    calls = spyFetch({ id: 'c2' }, 201);
    await createChannel(TEST_ASSERTION, { code: 'x', displayName: 'X', kind: 'web', enabled: false });
    expect((calls[0].body as { enabled?: boolean }).enabled).toBe(false);
  });

  // The PUT is a full replacement. An omitted field is not "unchanged" — it is
  // absent, and for `enabled` that reads as false.
  it('sends every mutable field on update, and NO organizer', async () => {
    const calls = spyFetch({ id: 'c1' }, 200);
    await updateChannel(TEST_ASSERTION, 'c1', {
      code: 'pos', displayName: 'Counter', kind: 'pos', enabled: false,
    });
    expect(calls[0].url).toContain('/api/catalog/channels/c1');
    // The write is still scoped to a tenant -- the channel id comes from a form
    // field and an id is not an authorization boundary -- but the tenant now
    // comes from the ASSERTION, not the body (TKT-245). toEqual, not
    // toMatchObject: an extra organizer_id creeping back in must fail here.
    expect(calls[0].body).toEqual({
      code: 'pos',
      display_name: 'Counter',
      kind: 'pos',
      enabled: false,
    });
    expect(calls[0].headers['X-Catalog-Organizer-Assertion']).toBe(TEST_ASSERTION);
  });
});

// The inventory client (TKT-244, ADR-057). Two operations, direct in-network, behind a
// credential this process did not previously hold.
describe('inventory allocation client', () => {
  const SLOT = '11111111-1111-1111-1111-111111111111';

  it('reads staff availability direct, with its own credential and never the shared one', async () => {
    const calls = spyFetch(
      { slot_id: SLOT, capacity: 100, buyer_held: 0, operational_held: 0, reservation_held: 0,
        confirmed: 0, available: 100, public_available: 60, offering_status: 'open', channels: [] },
      200,
    );
    await getStaffAvailability(SLOT, 'org-1');

    // INVENTORY_URL is unset in the suite, so this is the client's own default — what a
    // developer running `pnpm dev` outside compose gets.
    expect(calls[0].url).toBe(`http://localhost:8081/internal/slots/${SLOT}/availability?organizer_id=org-1`);
    expect(calls[0].method).toBe('GET');
    expect(calls[0].headers['X-Inventory-Staff-Write-Token']).toBe('inventory-test-credential');
    // Never the shared internal token, which the back office does not hold.
    expect(calls[0].headers['X-Internal-Token']).toBeUndefined();
    // Nor another service's credential: they exist to have different blast radii.
    expect(calls[0].headers['X-Catalog-Staff-Write-Token']).toBeUndefined();
    expect(calls[0].headers['X-Commerce-Staff-Write-Token']).toBeUndefined();
  });

  it('replaces the whole allocation set in one PUT, carrying every field', async () => {
    const calls = spyFetch({ slot_id: SLOT, allocations: [] }, 200);
    await replaceChannelAllocations(SLOT, {
      organizer_id: 'org-1',
      allocations: [
        { channel: 'reseller-acme', cap: 40, requires_code: true, sold_by: '22222222-2222-2222-2222-222222222222' },
        { channel: 'presale', cap: 20, requires_code: false },
      ],
    });

    expect(calls).toHaveLength(1); // ONE request: per-row saves would round-trip a stale set
    expect(calls[0].url).toBe(`http://localhost:8081/internal/slots/${SLOT}/channel-allocations`);
    expect(calls[0].method).toBe('PUT');
    expect(calls[0].headers['X-Inventory-Staff-Write-Token']).toBe('inventory-test-credential');
    expect(calls[0].headers['X-Internal-Token']).toBeUndefined();
    expect(calls[0].body).toEqual({
      organizer_id: 'org-1',
      allocations: [
        { channel: 'reseller-acme', cap: 40, requires_code: true, sold_by: '22222222-2222-2222-2222-222222222222' },
        { channel: 'presale', cap: 20, requires_code: false },
      ],
    });
  });

  // The refusal has to arrive with its code and channel intact, or the editor cannot put
  // the message beside the right row.
  it('carries the code and the named channel off a coded 409', async () => {
    spyFetch(
      { error: 'channel "reseller-acme" is allocated below its current consumption',
        code: 'allocation_cap_below_consumption', channel: 'reseller-acme' },
      409,
    );
    await expect(
      replaceChannelAllocations(SLOT, { organizer_id: 'org-1', allocations: [] }),
    ).rejects.toMatchObject({
      status: 409,
      code: 'allocation_cap_below_consumption',
      channel: 'reseller-acme',
    });
  });

  it('carries the code off the over-capacity 409, which names no channel', async () => {
    spyFetch({ error: 'channel allocations exceed pool capacity', code: 'allocation_caps_exceed_capacity' }, 409);
    const err = await replaceChannelAllocations(SLOT, { organizer_id: 'org-1', allocations: [] })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InventoryApiError);
    expect(err).toMatchObject({ status: 409, code: 'allocation_caps_exceed_capacity' });
    expect((err as InventoryApiError).channel).toBeUndefined();
  });

  // A missing credential is a configuration defect, not an inventory outage: it must say
  // so and name the variable, rather than surfacing as a bare 401 the operator would read
  // as a permissions problem.
  it('refuses to call without a credential, naming the variable', async () => {
    delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
    spyFetch({}, 200);
    await expect(getStaffAvailability(SLOT, 'org-1')).rejects.toThrow(/INVENTORY_STAFF_WRITE_TOKEN/);
    await expect(
      replaceChannelAllocations(SLOT, { organizer_id: 'org-1', allocations: [] }),
    ).rejects.toThrow(/INVENTORY_STAFF_WRITE_TOKEN/);
  });
});

// TKT-200. The three creates forward a caller-supplied Idempotency-Key.
//
// This is a WIRING test, and it asserts the value that crossed the boundary
// rather than that a function was called: the mechanism that fails silently here
// is a client that accepts the key and never puts it on the request, which looks
// identical from the call site.
describe('catalog create idempotency (TKT-200)', () => {
  function captureHeaders(body: unknown) {
    let sent: Headers | undefined;
    const spy = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      sent = new Headers(init?.headers);
      return jsonResponse(body);
    });
    vi.stubGlobal('fetch', spy);
    return { spy, header: () => sent?.get('Idempotency-Key') };
  }

  it('sends the caller\'s key verbatim on createEvent', async () => {
    const { header } = captureHeaders({
      id: '00000000-0000-0000-0000-0000000000e1',
      organizer_id: TEST_ORGANIZER_ID,
      name: { en: 'Night', fr: 'Nuit' },
      created_at: '2026-08-26T00:00:00Z',
    });

    await createEvent(TEST_ASSERTION, { en: 'Night', fr: 'Nuit' }, 'key-from-the-form');

    // Verbatim: not hashed, not prefixed, not regenerated. A client that minted
    // its own would give a retry a different key and create a second row.
    expect(header()).toBe('key-from-the-form');
  });

  it('sends the caller\'s key on createPerformance and createTicketType', async () => {
    const perf = captureHeaders({
      id: '00000000-0000-0000-0000-0000000000p1',
      organizer_id: TEST_ORGANIZER_ID,
      event_id: '00000000-0000-0000-0000-0000000000e1',
      venue_id: '00000000-0000-0000-0000-0000000000a2',
      kind: 'performance',
      status: 'draft',
      starts_at: '2026-09-01T20:00:00Z',
      timezone: 'UTC',
      created_at: '2026-08-26T00:00:00Z',
    });
    await createPerformance(
      TEST_ASSERTION,
      {
        eventId: '00000000-0000-0000-0000-0000000000e1',
        venueId: '00000000-0000-0000-0000-0000000000a2',
        startsAt: '2026-09-01T20:00:00Z',
        timezone: 'UTC',
      },
      'perf-key',
    );
    expect(perf.header()).toBe('perf-key');

    const tt = captureHeaders({
      id: '00000000-0000-0000-0000-0000000000t1',
      organizer_id: TEST_ORGANIZER_ID,
      performance_id: '00000000-0000-0000-0000-0000000000p1',
      name: { en: 'GA', fr: 'GA' },
      price: { amount: 2500, currency: 'EUR' },
      created_at: '2026-08-26T00:00:00Z',
    });
    await createTicketType(
      TEST_ASSERTION,
      {
        performanceId: '00000000-0000-0000-0000-0000000000p1',
        name: { en: 'GA', fr: 'GA' },
        amount: 2500,
        currency: 'EUR',
      },
      'tt-key',
    );
    expect(tt.header()).toBe('tt-key');
  });

  it('still sends the staff credential and the organizer assertion alongside it', async () => {
    // The key is an ADDITION, not a replacement. A refactor that built fresh
    // headers around the key and dropped the two credentials would 401 in
    // production and pass a test that only looked at Idempotency-Key.
    let sent: Headers | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_i: RequestInfo | URL, init?: RequestInit) => {
        sent = new Headers(init?.headers);
        return jsonResponse({
          id: '00000000-0000-0000-0000-0000000000e2',
          organizer_id: TEST_ORGANIZER_ID,
          name: { en: 'N', fr: 'N' },
          created_at: '2026-08-26T00:00:00Z',
        });
      }),
    );

    await createEvent(TEST_ASSERTION, { en: 'N', fr: 'N' }, 'k');

    expect(sent?.get('X-Catalog-Staff-Write-Token')).toBe('test-credential');
    expect(sent?.get('X-Catalog-Organizer-Assertion')).toBe(TEST_ASSERTION);
    expect(sent?.get('Idempotency-Key')).toBe('k');
  });
});

// TKT-203 / ADR-068. The resend goes DIRECT to access, not through the gateway: the
// operation lives under /internal/, which the gateway edge-denies by construction. Note
// the back office ALSO reads access through the gateway (the ticket bundle above) — that
// one is a public route, this one is not.
describe('ticket resend client', () => {
  const ORDER = 'abcdef01-2345-4678-89ab-cdef01234567';
  const OTHER = 'deadbeef-1234-4567-89ab-cdef01234567';
  const OK = { order_id: ORDER, ticket_count: 2, replay: false };

  it('sends the resend direct to access with the staff credential', async () => {
    const calls = spyFetch(OK, 200);
    const got = await redeliverOrderTickets({
      orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'key-1',
    });

    expect(got).toMatchObject({ ok: true, value: { ticketCount: 2, replay: false } });
    // ACCESS_URL is unset in the suite, so this is the client's own default — what a
    // developer running `pnpm dev` outside compose gets.
    expect(calls[0].url).toBe(`http://localhost:8084/internal/orders/${ORDER}/redeliveries`);
    expect(calls[0].method).toBe('POST');
    expect(calls[0].headers['X-Access-Staff-Write-Token']).toBe('access-test-credential');
    expect(calls[0].headers['Idempotency-Key']).toBe('key-1');
    // Never the shared internal token, which the back office does not hold.
    expect(calls[0].headers['X-Internal-Token']).toBeUndefined();
  });

  // COS-2, and the assertion this whole ticket turns on. The request must not carry a
  // recipient AT ALL — not an unused one, not an empty one, not one the server would
  // ignore. A field that exists is a field a future edit can start trusting.
  //
  // Asserted on the SERIALIZED body, not the typed object: a field added under another
  // name still passes a shape check written against the type, and it is the bytes on the
  // wire that would carry an address.
  it('sends no recipient of any kind in the request', async () => {
    const calls = spyFetch(OK, 200);
    await redeliverOrderTickets({
      orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'key-1',
    });

    expect(calls[0].body).toEqual({ organizer_id: 'org-1' });
    const serialized = JSON.stringify(calls[0].body);
    for (const forbidden of ['email', 'mail', 'address', 'recipient', 'to', 'guest', 'ref']) {
      expect(serialized).not.toContain(forbidden);
    }
  });

  // COS-6. Even if access answered with an address — a future contract change, a
  // misconfigured build, a compromised service — this client must not hand it upward.
  // Asserted at the boundary the value would cross, on the serialized result.
  it('does not surface a buyer address even when access sends one', async () => {
    spyFetch({ ...OK, email: 'buyer@example.test', ticket_link: 'https://x/en/tickets/abc' }, 200);
    const got = await redeliverOrderTickets({
      orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'key-1',
    });

    expect(got).toEqual({ ok: true, value: { orderId: ORDER, ticketCount: 2, replay: false } });
    const serialized = JSON.stringify(got);
    expect(serialized).not.toContain('buyer@example.test');
    expect(serialized).not.toContain('en/tickets/abc');
  });

  // Each refusal calls for a DIFFERENT operator action, so each gets its own kind. A
  // single "refused" would tell an agent to give up on a 503 that means "try again in a
  // moment", and to retry a 429 that means "not today".
  it.each([
    [404, 'not-found'],
    [503, 'not-yet'],
    [429, 'too-many'],
    [409, 'refused'],
    [400, 'refused'],
    [500, 'ambiguous'],
    [502, 'ambiguous'],
  ])('maps %i to %s', async (status, kind) => {
    spyFetch({ error: 'no' }, status);
    await expect(
      redeliverOrderTickets({ orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'k' }),
    ).resolves.toMatchObject({ ok: false, kind });
  });

  it('reports a replay as a replay rather than a fresh send', async () => {
    spyFetch({ ...OK, replay: true }, 200);
    await expect(
      redeliverOrderTickets({ orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'k' }),
    ).resolves.toMatchObject({ ok: true, value: { replay: true } });
  });

  // A response about a DIFFERENT order is refused rather than rendered: the page would
  // otherwise say "re-sent" under the heading of the order the operator typed.
  it('refuses a response about a different order', async () => {
    spyFetch({ ...OK, order_id: OTHER }, 200);
    await expect(
      redeliverOrderTickets({ orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'k' }),
    ).resolves.toMatchObject({ ok: false, kind: 'ambiguous' });
  });

  // A 200 that says nothing was sent is not a success. Rendering "re-sent 0 tickets"
  // would report an action that did not happen.
  it.each([
    ['a zero count', { ...OK, ticket_count: 0 }],
    ['a missing count', { order_id: ORDER, replay: false }],
    ['a missing replay flag', { order_id: ORDER, ticket_count: 1 }],
  ])('treats %s as ambiguous rather than a send', async (_name, body) => {
    spyFetch(body, 200);
    await expect(
      redeliverOrderTickets({ orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'k' }),
    ).resolves.toMatchObject({ ok: false, kind: 'ambiguous' });
  });

  // A missing credential is a CONFIGURATION defect, not an upstream outage. Reporting it
  // as ambiguous would tell the operator to retry a request that can never succeed, and
  // the message must name the variable so the fix is obvious.
  it('fails loudly, naming the variable, when the credential is unset', async () => {
    delete process.env.ACCESS_STAFF_WRITE_TOKEN;
    spyFetch(OK, 200);
    await expect(
      redeliverOrderTickets({ orderId: ORDER, organizerId: 'org-1', idempotencyKey: 'k' }),
    ).rejects.toThrow('ACCESS_STAFF_WRITE_TOKEN');
  });
});
