// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { UI_STRINGS } from '../src/lib/locales';

// TKT-365: the REAL page modules, rendered through Astro's container (the pattern in
// page-call-budget.test.ts). The .astro files are not type-checked, so what a page does
// with the table and the projection is visible only in its rendered output.
//
// 30 s per case, as in page-call-budget.test.ts: the first render transforms the page
// through Astro's vite plugin, and on a busy machine that alone passes the 5 s default.
const RENDER_TIMEOUT = 30_000;

const REF = '11111111-1111-4111-8111-111111111111';
const EVENT_ID = '00000000-0000-0000-0000-000000000001';
const INSTANT = '2026-07-14T22:30:05Z';

const historyEntry = (n: number, type: string) => ({
  id: `00000000-0000-4000-8000-00000000000${n}`,
  type,
  sequence: n,
  occurred_at: INSTANT,
});
const ticket = (n: number, zone: string | null, history: unknown[]) => ({
  ticket_id: `22222222-2222-4222-8222-22222222222${n}`,
  qr_payload: `credential-${n}`,
  issued_at: INSTANT,
  qr_url: `/qr-${n}.png`,
  validity: { status: 'valid' },
  performance_timezone: zone,
  history,
});
// Two tickets, two zones: one known (Kolkata, UTC+05:30) and one Access could not
// resolve. A page that formatted every ticket in one zone, or in the server's default,
// cannot render both cards correctly.
const bundle = {
  order_ref: REF,
  tickets: [
    ticket(1, 'Asia/Kolkata', [historyEntry(1, 'issued'), historyEntry(2, 'delivered'), historyEntry(3, 'future_kind')]),
    ticket(2, null, [historyEntry(1, 'issued')]),
  ],
};

const eventDetail = {
  id: EVENT_ID,
  organizer_id: '00000000-0000-0000-0000-000000000002',
  name: 'Concert',
  description: '',
  series: [],
  performances: [{
    id: '00000000-0000-0000-0000-000000000005',
    starts_at: '2026-09-01T18:00:00Z',
    timezone: 'Europe/Paris',
    venue_name: 'Salle Pleyel',
    venue: { id: '00000000-0000-0000-0000-000000000008', name: 'Salle Pleyel' },
    from_price: { amount: 4550, currency: 'EUR' },
    ticket_types: [{ id: '00000000-0000-0000-0000-000000000011', name: 'Fosse', price: { amount: 4550, currency: 'EUR' } }],
  }],
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json', 'cache-control': 'public, max-age=300' },
  });
}

async function render(page: string, params: Record<string, string>, answer: () => Response) {
  vi.stubGlobal('fetch', async () => answer());
  const { experimental_AstroContainer } = await import('astro/container');
  const { loadRenderers } = await import('astro:container');
  const { getContainerRenderer } = await import('@astrojs/react');
  const renderers = await loadRenderers([getContainerRenderer()]);
  const container = await experimental_AstroContainer.create({ renderers });
  const mod = await import(/* @vite-ignore */ page);
  return container.renderToResponse(mod.default, { params, routeType: 'page' });
}

const TICKET_PAGE = '../src/pages/[locale]/tickets/[orderRef].astro';
const EVENT_PAGE = '../src/pages/[locale]/events/[eventId].astro';
const LIST_PAGE = '../src/pages/[locale]/events/index.astro';

function decode(text: string): string {
  return text
    .replace(/&#(\d+);/g, (_, code: string) => String.fromCodePoint(Number(code)))
    .replace(/&#x([0-9a-f]+);/gi, (_, code: string) => String.fromCodePoint(parseInt(code, 16)))
    .replace(/&nbsp;/g, ' ')
    .replace(/&quot;/g, '"')
    .replace(/&amp;/g, '&');
}

/** Each history line of each ticket card, as [label, datetime attribute, shown time]. */
function historyLines(html: string): Array<Array<[string, string, string]>> {
  return html.split('<article').slice(1).map((card) =>
    [...card.matchAll(/<li[^>]*>([\s\S]*?)<\/li>/g)].map((li) => {
      const time = /<time[^>]*datetime="([^"]*)"[^>]*>([\s\S]*?)<\/time>/.exec(li[1]);
      const label = decode(li[1].replace(/<time[\s\S]*<\/time>/, '').replace(/<[^>]+>/g, '')).replace(/[\s—]+$/u, '').trim();
      return [label, time?.[1] ?? '', decode(time?.[2] ?? '')];
    }));
}

beforeEach(() => vi.resetModules());
afterEach(() => vi.unstubAllGlobals());

describe('the French ticket page (TKT-365)', () => {
  it('labels history in French and shows each time in its ticket\'s performance zone', async () => {
    const t = UI_STRINGS.fr;
    const response = await render(TICKET_PAGE, { locale: 'fr', orderRef: REF }, () => json(bundle));
    expect(response.status).toBe(200);
    const html = await response.text();
    const [kolkata, unknownZone] = historyLines(html);

    expect(kolkata.map(([label]) => label)).toEqual([t.historyIssued, t.historyDelivered, t.historyOther]);
    for (const [, datetime, shown] of kolkata) {
      expect(datetime).toBe(INSTANT);
      expect(shown).toContain('15 juil. 2026');
      expect(shown).toContain('04:00:05');
      expect(shown).toContain('UTC+5:30');
    }
    expect(unknownZone).toHaveLength(1);
    expect(unknownZone[0][0]).toBe(t.historyIssued);
    expect(unknownZone[0][2]).toContain('22:30:05');
    expect(unknownZone[0][2]).toContain('UTC');
    expect(unknownZone[0][2]).not.toContain('UTC+');

    // Service vocabulary never reaches the buyer.
    expect(html).not.toMatch(/>\s*(issued|delivered|future_kind)\b/);
    expect(html).toContain(`aria-label="${t.languageLabel}"`);
    expect(html).not.toContain('aria-label="Language"');
  }, RENDER_TIMEOUT);

  it('keeps English labels on the English page', async () => {
    const response = await render(TICKET_PAGE, { locale: 'en', orderRef: REF }, () => json(bundle));
    const [kolkata] = historyLines(await response.text());
    expect(kolkata.map(([label]) => label)).toEqual(['Issued', 'Delivered', 'Ticket activity']);
    expect(kolkata[0][2]).toContain('GMT+5:30');
  }, RENDER_TIMEOUT);

  it.each([
    ['fr', 404, 404, 'Introuvable'],
    ['fr', 500, 500, 'Service indisponible'],
    ['en', 404, 404, 'Not found'],
    ['en', 500, 500, 'Service unavailable'],
  ] as const)('answers a %s bundle read of %i with status %i and a localized body', async (locale, upstream, status, body) => {
    const response = await render(TICKET_PAGE, { locale, orderRef: REF }, () => json({ error: 'x' }, upstream));
    expect(response.status).toBe(status);
    expect(await response.text()).toBe(body);
  }, RENDER_TIMEOUT);
});

describe('the French event pages (TKT-365)', () => {
  it('writes the venue line with French punctuation', async () => {
    const response = await render(EVENT_PAGE, { locale: 'fr', eventId: EVENT_ID }, () => json(eventDetail));
    expect(response.status).toBe(200);
    const html = decode(await response.text());
    expect(html).toContain('Salle : Salle Pleyel');
    expect(html).toContain(`aria-label="${UI_STRINGS.fr.languageLabel}"`);
  }, RENDER_TIMEOUT);

  it('keeps the English venue line on the English page', async () => {
    const response = await render(EVENT_PAGE, { locale: 'en', eventId: EVENT_ID }, () => json(eventDetail));
    expect(decode(await response.text())).toContain('Venue: Salle Pleyel');
  }, RENDER_TIMEOUT);

  it.each([
    [EVENT_PAGE, { locale: 'fr', eventId: EVENT_ID }, 'Service indisponible'],
    [LIST_PAGE, { locale: 'fr' }, 'Service indisponible'],
    [EVENT_PAGE, { locale: 'en', eventId: EVENT_ID }, 'Service unavailable'],
    [LIST_PAGE, { locale: 'en' }, 'Service unavailable'],
  ])('answers a catalog outage on %s with 503 and a localized body', async (page, params, body) => {
    const response = await render(page, params, () => json({ error: 'x' }, 500));
    expect(response.status).toBe(503);
    expect(await response.text()).toBe(body);
  }, RENDER_TIMEOUT);
});
