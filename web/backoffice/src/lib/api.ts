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
export type SeatMapEdit = components['schemas']['SeatMapEdit'];
export type SeatMapVersionHistory = components['schemas']['SeatMapVersionHistory'];

// CatalogApiError carries the catalog's parsed { error } message and HTTP status
// (emulating the storefront's UpstreamError) so the UI can show an actionable
// message — e.g. the TKT-104 pinning-contract rejection — instead of a bare
// status code.
export class CatalogApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = 'CatalogApiError';
  }
}

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

// parseError pulls the catalog's { error } message off a non-2xx response so the
// UI can surface it; falls back to a generic line if the body is not the shared
// Error shape. Never throws while reading the body.
async function parseError(res: Response): Promise<CatalogApiError> {
  let message = `catalog request failed: ${res.status}`;
  try {
    const body = (await res.json()) as { error?: unknown };
    if (body && typeof body.error === 'string' && body.error) {
      message = body.error;
    }
  } catch {
    // non-JSON body — keep the generic message
  }
  return new CatalogApiError(res.status, message);
}

async function postCatalog<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(catalog(path), {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw await parseError(res);
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

// --- Publishing, safe-editing, versioning, GA config (US-020/021/022, TKT-105).
// Editing surfaces the TKT-104 store contract over the new catalog endpoint; the
// UI adds no domain logic. ---

/** Publish a draft map (TKT-103). Idempotent server-side. */
export function publishSeatMap(seatMapId: string): Promise<SeatMap> {
  return postCatalog<SeatMap>(`/seat-maps/${encodeURIComponent(seatMapId)}/publish`, null);
}

/**
 * Edit a published map, producing a new version (TKT-105). `edit` is the FULL
 * replacement geometry; seatMapId may be any version of the family. A rejected
 * edit (would orphan a pinned seat) throws a CatalogApiError carrying the
 * actionable {error} message and 409 status.
 */
export function editSeatMap(seatMapId: string, edit: SeatMapEdit): Promise<SeatMap> {
  return postCatalog<SeatMap>(`/seat-maps/${encodeURIComponent(seatMapId)}/edit`, edit);
}

/** A seat-map family's version history (hours tier); resolves from any version id. */
export async function listSeatMapVersions(seatMapId: string): Promise<SeatMapVersionHistory> {
  const res = await fetch(catalog(`/public/seat-maps/${encodeURIComponent(seatMapId)}/versions`));
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as SeatMapVersionHistory;
}

/** Set a venue's GA capacity (TKT-105 COS-5). */
export function updateVenueGaCapacity(
  venueId: string,
  gaCapacity: number,
  organizerId = DEFAULT_ORGANIZER_ID,
): Promise<Venue> {
  return postCatalog<Venue>(`/venues/${encodeURIComponent(venueId)}/ga-capacity`, {
    organizer_id: organizerId,
    ga_capacity: gaCapacity,
  });
}
