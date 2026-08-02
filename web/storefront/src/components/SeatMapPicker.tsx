import { useCallback, useEffect, useRef, useState } from 'react';

import type { SeatMapGeometry, SeatMapRow, SeatMapSeat, SeatMapSection } from '../lib/api';
import type { Locale } from '../lib/locales';
import { UI_STRINGS } from '../lib/locales';

// SeatMapPicker is TKT-174's seat concern, end to end: it reads the published
// geometry and the live occupancy, renders the map, owns the selection, and reports
// the selected identities upward. It deliberately does NOT own the reservation or
// the checkout — HoldPicker keeps those, so there is one reservation state and one
// checkout path (AC3) even though there are two components.
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

interface Props {
  organizerId: string;
  slotId: string;
  seatMapId: string;
  locale: Locale;
  onSelectionChange: (seats: string[]) => void;
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

export default function SeatMapPicker({ organizerId, slotId, seatMapId, locale, onSelectionChange }: Props) {
  const t = UI_STRINGS[locale];
  const [sections, setSections] = useState<Section[] | null>(null);
  const [occupancy, setOccupancy] = useState<Occupancy | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [failed, setFailed] = useState(false);
  const [notice, setNotice] = useState('');
  // Monotonic generation: an older poll must never overwrite a newer answer, and in
  // particular must never restore a seat that a 409 has just proven gone.
  const generation = useRef(0);

  const readOccupancy = useCallback(async () => {
    const mine = ++generation.current;
    try {
      const response = await fetch(
        `/api/inventory/slots/${encodeURIComponent(slotId)}/seat-occupancy?organizer_id=${encodeURIComponent(organizerId)}`,
      );
      if (!response.ok) throw new Error(String(response.status));
      const next = (await response.json()) as Occupancy;
      // Catalog and inventory each publish their own view of which map version a slot
      // is seated against. Disagreeing means a projection skew, and rendering either
      // one would put the buyer on a map that is not the one being claimed against.
      if (next.slot_id !== slotId || next.seat_map_id !== seatMapId) {
        setFailed(true);
        return null;
      }
      if (mine === generation.current) setOccupancy(next);
      return next;
    } catch {
      // A failure AFTER a good first read keeps the last known map: blanking it would
      // read as a sold-out house, which is a lie with a cost.
      setOccupancy((current) => {
        if (current === null) setFailed(true);
        return current;
      });
      return null;
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
        if (live) setFailed(true);
      }
    })();
    return () => { live = false; };
  }, [seatMapId]);

  useEffect(() => {
    void readOccupancy();
    const timer = window.setInterval(() => {
      // Don't poll a tab nobody is looking at. This is politeness toward a hot
      // on-sale, not a fix for it — ADR-004's shared cache tier is TKT-31's and is
      // not deployed, so every open tab still reaches inventory.
      if (document.visibilityState === 'visible') void readOccupancy();
    }, POLL_MS);
    return () => window.clearInterval(timer);
  }, [readOccupancy]);

  useEffect(() => { onSelectionChange(selected); }, [selected, onSelectionChange]);

  // Exposed so HoldPicker can fold a 409's identities back in without owning any of
  // this state.
  useEffect(() => {
    const onConflict = (event: Event) => {
      const lost = (event as CustomEvent<string[]>).detail;
      setOccupancy((current) => (current === null ? current : {
        ...current,
        unavailable_seat_identities: applySeatConflict(
          { selected, unavailable: current.unavailable_seat_identities }, lost,
        ).unavailable,
      }));
      setSelected((current) => applySeatConflict({ selected: current, unavailable: [] }, lost).selected);
      // Force a fresh read, and bump the generation first so any poll already in
      // flight cannot land after it and undo the conflict.
      void readOccupancy();
    };
    window.addEventListener(`seat-conflict:${slotId}`, onConflict);
    return () => window.removeEventListener(`seat-conflict:${slotId}`, onConflict);
  }, [slotId, selected, readOccupancy]);

  if (failed || (sections === null && occupancy === null && failed)) {
    return <p className="seat-map-unavailable" role="status">{t.seatSelectionUnavailable}</p>;
  }
  if (sections === null || occupancy === null) {
    return <p className="seat-map-loading" role="status">{t.seatMapLoading}</p>;
  }

  const taken = new Set(occupancy.unavailable_seat_identities);
  const open = occupancy.offering_status === 'open';
  const allSeats: SeatMapSeat[] = sections.flatMap((s) => (s.rows ?? []).flatMap((r: SeatMapRow) => r.seats ?? []));
  const ceiling = selectionCeiling(occupancy.remaining_capacity, allSeats.filter((s) => !taken.has(s.seat_identity)).length);

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
