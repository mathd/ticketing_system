//go:build smoke

package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Rollback targets, named rather than counted. These tests assert that a
// specific migration's Down guard refuses to discard data, which means naming
// the version to roll back *to* — a bare provider.Down() pops whatever happens
// to be on top, so every new migration shifted these assertions onto the wrong
// one (TKT-60's 0007 index is what surfaced it: the guards silently started
// testing the migration below their target).
const (
	versionBeforeTypedSlot = 3 // roll 0004_typed_slot down
	versionBeforeSeries    = 4 // roll 0005_series_seasons down
	versionBeforeFestivals = 5 // roll 0006_festivals down
	versionBeforeArchived  = 2 // roll 0003_archived_performance_lifecycle down
)

func TestArchivedLifecycleMigrationRollbackGuard(t *testing.T) {
	dsn := os.Getenv("CATALOG_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATALOG_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	newDB := func(t *testing.T) (*sql.DB, *goose.Provider) {
		t.Helper()
		schema := "catalog_migration_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
		db, err := sql.Open("pgx", dsn+"?search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
		if err != nil {
			t.Fatal(err)
		}
		return db, provider
	}

	t.Run("rollback succeeds without archived rows", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		// Name the target: a bare Down() pops whichever migration happens to be
		// newest, so this case silently stopped exercising 0003 the moment 0007
		// was added — it was passing on an index drop. Roll all the way past
		// 0003 and assert its columns are gone, so "0003 rolls back cleanly"
		// is actually what fails when it doesn't.
		if _, err := provider.DownTo(ctx, versionBeforeArchived); err != nil {
			t.Fatalf("down to before 0003: %v", err)
		}
		var archivedCols int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name='performances'
			AND column_name IN ('archived_at','archive_emitted_at')`).Scan(&archivedCols); err != nil {
			t.Fatal(err)
		}
		if archivedCols != 0 {
			t.Fatalf("0003 down left %d archived-lifecycle column(s) behind", archivedCols)
		}
	})

	t.Run("festival migration installs capacity-group integrity constraints and index", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		var festivalTable, groupIndex, groupFK, groupKind, festivalTenantKey bool
		if err := db.QueryRowContext(ctx, `SELECT
			to_regclass(current_schema() || '.festivals') IS NOT NULL,
			to_regclass(current_schema() || '.performances_capacity_group_idx') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.table_constraints
			 WHERE constraint_schema=current_schema() AND table_name='performances'
			 AND constraint_name='performances_capacity_group_fk' AND constraint_type='FOREIGN KEY'),
			EXISTS (SELECT 1 FROM information_schema.table_constraints
			 WHERE constraint_schema=current_schema() AND table_name='performances'
			 AND constraint_name='performances_capacity_group_kind' AND constraint_type='CHECK'),
			EXISTS (SELECT 1 FROM information_schema.table_constraints
			 WHERE constraint_schema=current_schema() AND table_name='festivals'
			 AND constraint_name='festivals_id_organizer_unique' AND constraint_type='UNIQUE')`).
			Scan(&festivalTable, &groupIndex, &groupFK, &groupKind, &festivalTenantKey); err != nil {
			t.Fatal(err)
		}
		if !festivalTable || !groupIndex || !groupFK || !groupKind || !festivalTenantKey {
			t.Fatalf("festival migration: table=%v index=%v fk=%v kind_check=%v tenant_key=%v",
				festivalTable, groupIndex, groupFK, groupKind, festivalTenantKey)
		}
	})

	t.Run("festival rollback guard preserves schema", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		organizerID, venueID, eventID, festivalID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
		), v AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id
		), e AS (
			INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
		), f AS (
			INSERT INTO festivals(id,organizer_id,name,shared_capacity)
			SELECT $4,id,'{"en":"f","fr":"f"}',10 FROM o RETURNING id
		)
		INSERT INTO performances(organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone,capacity_group_id)
		SELECT $1,e.id,v.id,'festival_day',DATE '2026-07-04','10:00','18:00','UTC',f.id FROM e,v,f`,
			organizerID, venueID, eventID, festivalID); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DownTo(ctx, versionBeforeFestivals); err == nil {
			t.Fatal("0006 down unexpectedly accepted festival data")
		}
		var festivalTable, groupIndex, groupFK, groupKind bool
		if err := db.QueryRowContext(ctx, `SELECT
			to_regclass(current_schema() || '.festivals') IS NOT NULL,
			to_regclass(current_schema() || '.performances_capacity_group_idx') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.table_constraints
			 WHERE constraint_schema=current_schema() AND table_name='performances'
			 AND constraint_name='performances_capacity_group_fk'),
			EXISTS (SELECT 1 FROM information_schema.table_constraints
			 WHERE constraint_schema=current_schema() AND table_name='performances'
			 AND constraint_name='performances_capacity_group_kind')`).
			Scan(&festivalTable, &groupIndex, &groupFK, &groupKind); err != nil {
			t.Fatal(err)
		}
		if !festivalTable || !groupIndex || !groupFK || !groupKind {
			t.Fatalf("failed 0006 down partially dropped festival schema: table=%v index=%v fk=%v kind_check=%v",
				festivalTable, groupIndex, groupFK, groupKind)
		}
	})

	t.Run("festival membership requires matching organizer", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		festivalOrganizerID, memberOrganizerID := uuid.New(), uuid.New()
		venueID, eventID, festivalID := uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'festival'),($2,'member') RETURNING id
		), v AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity)
			SELECT $3,id,'v',10 FROM o WHERE id=$2 RETURNING id
		), e AS (
			INSERT INTO events(id,organizer_id,name)
			SELECT $4,id,'{"en":"e","fr":"e"}' FROM o WHERE id=$2 RETURNING id
		)
		INSERT INTO festivals(id,organizer_id,name,shared_capacity)
		SELECT $5,id,'{"en":"f","fr":"f"}',10 FROM o WHERE id=$1`,
			festivalOrganizerID, memberOrganizerID, venueID, eventID, festivalID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO performances
			(organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone,capacity_group_id)
			VALUES($1,$2,$3,'festival_day',DATE '2026-07-04','10:00','18:00','UTC',$4)`,
			memberOrganizerID, eventID, venueID, festivalID); err == nil {
			t.Fatal("capacity-group foreign key accepted a member from another organizer")
		}
	})

	t.Run("only festival days may carry a capacity group", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		organizerID, venueID, eventID, festivalID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
		), v AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id
		), e AS (
			INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
		)
		INSERT INTO festivals(id,organizer_id,name,shared_capacity)
		SELECT $4,id,'{"en":"f","fr":"f"}',10 FROM o`, organizerID, venueID, eventID, festivalID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO performances
			(organizer_id,event_id,venue_id,starts_at,timezone,capacity_group_id)
			VALUES($1,$2,$3,now(),'UTC',$4)`, organizerID, eventID, venueID, festivalID); err == nil {
			t.Fatal("capacity-group CHECK accepted a non-festival-day performance")
		}
	})

	t.Run("series and season rollback guard preserves schema", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		organizerID, eventID := uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
		)
		INSERT INTO events(id,organizer_id,name) SELECT $2,id,'{"en":"e","fr":"e"}' FROM o`, organizerID, eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO series(organizer_id,event_id,name) VALUES($1,$2,'{"en":"run","fr":"série"}')`, organizerID, eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DownTo(ctx, versionBeforeSeries); err == nil {
			t.Fatal("0005 down unexpectedly accepted series data")
		}
		var allTablesPresent bool
		if err := db.QueryRowContext(ctx, `SELECT count(*)=5 FROM information_schema.tables
			WHERE table_schema=current_schema() AND table_name IN ('series','series_performances','seasons','season_series','season_events')`).Scan(&allTablesPresent); err != nil {
			t.Fatal(err)
		}
		if !allTablesPresent {
			t.Fatal("failed 0005 down partially dropped grouping tables")
		}
	})

	t.Run("existing performance backfills to kind performance", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		organizerID, venueID, eventID := uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
		), v AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id
		), e AS (
			INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
		)
		INSERT INTO performances(organizer_id,event_id,venue_id,starts_at,timezone,status)
		SELECT $1,e.id,v.id,now(),'UTC','published' FROM e,v`, organizerID, venueID, eventID); err != nil {
			t.Fatal(err)
		}
		var kind, reMode, closure string
		if err := db.QueryRowContext(ctx,
			`SELECT kind, re_entry_mode, closure_status FROM performances`).Scan(&kind, &reMode, &closure); err != nil {
			t.Fatal(err)
		}
		if kind != "performance" || reMode != "single" || closure != "open" {
			t.Fatalf("backfill defaults wrong: kind=%q re=%q closure=%q", kind, reMode, closure)
		}
	})

	t.Run("typed-slot CHECK constraints reject invalid rows", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		organizerID, venueID, eventID := uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
		), v AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id
		)
		INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o`,
			organizerID, venueID, eventID); err != nil {
			t.Fatal(err)
		}
		ins := func(cols, vals string) error {
			_, err := db.ExecContext(ctx, `INSERT INTO performances
				(organizer_id,event_id,venue_id,timezone`+cols+`)
				VALUES ($1,$2,$3,'UTC'`+vals+`)`, organizerID, eventID, venueID)
			return err
		}
		bad := map[string][2]string{
			"performance carrying an operating window": {",kind,operating_date,opens_at,closes_at,starts_at", ",'performance',DATE '2026-07-04','10:00','18:00',now()"},
			"day kind carrying starts_at":              {",kind,operating_date,opens_at,closes_at,starts_at", ",'operating_day',DATE '2026-07-04','10:00','18:00',now()"},
			"count_limited without max_entries":        {",starts_at,re_entry_mode", ",now(),'count_limited'"},
			"max_entries on single mode":               {",starts_at,re_entry_mode,max_entries", ",now(),'single',5"},
			"open closure carrying closed_at":          {",starts_at,closure_status,closed_at", ",now(),'open',now()"},
		}
		for name, cv := range bad {
			if err := ins(cv[0], cv[1]); err == nil {
				t.Fatalf("CHECK should have rejected: %s", name)
			}
		}
	})

	t.Run("rollback refuses performance rows carrying typed-slot state", func(t *testing.T) {
		// Each case is an otherwise-baseline performance made non-pristine by
		// TKT-51 state that 0003's schema cannot hold; the down must refuse it
		// rather than silently drop the state.
		cases := []struct{ name, cols, vals string }{
			{"closed", ",closure_status,closed_at,closure_version,closure_changed_at", ",'closed',now(),1,now()"},
			{"non-default re-entry", ",re_entry_mode,requires_exit", ",'multi',true"},
			{"closure history after reopen", ",closure_version,closure_emitted_version", ",2,2"},
			{"capacity group", ",capacity_group_id", ",gen_random_uuid()"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				db, provider := newDB(t)
				if _, err := provider.Up(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := provider.DownTo(ctx, versionBeforeSeries+1); err != nil { // down to 0005 applied
					t.Fatalf("down to 0005: %v", err)
				}
				organizerID, venueID, eventID := uuid.New(), uuid.New(), uuid.New()
				if _, err := db.ExecContext(ctx, `WITH o AS (
					INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
				), v AS (
					INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id
				), e AS (
					INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
				)
				INSERT INTO performances(organizer_id,event_id,venue_id,timezone,starts_at`+c.cols+`)
				SELECT $1,e.id,v.id,'UTC',now()`+c.vals+` FROM e,v`, organizerID, venueID, eventID); err != nil {
					t.Fatal(err)
				}
				if _, err := provider.DownTo(ctx, versionBeforeTypedSlot); err == nil {
					t.Fatalf("down unexpectedly accepted a performance carrying %s state", c.name)
				}
				var kindCol bool
				if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
					WHERE table_schema=current_schema() AND table_name='performances' AND column_name='kind')`).Scan(&kindCol); err != nil {
					t.Fatal(err)
				}
				if !kindCol {
					t.Fatal("failed down partially dropped the typed-slot columns")
				}
			})
		}
	})

	t.Run("rollback refuses non-performance slots without partial DDL", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DownTo(ctx, versionBeforeSeries+1); err != nil { // down to 0005 applied
			t.Fatalf("down to 0005: %v", err)
		}
		organizerID, venueID, eventID := uuid.New(), uuid.New(), uuid.New()
		// an operating_day slot: no starts_at, carries the operating window.
		if _, err := db.ExecContext(ctx, `WITH o AS (
			INSERT INTO organizers(id,name) VALUES($1,'m') RETURNING id
		), v AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id
		), e AS (
			INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
		)
		INSERT INTO performances(organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone,status)
		SELECT $1,e.id,v.id,'operating_day',DATE '2026-07-04','10:00','18:00','America/Toronto','published' FROM e,v`,
			organizerID, venueID, eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DownTo(ctx, versionBeforeTypedSlot); err == nil {
			t.Fatal("down unexpectedly accepted a non-performance slot")
		}
		var kindCol bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name='performances' AND column_name='kind')`).Scan(&kindCol); err != nil {
			t.Fatal(err)
		}
		if !kindCol {
			t.Fatal("failed down partially dropped the typed-slot columns")
		}
	})

	t.Run("rollback refuses archived rows without partial DDL", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		organizerID, venueID, eventID := uuid.New(), uuid.New(), uuid.New()
		_, err := db.ExecContext(ctx, `WITH inserted_organizer AS (
			INSERT INTO organizers(id,name) VALUES($1,'migration test') RETURNING id
		), inserted_venue AS (
			INSERT INTO venues(id,organizer_id,name,ga_capacity)
			SELECT $2,id,'venue',1 FROM inserted_organizer RETURNING id
		), inserted_event AS (
			INSERT INTO events(id,organizer_id,name)
			SELECT $3,id,'{"en":"event","fr":"event"}' FROM inserted_organizer RETURNING id
		)
		INSERT INTO performances(organizer_id,event_id,venue_id,starts_at,timezone,status,archived_at)
		SELECT $1,inserted_event.id,inserted_venue.id,now(),'UTC','archived',now()
		FROM inserted_event, inserted_venue`, organizerID, venueID, eventID)
		if err != nil {
			t.Fatal(err)
		}
		// Everything above 0003 has no data that its guard objects to (no grouping
		// rows; the row is kind 'performance' with starts_at set), so those roll
		// back cleanly. The archived-row guard being asserted lives in 0003.
		if _, err := provider.DownTo(ctx, versionBeforeArchived+1); err != nil {
			t.Fatalf("down to 0003 applied should succeed: %v", err)
		}
		if _, err := provider.DownTo(ctx, versionBeforeArchived); err == nil {
			t.Fatal("0003 down unexpectedly accepted archived row")
		}
		var archivedAt, archiveEmittedAt bool
		if err := db.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='performances' AND column_name='archived_at'),
			EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='performances' AND column_name='archive_emitted_at')`).Scan(&archivedAt, &archiveEmittedAt); err != nil {
			t.Fatal(err)
		}
		if !archivedAt || !archiveEmittedAt {
			t.Fatal("failed down partially dropped archived lifecycle columns")
		}
	})
}
