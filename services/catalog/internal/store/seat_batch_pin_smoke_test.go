//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
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

// TestListSeatMapPinsIsKeysetPagedAndResolvesFamilyVersion is the reconciliation read
// (TKT-112). Three things must hold: a bounded keyset drain visits every pin exactly once
// across families; the read is deliberately unfiltered (hold AND sale pins come back —
// classification belongs to the caller, not catalog); and every row carries a seat-map id
// that reaches the pin through the family-locked unpin path, INCLUDING for an edited family
// whose pin was created against a superseded version.
func TestListSeatMapPinsIsKeysetPagedAndResolvesFamilyVersion(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)

	// Family 1: edited, so the family has two versions and the pin predates the current one.
	v1 := seedPublishedMap(ctx, t, st, "Recon-one")
	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v1.ID,
		SeatIdentities: []string{"Orchestra/A/1"}, PinnedBy: "hold:recon-a"}); err != nil {
		t.Fatalf("pin family one: %v", err)
	}
	v1b, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: v1.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("2", 2)))}})
	if err != nil {
		t.Fatalf("edit family one: %v", err)
	}
	if v1b.ID == v1.ID {
		t.Fatal("edit must produce a new version row")
	}
	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v1b.ID,
		SeatIdentities: []string{"Orchestra/A/2"}, PinnedBy: "sale:recon-order"}); err != nil {
		t.Fatalf("sale pin family one: %v", err)
	}

	// Family 2: untouched, one version.
	v2 := seedPublishedMap(ctx, t, st, "Recon-two")
	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v2.ID,
		SeatIdentities: []string{"Orchestra/A/1"}, PinnedBy: "hold:recon-b"}); err != nil {
		t.Fatalf("pin family two: %v", err)
	}

	var total int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("fixture has %d pins want 3", total)
	}

	// Drain with a page of 2: more than one request, and the cursor must make progress.
	seen := map[string]uuid.UUID{}
	after := uuid.Nil
	pages := 0
	for {
		page, listErr := st.ListSeatMapPins(ctx, after, 2)
		if listErr != nil {
			t.Fatalf("list page after %s: %v", after, listErr)
		}
		if len(page) == 0 {
			break
		}
		pages++
		if pages > 5 {
			t.Fatal("drain did not terminate — the keyset cursor is not advancing")
		}
		if len(page) > 2 {
			t.Fatalf("page returned %d rows, limit was 2", len(page))
		}
		for _, p := range page {
			key := p.PinnedBy + "|" + p.SeatIdentity
			if _, dup := seen[key]; dup {
				t.Fatalf("pin %s returned twice across pages", key)
			}
			seen[key] = p.SeatMapID
			if p.OrganizerID != seatMapOrg {
				t.Fatalf("pin %s organizer = %s", key, p.OrganizerID)
			}
		}
		after = page[len(page)-1].ID
	}
	if pages < 2 {
		t.Fatalf("drained in %d page(s) — the fixture should need at least two", pages)
	}

	want := []string{"hold:recon-a|Orchestra/A/1", "hold:recon-b|Orchestra/A/1", "sale:recon-order|Orchestra/A/2"}
	if len(seen) != len(want) {
		t.Fatalf("drain saw %v want exactly %v", seen, want)
	}
	for _, key := range want {
		if _, ok := seen[key]; !ok {
			t.Fatalf("drain missed %s (saw %v)", key, seen)
		}
	}

	// The returned seat-map id must be USABLE: unpinning through it clears the pin, which
	// is only true if it resolves to the pin's family. The hold pin on family one was
	// created against the SUPERSEDED version, so this is the case a naive
	// "join on the pin's creation version" would get wrong.
	if err := st.UnpinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg,
		SeatMapID:      seen["hold:recon-a|Orchestra/A/1"],
		SeatIdentities: []string{"Orchestra/A/1"}, PinnedBy: "hold:recon-a"}); err != nil {
		t.Fatalf("unpin through the listed seat-map id: %v", err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins WHERE pinned_by=$1`, "hold:recon-a").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("the listed seat-map id did not reach the pin (%d left)", remaining)
	}
}

// TestListSeatMapPinsBoundsThePage: the caller cannot ask for an unbounded page, and a
// non-positive limit is a usage error rather than a silent full-table read.
func TestListSeatMapPinsBoundsThePage(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	if _, err := st.ListSeatMapPins(ctx, uuid.Nil, 0); err == nil {
		t.Fatal("limit 0 must be rejected, not read the whole table")
	}
	if _, err := st.ListSeatMapPins(ctx, uuid.Nil, MaxSeatMapPinPage+1); err == nil {
		t.Fatalf("limit above MaxSeatMapPinPage (%d) must be rejected", MaxSeatMapPinPage)
	}
}

// TKT-306: an unpin that matched NO family is distinguishable from one that found
// nothing to release.
//
// Both are successes — an unpin is idempotent and neither leaves work undone — and the
// old code returned bare nil for both. That erased the one case where the caller is
// wrong about what it is releasing: a WRONG ORGANIZER for a real map. The release
// reports success, the pins stay, and nothing notices until TKT-112's reconcile sweep
// reports pins naming a claim nobody remembers.
//
// Three cases, because two would not separate the claim. "Wrong organizer" and "map that
// does not exist" must BOTH give the sentinel — the resolving query keys on the pair, so
// a fix that only handled a missing id would leave the motivating case silent. And a real
// pair with nothing pinned must give nil, or the fix is "always return the sentinel",
// which satisfies the other two and destroys the distinction it claims to add.
func TestUnpinDistinguishesNoFamilyFromNothingToUnpin(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	v := enrichedTriple(ctx, t, st)

	seats := []string{"Orchestra/A/1", "Orchestra/A/2"}

	// A real map and organizer, nothing pinned: genuinely idempotent, nil.
	if err := st.UnpinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: seats, PinnedBy: "hold:none"}); err != nil {
		t.Fatalf("unpin with nothing pinned: %v, want nil — the pins are already gone, "+
			"which is exactly what the caller asked for", err)
	}

	// The motivating case: a REAL map, the WRONG organizer.
	if err := st.UnpinSeats(ctx, BatchPinInput{OrganizerID: uuid.New(), SeatMapID: v.ID,
		SeatIdentities: seats, PinnedBy: "hold:wrong-org"}); !errors.Is(err, ErrSeatMapFamilyNotFound) {
		t.Fatalf("unpin with a foreign organizer = %v, want ErrSeatMapFamilyNotFound — "+
			"answering nil reports a release that did not happen, and the pins survive "+
			"until the reconcile sweep finds them", err)
	}

	// A map id that does not exist at all: same answer, same reason.
	if err := st.UnpinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: uuid.New(),
		SeatIdentities: seats, PinnedBy: "hold:ghost"}); !errors.Is(err, ErrSeatMapFamilyNotFound) {
		t.Fatalf("unpin against an unknown map = %v, want ErrSeatMapFamilyNotFound", err)
	}

	// And the sentinel is not covering a REAL release: pin, unpin with the right pair,
	// and the rows are gone with a nil error. Without this the distinction could be
	// bought by breaking the operation.
	if err := st.PinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: seats, PinnedBy: "hold:real"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UnpinSeats(ctx, BatchPinInput{OrganizerID: seatMapOrg, SeatMapID: v.ID,
		SeatIdentities: seats, PinnedBy: "hold:real"}); err != nil {
		t.Fatalf("a real unpin = %v, want nil", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM seat_map_pins WHERE pinned_by=$1`, "hold:real").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d pins remain after a real unpin, want 0", n)
	}
}
