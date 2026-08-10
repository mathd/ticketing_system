import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// One file for all three service clients (catalog.ts, commerce.ts, access.ts):
// they share these fixtures, and the response-validation rules they exercise
// live in one place (upstream.ts).
import { getOrderTickets } from '../src/lib/access';
import {
  addSeatMapRow,
  addSeatMapSeat,
  addSeatMapSection,
  authenticateStaff,
  CatalogApiError,
  createChannel,
  listChannelsForOperator,
  createSeatMap,
  DEFAULT_ORGANIZER_ID,
  editSeatMap,
  getSeatMapGeometry,
  getVenues,
  listSeatMapVersions,
  listVenueSeatMaps,
  publishSeatMap,
  updateChannel,
  updateVenueGaCapacity,
} from '../src/lib/catalog';
import { getOrderState, refundOrder } from '../src/lib/commerce';

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
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
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
    await listChannelsForOperator('org-1');
    // CATALOG_URL is unset in the suite, so this is the client's own default —
    // what a developer running `pnpm dev` outside compose gets. Same shape as
    // the commerce refund's assertion above: the env var is read at module load,
    // so setting it inside a test would not take effect, and a test that
    // pretended otherwise would assert nothing.
    expect(calls[0].url).toBe('http://localhost:8081/internal/channels?organizer_id=org-1');
    // The gateway edge-denies every /api/<svc>/internal/ route by construction
    // (ADR-002), so routing this through the gateway would 404 in production
    // while passing any test that only checked the path.
    expect(calls[0].url).not.toContain('/api/catalog');
  });

  it('authenticates the operator read with the STAFF credential, never the internal token', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ channels: [] }, 200);
    await listChannelsForOperator('org-1');
    expect(calls[0].headers[STAFF]).toBe('the-credential');
    // The posture the whole ticket rests on: this process does not hold the
    // shared internal token and must never send one (compose.yaml).
    expect(calls[0].headers[INTERNAL]).toBeUndefined();
  });

  it('encodes the organizer rather than interpolating it raw', async () => {
    const calls = spyFetch({ channels: [] }, 200);
    await listChannelsForOperator('org 1&x=2');
    expect(calls[0].url).toContain('organizer_id=org%201%26x%3D2');
  });

  // A hand-mounted route is outside catalog's response validation (ADR-009), so
  // nothing upstream guarantees the key is present. `undefined` here would crash
  // the page's .map() with a stack trace instead of rendering an empty table.
  it('survives a body with no channels array', async () => {
    spyFetch({}, 200);
    await expect(listChannelsForOperator('org-1')).resolves.toEqual([]);
  });

  it('surfaces a refusal as CatalogApiError with its status', async () => {
    spyFetch({ error: 'unauthorized' }, 401);
    await expect(listChannelsForOperator('org-1')).rejects.toMatchObject({ status: 401 });
  });

  it('creates through the gateway with the write credential', async () => {
    process.env.CATALOG_STAFF_WRITE_TOKEN = 'the-credential';
    const calls = spyFetch({ id: 'c1', code: 'pos' }, 201);
    await createChannel('org-1', { code: 'pos', displayName: 'Box office', kind: 'pos' });
    expect(calls[0].url).toContain('/api/catalog/channels');
    expect(calls[0].headers[STAFF]).toBe('the-credential');
    expect(calls[0].headers[INTERNAL]).toBeUndefined();
  });

  // The contract's `default: true` only applies when the key is ABSENT. Sending
  // `enabled: false` explicitly is a different request, and conflating the two
  // is how a channel is created invisible to the storefront.
  it('omits enabled when the channel is created available, and sends false when it is not', async () => {
    let calls = spyFetch({ id: 'c1' }, 201);
    await createChannel('org-1', { code: 'pos', displayName: 'Box office', kind: 'pos' });
    expect(calls[0].body).not.toHaveProperty('enabled');

    calls = spyFetch({ id: 'c2' }, 201);
    await createChannel('org-1', { code: 'x', displayName: 'X', kind: 'web', enabled: false });
    expect((calls[0].body as { enabled?: boolean }).enabled).toBe(false);
  });

  // The PUT is a full replacement. An omitted field is not "unchanged" — it is
  // absent, and for `enabled` that reads as false.
  it('sends every field on update, including the immutable code', async () => {
    const calls = spyFetch({ id: 'c1' }, 200);
    await updateChannel('c1', { code: 'pos', displayName: 'Counter', kind: 'pos', enabled: false });
    expect(calls[0].url).toContain('/api/catalog/channels/c1');
    expect(calls[0].body).toEqual({ code: 'pos', display_name: 'Counter', kind: 'pos', enabled: false });
  });
});
