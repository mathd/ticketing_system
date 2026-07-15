// Catalog reads, through the gateway (the storefront consumes the public
// contract, ADR-002). Types are generated from the catalog's OpenAPI
// document (ADR-009) — regenerate with `make generate`.
import type { AstroGlobal } from 'astro';

import type { components } from './api-types.gen';
import { PageDataCache } from './cache';
import type { Locale } from './locales';

export type PublicEventList = components['schemas']['PublicEventList'];
export type PublicEventSummary = components['schemas']['PublicEventSummary'];
export type PublicEventDetail = components['schemas']['PublicEventDetail'];
export type PublicFestivalDetail = components['schemas']['PublicFestivalDetail'];

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

// Module-level singleton: the SSR process holds ONE page-data cache — the
// single cache owner of the minutes tier (ADR-006 ownership rule).
const cache = new PageDataCache();

async function pageRead<T>(astro: AstroGlobal, path: string): Promise<T> {
  const result = await cache.get<T>(`${GATEWAY_URL}/api/catalog${path}`);
  // The middleware turns this into the page's Cache-Control: the outgoing
  // TTL is the REMAINING data freshness, so staleness never stacks.
  astro.locals.pageData = { ageSeconds: result.ageSeconds, maxAgeSeconds: result.maxAgeSeconds };
  return result.data;
}

/** The list page's single aggregated call (ADR-004 rule 3). */
export function getPublicEvents(astro: AstroGlobal, locale: Locale): Promise<PublicEventList> {
  return pageRead<PublicEventList>(astro, `/public/events?locale=${locale}`);
}

/** The detail page's single aggregated call. */
export function getPublicEvent(
  astro: AstroGlobal,
  locale: Locale,
  eventId: string,
): Promise<PublicEventDetail> {
  return pageRead<PublicEventDetail>(
    astro,
    `/public/events/${encodeURIComponent(eventId)}?locale=${locale}`,
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
  return pageRead<PublicFestivalDetail>(
    astro,
    `/public/festivals/${encodeURIComponent(festivalId)}?locale=${locale}`,
  );
}
