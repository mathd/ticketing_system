// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const STAFF = '60000000-0000-4000-8000-000000000001';
const ORGANIZER = '00000000-0000-4000-8000-000000000001';
const VENUE = '10000000-0000-4000-8000-000000000001';
const OTHER_VENUE = '10000000-0000-4000-8000-000000000002';
const EVENT = '30000000-0000-4000-8000-000000000001';
const PERFORMANCE = '40000000-0000-4000-8000-000000000001';
const SLOT = '80000000-0000-4000-8000-000000000001';
const ORDER = '90000000-0000-4000-8000-000000000001';
const REFUND_KEY = 'a0000000-0000-4000-8000-000000000001';
const OTHER_REFUND_KEY = 'a0000000-0000-4000-8000-000000000002';
const ASSERTION = `v1.${STAFF}.${ORGANIZER}.99999999999.${'A'.repeat(43)}`;

const principal = {
  staffId: STAFF,
  organizerId: ORGANIZER,
  role: 'admin' as const,
  organizerAssertion: ASSERTION,
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

async function renderPage(
  path: string,
  request: Request,
  params: Record<string, string> = {},
): Promise<Response> {
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create({ astroConfig: { base: '/admin' } });
  const mod = await import(/* @vite-ignore */ path);
  return container.renderToResponse(mod.default, {
    request,
    params,
    locals: { staff: principal },
    routeType: 'page',
    partial: false,
  });
}

function post(path: string, fields: Record<string, string>): Request {
  return new Request(`http://backoffice.test${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(fields),
  });
}

function venues() {
  return {
    venues: [
      {
        id: VENUE,
        organizer_id: ORGANIZER,
        name: 'Hall',
        ga_capacity: 100,
        created_at: '2026-09-01T10:00:00Z',
      },
      {
        id: OTHER_VENUE,
        organizer_id: ORGANIZER,
        name: 'Arena',
        ga_capacity: 200,
        created_at: '2026-09-01T10:00:00Z',
      },
    ],
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

beforeEach(() => {
  vi.resetModules();
  process.env.CATALOG_STAFF_WRITE_TOKEN = 'catalog-test-token';
  process.env.COMMERCE_STAFF_WRITE_TOKEN = 'commerce-test-token';
  process.env.INVENTORY_STAFF_WRITE_TOKEN = 'inventory-test-token';
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
  delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
});

describe('the order page gates unresolved refunds before dispatch', () => {
  it('refuses a crafted fresh key and renders the server-tracked retry', async () => {
    const { unresolvedRefunds } = await import('../src/lib/unresolved-refunds');
    unresolvedRefunds.note(ORGANIZER, {
      orderId: ORDER,
      quantity: 2,
      reason: 'original request',
      idempotencyKey: REFUND_KEY,
    });
    let refundWrites = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === 'POST') {
        refundWrites++;
        throw new Error(`unexpected refund dispatch: ${url}`);
      }
      if (url.includes(`/api/commerce/orders/${ORDER}`)) {
        return json({ order_id: ORDER, status: 'completed' });
      }
      throw new Error(`unexpected request: ${url}`);
    }));

    const response = await renderPage(
      '../src/pages/orders.astro',
      post('/admin/orders', {
        _action: 'refund',
        order_id: ORDER,
        quantity: '1',
        reason: 'crafted replacement',
        idempotency_key: OTHER_REFUND_KEY,
      }),
    );
    const html = await response.text();

    expect(refundWrites).toBe(0);
    expect(html).toContain('already has an unresolved refund');
    expect(html).toContain(`name="idempotency_key" value="${REFUND_KEY}"`);
    expect(html).toContain('name="quantity" value="2"');
    expect(unresolvedRefunds.find(ORGANIZER, ORDER)).toMatchObject({
      idempotencyKey: REFUND_KEY,
      quantity: 2,
      reason: 'original request',
    });
  }, 30_000);

  it('blocks a stale second form while the first key is in flight', async () => {
    const firstWrite = deferred<Response>();
    let refundWrites = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === 'POST') {
        refundWrites++;
        if (refundWrites > 1) throw new Error(`unexpected second refund dispatch: ${url}`);
        return firstWrite.promise;
      }
      if (url.includes(`/api/commerce/orders/${ORDER}`)) {
        return json({ order_id: ORDER, status: 'completed' });
      }
      throw new Error(`unexpected request: ${url}`);
    }));

    const firstRender = renderPage(
      '../src/pages/orders.astro',
      post('/admin/orders', {
        _action: 'refund',
        order_id: ORDER,
        quantity: '2',
        reason: 'first form',
        idempotency_key: REFUND_KEY,
      }),
    );
    await vi.waitFor(() => expect(refundWrites).toBe(1));

    const staleResponse = await renderPage(
      '../src/pages/orders.astro',
      post('/admin/orders', {
        _action: 'refund',
        order_id: ORDER,
        quantity: '1',
        reason: 'stale form',
        idempotency_key: OTHER_REFUND_KEY,
      }),
    );
    expect(await staleResponse.text()).toContain('already has an unresolved refund');
    expect(refundWrites).toBe(1);

    firstWrite.resolve(json({ error: 'response lost after commit' }, 502));
    await firstRender;
    const { unresolvedRefunds } = await import('../src/lib/unresolved-refunds');
    expect(unresolvedRefunds.find(ORGANIZER, ORDER)).toMatchObject({
      idempotencyKey: REFUND_KEY,
    });
  }, 30_000);
});

describe('mutation pages classify unreadable success responses', () => {
  it('keeps the event key and tells the operator to reconcile before retrying', async () => {
    const key = '00000000-0000-4000-8000-000000000099';
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'POST') return json({}, 201);
      if (String(input).includes('/public/venues')) {
        return json(venues());
      }
      throw new Error(`unexpected request: ${String(input)}`);
    }));

    const response = await renderPage(
      '../src/pages/events/new.astro',
      post('/admin/events/new', {
        _action: 'create-event',
        idempotency_key: key,
        name_en: 'Night',
        name_fr: 'Nuit',
      }),
    );
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(html).toContain('may have saved this step');
    expect(html).toContain('Reload and reconcile the current event state before retrying');
    expect(html).toContain(`name="idempotency_key" value="${key}"`);
    expect(html).toMatch(/name="name_en"[^>]*value="Night"/);
    expect(html).toMatch(/name="name_fr"[^>]*value="Nuit"/);
  }, 30_000);

  it('keeps every performance field, its key, and the selected venue after ambiguity', async () => {
    const key = '00000000-0000-4000-8000-000000000098';
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'POST') return json({}, 201);
      if (String(input).includes('/public/venues')) return json(venues());
      throw new Error(`unexpected request: ${String(input)}`);
    }));

    const path = `/admin/events/new?event=${EVENT}`;
    const response = await renderPage(
      '../src/pages/events/new.astro',
      post(path, {
        _action: 'create-performance',
        idempotency_key: key,
        venue_id: OTHER_VENUE,
        starts_at: '2026-09-18T19:30:00+02:00',
        timezone: 'Europe/Paris',
      }),
    );
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(html).toContain(`name="idempotency_key" value="${key}"`);
    expect(html).toMatch(new RegExp(`<option value="${OTHER_VENUE}" selected`));
    expect(html).toMatch(/name="starts_at"[^>]*value="2026-09-18T19:30:00\+02:00"/);
    expect(html).toMatch(/name="timezone"[^>]*value="Europe\/Paris"/);
  }, 30_000);

  it('keeps every ticket-type field and its key after ambiguity', async () => {
    const key = '00000000-0000-4000-8000-000000000097';
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'POST') return json({}, 201);
      if (String(input).includes('/public/venues')) return json(venues());
      throw new Error(`unexpected request: ${String(input)}`);
    }));

    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}`;
    const response = await renderPage(
      '../src/pages/events/new.astro',
      post(path, {
        _action: 'create-ticket-type',
        idempotency_key: key,
        tt_name_en: 'Balcony',
        tt_name_fr: 'Balcon',
        amount: '4250',
        currency: 'CAD',
      }),
    );
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(html).toContain(`name="idempotency_key" value="${key}"`);
    expect(html).toMatch(/name="tt_name_en"[^>]*value="Balcony"/);
    expect(html).toMatch(/name="tt_name_fr"[^>]*value="Balcon"/);
    expect(html).toMatch(/name="amount"[^>]*value="4250"/);
    expect(html).toMatch(/name="currency"[^>]*value="CAD"/);
  }, 30_000);

  it('treats a fetch-observed connection reset as an ambiguous event write', async () => {
    let writeObserved = false;
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'POST') {
        writeObserved = true;
        return Promise.reject(new Error('connection reset'));
      }
      if (String(input).includes('/public/venues')) return Promise.resolve(json(venues()));
      return Promise.reject(new Error(`unexpected request: ${String(input)}`));
    }));

    const response = await renderPage(
      '../src/pages/events/new.astro',
      post('/admin/events/new', {
        _action: 'create-event',
        idempotency_key: 'reset-key',
        name_en: 'Night',
        name_fr: 'Nuit',
      }),
    );
    const html = await response.text();

    expect(writeObserved).toBe(true);
    expect(html).toContain('may have saved this step');
    expect(html).not.toContain('Something went wrong talking to the catalog');
  }, 30_000);

  it('does not claim a publish retry carries an idempotency key', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'POST') return json({}, 200);
      if (String(input).includes('/public/venues')) return json(venues());
      throw new Error(`unexpected request: ${String(input)}`);
    }));

    const path = `/admin/events/new?event=${EVENT}&performance=${PERFORMANCE}&ticket_type=ready`;
    const response = await renderPage(
      '../src/pages/events/new.astro',
      post(path, { _action: 'publish-performance' }),
    );
    const html = await response.text();

    expect(html).toContain('may have published this date');
    expect(html).not.toContain('idempotency key');
  }, 30_000);

  it('does not claim an unreadable channel create was rejected', async () => {
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) =>
      (init?.method ?? 'GET') === 'POST' ? json({}, 201) : json({ channels: [] }),
    ));

    const response = await renderPage(
      '../src/pages/channels.astro',
      post('/admin/channels', {
        _action: 'create',
        code: 'box-office',
        display_name: 'Box office',
        kind: 'pos',
        enabled: 'on',
      }),
    );
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(html).toContain('may have saved this channel change');
    expect(html).toContain('Reload and reconcile the channel list before retrying');
    expect(html).not.toContain('The change was not saved');
  }, 30_000);

  it('tells the operator to reconcile an undecidable 5xx venue write', async () => {
    let writeObserved = false;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if ((init?.method ?? 'GET') === 'POST') {
        writeObserved = true;
        return json({ error: 'failed after commit' }, 502);
      }
      if (url.includes('/public/venues?')) {
        return json(venues());
      }
      if (url.includes(`/public/venues/${VENUE}/seat-maps`)) {
        return json({ seat_maps: [] });
      }
      throw new Error(`unexpected request: ${url}`);
    }));

    const response = await renderPage(
      '../src/pages/venues/[id].astro',
      post(`/admin/venues/${VENUE}`, {
        _action: 'create-map',
        name: 'Floor',
      }),
      { id: VENUE },
    );
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(writeObserved).toBe(true);
    expect(html).toContain('may have saved this venue change');
    expect(html).toContain('Reload and reconcile the venue before retrying');
    expect(html).not.toContain('Could not save: Catalog accepted');
  }, 30_000);

  it('does not claim an unreadable allocation replace saved nothing', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes('/availability')) {
        return json({
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
          allocation_revision: 2,
        });
      }
      if (init?.method === 'PUT') return json({}, 200);
      if (String(input).includes('/internal/channels')) return json({ channels: [] });
      throw new Error(`unexpected request: ${String(input)}`);
    }));

    const response = await renderPage(
      '../src/pages/slots/[id].astro',
      post(`/admin/slots/${SLOT}`, { allocationRevision: '2' }),
      { id: SLOT },
    );
    const html = await response.text();

    expect(response.status).toBe(200);
    expect(html).toContain('may have saved this allocation set');
    expect(html).toContain('Reload and reconcile the current allocations before retrying');
    expect(html).not.toContain('Nothing was saved — try again');
  }, 30_000);
});
