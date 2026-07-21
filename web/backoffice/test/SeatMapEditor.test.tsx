// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import SeatMapEditor, { nextSeat, toEdit } from '../src/components/SeatMapEditor';
import type { SeatMapGeometry } from '../src/lib/api';

const ORG = '00000000-0000-0000-0000-000000000001';

const geometry: SeatMapGeometry = {
  map: { id: 'm1', organizer_id: ORG, venue_id: 'v1', name: 'Floor', version: 2, status: 'published', created_at: '2026-07-20T00:00:00Z' },
  sections: [
    {
      id: 's1',
      name: 'Orchestra',
      position: 1,
      rows: [{ id: 'r1', label: 'A', position: 1, seats: [{ id: 'x1', seat_identity: 'Orchestra/A/1', label: '1', position: 1 }] }],
    },
  ],
};

afterEach(cleanup);

describe('SeatMapEditor (TKT-105)', () => {
  it('renders the current geometry as editable seats', () => {
    render(<SeatMapEditor geometry={geometry} organizerId={ORG} action="edit-map" postTo="/admin/venues/v1" />);
    expect(screen.getByText('Orchestra')).toBeTruthy();
    expect((screen.getByLabelText('Seat Orchestra/A label') as HTMLInputElement).value).toBe('1');
  });

  it('serializes an untouched map to the exact SeatMapEdit shape (round-trips pinned seats)', () => {
    render(<SeatMapEditor geometry={geometry} organizerId={ORG} action="edit-map" postTo="/admin/venues/v1" />);
    const hidden = screen.getByTestId('geometry-input') as HTMLInputElement;
    expect(JSON.parse(hidden.value)).toEqual(toEdit(ORG, [{ name: 'Orchestra', position: 1, rows: [{ label: 'A', position: 1, seats: [{ label: '1', position: 1 }] }] }]));
  });

  it('reflects a seat removal in the serialized geometry (the change the server may reject)', () => {
    render(<SeatMapEditor geometry={geometry} organizerId={ORG} action="edit-map" postTo="/admin/venues/v1" />);
    fireEvent.click(screen.getByLabelText('Remove seat Orchestra/A/1'));
    const hidden = screen.getByTestId('geometry-input') as HTMLInputElement;
    const body = JSON.parse(hidden.value);
    expect(body.sections[0].rows[0].seats).toHaveLength(0);
  });

  it('reflects a seat rename in the serialized geometry', () => {
    render(<SeatMapEditor geometry={geometry} organizerId={ORG} action="edit-map" postTo="/admin/venues/v1" />);
    fireEvent.change(screen.getByLabelText('Seat Orchestra/A label'), { target: { value: '9' } });
    const hidden = screen.getByTestId('geometry-input') as HTMLInputElement;
    expect(JSON.parse(hidden.value).sections[0].rows[0].seats[0].label).toBe('9');
  });
});

// nextSeat must not collide with an existing label/position — deriving from
// length duplicates a seat identity the catalog then rejects (ai-review finding).
describe('nextSeat', () => {
  it('takes the next position above the current max, not the count', () => {
    // seats [1@1, 3@2] with a gap: length+1 would give label "3" (collision).
    const next = nextSeat([
      { label: '1', position: 1 },
      { label: '3', position: 2 },
    ]);
    expect(next.position).toBe(3);
    expect(next.label).not.toBe('3');
  });

  it('bumps the label past any existing label so the identity is unique', () => {
    // max position is 2, so the first candidate label "3" already exists → bump to "4".
    const next = nextSeat([
      { label: '3', position: 1 },
      { label: '4', position: 2 },
    ]);
    expect(next.label).toBe('5');
  });

  it('handles an empty row', () => {
    const next = nextSeat([]);
    expect(next).toEqual({ label: '1', position: 1 });
  });
});
