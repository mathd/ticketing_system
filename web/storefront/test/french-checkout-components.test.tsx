// @vitest-environment jsdom
import { cleanup, fireEvent, getDefaultNormalizer, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import HoldPicker from '../src/components/HoldPicker';
import SeatMapPicker from '../src/components/SeatMapPicker';
import { UI_STRINGS, type Locale } from '../src/lib/locales';

// TKT-365: the purchase islands in French. The expected French text is the table's,
// except where a test pins the grammar itself (the plural forms), which it spells out.

const ORG = '00000000-0000-0000-0000-000000000001';
const TT = '00000000-0000-0000-0000-000000000002';
const SLOT = '00000000-0000-0000-0000-000000000008';
const MAP = '00000000-0000-0000-0000-000000000009';
const SEAT_ONE = 'Stalls/A/1';
const SEAT_TWO = 'Stalls/A/2';
const SEAT_THREE = 'Stalls/A/3';

function heldReservation(extra: Record<string, unknown> = {}) {
  const now = new Date('2026-08-03T12:00:00Z');
  return {
    hold_id: '00000000-0000-0000-0000-000000000004',
    reservation_id: '00000000-0000-0000-0000-000000000003',
    buyer_id: '00000000-0000-0000-0000-000000000005',
    status: 'held', amount: 2500, currency: 'EUR',
    server_time: now.toISOString(),
    expires_at: new Date(now.getTime() + 10 * 60_000).toISOString(),
    ...extra,
  };
}

function geometry() {
  return {
    map: {
      id: MAP, organizer_id: ORG, venue_id: '00000000-0000-0000-0000-000000000010', name: 'Stalls',
      status: 'published', version: 1, published_at: '2026-08-03T12:00:00Z',
      orphan_prevention_enabled: false, created_at: '2026-08-03T11:00:00Z',
    },
    sections: [{
      id: '00000000-0000-0000-0000-000000000011', name: 'Stalls', position: 1,
      rows: [{
        id: '00000000-0000-0000-0000-000000000012', label: 'A', position: 1,
        seats: [SEAT_ONE, SEAT_TWO, SEAT_THREE].map((identity, index) => ({
          id: `00000000-0000-0000-0000-00000000002${index}`, seat_identity: identity, label: String(index + 1), position: index + 1,
        })),
      }],
    }],
  };
}

function occupancy(remaining: number) {
  return {
    slot_id: SLOT, seat_map_id: MAP, offering_status: 'open',
    remaining_capacity: remaining, unavailable_seat_identities: [],
  };
}

function seatedFetch(reservation: { body: unknown; status: number }, remaining = 6) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/api/catalog/public/seat-maps/')) return new Response(JSON.stringify(geometry()), { status: 200 });
    if (url.includes('/seat-occupancy')) return new Response(JSON.stringify(occupancy(remaining)), { status: 200 });
    if (url.includes('/reservations')) return new Response(JSON.stringify(reservation.body), { status: reservation.status });
    throw new Error(`unexpected fetch ${url}`);
  });
}

function seatButton(locale: Locale, label: string) {
  const t = UI_STRINGS[locale];
  return screen.findByRole('button', { name: new RegExp(`^Stalls, ${t.seatRow} A, ${t.seat} ${label}, `) });
}

async function reserveSeats(locale: Locale, seats: string[], reservation: { body: unknown; status: number }) {
  vi.stubGlobal('fetch', seatedFetch(reservation));
  render(<HoldPicker organizerId={ORG} ticketTypeId={TT} locale={locale} slotId={SLOT} seatMapId={MAP} />);
  for (const seat of seats) fireEvent.click(await seatButton(locale, seat));
  fireEvent.click(screen.getByRole('button', { name: UI_STRINGS[locale].reserveSeats }));
}

// The default normalizer collapses every \s, U+00A0 included, so it could not tell the
// French non-breaking space before « : » from a plain one. These queries compare exactly.
const EXACT = { normalizer: getDefaultNormalizer({ trim: false, collapseWhitespace: false }) };

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the checkout form in each locale', () => {
  it.each([
    ['fr', 'Paiement simulé', ['Accepté', 'Refusé', 'Délai dépassé'], 'Acheteur test', /^\d+\u00a0s$/],
    ['en', 'Fake payment', ['Success', 'Decline', 'Timeout'], 'Test Buyer', /^\d+s$/],
  ] as const)('labels the simulated payment, its choices and the defaults (%s)', async (locale, label, choices, buyer, countdown) => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(heldReservation()), { status: 201 })));
    render(<HoldPicker organizerId={ORG} ticketTypeId={TT} locale={locale} />);
    fireEvent.click(screen.getByRole('button', { name: UI_STRINGS[locale].reserve }));

    const select = await screen.findByRole('combobox', { name: label });
    const options = within(select).getAllByRole('option') as HTMLOptionElement[];
    expect(options.map((option) => option.textContent)).toEqual(choices);
    // The values are payment tokens, not copy: they stay the same in every locale.
    expect(options.map((option) => option.value)).toEqual(['fake-ok', 'fake-decline', 'fake-timeout']);
    expect((screen.getByRole('textbox', { name: UI_STRINGS[locale].nameLabel }) as HTMLInputElement).value).toBe(buyer);
    // The hold counts down on the real clock, so the number may already be 599. What the
    // locale decides is the FORMAT: French puts a no-break space before the unit, English none.
    const live = screen.getByText((_, element) => element?.getAttribute('aria-live') === 'polite');
    const prefix = `${UI_STRINGS[locale].heldFor} `;
    expect(live.textContent?.startsWith(prefix)).toBe(true);
    expect(live.textContent?.slice(prefix.length)).toMatch(countdown);
  });

  it('keeps a name the buyer typed when the language does not change', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(heldReservation()), { status: 201 })));
    render(<HoldPicker organizerId={ORG} ticketTypeId={TT} locale="fr" />);
    fireEvent.click(screen.getByRole('button', { name: UI_STRINGS.fr.reserve }));
    const name = await screen.findByRole('textbox', { name: UI_STRINGS.fr.nameLabel });
    fireEvent.change(name, { target: { value: 'Élise Martin' } });
    expect((name as HTMLInputElement).value).toBe('Élise Martin');
  });
});

// The conflict messages agree with the NUMBER of seats they name.
describe('seat refusals agree with the seat count', () => {
  it.each([
    ['fr', [SEAT_ONE], `Cette place n’est plus disponible : ${SEAT_ONE}`],
    ['fr', [SEAT_ONE, SEAT_TWO], `Ces places ne sont plus disponibles : ${SEAT_ONE}, ${SEAT_TWO}`],
    ['en', [SEAT_ONE, SEAT_TWO], `No longer available: ${SEAT_ONE}, ${SEAT_TWO}`],
  ] as const)('a taken refusal in %s naming %j', async (locale, taken, message) => {
    await reserveSeats(locale, ['1', '2'], { status: 409, body: { error: 'seat unavailable', code: 'seat_taken', seat_identities: taken } });
    expect(await screen.findByText(message, EXACT)).toBeTruthy();
  });

  it.each([
    ['fr', [SEAT_THREE], `Cela isolerait la place ${SEAT_THREE}. Ajoutez-la, ou choisissez-en une autre.`],
    ['fr', [SEAT_ONE, SEAT_THREE], `Cela isolerait les places ${SEAT_ONE}, ${SEAT_THREE}. Ajoutez-les, ou choisissez-en d’autres.`],
    ['en', [SEAT_THREE], `This would leave ${SEAT_THREE} on its own. Add it, or pick a different seat.`],
    ['en', [SEAT_ONE, SEAT_THREE], `This would leave ${SEAT_ONE}, ${SEAT_THREE} on their own. Add them, or pick different seats.`],
  ] as const)('an orphan refusal in %s naming %j', async (locale, stranded, message) => {
    await reserveSeats(locale, ['2'], { status: 409, body: { error: 'orphan', code: 'orphaned_seats', seat_identities: stranded } });
    expect(await screen.findByText(message, EXACT)).toBeTruthy();
  });
});

describe('the seat map counts in French', () => {
  function mountMap(remaining: number) {
    vi.stubGlobal('fetch', seatedFetch({ status: 500, body: {} }, remaining));
    render(<SeatMapPicker organizerId={ORG} slotId={SLOT} seatMapId={MAP} locale="fr" onSelectionChange={vi.fn()} />);
  }

  it('agrees the selection count, from zero', async () => {
    mountMap(3);
    await seatButton('fr', '1');
    expect(screen.getByRole('status').textContent).toBe('0 place sélectionnée sur 3');
    fireEvent.click(await seatButton('fr', '1'));
    expect(screen.getByRole('status').textContent).toBe('1 place sélectionnée sur 3');
    fireEvent.click(await seatButton('fr', '2'));
    expect(screen.getByRole('status').textContent).toBe('2 places sélectionnées sur 3');
  });

  it('agrees a one-seat ceiling', async () => {
    mountMap(1);
    fireEvent.click(await seatButton('fr', '1'));
    fireEvent.click(await seatButton('fr', '2'));
    expect(screen.getByRole('status').textContent).toBe('Vous pouvez sélectionner au plus 1 place.');
  });
});
