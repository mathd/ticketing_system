import { useMemo, useState } from 'react';

import type { SeatMapEdit, SeatMapGeometry } from '../lib/api';

// SeatMapEditor is the TKT-105 published-map editor. It loads the current
// published geometry as editable state and lets staff rename/add/remove
// sections, rows, and seats, then serializes the FULL replacement geometry
// (the shape the catalog edit endpoint expects — TKT-104's EditSeatMapInput)
// into a hidden field and submits the surrounding page form. Submitting an
// untouched map round-trips identically, so pinned seats are preserved unless
// the staffer deliberately changes them (an orphaning change is rejected by the
// server with a 409 the page surfaces — no domain logic lives here).

interface SeatState {
  label: string;
  position: number;
}
interface RowState {
  label: string;
  position: number;
  seats: SeatState[];
}
interface SectionState {
  name: string;
  position: number;
  rows: RowState[];
}

function fromGeometry(g: SeatMapGeometry): SectionState[] {
  return (g.sections ?? []).map((s) => ({
    name: s.name,
    position: s.position,
    rows: (s.rows ?? []).map((r) => ({
      label: r.label,
      position: r.position,
      seats: (r.seats ?? []).map((seat) => ({ label: seat.label, position: seat.position })),
    })),
  }));
}

// toEdit converts the editor state to the SeatMapEdit request body. Exported so
// the component test can assert the exact serialized shape without a DOM submit.
export function toEdit(organizerId: string, sections: SectionState[]): SeatMapEdit {
  return {
    organizer_id: organizerId,
    sections: sections.map((s) => ({
      name: s.name,
      position: s.position,
      rows: s.rows.map((r) => ({
        label: r.label,
        position: r.position,
        seats: r.seats.map((seat) => ({ label: seat.label, position: seat.position })),
      })),
    })),
  };
}

export interface SeatMapEditorProps {
  geometry: SeatMapGeometry;
  organizerId: string;
  /** The page action + hidden fields to submit alongside the geometry JSON. */
  action: string;
  postTo: string;
}

export default function SeatMapEditor({ geometry, organizerId, action, postTo }: SeatMapEditorProps) {
  const [sections, setSections] = useState<SectionState[]>(() => fromGeometry(geometry));

  const editJson = useMemo(() => JSON.stringify(toEdit(organizerId, sections)), [organizerId, sections]);

  const renameSeat = (si: number, ri: number, seatIdx: number, label: string) => {
    setSections((prev) =>
      prev.map((s, i) =>
        i !== si
          ? s
          : {
              ...s,
              rows: s.rows.map((r, j) =>
                j !== ri ? r : { ...r, seats: r.seats.map((seat, k) => (k !== seatIdx ? seat : { ...seat, label })) },
              ),
            },
      ),
    );
  };

  const removeSeat = (si: number, ri: number, seatIdx: number) => {
    setSections((prev) =>
      prev.map((s, i) =>
        i !== si
          ? s
          : { ...s, rows: s.rows.map((r, j) => (j !== ri ? r : { ...r, seats: r.seats.filter((_, k) => k !== seatIdx) })) },
      ),
    );
  };

  const addSeat = (si: number, ri: number) => {
    setSections((prev) =>
      prev.map((s, i) =>
        i !== si
          ? s
          : {
              ...s,
              rows: s.rows.map((r, j) =>
                j !== ri ? r : { ...r, seats: [...r.seats, { label: String(r.seats.length + 1), position: r.seats.length + 1 }] },
              ),
            },
      ),
    );
  };

  return (
    <form method="POST" action={postTo} className="editor">
      <input type="hidden" name="_action" value={action} />
      <input type="hidden" name="map_id" value={geometry.map.id} />
      <input type="hidden" name="geometry" value={editJson} data-testid="geometry-input" />

      <p className="hint">
        Edit the seats below and save — a new published version is created. Removing a seat that is currently sold or
        held is rejected.
      </p>

      {sections.map((s, si) => (
        <div className="sec" key={si}>
          <h4>{s.name}</h4>
          {s.rows.map((r, ri) => (
            <div className="rowline" key={ri}>
              <span className="rlabel">Row {r.label}</span>
              <span className="seats">
                {r.seats.map((seat, seatIdx) => (
                  <span className="seat-edit" key={seatIdx}>
                    <input
                      aria-label={`Seat ${s.name}/${r.label} label`}
                      value={seat.label}
                      onChange={(e) => renameSeat(si, ri, seatIdx, e.target.value)}
                    />
                    <button type="button" aria-label={`Remove seat ${s.name}/${r.label}/${seat.label}`} onClick={() => removeSeat(si, ri, seatIdx)}>
                      ×
                    </button>
                  </span>
                ))}
                <button type="button" onClick={() => addSeat(si, ri)}>
                  + seat
                </button>
              </span>
            </div>
          ))}
        </div>
      ))}

      <button type="submit" className="save">
        Save as new version
      </button>
    </form>
  );
}
