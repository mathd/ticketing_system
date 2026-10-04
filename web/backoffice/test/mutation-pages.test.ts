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
          inventory_kind: 'ga',
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

// ---------------------------------------------------------------------------------------
// TKT-286. The slot page for a SEATED pool: allocations read-only, plus one separate
// "Clear allocations" operation. Everything below drives the real page through the real
// Astro container and a stubbed inventory, and observes the only things a request can
// leave behind: the response and the PUTs inventory received.
// ---------------------------------------------------------------------------------------

const SELLER = 'c0000000-0000-4000-8000-000000000001';
const OTHER_ORGANIZER = '00000000-0000-4000-8000-0000000000ff';

/** A POST that can carry the same field twice, which `post()` (a Record) cannot. */
function postEntries(path: string, entries: Array<[string, string]>): Request {
  return new Request(`http://backoffice.test${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(entries),
  });
}

// Non-default on purpose, so a row rendered from defaults cannot pass: a code-gated
// reseller allocation with every optional boundary set (seconds and microseconds), and a
// plain one that is already released.
const legacyChannels = [
  {
    channel: 'reseller-acme',
    cap: 40,
    release_at: '2026-10-01T09:30:15.123456Z',
    released: false,
    opens_at: '2026-09-20T08:00:00Z',
    closes_at: '2026-09-30T22:15:30Z',
    window_open: false,
    requires_code: true,
    sold_by: SELLER,
    held: 3,
    confirmed: 4,
    available: 33,
  },
  {
    channel: 'pos',
    cap: 12,
    released: true,
    window_open: true,
    held: 1,
    confirmed: 0,
    available: 11,
  },
];

function availabilityBody(
  kind: 'ga' | 'seated',
  channels: unknown[] = legacyChannels,
  revision: number | null = 2,
) {
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
    channels,
    inventory_kind: kind,
    ...(revision === null ? {} : { allocation_revision: revision }),
  };
}

type Put = { url: string; body: Record<string, unknown> };

/** Stub inventory. `read` is the availability body, or 'fail' for a failed read. */
function stubInventory(
  read: unknown,
  put: (body: Record<string, unknown>) => Response = () => json({ slot_id: SLOT, allocations: [] }),
): Put[] {
  const puts: Put[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === 'PUT') {
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      puts.push({ url, body });
      return put(body);
    }
    if (url.includes('/availability')) {
      return read === 'fail' ? json({ error: 'inventory is down' }, 500) : json(read);
    }
    if (url.includes('/internal/channels')) return json({ channels: [] });
    throw new Error(`unexpected request: ${url}`);
  }));
  return puts;
}

const SLOT_PAGE = '../src/pages/slots/[id].astro';
const slotPath = `/admin/slots/${SLOT}`;
const clearEntries = (revision = '2'): Array<[string, string]> => [
  ['allocationRevision', revision],
  ['_action', 'clear-allocations'],
];

/** The text of one named cell of one row, so an assertion names WHICH value it checks. */
function cell(html: string, channel: string, field: string): string | undefined {
  const row = new RegExp(`<tr[^>]*data-channel="${channel}"[^>]*>([\\s\\S]*?)</tr>`).exec(html)?.[1];
  const td = row && new RegExp(`<td[^>]*data-field="${field}"[^>]*>([\\s\\S]*?)</td>`).exec(row)?.[1];
  return td?.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
}

/** Every `name="…"` inside every <form>, as one list per form. */
function formControlNames(html: string): string[][] {
  return [...html.matchAll(/<form\b[\s\S]*?<\/form>/g)].map((f) =>
    [...f[0].matchAll(/\bname="([^"]*)"/g)].map((m) => m[1]),
  );
}

describe('the slot page for a seated pool', () => {
  it('labels seated availability as pool capacity and omits GA allocation guidance', async () => {
    const body = { ...availabilityBody('seated'), available: 100, public_available: 74 };
    stubInventory(body);
    const response = await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT });
    const html = await response.text();

    expect(html).toMatch(/Available pool capacity:\s*<strong data-available="100">\s*100<\/strong>/);
    expect(html).not.toContain('stays available to the public channel');
    expect(html).not.toContain('74</strong> is currently available to the public');
  }, 30_000);

  it('renders the stored allocations as text, with no editable control (COS1)', async () => {
    const puts = stubInventory(availabilityBody('seated'));
    const response = await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT });
    const html = await response.text();

    expect(response.status).toBe(200);
    // Exact values, asserted per field, so dropping or defaulting one is visible.
    expect(cell(html, 'reseller-acme', 'cap')).toBe('40');
    expect(cell(html, 'reseller-acme', 'consumption')).toBe('7');
    expect(cell(html, 'reseller-acme', 'release')).toContain('2026-10-01T09:30:15.123456Z');
    expect(cell(html, 'reseller-acme', 'opens')).toBe('2026-09-20T08:00:00Z');
    expect(cell(html, 'reseller-acme', 'closes')).toBe('2026-09-30T22:15:30Z');
    expect(cell(html, 'reseller-acme', 'window')).toBe('closed');
    expect(cell(html, 'reseller-acme', 'requires-code')).toBe('yes');
    expect(cell(html, 'reseller-acme', 'sold-by')).toBe(SELLER);
    expect(cell(html, 'pos', 'cap')).toBe('12');
    expect(cell(html, 'pos', 'consumption')).toBe('1');
    expect(cell(html, 'pos', 'requires-code')).toBe('no');
    expect(cell(html, 'pos', 'sold-by')).toBe('—');

    // No editable control, and not a disabled copy of the editor either.
    expect(html).not.toMatch(/name="(cap|channel|releaseAt|clearRelease)\./);
    expect(html).not.toContain('data-action="save-allocations"');
    expect(html).not.toContain('Save allocations');
    expect(html).not.toMatch(/<input[^>]*type="(number|text|checkbox)"/);
    expect(puts).toHaveLength(0);
  }, 30_000);

  it('offers one separate clear form whose only named controls are the revision and the operation (COS1, D8)', async () => {
    stubInventory(availabilityBody('seated'));
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();

    const forms = formControlNames(html);
    expect(forms).toHaveLength(1);
    expect([...forms[0]].sort()).toEqual(['_action', 'allocationRevision']);
    expect(html).toMatch(/name="_action"[^>]*value="clear-allocations"|value="clear-allocations"[^>]*name="_action"/);
    expect(html).toMatch(/data-allocation-revision/);
    expect(html).toMatch(/name="allocationRevision"[^>]*value="2"|value="2"[^>]*name="allocationRevision"/);
    const button = /<button[^>]*data-action="clear-allocations"[^>]*>/.exec(html)?.[0] ?? '';
    expect(button).not.toBe('');
    expect(button).not.toContain('disabled');
    expect(button).not.toContain('name=');
    // The empty action retains a bookmarked query in the POST target; dispatch reads the
    // body `_action`. This asserts only the empty action; the browser test observes the
    // bookmarked submit (D3).
    expect(html).toMatch(/<form[^>]*action=""/);
  }, 30_000);

  it.each([
    ['no allocation rows', availabilityBody('seated', [], 2)],
    ['no usable rendered revision', availabilityBody('seated', legacyChannels, null)],
  ])('disables the clear button with %s (D7, a UI aid only)', async (_name, body) => {
    stubInventory(body);
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();
    const button = /<button[^>]*data-action="clear-allocations"[^>]*>/.exec(html)?.[0] ?? '';
    expect(button).toContain('disabled');
    expect(html).not.toContain('data-action="save-allocations"');
  }, 30_000);

  it('does not render the GA editor-empty claim that the whole slot sells publicly', async () => {
    stubInventory(availabilityBody('seated', []));
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();
    expect(html).not.toContain('the whole slot sells publicly');
  }, 30_000);

  it('clears the set with exactly an empty replace carrying the SUBMITTED revision (COS2)', async () => {
    // The fresh read says revision 5; the form carried 2. The write must say 2.
    const puts = stubInventory(availabilityBody('seated', legacyChannels, 5));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries('2')), { id: SLOT });

    expect(response.status).toBe(303);
    // The canonical slot URL: no query. (BASE_URL is not set under vitest, so the
    // `/admin` prefix is not asserted here; the browser spec asserts the real one.)
    expect(response.headers.get('location')).toMatch(new RegExp(`/slots/${SLOT}$`));
    expect(puts).toEqual([
      {
        url: `http://localhost:8081/internal/slots/${SLOT}/channel-allocations`,
        body: { organizer_id: ORGANIZER, allocation_revision: 2, allocations: [] },
      },
    ]);
  }, 30_000);

  it('ignores every other submitted field when clearing', async () => {
    const puts = stubInventory(availabilityBody('seated'));
    const response = await renderPage(
      SLOT_PAGE,
      postEntries(slotPath, [
        ...clearEntries('2'),
        ['organizer_id', OTHER_ORGANIZER],
        ['channel.0', 'reseller-acme'],
        ['cap.0', '1'],
        ['allocations', '[{"channel":"x","cap":1}]'],
        ['allocation_revision', '99'],
      ]),
      { id: SLOT },
    );

    expect(response.status).toBe(303);
    expect(puts.map((p) => p.body)).toEqual([
      { organizer_id: ORGANIZER, allocation_revision: 2, allocations: [] },
    ]);
  }, 30_000);

  it('refuses a stale clear, shows the reload instruction and re-renders the SUBMITTED revision (COS4)', async () => {
    // Inventory is now at revision 3; the page the operator loaded carried 2.
    const puts = stubInventory(availabilityBody('seated', legacyChannels, 3), () =>
      json({ error: 'conflict: allocation set revision mismatch', code: 'allocation_revision_mismatch' }, 409),
    );
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries('2')), { id: SLOT });
    const html = await response.text();

    expect(response.status).not.toBe(303);
    expect(puts).toHaveLength(1);
    expect(puts[0].body.allocation_revision).toBe(2);
    expect(html).toContain('Someone else changed this slot’s allocations');
    expect(html).toContain('nothing was cleared');
    expect(html).toMatch(/name="allocationRevision"[^>]*value="2"|value="2"[^>]*name="allocationRevision"/);
    expect(html).not.toMatch(/name="allocationRevision"[^>]*value="3"|value="3"[^>]*name="allocationRevision"/);
    // The stored rows stay on screen and the clear form stays offered.
    expect(cell(html, 'reseller-acme', 'cap')).toBe('40');
    expect(html).toContain('data-action="clear-allocations"');
  }, 30_000);

  it('does not claim an unreadable clear saved nothing', async () => {
    stubInventory(availabilityBody('seated'), () => json({}, 200));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries()), { id: SLOT });
    const html = await response.text();
    expect(response.status).toBe(200);
    expect(html).toContain('may have saved this allocation set');
    expect(html).not.toContain('Nothing was saved — try again');
  }, 30_000);

  it.each([
    ['with no rows', [['allocationRevision', '2']]],
    ['with valid legacy rows', [
      ['allocationRevision', '2'],
      ['channel.0', 'reseller-acme'], ['cap.0', '40'], ['releaseAt.0', '2026-10-01T09:30:15.123456Z'],
      ['channel.1', 'pos'], ['cap.1', '12'], ['releaseAt.1', ''],
    ]],
    ['with an invalid cap', [
      ['allocationRevision', '2'], ['channel.0', 'reseller-acme'], ['cap.0', 'not-a-cap'],
    ]],
  ] as Array<[string, Array<[string, string]>]>)('refuses an editor POST to a seated pool %s at form level (COS3)', async (_name, entries) => {
    const puts = stubInventory(availabilityBody('seated', _name === 'with no rows' ? [] : legacyChannels));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, entries), { id: SLOT });
    const html = await response.text();

    expect(puts).toHaveLength(0);
    expect(response.status).toBe(400);
    expect(html).toContain('data-form-error');
    expect(html).toContain('Use the Clear allocations action');
    expect(html).not.toContain('data-row-error');
    expect(html).not.toContain('data-action="save-allocations"');
    expect(html).not.toMatch(/<input[^>]*type="(number|text|checkbox)"/);
    const errorPosition = html.indexOf('data-form-error');
    const tablePosition = html.indexOf('<table');
    expect(errorPosition).toBeGreaterThanOrEqual(0);
    expect(tablePosition === -1 || errorPosition < tablePosition).toBe(true);
    if (_name !== 'with no rows') {
      expect(cell(html, 'reseller-acme', 'cap')).toBe('40');
      expect(cell(html, 'pos', 'cap')).toBe('12');
    }
  }, 30_000);

  it.each([
    ['an unknown operation', [['allocationRevision', '2'], ['_action', 'delete-everything']]],
    ['an empty operation', [['allocationRevision', '2'], ['_action', '']]],
    ['a repeated clear', [['allocationRevision', '2'], ['_action', 'clear-allocations'], ['_action', 'clear-allocations']]],
    ['clear then another operation', [['allocationRevision', '2'], ['_action', 'clear-allocations'], ['_action', 'save']]],
    ['another operation then clear', [['allocationRevision', '2'], ['_action', 'save'], ['_action', 'clear-allocations']]],
  ] as Array<[string, Array<[string, string]>]>)('refuses %s with a 400 and no write (D8)', async (_name, entries) => {
    const puts = stubInventory(availabilityBody('seated'));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, entries), { id: SLOT });
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);

  it.each([
    ['a missing revision', [['_action', 'clear-allocations']]],
    ['an empty revision', [['allocationRevision', ''], ['_action', 'clear-allocations']]],
    ['a malformed revision', [['allocationRevision', 'two'], ['_action', 'clear-allocations']]],
    ['a negative revision', [['allocationRevision', '-1'], ['_action', 'clear-allocations']]],
  ] as Array<[string, Array<[string, string]>]>)('refuses a clear with %s with a 400 and no write', async (_name, entries) => {
    const puts = stubInventory(availabilityBody('seated'));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, entries), { id: SLOT });
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);

  it('refuses a clear of an already empty seated set with a 400 and no write (D4)', async () => {
    // Inventory would accept it and burn a revision; the page is the only guard.
    const puts = stubInventory(availabilityBody('seated', []));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries()), { id: SLOT });
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);

  it('refuses a clear when its own fresh read failed, with a 400 and no write (D4)', async () => {
    const puts = stubInventory('fail');
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries()), { id: SLOT });
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);
});

describe('the slot page for a GA pool', () => {
  it('shows the public-channel guidance for unallocated GA capacity', async () => {
    stubInventory(availabilityBody('ga'));
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();

    expect(html).toContain('stays available to the public channel');
  }, 30_000);

  it('omits the public-channel guidance when the availability read fails', async () => {
    stubInventory('fail');
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();

    expect(html).toContain('Inventory is unavailable');
    expect(html).not.toContain('stays available to the public channel');
  }, 30_000);

  it('refuses the clear operation with a 400 and no write, because inventory would accept an empty GA replace (D4)', async () => {
    const puts = stubInventory(availabilityBody('ga'));
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries()), { id: SLOT });
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);

  it('still renders the editor, with no clear form and no operation selector (COS5)', async () => {
    stubInventory(availabilityBody('ga'));
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();

    expect(html).toContain('data-action="save-allocations"');
    expect(html).toContain('name="cap.0"');
    expect(html).not.toContain('clear-allocations');
    expect(html).not.toContain('name="_action"');
    expect(formControlNames(html)).toHaveLength(1);
  }, 30_000);

  it('runs a POST with no `_action` as the editor, exactly as before (COS5)', async () => {
    const puts = stubInventory(availabilityBody('ga'));
    const response = await renderPage(
      SLOT_PAGE,
      post(slotPath, {
        allocationRevision: '2',
        'channel.0': 'reseller-acme',
        'cap.0': '55',
        'releaseAt.0': '2026-10-01T09:30:15.123456Z',
        'channel.1': 'pos',
        'cap.1': '12',
        'releaseAt.1': '',
      }),
      { id: SLOT },
    );

    expect(response.status).toBe(303);
    // Written out by hand, not derived from toAllocationRequest: the stored window, code
    // gate and seller binding come from inventory's current set, only the cap changed.
    expect(puts).toHaveLength(1);
    expect(puts[0].body).toEqual({
      organizer_id: ORGANIZER,
      allocation_revision: 2,
      allocations: [
        {
          channel: 'reseller-acme',
          cap: 55,
          release_at: '2026-10-01T09:30:15.123456Z',
          opens_at: '2026-09-20T08:00:00Z',
          closes_at: '2026-09-30T22:15:30Z',
          requires_code: true,
          sold_by: SELLER,
        },
        { channel: 'pos', cap: 12, requires_code: false },
      ],
    });
  }, 30_000);

  it('refuses an editor POST that carries an operation selector it does not know (D8)', async () => {
    const puts = stubInventory(availabilityBody('ga'));
    const response = await renderPage(
      SLOT_PAGE,
      postEntries(slotPath, [['allocationRevision', '2'], ['_action', 'save'], ['channel.0', 'pos'], ['cap.0', '12']]),
      { id: SLOT },
    );
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);
});

describe('the slot page when the pool kind is unknown', () => {
  it.each([
    ['omitted (version skew)', (() => { const b: Record<string, unknown> = availabilityBody('seated'); delete b.inventory_kind; return b; })()],
    ['unknown', { ...availabilityBody('seated'), inventory_kind: 'hybrid' }],
  ])('shows the inventory-unavailable state, no editor and no clear form, when the kind is %s (D5)', async (_name, body) => {
    const puts = stubInventory(body);
    const html = await (await renderPage(SLOT_PAGE, new Request(`http://backoffice.test${slotPath}`), { id: SLOT })).text();

    expect(html).toContain('Inventory is unavailable');
    expect(html).not.toContain('stays available to the public channel');
    expect(html).not.toContain('data-action="save-allocations"');
    expect(html).not.toContain('data-action="clear-allocations"');
    expect(html).not.toContain('<form');
    expect(puts).toHaveLength(0);
  }, 30_000);

  it('refuses a clear POST when the kind is unreadable (D5)', async () => {
    const body: Record<string, unknown> = availabilityBody('seated');
    delete body.inventory_kind;
    const puts = stubInventory(body);
    const response = await renderPage(SLOT_PAGE, postEntries(slotPath, clearEntries()), { id: SLOT });
    expect(response.status).toBe(400);
    expect(puts).toHaveLength(0);
  }, 30_000);
});
