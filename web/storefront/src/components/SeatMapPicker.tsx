import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import type { SeatMapGeometry, SeatMapRow, SeatMapSeat, SeatMapSection } from '../lib/api';
import type { Locale } from '../lib/locales';
import { UI_STRINGS } from '../lib/locales';

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

/** What the parent needs to decide whether Reserve may be pressed. */
export interface SeatSelection {
  seats: string[];
  /** False whenever the selection cannot be submitted: empty, unreadable, closed, or over the ceiling. */
  claimable: boolean;
}

interface Props {
  organizerId: string;
  slotId: string;
  seatMapId: string;
  locale: Locale;
  onSelectionChange: (selection: SeatSelection) => void;
}

/** The 1..50 band the claim path enforces (store.MaxSeatsPerHold). */
const MAX_SEATS = 50;
/** ADR-004's seconds tier, and the exact max-age the occupancy read declares. */
const POLL_MS = 5000;

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

type ReadState = 'loading' | 'ok' | 'degraded' | 'failed';

export default function SeatMapPicker({ organizerId, slotId, seatMapId, locale, onSelectionChange }: Props) {
  const t = UI_STRINGS[locale];
  const [sections, setSections] = useState<Section[] | null>(null);
  const [occupancy, setOccupancy] = useState<Occupancy | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [readState, setReadState] = useState<ReadState>('loading');
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
  const visible = useRef(true);

  const readOccupancy = useCallback(async (options: { bypassCache?: boolean } = {}) => {
    inFlight.current?.abort();
    const controller = new AbortController();
    inFlight.current = controller;
    const mine = ++generation.current;
    try {
      const response = await fetch(
        `/api/inventory/slots/${encodeURIComponent(slotId)}/seat-occupancy?organizer_id=${encodeURIComponent(organizerId)}`,
        // After a conflict the cached body is exactly the thing we must not trust.
        { signal: controller.signal, cache: options.bypassCache ? 'no-store' : 'default' },
      );
      if (!response.ok) throw new Error(String(response.status));
      const next = (await response.json()) as Occupancy;
      if (mine !== generation.current) return;
      // Catalog and inventory each publish their own view of which map version a slot
      // is seated against. Disagreeing means a projection skew, and rendering either
      // one would put the buyer on a map that is not the one being claimed against.
      // This one is NOT retryable: it is a real disagreement, not a blip.
      if (next.slot_id !== slotId || next.seat_map_id !== seatMapId) {
        setReadState('failed');
        return;
      }
      setOccupancy(next);
      setReadState('ok');
    } catch (err) {
      if ((err as Error)?.name === 'AbortError') return;
      if (mine !== generation.current) return;
      // A failure AFTER a good first read keeps the last known map and says so:
      // blanking it would read as a sold-out house, which is a lie with a cost.
      // Before any good read there is nothing to degrade to, so it is a hard failure —
      // and either way a later good read clears it, because a transient blip must not
      // be sticky.
      setReadState((current) => (current === 'ok' || current === 'degraded' ? 'degraded' : 'failed'));
    }
  }, [organizerId, slotId, seatMapId]);

  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const response = await fetch(`/api/catalog/public/seat-maps/${encodeURIComponent(seatMapId)}`);
        if (!response.ok) throw new Error(String(response.status));
        const geometry = (await response.json()) as SeatMapGeometry;
        if (live) setSections(orderByPosition(geometry.sections ?? []));
      } catch {
        if (live) setReadState('failed');
      }
    })();
    return () => { live = false; };
  }, [seatMapId]);

  // Polling is SERIALISED, not on a fixed interval: the next read is scheduled only
  // once the current one settles. A fixed interval against responses slower than the
  // period accumulates overlapping requests that each supersede the last, so the map
  // can never finish loading while the load keeps growing — the worst possible
  // behaviour during the on-sale this read exists to serve (ai-review).
  useEffect(() => {
    let stopped = false;
    let timer: number | undefined;
    const tick = async () => {
      if (!stopped && visible.current) await readOccupancy();
      if (!stopped) timer = window.setTimeout(tick, POLL_MS);
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

  const usable = readState === 'ok' || readState === 'degraded';
  const claimable = usable && open && selected.length > 0 && selected.length <= ceiling;
  useEffect(() => {
    onSelectionChange({ seats: selected, claimable });
  }, [selected, claimable, onSelectionChange]);

  // The conflict channel from HoldPicker: a 409's identities are authoritative and are
  // held here until a no-store read confirms them, rather than being written into a
  // snapshot the next cached response would overwrite.
  useEffect(() => {
    const onConflict = (event: Event) => {
      const lost = (event as CustomEvent<string[]>).detail;
      setConflicted((current) => [...new Set([...current, ...lost])].sort());
      void readOccupancy({ bypassCache: true });
    };
    window.addEventListener(`seat-conflict:${slotId}`, onConflict);
    return () => window.removeEventListener(`seat-conflict:${slotId}`, onConflict);
  }, [slotId, readOccupancy]);

  if (readState === 'failed') {
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
      {readState === 'degraded' && <p className="seat-map-closed">{t.seatMapStale}</p>}
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
