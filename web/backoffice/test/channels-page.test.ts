// @vitest-environment node
//
// The channels page's optimistic token (TKT-325, ADR-080), rendered through the
// real Astro page with catalog stubbed at fetch.
//
// What this tier can see: which token each form renders, which values the hidden
// fields carry after a refusal, and whether the page sent a PUT at all — the page
// makes that call itself, so counting it here is honest. What it cannot see is a
// real browser submitting the rendered form; test/browser/channel-revision.mjs
// does that, with two actors.
//
// The stub catalog compares expected_revision with its stored row and bumps on
// success, like the real one. The STALE state is set up by giving the stub a
// newer row than the token the test submits, which is what another operator's
// save leaves behind.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const STAFF = '60000000-0000-4000-8000-000000000001';
const ORGANIZER = '00000000-0000-4000-8000-000000000001';
const WEB = '70000000-0000-4000-8000-000000000001';
const POS = '70000000-0000-4000-8000-000000000002';
const ASSERTION = `v2.catalog-org/test.${STAFF}.${ORGANIZER}.99999999999.${'A'.repeat(86)}`;
const PAGE = '../src/pages/channels.astro';
const STALE = 'This channel changed since this page was loaded, so nothing was saved.';

const principal = {
  staffId: STAFF,
  organizerId: ORGANIZER,
  role: 'admin' as const,
  organizerAssertion: ASSERTION,
};

interface Row {
  id: string;
  code: string;
  display_name: string;
  kind: string;
  enabled: boolean;
  revision: number;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

function wire(row: Row) {
  return { ...row, organizer_id: ORGANIZER, created_at: '2026-09-01T10:00:00Z', updated_at: '2026-09-01T10:00:00Z' };
}

/** A stub catalog holding rows; records every PUT body it receives. */
function stubCatalog(rows: Row[]) {
  const puts: Record<string, unknown>[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? 'GET';
    if (method === 'GET' && url.endsWith('/internal/channels')) {
      return json({ channels: rows.map(wire) });
    }
    if (method === 'PUT' && url.includes('/api/catalog/channels/')) {
      const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
      puts.push(body);
      const id = decodeURIComponent(url.split('/').pop() ?? '');
      const row = rows.find((r) => r.id === id);
      if (!row) return json({ error: 'referenced entity not found' }, 404);
      if (body.expected_revision !== row.revision) {
        return json({ error: 'channel changed since it was read; nothing was saved', code: 'channel_revision_mismatch' }, 409);
      }
      row.display_name = String(body.display_name);
      row.enabled = body.enabled === true;
      row.revision += 1;
      return json(wire(row));
    }
    throw new Error(`unexpected request: ${method} ${url}`);
  }));
  return puts;
}

async function render(request: Request): Promise<Response> {
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create({ astroConfig: { base: '/admin' } });
  // A variable path, as mutation-pages.test.ts does: tsc has no module type for
  // an .astro file, and Vite resolves it at run time.
  const mod = await import(/* @vite-ignore */ PAGE);
  return container.renderToResponse(mod.default, {
    request,
    locals: { staff: principal },
    routeType: 'page',
    partial: false,
  });
}

/** Render a page that must re-render (200), not redirect. */
async function renderPage(request: Request): Promise<string> {
  const response = await render(request);
  expect(response.status).toBe(200);
  return response.text();
}

function post(fields: Record<string, string>): Request {
  return new Request('http://backoffice.test/admin/channels', {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(fields),
  });
}

function get(): Request {
  return new Request('http://backoffice.test/admin/channels');
}

/** The HTML of one channel's table row. */
function row(html: string, code: string): string {
  const match = new RegExp(`<tr[^>]*data-channel-code="${code}"[\\s\\S]*?</tr>`).exec(html);
  if (!match) throw new Error(`no row for ${code}`);
  return match[0];
}

/** One of a row's forms, by its data-form name, or undefined. */
function form(rowHtml: string, name: 'toggle' | 'rename' | 'review'): string | undefined {
  return new RegExp(`<form[^>]*data-form="${name}"[\\s\\S]*?</form>`).exec(rowHtml)?.[0];
}

function mustForm(rowHtml: string, name: 'toggle' | 'rename' | 'review'): string {
  const found = form(rowHtml, name);
  if (!found) throw new Error(`no ${name} form in row`);
  return found;
}

/** The value of the named input in a form, or undefined if there is no such input. */
function value(formHtml: string, name: string): string | undefined {
  const input = new RegExp(`<input[^>]*name="${name}"[^>]*>`).exec(formHtml)?.[0];
  if (!input) return undefined;
  return /value="([^"]*)"/.exec(input)?.[1] ?? '';
}

beforeEach(() => {
  vi.resetModules();
  process.env.CATALOG_STAFF_WRITE_TOKEN = 'catalog-test-token';
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
});

describe('channels page: the optimistic revision (TKT-325)', () => {
  it('renders each row\'s own revision as the token on both of its forms', async () => {
    stubCatalog([
      { id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: true, revision: 4 },
      { id: POS, code: 'pos', display_name: 'Box office', kind: 'pos', enabled: false, revision: 0 },
    ]);
    const html = await renderPage(get());
    for (const [code, revision] of [['web', '4'], ['pos', '0']] as const) {
      const r = row(html, code);
      expect(value(mustForm(r, 'toggle'), 'expected_revision')).toBe(revision);
      expect(value(mustForm(r, 'rename'), 'expected_revision')).toBe(revision);
      expect(form(r, 'review')).toBeUndefined();
    }
  }, 30_000);

  it('forwards the SUBMITTED token unchanged, never a server re-read', async () => {
    // The stub row is at 5. The submit says 5, and the PUT must carry 5 — not
    // anything the page read during this request.
    const rows = [{ id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: true, revision: 5 }];
    const puts = stubCatalog(rows);
    const response = await render(post({
      _action: 'update', channel_id: WEB, code: 'web', kind: 'web', enabled: 'true',
      display_name: 'Main website', expected_revision: '5',
    }));
    expect(response.status).toBe(303);
    expect(puts).toEqual([{ code: 'web', display_name: 'Main website', kind: 'web', enabled: true, expected_revision: 5 }]);
  }, 30_000);

  // Direction (a): B disabled the channel after A's page loaded; A renames.
  it('refuses a stale rename and re-renders the submitted token with the fresh values', async () => {
    const rows = [
      { id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: false, revision: 1 },
      { id: POS, code: 'pos', display_name: 'Box office', kind: 'pos', enabled: true, revision: 6 },
    ];
    const puts = stubCatalog(rows);
    const html = await renderPage(post({
      _action: 'update', channel_id: WEB, code: 'web', kind: 'web',
      enabled: 'true', // A's stale snapshot
      display_name: 'Renamed by A', expected_revision: '0',
    }));

    expect(puts).toHaveLength(1);
    expect(rows[0]).toMatchObject({ enabled: false, display_name: 'Website', revision: 1 });

    const r = row(html, 'web');
    expect(r).toContain(STALE);
    const toggle = mustForm(r, 'toggle');
    const rename = mustForm(r, 'rename');
    // The submitted token on BOTH forms: a second click is refused again.
    expect(value(toggle, 'expected_revision')).toBe('0');
    expect(value(rename, 'expected_revision')).toBe('0');
    // The hidden fields come from the fresh read: B's disable, not A's snapshot.
    expect(value(rename, 'enabled')).toBe('false');
    // The toggle direction comes from the fresh state (A7): it now enables.
    expect(value(toggle, 'enabled')).toBe('true');
    expect(toggle).toContain('data-action="enable"');
    // A's attempted name is kept, because the failed action was a rename (A1).
    expect(value(rename, 'display_name')).toBe('Renamed by A');
    // Review is offered, and carries the attempted name.
    const review = mustForm(r, 'review');
    expect(value(review, '_action')).toBe('review');
    expect(value(review, 'channel_id')).toBe(WEB);
    expect(value(review, 'display_name')).toBe('Renamed by A');
    expect(value(review, 'expected_revision')).toBeUndefined();

    // Another row is untouched by the refusal: its own fresh token, no message.
    const other = row(html, 'pos');
    expect(value(mustForm(other, 'rename'), 'expected_revision')).toBe('6');
    expect(other).not.toContain(STALE);
  }, 30_000);

  // Direction (b): B renamed the channel after A's page loaded; A toggles.
  it('refuses a stale toggle and does not pre-fill the stale name', async () => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Renamed by B', kind: 'web', enabled: true, revision: 1 }];
    const puts = stubCatalog(rows);
    const html = await renderPage(post({
      _action: 'set-enabled', channel_id: WEB, code: 'web', kind: 'web',
      display_name: 'Website', // A's stale snapshot
      enabled: 'false', expected_revision: '0',
    }));

    expect(puts).toHaveLength(1);
    expect(rows[0]).toMatchObject({ enabled: true, display_name: 'Renamed by B', revision: 1 });

    const r = row(html, 'web');
    expect(r).toContain(STALE);
    const toggle = mustForm(r, 'toggle');
    const rename = mustForm(r, 'rename');
    expect(value(toggle, 'expected_revision')).toBe('0');
    expect(value(rename, 'expected_revision')).toBe('0');
    // Fresh hidden name on the toggle; the rename input shows B's name, not the
    // stale one the toggle submitted (A1) — one habitual Rename click would
    // otherwise overwrite B's rename after a review.
    expect(value(toggle, 'display_name')).toBe('Renamed by B');
    expect(value(rename, 'display_name')).toBe('Renamed by B');
    expect(toggle).toContain('data-action="disable"');
    // Review after a toggle refusal carries no name.
    expect(value(mustForm(r, 'review'), 'display_name')).toBeUndefined();
  }, 30_000);

  // A7. The fixture is chosen so the stale intent and the fresh direction
  // DIFFER: A's page showed the channel disabled (its button said Enable), and B
  // enabled it in the meantime. The refused row must now offer Disable. In the
  // two direction tests above they happen to agree, so neither can see this.
  it('renders the toggle direction from the fresh state after a refused toggle', async () => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: true, revision: 1 }];
    stubCatalog(rows);
    const r = row(await renderPage(post({
      _action: 'set-enabled', channel_id: WEB, code: 'web', kind: 'web', display_name: 'Website',
      enabled: 'true', // A's stale Enable
      expected_revision: '0',
    })), 'web');
    expect(r).toContain(STALE);
    const toggle = mustForm(r, 'toggle');
    expect(value(toggle, 'enabled')).toBe('false');
    expect(toggle).toContain('data-action="disable"');
    expect(value(toggle, 'expected_revision')).toBe('0');
  }, 30_000);

  it('refuses a second click with the same stale token, still without writing', async () => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: false, revision: 1 }];
    const puts = stubCatalog(rows);
    const first = row(await renderPage(post({
      _action: 'update', channel_id: WEB, code: 'web', kind: 'web', enabled: 'true',
      display_name: 'Renamed by A', expected_revision: '0',
    })), 'web');
    // Resubmit exactly what the refusal rendered.
    const rename = mustForm(first, 'rename');
    const second = row(await renderPage(post({
      _action: 'update', channel_id: WEB,
      code: value(rename, 'code') ?? '', kind: value(rename, 'kind') ?? '',
      enabled: value(rename, 'enabled') ?? '', display_name: value(rename, 'display_name') ?? '',
      expected_revision: value(rename, 'expected_revision') ?? '',
    })), 'web');
    expect(second).toContain(STALE);
    expect(puts.map((p) => p.expected_revision)).toEqual([0, 0]);
    expect(rows[0]).toMatchObject({ enabled: false, display_name: 'Website', revision: 1 });
  }, 30_000);

  it.each([
    ['missing', undefined],
    ['blank', ''],
    ['negative', '-1'],
    ['fractional', '1.5'],
  ])('refuses a %s token locally, with no PUT', async (_name, token) => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: true, revision: 0 }];
    const puts = stubCatalog(rows);
    for (const action of ['update', 'set-enabled']) {
      const fields: Record<string, string> = {
        _action: action, channel_id: WEB, code: 'web', kind: 'web', enabled: 'false', display_name: 'Renamed',
      };
      if (token !== undefined) fields.expected_revision = token;
      const r = row(await renderPage(post(fields)), 'web');
      expect(r).toContain('nothing was saved');
      expect(form(r, 'review')).toBeDefined();
    }
    expect(puts).toHaveLength(0);
    expect(rows[0]).toMatchObject({ display_name: 'Website', enabled: true, revision: 0 });
  }, 30_000);

  it('reviews the current channel with fresh tokens and NO write', async () => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: false, revision: 3 }];
    const puts = stubCatalog(rows);
    const html = await renderPage(post({ _action: 'review', channel_id: WEB, display_name: 'Renamed by A' }));

    expect(puts).toHaveLength(0);
    expect(rows[0]).toMatchObject({ display_name: 'Website', enabled: false, revision: 3 });
    const r = row(html, 'web');
    expect(r).not.toContain(STALE);
    const toggle = mustForm(r, 'toggle');
    const rename = mustForm(r, 'rename');
    expect(value(toggle, 'expected_revision')).toBe('3');
    expect(value(rename, 'expected_revision')).toBe('3');
    expect(value(rename, 'enabled')).toBe('false');
    expect(toggle).toContain('data-action="enable"');
    // The attempted rename is kept for the operator to resubmit deliberately.
    expect(value(rename, 'display_name')).toBe('Renamed by A');
    // And the create form is not polluted by the review's fields.
    const createName = /<input[^>]*id="display_name"[^>]*>/.exec(html)?.[0] ?? '';
    expect(createName).not.toBe('');
    expect(createName).not.toContain('Renamed by A');
  }, 30_000);

  it('reviews without a name after a toggle refusal and shows the current name', async () => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Renamed by B', kind: 'web', enabled: true, revision: 2 }];
    const puts = stubCatalog(rows);
    const html = await renderPage(post({ _action: 'review', channel_id: WEB }));
    expect(puts).toHaveLength(0);
    const rename = mustForm(row(html, 'web'), 'rename');
    expect(value(rename, 'display_name')).toBe('Renamed by B');
    expect(value(rename, 'expected_revision')).toBe('2');
  }, 30_000);

  it('saves deliberately after a review, keeping the other operator\'s change', async () => {
    const rows = [{ id: WEB, code: 'web', display_name: 'Website', kind: 'web', enabled: false, revision: 1 }];
    const puts = stubCatalog(rows);
    const reviewed = mustForm(row(await renderPage(post({ _action: 'review', channel_id: WEB, display_name: 'Renamed by A' })), 'web'), 'rename');
    // Submitting what the review rendered is the deliberate save.
    const response = await render(post({
      _action: 'update', channel_id: WEB,
      code: value(reviewed, 'code') ?? '', kind: value(reviewed, 'kind') ?? '',
      enabled: value(reviewed, 'enabled') ?? '', display_name: value(reviewed, 'display_name') ?? '',
      expected_revision: value(reviewed, 'expected_revision') ?? '',
    }));
    expect(response.status).toBe(303);
    expect(puts).toEqual([{ code: 'web', display_name: 'Renamed by A', kind: 'web', enabled: false, expected_revision: 1 }]);
    expect(rows[0]).toMatchObject({ display_name: 'Renamed by A', enabled: false, revision: 2 });
  }, 30_000);
});
