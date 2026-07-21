//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"
)

// enrichedTriple publishes Orchestra/A/1 then edits to Orchestra/A/1,2,3 so batch tests
// have a multi-seat geometry to pin against. Returns the current published version.
func enrichedTriple(ctx context.Context, t *testing.T, st *Postgres) SeatMap {
	t.Helper()
	m := seedPublishedMap(ctx, t, st, "Batch-pin")
	v2, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("2", 2), st1("3", 3)))}})
	if err != nil {
		t.Fatalf("enrich edit: %v", err)
	}
	return v2
}

// TestBatchPinIsAtomic: a set containing one absent identity pins NONE and returns
// ErrSeatIdentityNotFound; a fully-valid set pins all; UnpinSeats clears all.
func TestBatchPinIsAtomic(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	v := enrichedTriple(ctx, t, st)

	// One absent identity → whole batch rejected, nothing inserted.
	err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: []string{"Orchestra/A/1", "Orchestra/A/2", "Nowhere/Z/9"}, PinnedBy: "hold:batch"})
	if !errors.Is(err, ErrSeatIdentityNotFound) {
		t.Fatalf("batch with absent seat err = %v want ErrSeatIdentityNotFound", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins WHERE pinned_by=$1`, "hold:batch").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("partial batch left %d pins — not atomic", n)
	}

	// Fully-valid set pins all.
	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: []string{"Orchestra/A/1", "Orchestra/A/2"}, PinnedBy: "hold:batch"}); err != nil {
		t.Fatalf("valid batch pin: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins WHERE pinned_by=$1`, "hold:batch").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("valid batch pinned %d want 2", n)
	}

	// Idempotent re-pin.
	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: []string{"Orchestra/A/1", "Orchestra/A/2"}, PinnedBy: "hold:batch"}); err != nil {
		t.Fatalf("idempotent re-pin: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins WHERE pinned_by=$1`, "hold:batch").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("re-pin changed count to %d want 2", n)
	}

	// Unpin clears all.
	if err := st.UnpinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: []string{"Orchestra/A/1", "Orchestra/A/2"}, PinnedBy: "hold:batch"}); err != nil {
		t.Fatalf("batch unpin: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins WHERE pinned_by=$1`, "hold:batch").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("after unpin %d pins remain want 0", n)
	}
}

// TestBatchPinBlocksOrphaningEdit: a seat batch-pinned by an inventory hold makes an
// edit that drops it hard-reject — the AC3 interplay, exercised through the batch path.
func TestBatchPinBlocksOrphaningEdit(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	v := enrichedTriple(ctx, t, st)

	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: []string{"Orchestra/A/2"}, PinnedBy: "hold:h1"}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// Edit that keeps A/1,A/3 but drops the pinned A/2 → orphaning → rejected.
	_, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("3", 3)))}})
	if !errors.Is(err, ErrSeatMapEditOrphansPinned) {
		t.Fatalf("orphaning edit err = %v want ErrSeatMapEditOrphansPinned", err)
	}
}
