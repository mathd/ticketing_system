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
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Default tenant seeded by migrations 0002 (organizer) and 0008 (venues).
var (
	seatMapOrg   = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	seatMapVenue = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
)

// seatMapSmokeStore migrates a fresh schema and returns the store plus the goose
// provider (the migration rollback-guard test needs DownTo).
func seatMapSmokeStore(t *testing.T) (context.Context, *sql.DB, *Postgres, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("CATALOG_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATALOG_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "catalog_seatmap_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, db, &Postgres{db: db}, provider
}

func seedDraftMap(ctx context.Context, t *testing.T, st *Postgres, name string) SeatMap {
	t.Helper()
	m, err := st.CreateSeatMap(ctx, SeatMapInput{OrganizerID: seatMapOrg, VenueID: seatMapVenue, Name: name})
	if err != nil {
		t.Fatalf("create seat map: %v", err)
	}
	return m
}

// TestSeatMapAuthoringRoundTrip is COS-1/COS-2 against real Postgres: author a
// map -> section -> rows -> seats, then read the geometry back nested + ordered,
// with the seat identity composed server-side. GA capacity is untouched.
func TestSeatMapAuthoringRoundTrip(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)

	m := seedDraftMap(ctx, t, st, "Main floor")
	if m.Version != 1 || m.Status != "draft" {
		t.Fatalf("new map must be draft v1, got v%d %q", m.Version, m.Status)
	}

	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Insert seats out of order; the read must sort by position.
	if _, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "2", Position: 2}); err != nil {
		t.Fatal(err)
	}
	s1, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "1", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if s1.SeatIdentity != "Orchestra/A/1" {
		t.Fatalf("identity must be section/row/seat, got %q", s1.SeatIdentity)
	}

	g, err := st.GetSeatMapGeometry(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Sections) != 1 || len(g.Sections[0].Rows) != 1 || len(g.Sections[0].Rows[0].Seats) != 2 {
		t.Fatalf("geometry shape wrong: %+v", g)
	}
	seats := g.Sections[0].Rows[0].Seats
	if seats[0].Position != 1 || seats[1].Position != 2 {
		t.Fatalf("seats must be position-ordered, got %d then %d", seats[0].Position, seats[1].Position)
	}
	if seats[0].SeatIdentity != "Orchestra/A/1" || seats[1].SeatIdentity != "Orchestra/A/2" {
		t.Fatalf("identities wrong: %q %q", seats[0].SeatIdentity, seats[1].SeatIdentity)
	}

	// COS-1: the venue keeps its GA capacity — the two coexist.
	var ga int
	if err := db.QueryRowContext(ctx, `SELECT ga_capacity FROM venues WHERE id=$1`, seatMapVenue).Scan(&ga); err != nil {
		t.Fatal(err)
	}
	if ga <= 0 {
		t.Fatalf("GA capacity must be untouched, got %d", ga)
	}
}

// TestSeatMapRejectsDuplicateIdentity: re-adding the same section/row/seat is a
// conflict (COS-5) — the composed identity collides on UNIQUE(seat_map_id,
// seat_identity), surfaced as ErrSeatMapConflict, not a raw 500-shaped error.
func TestSeatMapRejectsDuplicateIdentity(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedDraftMap(ctx, t, st, "Floor")
	sec, _ := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	row, _ := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1})
	if _, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "1", Position: 1}); err != nil {
		t.Fatal(err)
	}
	// Same label (=> same identity), different position so only the identity
	// constraint fires.
	_, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "1", Position: 2})
	if !errors.Is(err, ErrSeatMapConflict) {
		t.Fatalf("duplicate identity must be ErrSeatMapConflict, got %v", err)
	}
}

// TestSeatMapRejectsCrossMapParent: a section from another map cannot parent a
// row here — the parent-scoped INSERT ... SELECT matches no row (COS-5).
func TestSeatMapRejectsCrossMapParent(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	mapA := seedDraftMap(ctx, t, st, "A")
	mapB := seedDraftMap(ctx, t, st, "B")
	secA, _ := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: mapA.ID, Name: "Orchestra", Position: 1})
	_, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: mapB.ID, SectionID: secA.ID, Label: "A", Position: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-map section parent must be ErrNotFound, got %v", err)
	}
}

func TestCreateSeatMapUnknownVenue(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	_, err := st.CreateSeatMap(ctx, SeatMapInput{OrganizerID: seatMapOrg, VenueID: uuid.New(), Name: "Nope"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown venue must be ErrNotFound, got %v", err)
	}
}

// TestGetSeatMapGeometryIsSeatScoped is the ADR-019 two-part proof for the
// seat-level read: the result is scoped (round trip above) AND the scan is
// scoped — the shipped seatMapSeatsScopedQuery reaches seat_map_seats through
// seat_map_seats_by_map and never sequentially scans it, even once the table is
// large enough that a scan would be the wrong choice.
func TestGetSeatMapGeometryIsSeatScoped(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	m := seedDraftMap(ctx, t, st, "Target")
	sec, _ := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	row, _ := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1})
	for i := 1; i <= 3; i++ {
		if _, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: fmt.Sprint(i), Position: int32(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.GetSeatMapGeometry(ctx, m.ID); err != nil {
		t.Fatal(err)
	}

	// A plan assertion is only meaningful once a seq scan is the wrong choice:
	// seed many other maps, each carrying one seat that references this row (so
	// seat_map_id varies while the FK stays valid), and ANALYZE. The target
	// read stays selective, so an index scan must win.
	//
	// NB: this raw bulk insert deliberately pairs a bulk seat_map_id with the
	// target's row_id — a cross-map parentage NO API path allows (the store's
	// parent-scoped INSERT ... SELECT forbids it). It exists only to grow the
	// table so the planner rejects a seq scan; the target read filters
	// seat_map_id and never sees these rows. Do not copy this shape into a test
	// that exercises the authoring path.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO seat_maps(id,organizer_id,venue_id,name)
		SELECT gen_random_uuid(), $1, $2, 'bulk-'||g FROM generate_series(1,3000) g`,
		seatMapOrg, seatMapVenue); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO seat_map_seats(organizer_id,seat_map_id,row_id,seat_identity,label,position)
		SELECT $1, m.id, $2, 'bulk/'||m.id, 'x', 1000 + (row_number() OVER ())
		FROM seat_maps m WHERE m.name LIKE 'bulk-%'`,
		seatMapOrg, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE seat_map_seats`); err != nil {
		t.Fatal(err)
	}
	plan := explainGenericPlan(ctx, t, db, seatMapSeatsScopedQuery, m.ID)
	assertReachesVia(t, plan, "seat_map_seats", "seat_map_seats_by_map")
}

// TestSeatMapMigrationRollbackGuard: 0009 Down refuses to discard authored
// geometry — rolling back past it with a seat map present raises, mirroring
// 0003/0006's guards.
func TestSeatMapMigrationRollbackGuard(t *testing.T) {
	ctx, db, _, provider := seatMapSmokeStore(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO seat_maps(organizer_id,venue_id,name) VALUES($1,$2,'guard')`,
		seatMapOrg, seatMapVenue); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, versionBeforeSeatMaps); err == nil {
		t.Fatal("0009 down unexpectedly accepted seat-map data")
	}
	// The guard is all-or-nothing: the tables must survive the refused rollback.
	var seatMaps bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass(current_schema() || '.seat_maps') IS NOT NULL`).Scan(&seatMaps); err != nil {
		t.Fatal(err)
	}
	if !seatMaps {
		t.Fatal("refused 0009 down partially dropped the seat-map schema")
	}
}

// TestSeatMapOrphanPreventionSetting is TKT-179 / ADR-041: the rule is a per-VERSION
// setting on the map, defaulting off.
//
// Per version rather than per family because a published version is immutable and an
// edit mints a new one (ADR-029); a seated pool binds to one specific version, and
// that binding is what stops a republish changing the rule a live pool enforces.
func TestSeatMapOrphanPreventionSetting(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)

	// Default off: every map that existed before this column did behaves as it did.
	plain := seedDraftMap(ctx, t, st, "Plain")
	if plain.OrphanPreventionEnabled {
		t.Fatal("a new seat map must default to orphan prevention OFF")
	}
	var stored bool
	if err := db.QueryRowContext(ctx,
		`SELECT orphan_prevention_enabled FROM seat_maps WHERE id=$1`, plain.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored {
		t.Fatal("the column must default false")
	}

	enabled, err := st.CreateSeatMap(ctx, SeatMapInput{
		OrganizerID: seatMapOrg, VenueID: seatMapVenue, Name: "Strict", OrphanPreventionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.OrphanPreventionEnabled {
		t.Fatal("an explicitly enabled map must come back enabled")
	}
	// And the read path carries it, not just the create response. This is the read
	// inventory will use to project geometry (ADR-041), so the flag has to travel with
	// the map it belongs to.
	geo, err := st.GetSeatMapGeometry(ctx, enabled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !geo.Map.OrphanPreventionEnabled {
		t.Fatal("the geometry read must carry the setting — a value only the writer can see is not a setting")
	}
	if listed, lerr := st.ListVenueSeatMaps(ctx, seatMapVenue); lerr != nil {
		t.Fatal(lerr)
	} else {
		for _, m := range listed {
			if m.ID == enabled.ID && !m.OrphanPreventionEnabled {
				t.Fatal("the venue list must carry the setting too")
			}
		}
	}
}
