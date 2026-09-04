import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  authenticateStaff,
  createChannel,
  createEvent,
  createPerformance,
  createTicketType,
  editSeatMap,
  getSeatMapGeometry,
  getVenues,
  listChannelsForOperator,
  listSeatMapVersions,
  publishPerformance,
  updateChannel,
  updateVenueGaCapacity,
} from '../src/lib/catalog';
import { getOrderState, refundOrder } from '../src/lib/commerce';
import { getOrderTickets, redeliverOrderTickets } from '../src/lib/access';
import { getStaffAvailability, replaceChannelAllocations } from '../src/lib/inventory';
import { AmbiguousMutationError } from '../src/lib/upstream';

const ORGANIZER = '00000000-0000-4000-8000-000000000001';
const OTHER_ORGANIZER = '00000000-0000-4000-8000-000000000002';
const VENUE = '10000000-0000-4000-8000-000000000001';
const OTHER_VENUE = '10000000-0000-4000-8000-000000000002';
const MAP = '20000000-0000-4000-8000-000000000001';
const OTHER_MAP = '20000000-0000-4000-8000-000000000002';
const EVENT = '30000000-0000-4000-8000-000000000001';
const PERFORMANCE = '40000000-0000-4000-8000-000000000001';
const OTHER_PERFORMANCE = '40000000-0000-4000-8000-000000000002';
const TICKET_TYPE = '50000000-0000-4000-8000-000000000001';
const STAFF = '60000000-0000-4000-8000-000000000001';
const OTHER_STAFF = '60000000-0000-4000-8000-000000000002';
const CHANNEL = '70000000-0000-4000-8000-000000000001';
const SLOT = '80000000-0000-4000-8000-000000000001';
const OTHER_SLOT = '80000000-0000-4000-8000-000000000002';
const ORDER = '90000000-0000-4000-8000-000000000001';
const OTHER_ORDER = '90000000-0000-4000-8000-000000000002';
const REFUND = 'a0000000-0000-4000-8000-000000000001';
const ASSERTION = `v1.${STAFF}.${ORGANIZER}.99999999999.${'A'.repeat(43)}`;
const MAX_INT32 = 2_147_483_647;

function answer(body: unknown, status = 200): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify(body), {
      status,
      headers: { 'content-type': 'application/json' },
    })),
  );
}

function venue(overrides: Record<string, unknown> = {}) {
  return {
    id: VENUE,
    organizer_id: ORGANIZER,
    name: 'Hall',
    ga_capacity: 100,
    created_at: '2026-09-01T10:00:00Z',
    ...overrides,
  };
}

function seatMap(overrides: Record<string, unknown> = {}) {
  return {
    id: MAP,
    organizer_id: ORGANIZER,
    venue_id: VENUE,
    name: 'Main',
    version: 1,
    status: 'draft',
    orphan_prevention_enabled: false,
    created_at: '2026-09-01T10:00:00Z',
    ...overrides,
  };
}

function performance(overrides: Record<string, unknown> = {}) {
  return {
    id: PERFORMANCE,
    organizer_id: ORGANIZER,
    event_id: EVENT,
    venue_id: VENUE,
    kind: 'performance',
    starts_at: '2026-09-01T20:00:00Z',
    timezone: 'America/Toronto',
    re_entry: { mode: 'single', requires_exit: false },
    closure: { status: 'open' },
    status: 'draft',
    created_at: '2026-09-01T10:00:00Z',
    ...overrides,
  };
}

function channel(overrides: Record<string, unknown> = {}) {
  return {
    id: CHANNEL,
    organizer_id: ORGANIZER,
    code: 'pos',
    display_name: 'Box office',
    kind: 'pos',
    enabled: true,
    created_at: '2026-09-01T10:00:00Z',
    updated_at: '2026-09-01T10:00:00Z',
    ...overrides,
  };
}

function staffAvailability(overrides: Record<string, unknown> = {}) {
  return {
    slot_id: SLOT,
    capacity: 100,
    buyer_held: 1,
    operational_held: 2,
    reservation_held: 3,
    confirmed: 4,
    available: 90,
    public_available: 50,
    offering_status: 'open',
    channels: [],
    allocation_revision: 2,
    ...overrides,
  };
}

function refund(overrides: Record<string, unknown> = {}) {
  return {
    refund_id: REFUND,
    order_id: ORDER,
    quantity: 1,
    amount: 0,
    currency: 'EUR',
    refund_status: 'partial',
    refunded_quantity: 1,
    refunded_amount: 0,
    replay: false,
    tickets_voided: true,
    capacity_returned: true,
    ...overrides,
  };
}

beforeEach(() => {
  process.env.CATALOG_STAFF_WRITE_TOKEN = 'catalog-test-token';
  process.env.COMMERCE_STAFF_WRITE_TOKEN = 'commerce-test-token';
  process.env.INVENTORY_STAFF_WRITE_TOKEN = 'inventory-test-token';
  process.env.ACCESS_STAFF_WRITE_TOKEN = 'access-test-token';
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.CATALOG_STAFF_WRITE_TOKEN;
  delete process.env.COMMERCE_STAFF_WRITE_TOKEN;
  delete process.env.INVENTORY_STAFF_WRITE_TOKEN;
  delete process.env.ACCESS_STAFF_WRITE_TOKEN;
});

describe('catalog response decoding', () => {
  it.each([
    ['an absent field', { venues: [venue({ id: undefined })] }],
    ['the wrong collection type', { venues: {} }],
    ['another organizer', { venues: [venue({ organizer_id: OTHER_ORGANIZER })] }],
    ['a negative capacity', { venues: [venue({ ga_capacity: -1 })] }],
    ['an impossible date', { venues: [venue({ created_at: '2026-02-30T10:00:00Z' })] }],
  ])('rejects venue data with %s', async (_name, body) => {
    answer(body);
    await expect(getVenues(ORGANIZER)).rejects.toThrow();
  });

  it.each([
    ['an absent required field', { map: seatMap({ orphan_prevention_enabled: undefined }), sections: [] }],
    ['the wrong collection type', { map: seatMap(), sections: {} }],
    ['another map identity', { map: seatMap({ id: OTHER_MAP }), sections: [] }],
    ['an invalid version', { map: seatMap({ version: 0 }), sections: [] }],
    ['an unknown status', { map: seatMap({ status: 'ready' }), sections: [] }],
    ['an impossible date', { map: seatMap({ created_at: '2026-02-30T10:00:00Z' }), sections: [] }],
  ])('rejects seat-map geometry with %s', async (_name, body) => {
    answer(body);
    await expect(getSeatMapGeometry(MAP)).rejects.toThrow();
  });

  it.each([
    ['another organizer', { organizer_id: OTHER_ORGANIZER }],
    ['another venue', { venue_id: OTHER_VENUE }],
  ])('classifies an edited seat map for %s as ambiguous', async (_name, mismatch) => {
    answer(seatMap({
      id: OTHER_MAP,
      version: 2,
      status: 'published',
      published_at: '2026-09-01T11:00:00Z',
      ...mismatch,
    }), 201);

    await expect(editSeatMap(
      MAP,
      { sections: [] },
      ORGANIZER,
      VENUE,
      ASSERTION,
    )).rejects.toBeInstanceOf(AmbiguousMutationError);
  });

  it('rejects a version family that does not contain the requested map', async () => {
    answer({ versions: [seatMap({ id: OTHER_MAP })], current_version: 1 });
    await expect(listSeatMapVersions(MAP, ORGANIZER, VENUE)).rejects.toThrow(/requested map/);
  });

  it.each([
    ['another organizer', [
      seatMap({ id: OTHER_MAP, version: 2, organizer_id: OTHER_ORGANIZER }),
      seatMap(),
    ]],
    ['another venue', [
      seatMap({ id: OTHER_MAP, version: 2, venue_id: OTHER_VENUE }),
      seatMap(),
    ]],
    ['another family name', [
      seatMap({ id: OTHER_MAP, version: 2, name: 'Balcony' }),
      seatMap(),
    ]],
    ['a duplicate family version', [
      seatMap({ id: OTHER_MAP, version: 1 }),
      seatMap(),
    ]],
  ])('rejects a version history containing %s', async (_name, versions) => {
    answer({ versions });
    await expect(listSeatMapVersions(MAP, ORGANIZER, VENUE)).rejects.toThrow();
  });

  it('rejects current_version unless it names the highest published member', async () => {
    answer({
      current_version: 2,
      versions: [
        seatMap({ id: OTHER_MAP, version: 2, status: 'draft' }),
        seatMap({ status: 'published', published_at: '2026-09-01T11:00:00Z' }),
      ],
    });
    await expect(listSeatMapVersions(MAP, ORGANIZER, VENUE)).rejects.toThrow(/published member/);
  });

  it('rejects current_version above the catalog int32 maximum', async () => {
    answer({
      current_version: MAX_INT32 + 1,
      versions: [seatMap({ status: 'published', published_at: '2026-09-01T11:00:00Z' })],
    });
    await expect(listSeatMapVersions(MAP, ORGANIZER, VENUE)).rejects.toThrow(/int32 maximum/);
  });

  it.each([
    ['a malformed staff id', { staff_id: 'staff', organizer_id: ORGANIZER, role: 'admin', organizer_assertion: ASSERTION }],
    ['a malformed organizer id', { staff_id: STAFF, organizer_id: 'organizer', role: 'admin', organizer_assertion: ASSERTION }],
    ['an absent assertion', { staff_id: STAFF, organizer_id: ORGANIZER, role: 'admin' }],
    ['an unknown role', { staff_id: STAFF, organizer_id: ORGANIZER, role: 'owner', organizer_assertion: ASSERTION }],
  ])('rejects a staff principal with %s', async (_name, body) => {
    answer(body);
    await expect(authenticateStaff('staff@example.test', 'password')).rejects.toThrow();
  });

  it.each([
    ['the wrong version', `v2.${STAFF}.${ORGANIZER}.99999999999.${'A'.repeat(43)}`],
    ['the wrong field count', `v1.${STAFF}.${ORGANIZER}.99999999999`],
    ['a malformed embedded staff id', `v1.staff.${ORGANIZER}.99999999999.${'A'.repeat(43)}`],
    ['a malformed embedded organizer id', `v1.${STAFF}.organizer.99999999999.${'A'.repeat(43)}`],
    ['a malformed expiry', `v1.${STAFF}.${ORGANIZER}.later.${'A'.repeat(43)}`],
    ['an overflowing expiry', `v1.${STAFF}.${ORGANIZER}.9223372036854775808.${'A'.repeat(43)}`],
    ['a malformed signature', `v1.${STAFF}.${ORGANIZER}.99999999999.short`],
  ])('rejects a principal whose organizer assertion has %s', async (_name, assertion) => {
    answer({
      staff_id: STAFF,
      organizer_id: ORGANIZER,
      role: 'admin',
      organizer_assertion: assertion,
    });
    await expect(authenticateStaff('staff@example.test', 'password')).rejects.toThrow();
  });

  it('rejects an assertion bound to another organizer', async () => {
    answer({
      staff_id: STAFF,
      organizer_id: ORGANIZER,
      role: 'admin',
      organizer_assertion: `v1.${STAFF}.${OTHER_ORGANIZER}.99999999999.${'A'.repeat(43)}`,
    });
    await expect(authenticateStaff('staff@example.test', 'password')).rejects.toThrow(/organizer/);
  });

  it('rejects an assertion bound to another staff member', async () => {
    answer({
      staff_id: STAFF,
      organizer_id: ORGANIZER,
      role: 'admin',
      organizer_assertion: `v1.${OTHER_STAFF}.${ORGANIZER}.99999999999.${'A'.repeat(43)}`,
    });
    await expect(authenticateStaff('staff@example.test', 'password')).rejects.toThrow(/staff/);
  });

  it('binds assertion identities case-insensitively', async () => {
    answer({
      staff_id: STAFF,
      organizer_id: ORGANIZER,
      role: 'admin',
      organizer_assertion: `v1.${STAFF.toUpperCase()}.${ORGANIZER.toUpperCase()}.99999999999.${'A'.repeat(43)}`,
    });
    await expect(authenticateStaff('staff@example.test', 'password')).resolves.toMatchObject({
      staffId: STAFF,
      organizerId: ORGANIZER,
    });
  });

  it.each([
    ['an unknown kind', performance({
      kind: 'concert',
      operating_date: '2026-09-01',
      opens_at: '10:00',
      closes_at: '22:00',
    })],
    ['an absent discriminated date', performance({ starts_at: undefined })],
    ['an invalid date', performance({ starts_at: 'tomorrow' })],
    ['an absent re-entry policy', performance({ re_entry: undefined })],
    ['another organizer identity', performance({ organizer_id: OTHER_ORGANIZER })],
    ['another event identity', performance({ event_id: '30000000-0000-4000-8000-000000000002' })],
  ])('rejects a performance with %s', async (_name, body) => {
    answer(body, 201);
    await expect(createPerformance(
      ASSERTION,
      ORGANIZER,
      { eventId: EVENT, venueId: VENUE, startsAt: '2026-09-01T20:00:00Z', timezone: 'America/Toronto' },
      'key',
    )).rejects.toThrow();
  });

  it('rejects a created performance attributed to another venue', async () => {
    answer(performance({ venue_id: OTHER_VENUE }), 201);
    const error = await createPerformance(
      ASSERTION,
      ORGANIZER,
      { eventId: EVENT, venueId: VENUE, startsAt: '2026-09-01T20:00:00Z', timezone: 'America/Toronto' },
      'key',
    ).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/venue_id/) });
  });

  it('rejects a published response for another performance id', async () => {
    answer(performance({ id: OTHER_PERFORMANCE, status: 'published' }));
    const error = await publishPerformance(PERFORMANCE, ASSERTION, ORGANIZER)
      .catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/performance id/) });
  });

  it('rejects a GA-capacity response for another venue id', async () => {
    answer(venue({ id: OTHER_VENUE, ga_capacity: 250 }));
    const error = await updateVenueGaCapacity(VENUE, 250, ASSERTION)
      .catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/venue id/) });
  });

  it.each([
    ['a negative amount', { amount: -1, currency: 'EUR' }],
    ['a fractional amount', { amount: 1.5, currency: 'EUR' }],
    ['a malformed currency', { amount: 100, currency: 'eur' }],
  ])('rejects ticket money with %s', async (_name, price) => {
    answer({
      id: TICKET_TYPE,
      organizer_id: ORGANIZER,
      performance_id: PERFORMANCE,
      name: { en: 'GA' },
      price,
      created_at: '2026-09-01T10:00:00Z',
    }, 201);
    await expect(createTicketType(
      ASSERTION,
      ORGANIZER,
      { performanceId: PERFORMANCE, name: { en: 'GA' }, amount: 100, currency: 'EUR' },
      'key',
    )).rejects.toThrow();
  });

  it('rejects an update response for another channel', async () => {
    answer(channel({ id: '70000000-0000-4000-8000-000000000002' }));
    const error = await updateChannel(ASSERTION, ORGANIZER, CHANNEL, {
      code: 'pos', displayName: 'Box office', kind: 'pos', enabled: true,
    }).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/not the one requested/) });
  });

  it('rejects a create response with a different channel code', async () => {
    answer(channel({ code: 'web' }), 201);
    const error = await createChannel(ASSERTION, ORGANIZER, {
      code: 'pos', displayName: 'Box office', kind: 'pos', enabled: true,
    }).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/code/) });
  });

  it('rejects a created channel attributed to another organizer', async () => {
    answer(channel({ organizer_id: OTHER_ORGANIZER }), 201);
    const error = await createChannel(ASSERTION, ORGANIZER, {
      code: 'pos', displayName: 'Box office', kind: 'pos', enabled: true,
    }).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/organizer_id/) });
  });

  it('rejects an updated channel attributed to another organizer', async () => {
    answer(channel({ organizer_id: OTHER_ORGANIZER }));
    const error = await updateChannel(ASSERTION, ORGANIZER, CHANNEL, {
      code: 'pos', displayName: 'Box office', kind: 'pos', enabled: true,
    }).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/organizer_id/) });
  });

  it('rejects a created event attributed to another organizer', async () => {
    answer({
      id: EVENT,
      organizer_id: OTHER_ORGANIZER,
      name: { en: 'Night' },
      created_at: '2026-09-01T10:00:00Z',
    }, 201);
    const error = await createEvent(ASSERTION, ORGANIZER, { en: 'Night' }, 'key')
      .catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/organizer_id/) });
  });

  it('rejects a ticket type attributed to another organizer', async () => {
    answer({
      id: TICKET_TYPE,
      organizer_id: OTHER_ORGANIZER,
      performance_id: PERFORMANCE,
      name: { en: 'GA' },
      price: { amount: 100, currency: 'EUR' },
      created_at: '2026-09-01T10:00:00Z',
    }, 201);
    const error = await createTicketType(
      ASSERTION,
      ORGANIZER,
      { performanceId: PERFORMANCE, name: { en: 'GA' }, amount: 100, currency: 'EUR' },
      'key',
    ).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/organizer_id/) });
  });

  it('rejects a ticket type attributed to another performance', async () => {
    answer({
      id: TICKET_TYPE,
      organizer_id: ORGANIZER,
      performance_id: OTHER_PERFORMANCE,
      name: { en: 'GA' },
      price: { amount: 100, currency: 'EUR' },
      created_at: '2026-09-01T10:00:00Z',
    }, 201);
    const error = await createTicketType(
      ASSERTION,
      ORGANIZER,
      { performanceId: PERFORMANCE, name: { en: 'GA' }, amount: 100, currency: 'EUR' },
      'key',
    ).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(AmbiguousMutationError);
    expect((error as Error).cause).toMatchObject({ message: expect.stringMatching(/performance_id/) });
  });

  it.each([
    ['venue capacity', () => getVenues(ORGANIZER), { venues: [venue({ ga_capacity: MAX_INT32 + 1 })] }],
    [
      'seat-map version',
      () => getSeatMapGeometry(MAP),
      { map: seatMap({ version: MAX_INT32 + 1 }), sections: [] },
    ],
    [
      'section position',
      () => getSeatMapGeometry(MAP),
      {
        map: seatMap(),
        sections: [{ id: 'b0000000-0000-4000-8000-000000000001', name: 'Floor', position: MAX_INT32 + 1 }],
      },
    ],
    [
      'row position',
      () => getSeatMapGeometry(MAP),
      {
        map: seatMap(),
        sections: [{
          id: 'b0000000-0000-4000-8000-000000000001',
          name: 'Floor',
          position: 1,
          rows: [{ id: 'c0000000-0000-4000-8000-000000000001', label: 'A', position: MAX_INT32 + 1 }],
        }],
      },
    ],
    [
      'seat position',
      () => getSeatMapGeometry(MAP),
      {
        map: seatMap(),
        sections: [{
          id: 'b0000000-0000-4000-8000-000000000001',
          name: 'Floor',
          position: 1,
          rows: [{
            id: 'c0000000-0000-4000-8000-000000000001',
            label: 'A',
            position: 1,
            seats: [{
              id: 'd0000000-0000-4000-8000-000000000001',
              seat_identity: 'Floor/A/1',
              label: '1',
              position: MAX_INT32 + 1,
            }],
          }],
        }],
      },
    ],
  ])('rejects %s above the catalog int32 maximum', async (_name, read, body) => {
    answer(body);
    await expect(read()).rejects.toThrow(/int32 maximum/);
  });

  it('rejects a re-entry maximum above int32', async () => {
    answer(performance({
      re_entry: { mode: 'count_limited', max_entries: MAX_INT32 + 1, requires_exit: true },
    }), 201);
    await expect(createPerformance(
      ASSERTION,
      ORGANIZER,
      { eventId: EVENT, venueId: VENUE, startsAt: '2026-09-01T20:00:00Z', timezone: 'America/Toronto' },
      'key',
    )).rejects.toBeInstanceOf(AmbiguousMutationError);
  });

  it('rejects a composed seat identity longer than 200 Unicode characters', async () => {
    const geometry = (seatIdentity: string) => ({
      map: seatMap(),
      sections: [{
        id: 'b0000000-0000-4000-8000-000000000001',
        name: 'Floor',
        position: 1,
        rows: [{
          id: 'c0000000-0000-4000-8000-000000000001',
          label: 'A',
          position: 1,
          seats: [{
            id: 'd0000000-0000-4000-8000-000000000001',
            seat_identity: seatIdentity,
            label: '1',
            position: 1,
          }],
        }],
      }],
    });

    answer(geometry('🎟'.repeat(201)));
    await expect(getSeatMapGeometry(MAP)).rejects.toThrow(/longer than 200 characters/);
  });

  it('enforces the channel-code maximum in Unicode characters', async () => {
    answer({ channels: [channel({ code: '🎫'.repeat(100) })] });
    await expect(listChannelsForOperator(ASSERTION, ORGANIZER)).resolves.toBeDefined();

    answer({ channels: [channel({ code: '🎫'.repeat(101) })] });
    await expect(listChannelsForOperator(ASSERTION, ORGANIZER)).rejects.toThrow(/100 characters/);
  });

  it('rejects any listed channel attributed to another organizer', async () => {
    answer({ channels: [channel(), channel({ organizer_id: OTHER_ORGANIZER })] });
    await expect(listChannelsForOperator(ASSERTION, ORGANIZER)).rejects.toThrow(/organizer_id/);
  });
});

describe('inventory response decoding', () => {
  it.each([
    ['an absent counter', staffAvailability({ confirmed: undefined })],
    ['the wrong collection type', staffAvailability({ channels: {} })],
    ['another slot identity', staffAvailability({ slot_id: OTHER_SLOT })],
    ['a negative counter', staffAvailability({ available: -1 })],
    ['an unknown offering status', staffAvailability({ offering_status: 'paused' })],
    ['an invalid allocation revision', staffAvailability({ allocation_revision: -1 })],
    ['a malformed nested date', staffAvailability({ channels: [{
      channel: 'presale', cap: 10, released: false, window_open: true,
      held: 0, confirmed: 0, available: 10, opens_at: 'tomorrow',
    }] })],
    ['a malformed reseller id', staffAvailability({ channels: [{
      channel: 'reseller', cap: 10, released: false, window_open: true,
      held: 0, confirmed: 0, available: 10, sold_by: 'reseller',
    }] })],
  ])('rejects staff availability with %s', async (_name, body) => {
    answer(body);
    await expect(getStaffAvailability(SLOT, ORGANIZER)).rejects.toThrow();
  });

  const allocation = {
    channel: 'presale', cap: 10, requires_code: false,
    opens_at: '2026-09-01T10:00:00Z',
  };

  it.each([
    ['another slot identity', { slot_id: OTHER_SLOT, allocations: [allocation] }],
    ['the wrong collection type', { slot_id: SLOT, allocations: {} }],
    ['a zero cap', { slot_id: SLOT, allocations: [{ ...allocation, cap: 0 }] }],
    ['an invalid date', { slot_id: SLOT, allocations: [{ ...allocation, opens_at: 'later' }] }],
    ['a malformed reseller id', { slot_id: SLOT, allocations: [{ ...allocation, sold_by: 'reseller' }] }],
  ])('rejects allocation results with %s', async (_name, body) => {
    answer(body);
    await expect(replaceChannelAllocations(SLOT, {
      organizer_id: ORGANIZER,
      allocation_revision: 1,
      allocations: [],
    })).rejects.toThrow();
  });

  it('applies the contract default when requires_code is absent', async () => {
    answer({
      slot_id: SLOT,
      allocations: [{ channel: 'presale', cap: 10 }],
    });
    await expect(replaceChannelAllocations(SLOT, {
      organizer_id: ORGANIZER,
      allocation_revision: 1,
      allocations: [],
    })).resolves.toEqual({
      slot_id: SLOT,
      allocations: [{ channel: 'presale', cap: 10, requires_code: false }],
    });
  });
});

describe('commerce and access response decoding', () => {
  it('rejects a malformed order id before treating its status as real', async () => {
    answer({ order_id: 'order', status: 'completed' });
    await expect(getOrderState(ORDER)).resolves.toEqual({ ok: false, kind: 'unavailable' });
  });

  it.each([
    ['a malformed refund id', refund({ refund_id: 'refund' })],
    ['another order identity', refund({ order_id: OTHER_ORDER })],
    ['a zero quantity', refund({ quantity: 0 })],
    ['a negative amount', refund({ amount: -1 })],
    ['a negative cumulative count', refund({ refunded_quantity: -1 })],
    ['a malformed currency', refund({ currency: 'eur' })],
    ['an unknown status', refund({ refund_status: 'reversed' })],
    ['an absent boolean', refund({ replay: undefined })],
  ])('treats a refund with %s as ambiguous', async (_name, body) => {
    answer(body);
    await expect(refundOrder({
      orderId: ORDER,
      quantity: 1,
      reason: 'requested',
      actor: STAFF,
      organizerId: ORGANIZER,
      idempotencyKey: 'key',
    })).resolves.toMatchObject({ ok: false, kind: 'ambiguous' });
  });

  it.each([
    ['a malformed order id', { order_id: 'order', ticket_count: 1, replay: false }],
    ['another order identity', { order_id: OTHER_ORDER, ticket_count: 1, replay: false }],
    ['a fractional count', { order_id: ORDER, ticket_count: 1.5, replay: false }],
    ['a zero count', { order_id: ORDER, ticket_count: 0, replay: false }],
    ['a count above the contract maximum', { order_id: ORDER, ticket_count: 51, replay: false }],
    ['an absent flag', { order_id: ORDER, ticket_count: 1 }],
  ])('treats a redelivery with %s as ambiguous', async (_name, body) => {
    answer(body);
    await expect(redeliverOrderTickets({
      orderId: ORDER,
      organizerId: ORGANIZER,
      idempotencyKey: 'key',
    })).resolves.toMatchObject({ ok: false, kind: 'ambiguous' });
  });
});

describe('literal contract edges remain decodable', () => {
  it('accepts the smallest persisted catalog values', async () => {
    answer({ venues: [venue({ ga_capacity: 1 })] });
    await expect(getVenues(ORGANIZER)).resolves.toMatchObject([{ ga_capacity: 1 }]);

    answer({
      map: seatMap({ version: 1 }),
      sections: [{
        id: 'b0000000-0000-4000-8000-000000000001',
        name: 'Floor',
        position: 1,
        rows: [{
          id: 'c0000000-0000-4000-8000-000000000001',
          label: 'A',
          position: 1,
          seats: [{
            id: 'd0000000-0000-4000-8000-000000000001',
            seat_identity: 'Floor/A/1',
            label: '1',
            position: 1,
          }],
        }],
      }],
    });
    await expect(getSeatMapGeometry(MAP)).resolves.toMatchObject({
      map: { version: 1 },
      sections: [{ position: 1, rows: [{ position: 1, seats: [{ position: 1 }] }] }],
    });
  });

  it('accepts catalog int32 maxima and a 200-character seat identity', async () => {
    answer({ venues: [venue({ ga_capacity: MAX_INT32 })] });
    await expect(getVenues(ORGANIZER)).resolves.toMatchObject([{ ga_capacity: MAX_INT32 }]);

    answer({
      map: seatMap({ version: MAX_INT32 }),
      sections: [{
        id: 'b0000000-0000-4000-8000-000000000001',
        name: 'Floor',
        position: MAX_INT32,
        rows: [{
          id: 'c0000000-0000-4000-8000-000000000001',
          label: 'A',
          position: MAX_INT32,
          seats: [{
            id: 'd0000000-0000-4000-8000-000000000001',
            seat_identity: '🎟'.repeat(200),
            label: '1',
            position: MAX_INT32,
          }],
        }],
      }],
    });
    await expect(getSeatMapGeometry(MAP)).resolves.toMatchObject({
      map: { version: MAX_INT32 },
      sections: [{ position: MAX_INT32, rows: [{ position: MAX_INT32, seats: [{ position: MAX_INT32 }] }] }],
    });

    answer(performance({
      re_entry: { mode: 'count_limited', max_entries: MAX_INT32, requires_exit: true },
    }), 201);
    await expect(createPerformance(
      ASSERTION,
      ORGANIZER,
      { eventId: EVENT, venueId: VENUE, startsAt: '2026-09-01T20:00:00Z', timezone: 'America/Toronto' },
      'key',
    )).resolves.toMatchObject({ re_entry: { max_entries: MAX_INT32 } });
  });

  it('accepts zero counters, a cap of one, and the largest exact revision', async () => {
    answer(staffAvailability({
      capacity: 1,
      buyer_held: 0,
      operational_held: 0,
      reservation_held: 0,
      confirmed: 0,
      available: 0,
      public_available: 0,
      allocation_revision: Number.MAX_SAFE_INTEGER,
      channels: [{
        channel: 'presale',
        cap: 1,
        released: false,
        window_open: true,
        held: 0,
        confirmed: 0,
        available: 0,
      }],
    }));
    await expect(getStaffAvailability(SLOT, ORGANIZER)).resolves.toMatchObject({
      capacity: 1,
      allocation_revision: Number.MAX_SAFE_INTEGER,
      channels: [{ cap: 1, held: 0, confirmed: 0, available: 0 }],
    });
  });

  it('accepts the documented refund and redelivery limits', async () => {
    answer(refund({
      quantity: 50,
      amount: Number.MAX_SAFE_INTEGER,
      refunded_quantity: 50,
      refunded_amount: Number.MAX_SAFE_INTEGER,
    }));
    await expect(refundOrder({
      orderId: ORDER,
      quantity: 50,
      reason: 'requested',
      actor: STAFF,
      organizerId: ORGANIZER,
      idempotencyKey: 'key',
    })).resolves.toMatchObject({
      ok: true,
      value: {
        quantity: 50,
        amount: Number.MAX_SAFE_INTEGER,
        refundedQuantity: 50,
        refundedAmount: Number.MAX_SAFE_INTEGER,
      },
    });

    answer({ order_id: ORDER, ticket_count: 50, replay: false });
    await expect(redeliverOrderTickets({
      orderId: ORDER,
      organizerId: ORGANIZER,
      idempotencyKey: 'key',
    })).resolves.toMatchObject({ ok: true, value: { ticketCount: 50 } });
  });

  it('accepts the largest exact lifecycle sequence', async () => {
    answer({
      order_ref: ORDER,
      tickets: [{
        ticket_id: 'e0000000-0000-4000-8000-000000000001',
        issued_at: '2026-09-01T10:00:00Z',
        history: [{
          id: 'f0000000-0000-4000-8000-000000000001',
          type: 'issued',
          sequence: Number.MAX_SAFE_INTEGER,
          occurred_at: '2026-09-01T10:00:00Z',
        }],
      }],
    });
    await expect(getOrderTickets(ORDER)).resolves.toMatchObject({
      ok: true,
      value: [{ history: [{ sequence: Number.MAX_SAFE_INTEGER }] }],
    });
  });
});
