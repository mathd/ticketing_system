// Catalog reads, through the gateway (the back-office consumes the public
// contract, ADR-002/ADR-009). Types are generated from the catalog's OpenAPI
// document — regenerate with `make generate`.
import type { components } from './api-types.gen';

export type Venue = components['schemas']['Venue'];
export type PublicVenueList = components['schemas']['PublicVenueList'];

const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8080';

// v1 has a single organizer (US-002 AC5); the fixed UUID is seeded by the
// catalog migration (0002). When admin auth lands this comes from the session.
export const DEFAULT_ORGANIZER_ID = '00000000-0000-0000-0000-000000000001';

/** The venue list page's single read: the organizer's venues (hours tier). */
export async function getVenues(organizerId = DEFAULT_ORGANIZER_ID): Promise<Venue[]> {
  const url = `${GATEWAY_URL}/api/catalog/public/venues?organizer_id=${encodeURIComponent(organizerId)}`;
  const res = await fetch(url);
  if (!res.ok) {
    throw new Error(`catalog venue read failed: ${res.status}`);
  }
  const body = (await res.json()) as PublicVenueList;
  return body.venues;
}
