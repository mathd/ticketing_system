// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const EVENT = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1';
const OTHER_EVENT = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2';
const ORGANIZER = 'cccccccc-cccc-4ccc-8ccc-ccccccccccc3';
const PERFORMANCE = 'dddddddd-dddd-4ddd-8ddd-ddddddddddd4';
const OTHER_PERFORMANCE = '11111111-1111-4111-8111-111111111111';
const VENUE = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee5';
const TICKET_TYPE = 'ffffffff-ffff-4fff-8fff-fffffffffff6';

const performance = {
  id: PERFORMANCE,
  starts_at: '2026-09-01T18:00:00Z',
  timezone: 'America/Toronto',
  venue_name: 'Grand Hall',
  venue: { id: VENUE, name: 'Grand Hall' },
  from_price: { amount: 2500, currency: 'CAD' },
  ticket_types: [{ id: TICKET_TYPE, name: 'General admission', price: { amount: 2500, currency: 'CAD' } }],
};

const eventList = {
  events: [{
    id: EVENT,
    name: 'Opening night',
    description: '',
    performances: [performance],
  }],
};

const eventDetail = {
  id: EVENT,
  organizer_id: ORGANIZER,
  name: 'Opening night',
  description: '',
  series: [],
  performances: [performance],
};

function respond(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json', 'cache-control': 'public, max-age=300' },
  });
}

function astro() {
  return { locals: {} };
}

beforeEach(() => vi.resetModules());

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('catalog page-data response decoding', () => {
  it.each([
    ['an absent events array', {}],
    ['a non-array events field', { events: {} }],
    ['a malformed event identity', {
      events: [{ ...eventList.events[0], id: 'event-1' }],
    }],
    ['a malformed performance date', {
      events: [{
        ...eventList.events[0],
        performances: [{ ...performance, starts_at: 'tonight' }],
      }],
    }],
    ['a negative money amount', {
      events: [{
        ...eventList.events[0],
        performances: [{ ...performance, from_price: { amount: -1, currency: 'CAD' } }],
      }],
    }],
    ['a fractional money amount', {
      events: [{
        ...eventList.events[0],
        performances: [{ ...performance, from_price: { amount: 12.5, currency: 'CAD' } }],
      }],
    }],
    ['an invalid currency', {
      events: [{
        ...eventList.events[0],
        performances: [{ ...performance, from_price: { amount: 2500, currency: 'cad' } }],
      }],
    }],
  ])('rejects a successful list response containing %s', async (_name, body) => {
    vi.stubGlobal('fetch', async () => respond(body));
    const { getPublicEvents } = await import('../src/lib/api');

    await expect(getPublicEvents(astro() as never, 'en')).rejects.toThrow();
  });

  it('rejects a detail response about a different event', async () => {
    vi.stubGlobal('fetch', async () => respond({ ...eventDetail, id: OTHER_EVENT }));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT)).rejects.toThrow(
      'event id does not match the request',
    );
  });

  it('accepts and canonicalises a detail identity that differs from the request only by case', async () => {
    vi.stubGlobal('fetch', async () => respond(eventDetail));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT.toUpperCase())).resolves.toMatchObject({ id: EVENT });
  });

  it('rejects nested UUID-set duplicates even when their wire casing differs', async () => {
    vi.stubGlobal('fetch', async () => respond({
      ...eventDetail,
      series: [{
        id: '11111111-1111-4111-8111-111111111111',
        name: 'Series',
        performance_ids: [PERFORMANCE, PERFORMANCE.toUpperCase()],
      }],
    }));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT)).rejects.toThrow(
      'performance_ids must not contain duplicates',
    );
  });

  it('rejects a series member that is absent from the returned performances', async () => {
    vi.stubGlobal('fetch', async () => respond({
      ...eventDetail,
      series: [{
        id: '22222222-2222-4222-8222-222222222222',
        name: 'Series',
        performance_ids: [OTHER_PERFORMANCE],
      }],
    }));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT)).rejects.toThrow(
      'performance_ids must reference a returned performance',
    );
  });

  it('rejects one performance assigned to two series, including mixed UUID casing', async () => {
    vi.stubGlobal('fetch', async () => respond({
      ...eventDetail,
      series: [
        {
          id: '22222222-2222-4222-8222-222222222222',
          name: 'First series',
          performance_ids: [PERFORMANCE],
        },
        {
          id: '33333333-3333-4333-8333-333333333333',
          name: 'Second series',
          performance_ids: [PERFORMANCE.toUpperCase()],
        },
      ],
    }));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT)).rejects.toThrow(
      'a performance must not belong to more than one series',
    );
  });

  it('rejects duplicate returned performance identities after canonicalisation', async () => {
    vi.stubGlobal('fetch', async () => respond({
      ...eventDetail,
      performances: [performance, { ...performance, id: PERFORMANCE.toUpperCase() }],
    }));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT)).rejects.toThrow(
      'performances must not contain duplicate identities',
    );
  });

  it('rejects duplicate series identities after canonicalisation', async () => {
    const seriesId = '22222222-2222-4222-8222-222222222222';
    vi.stubGlobal('fetch', async () => respond({
      ...eventDetail,
      series: [
        { id: seriesId, name: 'First series', performance_ids: [] },
        { id: seriesId.toUpperCase(), name: 'Second series', performance_ids: [] },
      ],
    }));
    const { getPublicEvent } = await import('../src/lib/api');

    await expect(getPublicEvent(astro() as never, 'en', EVENT)).rejects.toThrow(
      'series must not contain duplicate identities',
    );
  });

  it('rejects a festival response whose days are not an array', async () => {
    vi.stubGlobal('fetch', async () => respond({
      id: EVENT,
      organizer_id: ORGANIZER,
      name: 'Festival',
      days: {},
    }));
    const { getPublicFestival } = await import('../src/lib/api');

    await expect(getPublicFestival(astro() as never, 'en', EVENT)).rejects.toThrow(
      'days must be an array',
    );
  });

  it('checks an impossible calendar date on both a cache miss and a raw-body cache hit', async () => {
    const malformed = {
      events: [{
        ...eventList.events[0],
        performances: [{ ...performance, starts_at: '2025-02-29T18:00:00Z' }],
      }],
    };
    const fetchStub = vi.fn(async () => respond(malformed));
    vi.stubGlobal('fetch', fetchStub);
    const { getPublicEvents } = await import('../src/lib/api');

    await expect(getPublicEvents(astro() as never, 'en')).rejects.toThrow();
    await expect(getPublicEvents(astro() as never, 'en')).rejects.toThrow();
    expect(fetchStub).toHaveBeenCalledOnce();
  });

  it('accepts lowercase RFC 3339 separators and offsets through the cacheable Catalog boundary', async () => {
    const valid = {
      events: [{
        ...eventList.events[0],
        performances: [{ ...performance, starts_at: '2024-02-29t18:00:00-05:30' }],
      }],
    };
    const fetchStub = vi.fn(async () => respond(valid));
    vi.stubGlobal('fetch', fetchStub);
    const { getPublicEvents } = await import('../src/lib/api');

    const expected = {
      events: [{ performances: [{ starts_at: '2024-02-29t18:00:00-05:30' }] }],
    };
    await expect(getPublicEvents(astro() as never, 'en')).resolves.toMatchObject(expected);
    await expect(getPublicEvents(astro() as never, 'en')).resolves.toMatchObject(expected);
    expect(fetchStub).toHaveBeenCalledOnce();
  });
});
