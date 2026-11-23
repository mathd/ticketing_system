// @vitest-environment node
//
// TKT-602, ADR-088: a Back Office upstream request stops at its configured first
// hop. Each wire case below runs a real client function with native fetch against
// a first hop that answers 307 to a target on another origin. The first hop must
// receive exactly one request with the expected credential markers. The target
// must receive NO request: on the base code, fetch followed the redirect and sent
// the staff token, the organizer assertion, the password or the guest capability
// to the target. Each case then asserts the outcome the fetch inventory gives the
// caller: an unavailable read, an ambiguous write, or no confirmation.
import { connect } from 'node:net';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  type Answer,
  clearClientUrls,
  type Hit,
  jsonAnswer,
  pointClientsAt,
  type RedirectPair,
  redirectAnswer,
  startRedirectPair,
} from './redirect-fixture';

// Synthetic values only. Each credential is distinct, so a marker in a recorded
// request names the client that sent it.
const CATALOG_TOKEN = 'catalog-wire-token-602';
const COMMERCE_TOKEN = 'commerce-wire-token-602';
const INVENTORY_TOKEN = 'inventory-wire-token-602';
const ACCESS_TOKEN = 'access-wire-token-602';
const PASSWORD = 'wire-password-marker-602';

const STAFF = '60200000-0000-4000-8000-000000000001';
const ORGANIZER = '60200000-0000-4000-8000-0000000000aa';
const ASSERTION = `v2.catalog-org/test.${STAFF}.${ORGANIZER}.99999999999.${'W'.repeat(86)}`;
const ORDER = '60200000-0000-4000-8000-000000000002';
const REFUND = '60200000-0000-4000-8000-000000000003';
const REFUND_KEY = '60200000-0000-4000-8000-000000000004';
const RESEND_KEY = '60200000-0000-4000-8000-000000000005';
const EVENT_KEY = '60200000-0000-4000-8000-000000000006';
const EVENT = '60200000-0000-4000-8000-000000000007';
const CHANNEL = '60200000-0000-4000-8000-000000000008';
const SLOT = '60200000-0000-4000-8000-000000000009';
// The guest retrieval capability (ADR-012). The redirect keeps it in the path,
// so a followed redirect hands it to the target.
const GUEST_REF = '60200000-0000-4000-8000-0000000000ca';

let pair: RedirectPair | undefined;

beforeEach(() => {
  vi.resetModules();
  process.env.CATALOG_STAFF_WRITE_TOKEN = CATALOG_TOKEN;
  process.env.COMMERCE_STAFF_WRITE_TOKEN = COMMERCE_TOKEN;
  process.env.INVENTORY_STAFF_WRITE_TOKEN = INVENTORY_TOKEN;
  process.env.ACCESS_STAFF_WRITE_TOKEN = ACCESS_TOKEN;
});

afterEach(async () => {
  await pair?.close();
  pair = undefined;
  clearClientUrls();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
  delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
  delete process.env.ACCESS_STAFF_WRITE_TOKEN;
  vi.unstubAllGlobals();
  vi.resetModules();
});

/**
 * Start a first hop that redirects exactly `method path` to the same path on the
 * target, and point every client base URL at the first hop. The clients read
 * their base URLs at import, so callers import them after this.
 */
async function redirecting(method: string, path: string, targetAnswer: Answer): Promise<RedirectPair> {
  pair = await startRedirectPair({
    firstHop: (hit, targetOrigin) =>
      hit.method === method && hit.path === path
        ? redirectAnswer(targetOrigin, path)
        : jsonAnswer({ error: `first hop has no route for ${hit.method} ${hit.path}` }, 404),
    target: () => targetAnswer,
  });
  pointClientsAt(pair.firstHop.origin);
  return pair;
}

const MARKER_HEADERS = [
  'x-catalog-staff-write-token',
  'x-catalog-organizer-assertion',
  'x-commerce-staff-write-token',
  'x-inventory-staff-write-token',
  'x-access-staff-write-token',
  'idempotency-key',
] as const;

/** What a listener received: method, path, credential and key headers, body. */
function markers(hits: Hit[]) {
  return hits.map((hit) => {
    const seen: Record<string, unknown> = { method: hit.method, path: hit.path };
    for (const name of MARKER_HEADERS) {
      if (hit.headers[name] !== undefined) seen[name] = hit.headers[name];
    }
    if (hit.body !== '') seen.body = JSON.parse(hit.body);
    return seen;
  });
}

function expectOneFirstHopRequest(p: RedirectPair, expected: Record<string, unknown>) {
  expect(markers(p.firstHop.hits)).toEqual([expected]);
}

/** The redirect target must receive nothing. On the base code it received the request. */
function expectNoTargetRequest(p: RedirectPair) {
  expect(markers(p.target.hits)).toEqual([]);
}

function principal() {
  return { staff_id: STAFF, organizer_id: ORGANIZER, role: 'admin', organizer_assertion: ASSERTION };
}

function staffAvailability() {
  return {
    slot_id: SLOT,
    capacity: 100,
    buyer_held: 0,
    operational_held: 0,
    reservation_held: 0,
    confirmed: 0,
    available: 100,
    public_available: 100,
    offering_status: 'open',
    channels: [],
    inventory_kind: 'ga',
    allocation_revision: 1,
  };
}

function channel() {
  return {
    id: CHANNEL,
    organizer_id: ORGANIZER,
    code: 'pos',
    display_name: 'Box office',
    kind: 'pos',
    enabled: true,
    revision: 4,
    created_at: '2026-11-22T10:00:00Z',
    updated_at: '2026-11-22T10:00:00Z',
  };
}

describe('Back Office upstream calls do not follow a redirect (wire)', () => {
  it('refund POST: the target gets nothing and the outcome stays ambiguous', async () => {
    const path = `/internal/orders/${ORDER}/refunds`;
    // A coded abandonment at the target. Followed, it would settle the saved key.
    const p = await redirecting('POST', path, jsonAnswer({ error: 'refund abandoned', code: 'refund_abandoned' }, 409));
    const { refundOrder } = await import('../src/lib/commerce');

    const outcome = await refundOrder({
      orderId: ORDER,
      quantity: 2,
      reason: 'wire reason',
      actor: STAFF,
      assertion: ASSERTION,
      idempotencyKey: REFUND_KEY,
    });

    expectOneFirstHopRequest(p, {
      method: 'POST',
      path,
      'x-commerce-staff-write-token': COMMERCE_TOKEN,
      'x-catalog-organizer-assertion': ASSERTION,
      'idempotency-key': REFUND_KEY,
      body: { quantity: 2, actor: STAFF, reason: 'wire reason' },
    });
    expectNoTargetRequest(p);
    expect(outcome).toEqual({ ok: false, kind: 'ambiguous', message: 'Commerce could not be reached.' });
  });

  it('abandonment lookup: the target gets nothing and nothing is confirmed', async () => {
    const path = `/internal/orders/${ORDER}`;
    // An exact committed abandonment at the target. Followed, it would confirm.
    const p = await redirecting('GET', path, jsonAnswer({
      order_id: ORDER,
      organizer_id: ORGANIZER,
      status: 'completed',
      refunds: [{ refund_id: REFUND, idempotency_key: REFUND_KEY, abandoned: true }],
    }));
    const { confirmsAbandonedRefund } = await import('../src/lib/commerce');

    const confirmed = await confirmsAbandonedRefund({
      orderId: ORDER,
      organizerId: ORGANIZER,
      idempotencyKey: REFUND_KEY,
      assertion: ASSERTION,
    });

    expectOneFirstHopRequest(p, {
      method: 'GET',
      path,
      'x-commerce-staff-write-token': COMMERCE_TOKEN,
      'x-catalog-organizer-assertion': ASSERTION,
    });
    expectNoTargetRequest(p);
    expect(confirmed).toBe(false);
  });

  it('staff authentication: the target gets neither password nor token, and sign-in fails', async () => {
    const path = '/api/catalog/staff/authenticate';
    const p = await redirecting('POST', path, jsonAnswer(principal()));
    const { authenticateStaff } = await import('../src/lib/catalog');
    const { UpstreamRedirectError } = await import('../src/lib/upstream');

    const error = await authenticateStaff('ada@example.test', PASSWORD).catch((cause: unknown) => cause);

    expectOneFirstHopRequest(p, {
      method: 'POST',
      path,
      'x-catalog-staff-write-token': CATALOG_TOKEN,
      body: { identifier: 'ada@example.test', password: PASSWORD },
    });
    expectNoTargetRequest(p);
    // Thrown, never null: null means "wrong password", and the login page shows
    // a thrown failure as temporarily unavailable.
    expect(error).toBeInstanceOf(UpstreamRedirectError);
    expect(error).toMatchObject({ status: 307 });
  });

  it('operator channel read: the target gets nothing and the read fails', async () => {
    const path = '/internal/channels';
    const p = await redirecting('GET', path, jsonAnswer({ channels: [] }));
    const { listChannelsForOperator } = await import('../src/lib/catalog');
    const { UpstreamRedirectError } = await import('../src/lib/upstream');

    const error = await listChannelsForOperator(ASSERTION, ORGANIZER).catch((cause: unknown) => cause);

    expectOneFirstHopRequest(p, {
      method: 'GET',
      path,
      'x-catalog-staff-write-token': CATALOG_TOKEN,
      'x-catalog-organizer-assertion': ASSERTION,
    });
    expectNoTargetRequest(p);
    // Not an empty registry: the page shows its unavailable state.
    expect(error).toBeInstanceOf(UpstreamRedirectError);
  });

  it('staff availability read: the target gets nothing and the read fails', async () => {
    const path = `/internal/slots/${SLOT}/availability?organizer_id=${ORGANIZER}`;
    const p = await redirecting('GET', path, jsonAnswer(staffAvailability()));
    const { getStaffAvailability } = await import('../src/lib/inventory');
    const { UpstreamRedirectError } = await import('../src/lib/upstream');

    const error = await getStaffAvailability(SLOT, ORGANIZER).catch((cause: unknown) => cause);

    expectOneFirstHopRequest(p, {
      method: 'GET',
      path,
      'x-inventory-staff-write-token': INVENTORY_TOKEN,
    });
    expectNoTargetRequest(p);
    // Not an inventory refusal such as a 404: Inventory did not answer.
    expect(error).toBeInstanceOf(UpstreamRedirectError);
  });

  it('ticket resend: the target gets nothing and the outcome stays ambiguous', async () => {
    const path = `/internal/orders/${ORDER}/redeliveries`;
    const p = await redirecting('POST', path, jsonAnswer({ order_id: ORDER, ticket_count: 1, replay: false }));
    const { redeliverOrderTickets } = await import('../src/lib/access');

    const outcome = await redeliverOrderTickets({ orderId: ORDER, organizerId: ORGANIZER, idempotencyKey: RESEND_KEY });

    expectOneFirstHopRequest(p, {
      method: 'POST',
      path,
      'x-access-staff-write-token': ACCESS_TOKEN,
      'idempotency-key': RESEND_KEY,
      body: { organizer_id: ORGANIZER },
    });
    expectNoTargetRequest(p);
    // The existing resend copy names a timeout (ADR-088, D7).
    expect(outcome).toEqual({ ok: false, kind: 'ambiguous', message: 'access did not answer in time.' });
  });

  it('event create (postCatalog): the target gets nothing and the write is ambiguous', async () => {
    const path = '/api/catalog/events';
    const p = await redirecting('POST', path, jsonAnswer({
      id: EVENT,
      organizer_id: ORGANIZER,
      name: { en: 'Night' },
      created_at: '2026-11-22T10:00:00Z',
    }, 201));
    const { createEvent } = await import('../src/lib/catalog');
    const { AmbiguousMutationError, UpstreamRedirectError } = await import('../src/lib/upstream');

    const error = await createEvent(ASSERTION, ORGANIZER, { en: 'Night' }, EVENT_KEY).catch((cause: unknown) => cause);

    expectOneFirstHopRequest(p, {
      method: 'POST',
      path,
      'x-catalog-staff-write-token': CATALOG_TOKEN,
      'x-catalog-organizer-assertion': ASSERTION,
      'idempotency-key': EVENT_KEY,
      body: { name: { en: 'Night' } },
    });
    expectNoTargetRequest(p);
    // Ambiguous, never a decided CatalogApiError with status 307.
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toBeInstanceOf(UpstreamRedirectError);
  });

  it('channel update: the target gets nothing and the write is ambiguous', async () => {
    const path = `/api/catalog/channels/${CHANNEL}`;
    const p = await redirecting('PUT', path, jsonAnswer(channel()));
    const { updateChannel } = await import('../src/lib/catalog');
    const { AmbiguousMutationError, UpstreamRedirectError } = await import('../src/lib/upstream');

    const error = await updateChannel(ASSERTION, ORGANIZER, CHANNEL, {
      code: 'pos',
      displayName: 'Box office',
      kind: 'pos',
      enabled: true,
      expectedRevision: 3,
    }).catch((cause: unknown) => cause);

    expectOneFirstHopRequest(p, {
      method: 'PUT',
      path,
      'x-catalog-staff-write-token': CATALOG_TOKEN,
      'x-catalog-organizer-assertion': ASSERTION,
      body: { code: 'pos', display_name: 'Box office', kind: 'pos', enabled: true, expected_revision: 3 },
    });
    expectNoTargetRequest(p);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toBeInstanceOf(UpstreamRedirectError);
  });

  it('allocation replace: the target gets nothing and the write is ambiguous', async () => {
    const path = `/internal/slots/${SLOT}/channel-allocations`;
    const p = await redirecting('PUT', path, jsonAnswer({ slot_id: SLOT, allocations: [] }));
    const { replaceChannelAllocations } = await import('../src/lib/inventory');
    const { AmbiguousMutationError, UpstreamRedirectError } = await import('../src/lib/upstream');

    const error = await replaceChannelAllocations(SLOT, { organizer_id: ORGANIZER, allocations: [] })
      .catch((cause: unknown) => cause);

    expectOneFirstHopRequest(p, {
      method: 'PUT',
      path,
      'x-inventory-staff-write-token': INVENTORY_TOKEN,
      body: { organizer_id: ORGANIZER, allocations: [] },
    });
    expectNoTargetRequest(p);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toBeInstanceOf(UpstreamRedirectError);
  });

  it('guest ticket bundle: the target never sees the capability and the read is unavailable', async () => {
    const path = `/api/access/orders/${GUEST_REF}/tickets`;
    const p = await redirecting('GET', path, jsonAnswer({ order_ref: GUEST_REF, tickets: [] }));
    const { getOrderTickets } = await import('../src/lib/access');

    const read = await getOrderTickets(GUEST_REF);

    expectOneFirstHopRequest(p, { method: 'GET', path });
    expectNoTargetRequest(p);
    // Unavailable, never not-found: a redirect does not establish absence.
    expect(read).toEqual({ ok: false, kind: 'unavailable' });
  });
});

// Refusing redirects must not refuse direct answers. These pin the decided
// outcomes a direct first hop still produces over native fetch.
describe('direct first-hop answers still decide (wire)', () => {
  async function direct(answer: Answer): Promise<RedirectPair> {
    pair = await startRedirectPair({ firstHop: () => answer });
    pointClientsAt(pair.firstHop.origin);
    return pair;
  }

  it('a direct 401 from staff authentication is still a wrong password', async () => {
    await direct(jsonAnswer({ error: 'invalid credentials' }, 401));
    const { authenticateStaff } = await import('../src/lib/catalog');
    await expect(authenticateStaff('ada@example.test', PASSWORD)).resolves.toBeNull();
  });

  it('a direct 200 from staff authentication still signs in', async () => {
    await direct(jsonAnswer(principal()));
    const { authenticateStaff } = await import('../src/lib/catalog');
    await expect(authenticateStaff('ada@example.test', PASSWORD)).resolves.toMatchObject({
      staffId: STAFF,
      organizerId: ORGANIZER,
      organizerAssertion: ASSERTION,
    });
  });

  it('a direct coded 409 still settles the refund key as abandoned', async () => {
    await direct(jsonAnswer({ error: 'refund abandoned', code: 'refund_abandoned' }, 409));
    const { refundOrder } = await import('../src/lib/commerce');
    await expect(refundOrder({
      orderId: ORDER,
      quantity: 2,
      reason: 'wire reason',
      actor: STAFF,
      assertion: ASSERTION,
      idempotencyKey: REFUND_KEY,
    })).resolves.toMatchObject({ ok: false, kind: 'abandoned' });
  });

  it('a direct 404 from the guest ticket bundle is still not-found', async () => {
    await direct(jsonAnswer({ error: 'not found' }, 404));
    const { getOrderTickets } = await import('../src/lib/access');
    await expect(getOrderTickets(GUEST_REF)).resolves.toEqual({ ok: false, kind: 'not-found' });
  });
});

// The fixture itself. The headers already carry the credentials, so the target
// must count a request whose upload stops before the body ends.
describe('the counting target (wire fixture)', () => {
  it('counts a request at its headers, also when the body never ends', async () => {
    const p = await startRedirectPair({ firstHop: () => jsonAnswer({}) });
    pair = p;
    const port = Number(new URL(p.target.origin).port);
    const path = `/internal/orders/${ORDER}/refunds`;
    const socket = connect(port, '127.0.0.1');
    try {
      await new Promise<void>((resolve, reject) => {
        socket.once('connect', resolve);
        socket.once('error', reject);
      });
      const interim = new Promise<string>((resolve, reject) => {
        socket.once('data', (chunk) => resolve(String(chunk)));
        socket.once('error', reject);
      });
      // Headers and 11 of the 64 announced body bytes. Node writes 100 Continue
      // and calls the request handler in one turn, so the reply below means the
      // target has the headers.
      socket.write(
        `POST ${path} HTTP/1.1\r\nHost: 127.0.0.1:${port}\r\n` +
          `X-Commerce-Staff-Write-Token: ${COMMERCE_TOKEN}\r\nContent-Type: application/json\r\n` +
          'Content-Length: 64\r\nExpect: 100-continue\r\n\r\n{"quantity"',
      );
      expect(await interim).toMatch(/^HTTP\/1\.1 100 Continue/);
    } finally {
      // The upload stops here: the body never ends.
      socket.destroy();
    }
    expect(() => expectNoTargetRequest(p)).toThrow();
    expect(markers(p.target.hits)).toEqual([
      { method: 'POST', path, 'x-commerce-staff-write-token': COMMERCE_TOKEN },
    ]);
  });
});

// The guard itself, with a fetch stub. The wire cases above prove what native
// fetch sends. These cases reach inputs that the pinned Node cannot produce over
// a socket, or that need exact control: each 3xx bound, a status-0
// `opaqueredirect`, a failing body cancel, and a caller's own redirect option.
describe('fetchUpstream refuses a redirect before any body read (stub)', () => {
  const READS = ['json', 'clone', 'text', 'arrayBuffer', 'blob', 'formData', 'bytes'] as const;

  /** Spy on every body reader of a real Response. */
  function spyOnReads(response: Response) {
    return READS.map((name) => vi.spyOn(response, name));
  }

  function answerWith(response: Response) {
    const seen: RequestInit[] = [];
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(init ?? {});
      return Promise.resolve(response);
    }));
    return seen;
  }

  function redirectResponse(status: number, location = 'http://target.invalid/elsewhere') {
    return new Response(JSON.stringify({ error: 'moved' }), {
      status,
      headers: { location, 'content-type': 'application/json' },
    });
  }

  it.each([300, 399])('refuses a non-opaque %i without reading its body', async (status) => {
    const { fetchUpstream, UpstreamRedirectError } = await import('../src/lib/upstream');
    const response = redirectResponse(status);
    const reads = spyOnReads(response);
    // The stream itself (R1-6): no reader is taken, and the body is cancelled.
    const stream = response.body!;
    const getReader = vi.spyOn(stream, 'getReader');
    const cancel = vi.spyOn(stream, 'cancel');
    answerWith(response);

    const error = await fetchUpstream('http://first-hop.invalid/read', {}).catch((cause: unknown) => cause);

    expect(error).toBeInstanceOf(UpstreamRedirectError);
    expect(error).toMatchObject({ status });
    for (const read of reads) expect(read).not.toHaveBeenCalled();
    expect(getReader).not.toHaveBeenCalled();
    expect(cancel).toHaveBeenCalledTimes(1);
    expect(stream.locked).toBe(false);
  });

  it('refuses a status-0 opaqueredirect without reading its body', async () => {
    const { fetchUpstream, UpstreamRedirectError } = await import('../src/lib/upstream');
    // Duck-typed: `new Response` cannot build status 0. A runtime with browser
    // redirect semantics returns this shape for a manual-mode redirect.
    const reads = Object.fromEntries(READS.map((name) => [name, vi.fn()]));
    const opaque = {
      type: 'opaqueredirect',
      status: 0,
      ok: false,
      redirected: false,
      headers: new Headers(),
      body: null,
      bodyUsed: false,
      ...reads,
    } as unknown as Response;
    answerWith(opaque);

    const error = await fetchUpstream('http://first-hop.invalid/read', {}).catch((cause: unknown) => cause);

    expect(error).toBeInstanceOf(UpstreamRedirectError);
    expect(error).toMatchObject({ status: 0 });
    for (const read of Object.values(reads)) expect(read).not.toHaveBeenCalled();
  });

  it.each([299, 400])('passes a %i to the caller unread', async (status) => {
    const { fetchUpstream } = await import('../src/lib/upstream');
    const response = new Response(JSON.stringify({ error: 'not a redirect' }), { status });
    const reads = spyOnReads(response);
    answerWith(response);

    await expect(fetchUpstream('http://first-hop.invalid/read', {})).resolves.toBe(response);
    for (const read of reads) expect(read).not.toHaveBeenCalled();
  });

  it('reports the redirect even when cancelling its body fails', async () => {
    const { fetchUpstream, UpstreamRedirectError } = await import('../src/lib/upstream');
    // A plain function, not vi.fn: a vitest spy attaches its own handler to a
    // returned promise, so the rejection would never be unhandled and the case
    // could not see a missing handler in fetchUpstream.
    let cancelCalls = 0;
    const cancel = () => {
      cancelCalls++;
      return Promise.reject(new Error('cancel failed'));
    };
    const failingCancel = {
      type: 'basic',
      status: 307,
      ok: false,
      redirected: false,
      headers: new Headers({ location: 'http://target.invalid/elsewhere' }),
      body: { cancel },
      bodyUsed: false,
      json: vi.fn(),
      clone: vi.fn(),
      text: vi.fn(),
    } as unknown as Response;
    answerWith(failingCancel);
    const unhandled: unknown[] = [];
    const record = (reason: unknown) => unhandled.push(reason);
    process.on('unhandledRejection', record);
    try {
      const error = await fetchUpstream('http://first-hop.invalid/write', { method: 'POST' })
        .catch((cause: unknown) => cause);
      // Let a rejection without a handler reach the process.
      await new Promise((resolve) => setTimeout(resolve, 50));

      expect(error).toBeInstanceOf(UpstreamRedirectError);
      expect(cancelCalls).toBe(1);
      expect(unhandled).toEqual([]);
    } finally {
      process.off('unhandledRejection', record);
    }
  });

  it('puts neither the request URL nor the Location value in the error', async () => {
    const { fetchUpstream, UpstreamRedirectError } = await import('../src/lib/upstream');
    const requestMarker = 'request-url-marker-602';
    const locationMarker = 'location-marker-602';
    answerWith(redirectResponse(307, `http://target.invalid/${locationMarker}`));

    const error = await fetchUpstream(`http://first-hop.invalid/api/access/orders/${requestMarker}/tickets`, {})
      .catch((cause: unknown) => cause);

    expect(error).toBeInstanceOf(UpstreamRedirectError);
    const err = error as Error;
    // Every own property, enumerable or not (message, stack, status, ...).
    const texts = [String(err), ...Reflect.ownKeys(err).map((key) => String((err as never)[key]))];
    for (const text of texts) {
      expect(text).not.toContain(requestMarker);
      expect(text).not.toContain(locationMarker);
    }
    // Exactly the permitted own properties (R1-6). Error defines `stack` and
    // `message`; the class adds `name` and `status`. Each is a primitive, so a
    // structured logger cannot print an attached response, headers or body.
    expect(Reflect.ownKeys(err).sort()).toEqual(['message', 'name', 'stack', 'status']);
    expect(typeof err.message).toBe('string');
    expect(err.name).toBe('UpstreamRedirectError');
    expect(typeof err.stack).toBe('string');
    expect((err as InstanceType<typeof UpstreamRedirectError>).status).toBe(307);
  });

  it("sends redirect 'manual' even when the caller asks to follow", async () => {
    const { fetchUpstream } = await import('../src/lib/upstream');
    const seen = answerWith(new Response('{}', { status: 200 }));
    const signal = new AbortController().signal;

    await fetchUpstream('http://first-hop.invalid/write', {
      method: 'POST',
      headers: { 'x-marker': 'kept' },
      body: '{}',
      signal,
      redirect: 'follow',
    });

    expect(seen).toEqual([
      { method: 'POST', headers: { 'x-marker': 'kept' }, body: '{}', signal, redirect: 'manual' },
    ]);
  });
});
