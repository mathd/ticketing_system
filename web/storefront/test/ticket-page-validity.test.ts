// @vitest-environment node
import { afterEach, describe, expect, it, vi } from 'vitest';

import { UI_STRINGS } from '../src/lib/locales';

// TKT-355: the ticket page shows a QR only for a ticket whose Access verdict is valid,
// and it says why the others are voided. The REAL page module renders through Astro's
// container (the pattern in page-call-budget.test.ts), because the .astro file is not
// typechecked and a rule kept in it is invisible to every other test.
//
// The fixture is built so that the history CONTRADICTS the verdict, in both directions.
// A page that still reads history, alone or OR-ed with the verdict, cannot pass:
//   - ticket 1: verdict voided/refunded, history issued only -> no image;
//   - ticket 2: verdict valid, history issued + refunded     -> image;
//   - ticket 3: verdict valid, history issued only            -> image;
//   - ticket 4: verdict voided/exchanged, history issued only -> no image.
// Voided tickets sit first AND last, so a page that applies one ticket's verdict to its
// neighbour, or to every ticket, also fails.

const REF = '11111111-1111-4111-8111-111111111111';
const event = (n: number, type: string) => ({
  id: `00000000-0000-4000-8000-00000000000${n}`,
  type,
  sequence: n,
  occurred_at: '2026-08-03T12:00:00Z',
});
const ticket = (n: number, validity: unknown, history: unknown[]) => ({
  ticket_id: `22222222-2222-4222-8222-22222222222${n}`,
  qr_payload: `credential-${n}`,
  issued_at: '2026-08-03T12:00:00Z',
  qr_url: `/qr-${n}.png`,
  history,
  validity,
  performance_timezone: 'Europe/Paris',
});

// 30 s per case, as in page-call-budget.test.ts: the first render transforms the page
// through Astro's vite plugin, and under load that alone passes the 5 s default (it did,
// twice in a row, during the TKT-365 plan review).
const RENDER_TIMEOUT = 30_000;
const bundle = {
  order_ref: REF,
  tickets: [
    ticket(1, { status: 'voided', reason: 'refunded' }, [event(1, 'issued')]),
    ticket(2, { status: 'valid' }, [event(1, 'issued'), event(2, 'refunded')]),
    ticket(3, { status: 'valid' }, [event(1, 'issued')]),
    ticket(4, { status: 'voided', reason: 'exchanged' }, [event(1, 'issued')]),
  ],
};

async function renderTicketPage(locale: string, body: unknown) {
  vi.stubGlobal('fetch', async () =>
    new Response(JSON.stringify(body), { status: 200, headers: { 'content-type': 'application/json' } }));
  const { experimental_AstroContainer } = await import('astro/container');
  const container = await experimental_AstroContainer.create();
  // A variable path, as page-call-budget.test.ts does: tsc has no types for .astro
  // modules, and vite resolves this one at run time.
  const page = '../src/pages/[locale]/tickets/[orderRef].astro';
  const mod = await import(/* @vite-ignore */ page);
  return container.renderToResponse(mod.default, { params: { locale, orderRef: REF }, routeType: 'page' });
}

/** The rendered cards, in page order. */
function cards(html: string): string[] {
  return html.split('<article').slice(1).map((card) => card.slice(0, card.indexOf('</article>')));
}

function decode(text: string): string {
  return text
    .replace(/&#(\d+);/g, (_, code: string) => String.fromCodePoint(Number(code)))
    .replace(/&#x([0-9a-f]+);/gi, (_, code: string) => String.fromCodePoint(parseInt(code, 16)))
    .replace(/&quot;/g, '"')
    .replace(/&amp;/g, '&');
}

/** The void notice of one card, as text, or null when the card has none. */
function voidNotice(card: string): string | null {
  const match = /<p class="voided"[^>]*>([\s\S]*?)<\/p>/.exec(card);
  return match ? decode(match[1].replace(/<[^>]+>/g, ' ')).replace(/\s+/g, ' ').trim() : null;
}

afterEach(() => vi.unstubAllGlobals());

describe('ticket page renders the Access verdict (TKT-355)', () => {
  it.each(['en', 'fr'] as const)('shows a QR only for valid tickets, and the void reason for the others (%s)', async (locale) => {
    const t = UI_STRINGS[locale];
    // Two distinct reason strings, or a swapped mapping would render identically.
    expect(t.ticketVoidedRefunded).not.toBe(t.ticketVoidedExchanged);

    const response = await renderTicketPage(locale, bundle);
    expect(response.status).toBe(200);
    const html = await response.text();
    const rendered = cards(html);
    expect(rendered).toHaveLength(4);

    // Voided first: no image anywhere on the page for it, and the refund reason.
    expect(rendered[0]).not.toContain('<img');
    expect(html).not.toContain('/qr-1.png');
    expect(voidNotice(rendered[0])).toBe(`${t.ticketVoided} ${t.ticketVoidedRefunded}`);

    // Valid by verdict although its history holds a refund: the verdict decides.
    expect(rendered[1]).toContain('src="/qr-2.png"');
    expect(voidNotice(rendered[1])).toBeNull();

    expect(rendered[2]).toContain('src="/qr-3.png"');
    expect(voidNotice(rendered[2])).toBeNull();

    // Voided last: the exchange reason.
    expect(rendered[3]).not.toContain('<img');
    expect(html).not.toContain('/qr-4.png');
    expect(voidNotice(rendered[3])).toBe(`${t.ticketVoided} ${t.ticketVoidedExchanged}`);

    expect((html.match(/<img/g) ?? []).length).toBe(2);
  }, RENDER_TIMEOUT);

  it('answers 503, not a page of valid QRs, when the verdict is missing', async () => {
    const withoutVerdict = {
      ...bundle,
      tickets: bundle.tickets.map(({ validity: _validity, ...rest }) => rest),
    };
    const response = await renderTicketPage('en', withoutVerdict);
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain('<img');
  }, RENDER_TIMEOUT);
});
