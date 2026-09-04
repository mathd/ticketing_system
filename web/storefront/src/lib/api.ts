// Catalog reads, through the gateway (the storefront consumes the public
// contract, ADR-002). Types are generated from the catalog's OpenAPI
// document (ADR-009) — regenerate with `make generate`.
import type { AstroGlobal } from 'astro';

import type { components } from './api-types.gen';
import { PageDataCache } from './cache';
import type { Locale } from './locales';
import { dateTimeField, sameUuid, uuidField } from './wire-primitives';

export type PublicEventList = components['schemas']['PublicEventList'];
export type PublicEventDetail = components['schemas']['PublicEventDetail'];
export type PublicFestivalDetail = components['schemas']['PublicFestivalDetail'];
// The seat picker (TKT-174) reads geometry browser-side, not through pageRead —
// it is the hours tier, and the SSR cache owns the minutes tier (ADR-004/ADR-006).
export type SeatMapGeometry = components['schemas']['SeatMapGeometry'];
export type SeatMapSection = NonNullable<SeatMapGeometry['sections']>[number];
export type SeatMapRow = NonNullable<SeatMapSection['rows']>[number];
export type SeatMapSeat = NonNullable<SeatMapRow['seats']>[number];

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';
const CURRENCY = /^[A-Z]{3}$/;

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function objectBody(value: unknown, name: string): Record<string, unknown> {
  if (!isObject(value)) throw new TypeError(`${name} must be an object`);
  return value;
}

function stringField(value: unknown, name: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${name} must be a non-empty string`);
  }
  return value;
}

function optionalString(value: unknown, name: string): string | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== 'string') throw new TypeError(`${name} must be a string`);
  return value;
}

function moneyField(value: unknown, name: string): components['schemas']['Money'] {
  const money = objectBody(value, name);
  if (typeof money.amount !== 'number' || !Number.isSafeInteger(money.amount) || money.amount < 0) {
    throw new TypeError(`${name}.amount must be a non-negative integer`);
  }
  const currency = stringField(money.currency, `${name}.currency`);
  if (!CURRENCY.test(currency)) throw new TypeError(`${name}.currency must be an ISO code`);
  return { amount: money.amount, currency };
}

function arrayField<T>(
  value: unknown,
  name: string,
  decode: (item: unknown, index: number) => T,
): T[] {
  if (!Array.isArray(value)) throw new TypeError(`${name} must be an array`);
  return value.map(decode);
}

function uuidArray(value: unknown, name: string): string[] {
  const result = arrayField(value, name, (item, index) => uuidField(item, `${name}[${index}]`));
  if (new Set(result).size !== result.length) throw new TypeError(`${name} must not contain duplicates`);
  return result;
}

function decodePublicVenue(value: unknown, name: string): components['schemas']['PublicVenue'] {
  const venue = objectBody(value, name);
  return {
    id: uuidField(venue.id, `${name}.id`),
    name: stringField(venue.name, `${name}.name`),
  };
}

function decodePublicTicketType(
  value: unknown,
  index: number,
): components['schemas']['PublicTicketType'] {
  const name = `ticket_types[${index}]`;
  const ticketType = objectBody(value, name);
  return {
    id: uuidField(ticketType.id, `${name}.id`),
    name: stringField(ticketType.name, `${name}.name`),
    price: moneyField(ticketType.price, `${name}.price`),
  };
}

function decodePublicPerformanceDetail(
  value: unknown,
  index: number,
): components['schemas']['PublicPerformanceDetail'] {
  const name = `performances[${index}]`;
  const performance = objectBody(value, name);
  const seatMapId = performance.seat_map_id;
  return {
    id: uuidField(performance.id, `${name}.id`),
    starts_at: dateTimeField(performance.starts_at, `${name}.starts_at`),
    timezone: stringField(performance.timezone, `${name}.timezone`),
    venue: decodePublicVenue(performance.venue, `${name}.venue`),
    ...(seatMapId === undefined ? {} : { seat_map_id: uuidField(seatMapId, `${name}.seat_map_id`) }),
    ticket_types: arrayField(performance.ticket_types, `${name}.ticket_types`, decodePublicTicketType),
  };
}

function decodePublicPerformanceSummary(
  value: unknown,
  index: number,
): components['schemas']['PublicPerformanceSummary'] {
  const name = `performances[${index}]`;
  const performance = objectBody(value, name);
  return {
    id: uuidField(performance.id, `${name}.id`),
    starts_at: dateTimeField(performance.starts_at, `${name}.starts_at`),
    timezone: stringField(performance.timezone, `${name}.timezone`),
    venue_name: stringField(performance.venue_name, `${name}.venue_name`),
    from_price: moneyField(performance.from_price, `${name}.from_price`),
  };
}

function decodePublicEvents(value: unknown): PublicEventList {
  const body = objectBody(value, 'public event list');
  return {
    events: arrayField(body.events, 'events', (value, index) => {
      const name = `events[${index}]`;
      const event = objectBody(value, name);
      const description = optionalString(event.description, `${name}.description`);
      return {
        id: uuidField(event.id, `${name}.id`),
        name: stringField(event.name, `${name}.name`),
        ...(description === undefined ? {} : { description }),
        performances: arrayField(
          event.performances,
          `${name}.performances`,
          decodePublicPerformanceSummary,
        ),
      };
    }),
  };
}

function decodePublicEvent(value: unknown, expectedId: string): PublicEventDetail {
  const body = objectBody(value, 'public event');
  const id = uuidField(body.id, 'id');
  if (!sameUuid(id, expectedId)) throw new TypeError('event id does not match the request');
  const description = optionalString(body.description, 'description');
  const performances = arrayField(body.performances, 'performances', decodePublicPerformanceDetail);
  const performanceIds = new Set<string>();
  for (const performance of performances) {
    if (performanceIds.has(performance.id)) {
      throw new TypeError('performances must not contain duplicate identities');
    }
    performanceIds.add(performance.id);
  }
  const seriesIds = new Set<string>();
  const groupedPerformanceIds = new Set<string>();
  const series = arrayField(body.series, 'series', (value, index) => {
    const name = `series[${index}]`;
    const item = objectBody(value, name);
    const seriesId = uuidField(item.id, `${name}.id`);
    if (seriesIds.has(seriesId)) throw new TypeError('series must not contain duplicate identities');
    seriesIds.add(seriesId);
    const members = uuidArray(item.performance_ids, `${name}.performance_ids`);
    for (const performanceId of members) {
      if (!performanceIds.has(performanceId)) {
        throw new TypeError(`${name}.performance_ids must reference a returned performance`);
      }
      if (groupedPerformanceIds.has(performanceId)) {
        throw new TypeError('a performance must not belong to more than one series');
      }
      groupedPerformanceIds.add(performanceId);
    }
    return {
      id: seriesId,
      name: stringField(item.name, `${name}.name`),
      performance_ids: members,
    };
  });
  return {
    id,
    organizer_id: uuidField(body.organizer_id, 'organizer_id'),
    name: stringField(body.name, 'name'),
    ...(description === undefined ? {} : { description }),
    series,
    performances,
  };
}

function decodePublicFestival(value: unknown, expectedId: string): PublicFestivalDetail {
  const body = objectBody(value, 'public festival');
  const id = uuidField(body.id, 'id');
  if (!sameUuid(id, expectedId)) throw new TypeError('festival id does not match the request');
  return {
    id,
    organizer_id: uuidField(body.organizer_id, 'organizer_id'),
    name: stringField(body.name, 'name'),
    days: arrayField(body.days, 'days', decodePublicPerformanceDetail),
  };
}

// Module-level singleton: the SSR process holds ONE page-data cache — the
// single cache owner of the minutes tier (ADR-006 ownership rule).
const cache = new PageDataCache();

async function pageRead<T>(
  astro: AstroGlobal,
  path: string,
  decode: (value: unknown) => T,
): Promise<T> {
  const result = await cache.get(`${GATEWAY_URL}/api/catalog${path}`, decode);
  // The middleware turns this into the page's Cache-Control: the outgoing
  // TTL is the REMAINING data freshness, so staleness never stacks.
  astro.locals.pageData = { ageSeconds: result.ageSeconds, maxAgeSeconds: result.maxAgeSeconds };
  return result.data;
}

/** The list page's single aggregated call (ADR-004 rule 3). */
export function getPublicEvents(astro: AstroGlobal, locale: Locale): Promise<PublicEventList> {
  return pageRead(astro, `/public/events?locale=${locale}`, decodePublicEvents);
}

/** The detail page's single aggregated call. */
export function getPublicEvent(
  astro: AstroGlobal,
  locale: Locale,
  eventId: string,
): Promise<PublicEventDetail> {
  return pageRead(
    astro,
    `/public/events/${encodeURIComponent(eventId)}?locale=${locale}`,
    (value) => decodePublicEvent(value, eventId),
  );
}

/**
 * The festival page's single aggregated call: a festival and its days as one
 * grouped offer (US-011). Same minutes tier as the event reads (ADR-004).
 */
export function getPublicFestival(
  astro: AstroGlobal,
  locale: Locale,
  festivalId: string,
): Promise<PublicFestivalDetail> {
  return pageRead(
    astro,
    `/public/festivals/${encodeURIComponent(festivalId)}?locale=${locale}`,
    (value) => decodePublicFestival(value, festivalId),
  );
}
