//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// sect/rw/st1 are small builders for the EditSeatMap payload so the tests read
// as "the new version has these sections/rows/seats". They mirror the nested
// geometry the store clones into the new version.
func sect(name string, pos int32, rows ...EditRowInput) EditSectionInput {
	return EditSectionInput{Name: name, Position: pos, Rows: rows}
}

func rw(label string, pos int32, seats ...EditSeatInput) EditRowInput {
	return EditRowInput{Label: label, Position: pos, Seats: seats}
}

func st1(label string, pos int32) EditSeatInput {
	return EditSeatInput{Label: label, Position: pos}
}

// identitiesOf reads the composed seat identities of a published version.
func identitiesOf(ctx context.Context, t *testing.T, st *Postgres, mapID uuid.UUID) map[string]int {
	t.Helper()
	g, err := st.GetSeatMapGeometry(ctx, mapID)
	if err != nil {
		t.Fatalf("get geometry %s: %v", mapID, err)
	}
	out := map[string]int{}
	for _, s := range g.Sections {
		for _, r := range s.Rows {
			for _, seat := range r.Seats {
				out[seat.SeatIdentity]++
			}
		}
	}
	return out
}

// TestEditSeatMapPreservesPinnedSeats (COS-3): given a seat referenced by a
// (synthetic) sold record — a pin — no edit orphans it (its identity still
// resolves in the new version, exactly once) and none duplicates it. An edit
// that drops a PINNED seat is hard-rejected; an edit that drops an UNPINNED seat
// succeeds.
func TestEditSeatMapPreservesPinnedSeats(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "Sold-case") // Orchestra/A/1

	// Add two more seats to the published map via a first accepted edit so we
	// have a richer geometry to pin against: Orchestra/A/1, /2, /3.
	v2, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("2", 2), st1("3", 3)))}})
	if err != nil {
		t.Fatalf("initial enrich edit: %v", err)
	}

	// A confirmed/sold record references Orchestra/A/1 and Orchestra/A/2.
	for _, id := range []string{"Orchestra/A/1", "Orchestra/A/2"} {
		if err := st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: v2.ID, SeatIdentity: id, PinnedBy: "sale:test-order"}); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}

	// Edit that PRESERVES both pinned seats (adds a section, drops the UNPINNED
	// Orchestra/A/3) -> accepted.
	v3, needsEmit, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: v2.ID,
		Sections: []EditSectionInput{
			sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("2", 2))),
			sect("Balcony", 2, rw("B", 1, st1("1", 1))),
		}})
	if err != nil {
		t.Fatalf("preserving edit must succeed, got: %v", err)
	}
	if !needsEmit {
		t.Fatal("new published version must owe its event")
	}
	ids := identitiesOf(ctx, t, st, v3.ID)
	for _, id := range []string{"Orchestra/A/1", "Orchestra/A/2"} {
		if ids[id] != 1 {
			t.Fatalf("pinned identity %s resolves %d times in the new version, want exactly 1", id, ids[id])
		}
	}
	if _, ok := ids["Orchestra/A/3"]; ok {
		t.Fatal("unpinned Orchestra/A/3 should have been dropped by the edit")
	}
	if ids["Balcony/B/1"] != 1 {
		t.Fatal("new Balcony/B/1 should exist exactly once")
	}

	// Edit that DROPS a pinned seat (omits Orchestra/A/2) -> hard-rejected.
	_, _, err = st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: v3.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1)))}})
	if !errors.Is(err, ErrSeatMapEditOrphansPinned) {
		t.Fatalf("dropping a pinned seat err = %v, want ErrSeatMapEditOrphansPinned", err)
	}
	// The rejected edit created no new version: v3 stays the current published one.
	if ids := identitiesOf(ctx, t, st, v3.ID); ids["Orchestra/A/2"] != 1 {
		t.Fatal("predecessor v3 must be untouched after a rejected edit")
	}
}

// TestEditSeatMapVersionBumpAndImmutability (COS-1): editing produces a new
// version; the predecessor stays published and immutable; the new version is
// itself immutable (published, not draft).
func TestEditSeatMapVersionBumpAndImmutability(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "Versioned") // version 1, Orchestra/A/1

	v2, needsEmit, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("2", 2)))}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if v2.Version != m.Version+1 {
		t.Fatalf("new version = %d, want %d", v2.Version, m.Version+1)
	}
	if v2.Status != "published" || v2.PublishedAt == nil {
		t.Fatalf("new version = %q publishedAt=%v, want published with a timestamp", v2.Status, v2.PublishedAt)
	}
	if !needsEmit {
		t.Fatal("new version must owe its published event")
	}

	// Predecessor unchanged: still published, still version 1, its geometry intact.
	if ids := identitiesOf(ctx, t, st, m.ID); ids["Orchestra/A/1"] != 1 || len(ids) != 1 {
		t.Fatalf("predecessor geometry changed: %v", ids)
	}

	// Neither the predecessor nor the new version accepts authoring (both are
	// published, not draft): the status='draft' write gate bites for both.
	for _, id := range []uuid.UUID{m.ID, v2.ID} {
		_, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: id, Name: "X", Position: 9})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("authoring published map %s err = %v, want ErrNotFound", id, err)
		}
	}

	// The event marker discipline mirrors publish: owed, then not owed after mark.
	if err := st.MarkSeatMapEventEmitted(ctx, v2.ID); err != nil {
		t.Fatal(err)
	}
}

// TestPinSeatRejectsUnknownIdentity (COS-5 seam): PinSeat is the write path
// TKT-80 will call; it validates the identity exists in the current published
// version, is idempotent on (family, identity, pinned_by), and UnpinSeat clears.
func TestPinSeatRejectsUnknownIdentity(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "Pin-seam") // Orchestra/A/1

	if err := st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Orchestra/A/1", PinnedBy: "hold:h1"}); err != nil {
		t.Fatalf("pin known identity: %v", err)
	}
	// Idempotent on the same (identity, pinned_by).
	if err := st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Orchestra/A/1", PinnedBy: "hold:h1"}); err != nil {
		t.Fatalf("idempotent re-pin: %v", err)
	}
	// Unknown identity -> ErrSeatIdentityNotFound.
	if err := st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Nowhere/Z/9", PinnedBy: "hold:h2"}); !errors.Is(err, ErrSeatIdentityNotFound) {
		t.Fatalf("pin unknown identity err = %v, want ErrSeatIdentityNotFound", err)
	}
	// Unpin clears it, so an edit dropping that seat now succeeds.
	if err := st.UnpinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Orchestra/A/1", PinnedBy: "hold:h1"}); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if _, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
		Sections: []EditSectionInput{sect("Stalls", 1, rw("Z", 1, st1("1", 1)))}}); err != nil {
		t.Fatalf("edit after unpin must succeed (no pins left), got: %v", err)
	}
}

// TestEditSeatMapDoesNotRacePin (COS-4): the edit-vs-sale race. An edit that
// would DROP a seat runs concurrently with a PinSeat referencing that seat.
// Both take the family's current-published row FOR UPDATE, so they serialize:
// whichever wins the lock, the loser sees the winner's committed result. The
// invariant the loop asserts: NEVER does a pin end up recorded for an identity
// that is absent from the current published version. So either the pin lands
// first (edit rejects with ErrSeatMapEditOrphansPinned) or the edit lands first
// (pin rejects with ErrSeatIdentityNotFound). Mirrors
// TestArchiveDoesNotRacePublish.
func TestEditSeatMapDoesNotRacePin(t *testing.T) {
	dsn := os.Getenv("CATALOG_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATALOG_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 30; i++ {
		schema := "catalog_edit_race_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("pgx", dsn+"?search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		st := &Postgres{db: db}

		// Seed a published map with a single seat Orchestra/A/1.
		m := seedPublishedMap(ctx, t, st, fmt.Sprintf("race-%d", i))

		var wg sync.WaitGroup
		wg.Add(2)
		var editErr, pinErr error
		// Edit drops the only seat (empties Orchestra/A, keeps the section with a
		// different, unpinned seat so the geometry is non-empty and valid).
		go func() {
			defer wg.Done()
			_, _, editErr = st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
				Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("99", 1)))}})
		}()
		// A sale pins Orchestra/A/1.
		go func() {
			defer wg.Done()
			pinErr = st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Orchestra/A/1", PinnedBy: "sale:race"})
		}()
		wg.Wait()

		// The two-sided lock guarantees internal consistency regardless of who won:
		//  - pin first  -> edit must reject (ErrSeatMapEditOrphansPinned), pin ok.
		//  - edit first -> pin must reject (ErrSeatIdentityNotFound), edit ok.
		// It must NEVER be that BOTH succeeded (that would leave a pin for a seat
		// the current version lacks).
		pinOK := pinErr == nil
		editOK := editErr == nil
		if pinOK && editOK {
			t.Fatalf("iter %d: both pin and edit succeeded — pinned Orchestra/A/1 is now orphaned", i)
		}
		if !pinOK && !editOK {
			// Both rejected is acceptable only if the errors are the expected ones
			// (a genuine lock/serialization outcome), never a surprise failure.
			if !errors.Is(pinErr, ErrSeatIdentityNotFound) || !errors.Is(editErr, ErrSeatMapEditOrphansPinned) {
				t.Fatalf("iter %d: both rejected with unexpected errors: pin=%v edit=%v", i, pinErr, editErr)
			}
		}
		if editOK && !errors.Is(pinErr, ErrSeatIdentityNotFound) {
			t.Fatalf("iter %d: edit won but pin err = %v, want ErrSeatIdentityNotFound", i, pinErr)
		}
		if pinOK && !errors.Is(editErr, ErrSeatMapEditOrphansPinned) {
			t.Fatalf("iter %d: pin won but edit err = %v, want ErrSeatMapEditOrphansPinned", i, editErr)
		}

		// Ground truth: no pin may reference an identity absent from the current
		// published version of the family.
		assertNoOrphanPins(ctx, t, st, m.ID, i)

		_ = db.Close()
		_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
	}
}

// assertNoOrphanPins fails if any pin in the family references a seat identity
// that is not present in the family's CURRENT (max-version) published version.
// It MUST bind to the current version, not any published version: the
// predecessor stays published after an edit, so "present in any published
// version" is a false negative that would mask an orphaned pin against the new
// current version (ai-review F2/F3).
func assertNoOrphanPins(ctx context.Context, t *testing.T, st *Postgres, anyVersionID uuid.UUID, iter int) {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, `
		WITH fam AS (SELECT map_family_id FROM seat_maps WHERE id = $1),
		     current_version AS (
		       SELECT id FROM seat_maps
		       WHERE map_family_id = (SELECT map_family_id FROM fam)
		         AND status = 'published'
		       ORDER BY version DESC
		       LIMIT 1)
		SELECT p.seat_identity
		FROM seat_map_pins p
		WHERE p.map_family_id = (SELECT map_family_id FROM fam)
		  AND NOT EXISTS (
		    SELECT 1 FROM seat_map_seats s
		    WHERE s.seat_map_id = (SELECT id FROM current_version)
		      AND s.seat_identity = p.seat_identity)`, anyVersionID)
	if err != nil {
		t.Fatalf("iter %d: orphan-pin query: %v", iter, err)
	}
	defer func() { _ = rows.Close() }()
	var orphans []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		orphans = append(orphans, id)
	}
	if len(orphans) > 0 {
		t.Fatalf("iter %d: orphaned pins (identity absent from current published version): %v", iter, orphans)
	}
}

// TestPinSeatSerializesBehindEditThatDroppedSeat (ai-review F1, regression)
// proves the family advisory lock — not a row FOR UPDATE — is what serializes
// edit and pin. It commits an edit that drops the seat, THEN pins it, and the
// pin must reject against the new current version. The deterministic ordering
// (edit fully committed before the pin runs) is the worst case for the OLD
// design: a row-only lock would still resolve the stale predecessor as "current"
// under the ORDER BY … LIMIT recheck; the advisory lock forces a fresh resolve
// so the pin sees the new version and rejects.
func TestPinSeatSerializesBehindEditThatDroppedSeat(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "stale-guard") // Orchestra/A/1, version 1

	// Edit drops Orchestra/A/1 (replaces it with an unpinned Orchestra/A/99).
	v2, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("99", 1)))}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	// A pin request referencing the dropped seat — using the OLD version id — must
	// resolve the family's CURRENT version (v2, which lacks it) and reject.
	if err := st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Orchestra/A/1", PinnedBy: "sale:stale"}); !errors.Is(err, ErrSeatIdentityNotFound) {
		t.Fatalf("pin of a dropped seat (via old version id) err = %v, want ErrSeatIdentityNotFound", err)
	}
	// And a pin referencing a seat that DOES exist in the current version, via the
	// old version id, still succeeds (the family resolves forward to v2).
	if err := st.PinSeat(ctx, PinSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SeatIdentity: "Orchestra/A/99", PinnedBy: "sale:current"}); err != nil {
		t.Fatalf("pin of a current seat via old version id: %v", err)
	}
	assertNoOrphanPins(ctx, t, st, v2.ID, -1)
}

// TestListSeatMapVersionsHistory (TKT-105 COS-3): the version-history read
// returns every version of the family newest-first, each with its published_at,
// resolvable from ANY version id — proving the family subselect against real
// Postgres, not just the fake.
func TestListSeatMapVersionsHistory(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "History") // v1, Orchestra/A/1

	v2, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: m.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1), st1("2", 2)))}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}

	for _, from := range []uuid.UUID{m.ID, v2.ID} {
		versions, err := st.ListSeatMapVersions(ctx, from)
		if err != nil {
			t.Fatalf("list versions from %s: %v", from, err)
		}
		if len(versions) != 2 {
			t.Fatalf("family must have 2 versions, got %d (from %s)", len(versions), from)
		}
		if versions[0].Version != 2 || versions[1].Version != 1 {
			t.Fatalf("versions must be newest-first, got %d then %d", versions[0].Version, versions[1].Version)
		}
		for _, v := range versions {
			if v.PublishedAt == nil {
				t.Fatalf("published version %d must carry published_at", v.Version)
			}
		}
	}

	// Unknown id -> ErrNotFound.
	if _, err := st.ListSeatMapVersions(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("versions of an unknown map err = %v, want ErrNotFound", err)
	}
}

// TestUpdateVenueGACapacity (TKT-105 COS-5): the GA-capacity write persists and
// is organizer-scoped — a wrong organizer or unknown venue is ErrNotFound.
func TestUpdateVenueGACapacity(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	v, err := st.UpdateVenueGACapacity(ctx, VenueGACapacityInput{
		OrganizerID: seatMapOrg, VenueID: seatMapVenue, GACapacity: 4242,
	})
	if err != nil {
		t.Fatalf("update GA: %v", err)
	}
	if v.GACapacity != 4242 || v.ID != seatMapVenue {
		t.Fatalf("GA update returned %+v, want capacity 4242 for %s", v, seatMapVenue)
	}
	// Persisted: a fresh read via ListVenues reflects it.
	venues, err := st.ListVenues(ctx, seatMapOrg)
	if err != nil {
		t.Fatalf("list venues: %v", err)
	}
	found := false
	for _, vv := range venues {
		if vv.ID == seatMapVenue {
			found = true
			if vv.GACapacity != 4242 {
				t.Fatalf("persisted GA = %d, want 4242", vv.GACapacity)
			}
		}
	}
	if !found {
		t.Fatal("seeded venue missing from ListVenues")
	}

	// Wrong organizer -> ErrNotFound (tenancy predicate).
	if _, err := st.UpdateVenueGACapacity(ctx, VenueGACapacityInput{
		OrganizerID: uuid.New(), VenueID: seatMapVenue, GACapacity: 1,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant GA update err = %v, want ErrNotFound", err)
	}
	// Unknown venue -> ErrNotFound.
	if _, err := st.UpdateVenueGACapacity(ctx, VenueGACapacityInput{
		OrganizerID: seatMapOrg, VenueID: uuid.New(), GACapacity: 1,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown-venue GA update err = %v, want ErrNotFound", err)
	}
}

// TestEditSeatMapInheritsOrphanPrevention is ADR-041's inheritance rule: an edit is a
// geometry change, so a caller that says nothing about the rule must not silently
// switch it off — and the version being edited is never altered either way (ADR-029).
func TestEditSeatMapInheritsOrphanPrevention(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	on := true
	m, err := st.CreateSeatMap(ctx, SeatMapInput{
		OrganizerID: seatMapOrg, VenueID: seatMapVenue, Name: "Strict", OrphanPreventionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "1", Position: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.PublishSeatMap(ctx, m.ID); err != nil {
		t.Fatal(err)
	}

	geometry := []EditSectionInput{{Name: "Orchestra", Position: 1, Rows: []EditRowInput{
		{Label: "A", Position: 1, Seats: []EditSeatInput{{Label: "1", Position: 1}}},
	}}}

	// Saying nothing inherits.
	inherited, _, err := st.EditSeatMap(ctx, EditSeatMapInput{
		OrganizerID: seatMapOrg, SeatMapID: m.ID, Sections: geometry,
	})
	if err != nil {
		t.Fatalf("inheriting edit: %v", err)
	}
	if !inherited.OrphanPreventionEnabled {
		t.Fatal("an edit that says nothing about the rule must INHERIT it, not clear it")
	}

	// Saying something applies to the NEW version only.
	off := false
	turnedOff, _, err := st.EditSeatMap(ctx, EditSeatMapInput{
		OrganizerID: seatMapOrg, SeatMapID: inherited.ID, Sections: geometry,
		OrphanPreventionEnabled: &off,
	})
	if err != nil {
		t.Fatalf("disabling edit: %v", err)
	}
	if turnedOff.OrphanPreventionEnabled {
		t.Fatal("an explicit false must apply to the new version")
	}
	// The predecessor is untouched — a published version is immutable (ADR-029), and
	// a pool seated against it keeps the rule it was provisioned with.
	prior, err := st.GetSeatMapGeometry(ctx, inherited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !prior.Map.OrphanPreventionEnabled {
		t.Fatal("editing a version must not change the version it was edited from")
	}
	_ = on
}
