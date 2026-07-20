// Catalog reads, through the gateway (the back-office consumes the public
// contract, ADR-002/ADR-009). Types are generated from the catalog's OpenAPI
// document — regenerate with `make generate`.
import type { components } from './api-types.gen';

export type Venue = components['schemas']['Venue'];
export type PublicVenueList = components['schemas']['PublicVenueList'];
export type SeatMap = components['schemas']['SeatMap'];
export type SeatMapList = components['schemas']['SeatMapList'];
export type SeatMapGeometry = components['schemas']['SeatMapGeometry'];
export type SeatSection = components['schemas']['SeatSection'];
export type SeatRow = components['schemas']['SeatRow'];
export type Seat = components['schemas']['Seat'];

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

/** One venue by id (v1 has no single-venue read; filter the organizer list). */
export async function getVenue(
  venueId: string,
  organizerId = DEFAULT_ORGANIZER_ID,
): Promise<Venue | undefined> {
  const venues = await getVenues(organizerId);
  return venues.find((v) => v.id === venueId);
}

const catalog = (path: string) => `${GATEWAY_URL}/api/catalog${path}`;

async function postCatalog<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(catalog(path), {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw new Error(`catalog write failed: ${res.status}`);
  }
  return (await res.json()) as T;
}

// --- Seat-map authoring (US-019). Writes go through the gateway server-side,
// same posture as the reads; the browser never talks to the gateway directly. ---

export function createSeatMap(
  venueId: string,
  name: string,
  organizerId = DEFAULT_ORGANIZER_ID,
): Promise<SeatMap> {
  return postCatalog<SeatMap>(`/venues/${encodeURIComponent(venueId)}/seat-maps`, {
    organizer_id: organizerId,
    name,
  });
}

export function addSeatMapSection(
  seatMapId: string,
  section: { name: string; position: number },
  organizerId = DEFAULT_ORGANIZER_ID,
): Promise<SeatSection> {
  return postCatalog<SeatSection>(`/seat-maps/${encodeURIComponent(seatMapId)}/sections`, {
    organizer_id: organizerId,
    ...section,
  });
}

export function addSeatMapRow(
  seatMapId: string,
  row: { section_id: string; label: string; position: number },
  organizerId = DEFAULT_ORGANIZER_ID,
): Promise<SeatRow> {
  return postCatalog<SeatRow>(`/seat-maps/${encodeURIComponent(seatMapId)}/rows`, {
    organizer_id: organizerId,
    ...row,
  });
}

export function addSeatMapSeat(
  seatMapId: string,
  seat: { row_id: string; label: string; position: number },
  organizerId = DEFAULT_ORGANIZER_ID,
): Promise<Seat> {
  return postCatalog<Seat>(`/seat-maps/${encodeURIComponent(seatMapId)}/seats`, {
    organizer_id: organizerId,
    ...seat,
  });
}

/** A venue's seat-map summaries (hours tier), for the venue page. */
export async function listVenueSeatMaps(venueId: string): Promise<SeatMap[]> {
  const res = await fetch(catalog(`/public/venues/${encodeURIComponent(venueId)}/seat-maps`));
  if (!res.ok) {
    throw new Error(`seat-map list failed: ${res.status}`);
  }
  return ((await res.json()) as SeatMapList).seat_maps;
}

/** One seat map's full geometry (hours tier). */
export async function getSeatMapGeometry(seatMapId: string): Promise<SeatMapGeometry> {
  const res = await fetch(catalog(`/public/seat-maps/${encodeURIComponent(seatMapId)}`));
  if (!res.ok) {
    throw new Error(`seat-map geometry read failed: ${res.status}`);
  }
  return (await res.json()) as SeatMapGeometry;
}
