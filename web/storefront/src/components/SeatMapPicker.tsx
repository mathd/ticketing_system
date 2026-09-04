import { useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';
import type { Ref } from 'react';

import { parseMaxAge } from '../lib/cache';
import type { SeatMapGeometry, SeatMapRow, SeatMapSeat, SeatMapSection } from '../lib/api';
import type { Locale } from '../lib/locales';
import { UI_STRINGS } from '../lib/locales';
import { dateTimeField, sameUuid, uuidField } from '../lib/wire-primitives';

// SeatMapPicker is TKT-174's seat concern, end to end: it reads the published
// geometry and the live occupancy, renders the map, owns the selection, and reports
// both the selection AND whether that selection is currently claimable. It
// deliberately does NOT own the reservation or the checkout — HoldPicker keeps those,
// so there is one reservation state and one checkout path (AC3).
//
// Reporting claimability rather than just identities is load-bearing: the Reserve
// button lives in the PARENT, so a child that merely stops rendering its own controls
// still leaves a purchase control enabled over a selection nothing can vouch for
// (ai-review). Fail-closed has to cross the component boundary or it is not closed.
//
// Both reads happen browser-side, and that is the decided architecture rather than a
// convenience: ADR-006's accepted option is "page HTML at the minutes tier with
// availability as a seconds-tier React island", and it names this ticket. Occupancy
// must never go through the SSR PageDataCache, which owns the minutes tier — routing
// it there would freeze the map for minutes during an on-sale. And because occupancy
// is structurally absent from the cached HTML, the event page's single-source rule is
// intact: there is no cached copy of this data to go stale against.

type Section = SeatMapSection;

/** The seat-occupancy read (TKT-172). `remaining_capacity` is a CEILING, not a seat count. */
interface Occupancy {
  slot_id: string;
  seat_map_id: string;
  offering_status: 'open' | 'closed' | 'archived';
  remaining_capacity: number;
  unavailable_seat_identities: string[];
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isCallerCancellation(value: unknown): boolean {
  return value instanceof Error &&
    value.name === 'AbortError' &&
    !('timedOut' in value && value.timedOut === true);
}

function objectBody(value: unknown, name: string): Record<string, unknown> {
  if (!isObject(value)) throw new TypeError(`${name} must be an object`);
  return value;
}

function stringField(value: unknown, name: string, maximumLength?: number): string {
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    (maximumLength !== undefined && [...value].length > maximumLength)
  ) {
    throw new TypeError(`${name} must be a non-empty string${maximumLength ? ` of at most ${maximumLength} characters` : ''}`);
  }
  return value;
}

function integerField(value: unknown, name: string, minimum: number): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) {
    throw new TypeError(`${name} must be an integer of at least ${minimum}`);
  }
  return value;
}

function stringArray(value: unknown, name: string, maximumLength: number): string[] {
  if (!Array.isArray(value)) throw new TypeError(`${name} must be an array`);
  const result = value.map((item, index) => stringField(item, `${name}[${index}]`, maximumLength));
  if (new Set(result).size !== result.length) throw new TypeError(`${name} must not contain duplicates`);
  return result;
}

function decodeOccupancy(value: unknown): Occupancy {
  const body = objectBody(value, 'seat occupancy');
  const status = body.offering_status;
  if (status !== 'open' && status !== 'closed' && status !== 'archived') {
    throw new TypeError('offering_status is invalid');
  }
  return {
    slot_id: uuidField(body.slot_id, 'slot_id'),
    seat_map_id: uuidField(body.seat_map_id, 'seat_map_id'),
    offering_status: status,
    remaining_capacity: integerField(body.remaining_capacity, 'remaining_capacity', 0),
    unavailable_seat_identities: stringArray(
      body.unavailable_seat_identities,
      'unavailable_seat_identities',
      200,
    ),
  };
}

function decodeSeatMapGeometry(
  value: unknown,
  expectedMapId: string,
  expectedOrganizerId: string,
): SeatMapGeometry {
  const body = objectBody(value, 'seat map geometry');
  const map = objectBody(body.map, 'map');
  const mapId = uuidField(map.id, 'map.id');
  const organizerId = uuidField(map.organizer_id, 'map.organizer_id');
  if (!sameUuid(mapId, expectedMapId) || !sameUuid(organizerId, expectedOrganizerId)) {
    throw new TypeError('seat map identity does not match the request');
  }
  const status = map.status;
  if (status !== 'draft' && status !== 'published' && status !== 'archived') {
    throw new TypeError('map.status is invalid');
  }
  if (typeof map.orphan_prevention_enabled !== 'boolean') {
    throw new TypeError('map.orphan_prevention_enabled must be boolean');
  }
  if (!Array.isArray(body.sections)) throw new TypeError('sections must be an array');
  const identities = new Set<string>();
  const sections = body.sections.map((value, sectionIndex): SeatMapSection => {
    const section = objectBody(value, `sections[${sectionIndex}]`);
    let rows: SeatMapRow[] | undefined;
    if (section.rows !== undefined) {
      if (!Array.isArray(section.rows)) throw new TypeError(`sections[${sectionIndex}].rows must be an array`);
      rows = section.rows.map((value, rowIndex): SeatMapRow => {
        const row = objectBody(value, `sections[${sectionIndex}].rows[${rowIndex}]`);
        let seats: SeatMapSeat[] | undefined;
        if (row.seats !== undefined) {
          if (!Array.isArray(row.seats)) {
            throw new TypeError(`sections[${sectionIndex}].rows[${rowIndex}].seats must be an array`);
          }
          seats = row.seats.map((value, seatIndex): SeatMapSeat => {
            const path = `sections[${sectionIndex}].rows[${rowIndex}].seats[${seatIndex}]`;
            const seat = objectBody(value, path);
            const identity = stringField(seat.seat_identity, `${path}.seat_identity`, 200);
            if (identities.has(identity)) throw new TypeError('seat identities must be unique');
            identities.add(identity);
            return {
              id: uuidField(seat.id, `${path}.id`),
              seat_identity: identity,
              label: stringField(seat.label, `${path}.label`),
              position: integerField(seat.position, `${path}.position`, 1),
            };
          });
        }
        return {
          id: uuidField(row.id, `sections[${sectionIndex}].rows[${rowIndex}].id`),
          label: stringField(row.label, `sections[${sectionIndex}].rows[${rowIndex}].label`),
          position: integerField(row.position, `sections[${sectionIndex}].rows[${rowIndex}].position`, 1),
          ...(seats === undefined ? {} : { seats }),
        };
      });
    }
    return {
      id: uuidField(section.id, `sections[${sectionIndex}].id`),
      name: stringField(section.name, `sections[${sectionIndex}].name`),
      position: integerField(section.position, `sections[${sectionIndex}].position`, 1),
      ...(rows === undefined ? {} : { rows }),
    };
  });
  const publishedAt = map.published_at;
  return {
    map: {
      id: mapId,
      organizer_id: organizerId,
      venue_id: uuidField(map.venue_id, 'map.venue_id'),
      name: stringField(map.name, 'map.name'),
      version: integerField(map.version, 'map.version', 1),
      status,
      ...(publishedAt === undefined ? {} : { published_at: dateTimeField(publishedAt, 'map.published_at') }),
      orphan_prevention_enabled: map.orphan_prevention_enabled,
      created_at: dateTimeField(map.created_at, 'map.created_at'),
    },
    sections,
  };
}

/** What the parent needs to decide whether Reserve may be pressed. */
export interface SeatSelection {
  seats: string[];
  /** False whenever the selection cannot be submitted: empty, unreadable, closed, or over the ceiling. */
  claimable: boolean;
}

/**
 * What the parent may ask the map to do. HoldPicker owns the reservation, so it is the
 * only thing that ever sees a 409 — but the seats are the MAP's state, so the parent
 * has to hand them over rather than act on them.
 */
export interface SeatMapHandle {
  /** Fold a refused reservation's contended identities in, and re-read authoritatively. */
  applyConflict: (lost: string[]) => void;
}

interface Props {
  organizerId: string;
  slotId: string;
  seatMapId: string;
  locale: Locale;
  onSelectionChange: (selection: SeatSelection) => void;
  ref?: Ref<SeatMapHandle>;
}

/** The 1..50 band the claim path enforces (store.MaxSeatsPerHold). */
const MAX_SEATS = 50;
/**
 * Fallback poll cadence, used only when a response does not tell us its own
 * (TKT-208).
 *
 * The cadence is DERIVED from each successful read's Cache-Control max-age —
 * ADR-004 rule 3: "each response's TTL drives the client refresh cadence. No
 * polling faster than the endpoint's TTL." This constant used to be the cadence
 * itself, at 5000ms, which happened to equal the declared tier. Happening to
 * match is not the same as being derived: nothing kept the two in step, and a
 * tier change would have silently left the client polling at the old rate.
 */
const FALLBACK_POLL_MS = 5000;

/**
 * Floor on a derived cadence. No effect today — the contract declares 5s — but a
 * parsing surprise or a future contract admitting a tiny positive value must not
 * turn this into a hot loop against the service it exists to protect.
 */
const MIN_POLL_MS = 1000;

/**
 * Operational ceiling on a derived cadence: one minute.
 *
 * Scoped to this seconds-tier occupancy endpoint, not to the system. One minute
 * leaves room for a legitimate TTL increase while preventing a mistaken long
 * cache lifetime from suspending seat-map refresh for the buyer's session.
 */
const MAX_POLL_MS = 60 * 1000;

/**
 * pollDelayFromResponse turns a response's declared freshness into the delay
 * before the next routine read.
 *
 * Anything the contract does not promise — a missing header, no-store, zero,
 * malformed, non-finite, or a value a timer cannot hold — falls back rather than
 * inventing a cadence. The conservative direction is the CURRENT load, not a
 * faster one.
 */
export function pollDelayFromResponse(cacheControl: string | null): number {
  const seconds = parseMaxAge(cacheControl);
  if (!Number.isFinite(seconds) || seconds <= 0) return FALLBACK_POLL_MS;
  const ms = seconds * 1000;
  if (ms > MAX_POLL_MS) return FALLBACK_POLL_MS;
  return Math.max(MIN_POLL_MS, ms);
}

/**
 * orderByPosition sorts sections, rows and seats by `position` at every level.
 * The API documents the order but the picker must not depend on it: a response that
 * arrives out of order would otherwise render a map whose seats are in the wrong
 * physical places, which is both wrong and completely plausible-looking.
 */
export function orderByPosition(sections: Section[]): Section[] {
  const byPosition = (a: { position: number }, b: { position: number }) => a.position - b.position;
  return [...sections].sort(byPosition).map((section) => ({
    ...section,
    rows: [...(section.rows ?? [])].sort(byPosition).map((row: SeatMapRow) => ({
      ...row,
      seats: [...(row.seats ?? [])].sort(byPosition),
    })),
  }));
}

/**
 * selectionCeiling is how many seats may be selected: the smallest of the claim
 * path's 1..50 band, the pool's remaining headroom, and the seats actually free on
 * the map.
 *
 * All three bind for different reasons. `remaining` is an aggregate CEILING and not a
 * seat count — inventory does not hold the seat universe — so it can sit above the
 * free-seat count on a small map in a large venue, and below it after a draining
 * capacity cut. Taking the minimum is what keeps the picker from offering a selection
 * the claim would refuse either way.
 */
export function selectionCeiling(remaining: number, freeSeats: number): number {
  return Math.max(0, Math.min(MAX_SEATS, remaining, freeSeats));
}

/**
 * applySeatConflict folds a refused reservation's contended identities back into the
 * map: the lost seats become unavailable, and every seat that was NOT contended stays
 * selected. Clearing the whole selection would be easier and would throw away work the
 * buyer did — TKT-173 goes to real trouble to return only the seats actually lost, so
 * discarding that precision here would waste it.
 */
export function applySeatConflict(
  state: { selected: string[]; unavailable: string[] },
  lost: string[],
): { selected: string[]; unavailable: string[] } {
  const gone = new Set(lost);
  return {
    selected: state.selected.filter((seat) => !gone.has(seat)),
    unavailable: [...new Set([...lost, ...state.unavailable])].sort(),
  };
}

/**
 * reconcileSelection prunes a selection against a freshly-read occupancy: seats that
 * became taken drop out, and the rest is trimmed to the new ceiling (oldest kept).
 *
 * Without this the UI can render a seat as unavailable while still counting it and
 * submitting it, or show five selected against a ceiling of three — and inventory then
 * refuses a request the buyer was invited to make (ai-review).
 */
export function reconcileSelection(selected: string[], taken: Set<string>, ceiling: number): string[] {
  return selected.filter((seat) => !taken.has(seat)).slice(0, ceiling);
}

/** Occupancy is read repeatedly, so it can degrade; geometry is read once, so it cannot. */
type OccupancyState = 'loading' | 'ok' | 'degraded' | 'failed';
type GeometryState = 'loading' | 'ok' | 'failed';

/** A read that has not answered in this long is treated as failed rather than awaited. */
const READ_TIMEOUT_MS = 8000;

/**
 * boundedFetch resolves a Response, or throws with `timedOut` set when the deadline
 * fired rather than the caller aborting.
 *
 * The flag is an explicit closure boolean, not an inspection of `signal.reason`: a
 * plain `abort()` produces an AbortError DOMException, and DOMException IS an Error,
 * so `reason instanceof Error` cannot tell a deadline from an ordinary supersession
 * (ai-review pass 3). Getting that backwards means either reporting a healthy
 * cancellation as a failure, or — worse — swallowing a real timeout and leaving the
 * last snapshot claimable.
 */
async function boundedJSON<T>(
  url: string,
  init: RequestInit,
  controller: AbortController,
  decode: (value: unknown) => T,
): Promise<{ body: T; cacheControl: string | null }> {
  let timedOut = false;
  const deadline = window.setTimeout(() => { timedOut = true; controller.abort(); }, READ_TIMEOUT_MS);
  try {
    const response = await fetch(url, { ...init, signal: controller.signal });
    if (!response.ok) throw new Error(String(response.status));
    // The body is read INSIDE the deadline. fetch() resolves when headers arrive, not
    // when the body has been consumed, so clearing the timer on the response would
    // leave a stalled body unbounded — and a stalled body is worse than a stalled
    // connection here: the caller never reaches its `finally`, so the authoritative-read
    // guard is never released and every later poll is skipped for good (ai-review
    // pass 4).
    // The header rides along with the body: the caller derives its poll cadence
    // from this response's declared freshness (TKT-208), and discarding the
    // Response here is what previously made that impossible.
    const cacheControl = response.headers.get('cache-control');
    return { body: decode(await response.json()), cacheControl };
  } catch (err) {
    // A DOMException is an Error, so this rides along on the real abort object rather
    // than replacing it — the caller still sees name === 'AbortError'.
    const failure = err instanceof Error ? err : new Error('seat-map read failed');
    throw Object.assign(failure, { timedOut });
  } finally {
    window.clearTimeout(deadline);
  }
}

export default function SeatMapPicker({ organizerId, slotId, seatMapId, locale, onSelectionChange, ref }: Props) {
  const t = UI_STRINGS[locale];
  const [sections, setSections] = useState<Section[] | null>(null);
  const [occupancy, setOccupancy] = useState<Occupancy | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  // Tracked SEPARATELY. Sharing one field let a successful occupancy poll clear a
  // terminal geometry failure, after which `sections` stayed null forever and the
  // picker rendered "loading" for the rest of the session — neither usable nor
  // honest (ai-review pass 2).
  const [occupancyState, setOccupancyState] = useState<OccupancyState>('loading');
  const [geometryState, setGeometryState] = useState<GeometryState>('loading');
  const [notice, setNotice] = useState('');
  // Seats a 409 proved gone. Kept SEPARATELY and unioned with every server snapshot
  // rather than written into one: the occupancy read is cacheable for five seconds, so
  // the refresh that follows a conflict can legitimately return a body that still shows
  // the seat free. Replacing state with that body resurrects a seat the server has
  // already refused, and the buyer reselects it and fails again (ai-review).
  const [conflicted, setConflicted] = useState<string[]>([]);
  // Monotonic generation: an older read must never commit over a newer one — neither
  // its success NOR its failure.
  const generation = useRef(0);
  const inFlight = useRef<AbortController | null>(null);
  // An authoritative (post-conflict, no-store) read in flight. The routine poll must
  // not abort it: only a successful authoritative read clears the conflict overlay, so
  // a poll landing in the 5–8s window would supersede it and strand the overlay for
  // the life of the picker (ai-review pass 3).
  const authoritative = useRef<AbortController | null>(null);
  const visible = useRef(true);

  // Cadence derived from the latest successful routine read (TKT-208).
  const pollDelay = useRef(FALLBACK_POLL_MS);

  const readOccupancy = useCallback(async (options: { bypassCache?: boolean } = {}) => {
    inFlight.current?.abort();
    const controller = new AbortController();
    inFlight.current = controller;
    if (options.bypassCache) authoritative.current = controller;
    const mine = ++generation.current;
    try {
      const { body: next, cacheControl } = await boundedJSON<Occupancy>(
        `/api/inventory/slots/${encodeURIComponent(slotId)}/seat-occupancy?organizer_id=${encodeURIComponent(organizerId)}`,
        // After a conflict the cached body is exactly the thing we must not trust.
        { cache: options.bypassCache ? 'no-store' : 'default' },
        controller,
        decodeOccupancy,
      );
      if (mine !== generation.current) return;
      // Catalog and inventory each publish their own view of which map version a slot
      // is seated against. Disagreeing means a projection skew, and rendering either
      // one would put the buyer on a map that is not the one being claimed against.
      // This one is NOT retryable: it is a real disagreement, not a blip.
      if (!sameUuid(next.slot_id, slotId) || !sameUuid(next.seat_map_id, seatMapId)) {
        setOccupancyState('failed');
        return;
      }
      setOccupancy(next);
      setOccupancyState('ok');
      // Derived from THIS response, not a mount-time constant: if successive
      // responses declare different tiers the cadence follows them (TKT-208).
      pollDelay.current = pollDelayFromResponse(cacheControl);
      // A no-store read is authoritative — bypassing the cache is exactly what makes
      // it so — and therefore supersedes the conflict overlay. Without this the
      // overlay only ever grows, and a seat that caused one conflict stays dark for
      // the life of the tab even after its hold is released (ai-review pass 2).
      if (options.bypassCache) setConflicted([]);
    } catch (err) {
      // A timeout aborts too, but it is a real failure and must be reported as one.
      if (isCallerCancellation(err)) return;
      if (mine !== generation.current) return;
      // A failure AFTER a good first read keeps the last known map and says so:
      // blanking it would read as a sold-out house, which is a lie with a cost.
      // Before any good read there is nothing to degrade to, so it is a hard failure —
      // and either way a later good read clears it, because a transient blip must not
      // be sticky.
      setOccupancyState((current) => (current === 'ok' || current === 'degraded' ? 'degraded' : 'failed'));
    } finally {
      if (authoritative.current === controller) authoritative.current = null;
    }
  }, [organizerId, slotId, seatMapId]);

  // Geometry is read once — and it needs the deadline just as much as occupancy does.
  // A hung geometry read leaves `sections` null forever, and no amount of successful
  // polling can move the render past "loading": exactly the permanent-loading failure
  // the split read states were meant to end (ai-review pass 3).
  useEffect(() => {
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const { body: geometry } = await boundedJSON<SeatMapGeometry>(
          `/api/catalog/public/seat-maps/${encodeURIComponent(seatMapId)}`,
          {},
          controller,
          (value) => decodeSeatMapGeometry(value, seatMapId, organizerId),
        );
        if (live) {
          setSections(orderByPosition(geometry.sections ?? []));
          setGeometryState('ok');
        }
      } catch (err) {
        // An unmount abort is not a failure; a deadline is.
        if (isCallerCancellation(err)) return;
        if (live) setGeometryState('failed');
      }
    })();
    return () => { live = false; controller.abort(); };
  }, [seatMapId, organizerId]);

  // Polling is SERIALISED, not on a fixed interval: the next read is scheduled only
  // once the current one settles. A fixed interval against responses slower than the
  // period accumulates overlapping requests that each supersede the last, so the map
  // can never finish loading while the load keeps growing — the worst possible
  // behaviour during the on-sale this read exists to serve (ai-review).
  useEffect(() => {
    let stopped = false;
    let timer: number | undefined;
    const tick = async () => {
      // Skip — do not abort — while an authoritative read is outstanding.
      if (!stopped && visible.current && authoritative.current === null) await readOccupancy();
      // A failed read carries no usable TTL, so the last derived delay stands
      // (and the fallback before any success).
      if (!stopped) timer = window.setTimeout(tick, pollDelay.current);
    };
    void tick();
    const onVisibility = () => { visible.current = document.visibilityState === 'visible'; };
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      stopped = true;
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisibility);
      // Abandon the outstanding read: an unmounted picker must not keep a socket open
      // or land a state update.
      inFlight.current?.abort();
    };
  }, [readOccupancy]);

  const taken = useMemo(
    () => new Set([...(occupancy?.unavailable_seat_identities ?? []), ...conflicted]),
    [occupancy, conflicted],
  );
  const open = occupancy?.offering_status === 'open';
  const allSeats: SeatMapSeat[] = useMemo(
    () => (sections ?? []).flatMap((s) => (s.rows ?? []).flatMap((r: SeatMapRow) => r.seats ?? [])),
    [sections],
  );
  const ceiling = occupancy
    ? selectionCeiling(occupancy.remaining_capacity, allSeats.filter((s) => !taken.has(s.seat_identity)).length)
    : 0;

  // Every accepted snapshot reconciles the selection. A seat that became taken drops
  // out, and the rest is trimmed to the new ceiling.
  useEffect(() => {
    setSelected((current) => {
      const next = reconcileSelection(current, taken, ceiling);
      return next.length === current.length && next.every((s, i) => s === current[i]) ? current : next;
    });
  }, [taken, ceiling]);

  // Claimable requires a CURRENT read. `degraded` keeps the map on screen — blanking
  // it would read as a sold-out house — but a selection resting on occupancy the
  // component has just declared unreadable must not be submittable. Including
  // degraded here is how the fail-closed boundary silently reopened (ai-review pass 2).
  const claimable = occupancyState === 'ok' && open && selected.length > 0 && selected.length <= ceiling;
  useEffect(() => {
    onSelectionChange({ seats: selected, claimable });
  }, [selected, claimable, onSelectionChange]);

  // The conflict channel from HoldPicker: a 409's identities are authoritative and are
  // held here until a no-store read confirms them, rather than being written into a
  // snapshot the next cached response would overwrite.
  //
  // An imperative handle, not a `window` CustomEvent keyed by slot id (TKT-184). The two
  // components are directly composed — HoldPicker renders this one — so the DOM was
  // being used as a message bus between a parent and its own child. That indirection
  // bought nothing and cost the type checker: a mistyped detail, a stale slot id in the
  // event name, or two pickers mounted for one slot all failed silently at runtime.
  useImperativeHandle(ref, () => ({
    applyConflict(lost: string[]) {
      setConflicted((current) => [...new Set([...current, ...lost])].sort());
      void readOccupancy({ bypassCache: true });
    },
  }), [readOccupancy]);

  if (geometryState === 'failed' || occupancyState === 'failed') {
    return <p className="seat-map-unavailable" role="status">{t.seatSelectionUnavailable}</p>;
  }
  if (sections === null || occupancy === null) {
    return <p className="seat-map-loading" role="status">{t.seatMapLoading}</p>;
  }

  function toggle(identity: string) {
    setSelected((current) => {
      if (current.includes(identity)) {
        setNotice('');
        return current.filter((seat) => seat !== identity);
      }
      if (current.length >= ceiling) {
        setNotice(t.seatLimitReached.replace('{n}', String(ceiling)));
        return current;
      }
      setNotice('');
      return [...current, identity];
    });
  }

  return (
    <div className="seat-map">
      <ul className="seat-legend">
        <li><span className="seat-swatch free" aria-hidden="true" />{t.seatFree}</li>
        <li><span className="seat-swatch selected" aria-hidden="true">✓</span>{t.seatSelected}</li>
        <li><span className="seat-swatch taken" aria-hidden="true">×</span>{t.seatTaken}</li>
      </ul>
      {!open && <p className="seat-map-closed">{t.seatsNotOnSale}</p>}
      {occupancyState === 'degraded' && <p className="seat-map-closed">{t.seatMapStale}</p>}
      {sections.map((section) => (
        <section key={section.id} className="seat-section">
          <h5>{section.name}</h5>
          {(section.rows ?? []).map((row: SeatMapRow) => (
            <div key={row.id} className="seat-row">
              <span className="seat-row-label" aria-hidden="true">{row.label}</span>
              {(row.seats ?? []).map((seat: SeatMapSeat) => {
                const isTaken = taken.has(seat.seat_identity);
                const isSelected = selected.includes(seat.seat_identity);
                // The state is carried by text, not by colour alone: the marker is a
                // real character and the accessible name says the state in words.
                const state = isTaken ? t.seatTaken : isSelected ? t.seatSelected : t.seatFree;
                return (
                  <button
                    key={seat.id}
                    type="button"
                    className={`seat ${isTaken ? 'taken' : isSelected ? 'selected' : 'free'}`}
                    disabled={isTaken || !open}
                    // A taken seat is disabled, NOT "pressed": they are different
                    // states and conflating them is invisible without a screen reader.
                    aria-pressed={isTaken ? undefined : isSelected}
                    aria-label={`${section.name}, ${t.seatRow} ${row.label}, ${t.seat} ${seat.label}, ${state}`}
                    onClick={() => toggle(seat.seat_identity)}
                  >
                    <span aria-hidden="true">{isTaken ? '×' : isSelected ? '✓' : seat.label}</span>
                  </button>
                );
              })}
            </div>
          ))}
        </section>
      ))}
      <p className="seat-status" role="status">
        {notice || t.seatsSelected.replace('{n}', String(selected.length)).replace('{max}', String(ceiling))}
      </p>
    </div>
  );
}
