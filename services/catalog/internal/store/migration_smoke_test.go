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
		_, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Down(ctx); err != nil {
			t.Fatalf("down: %v", err)
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
			"count_limited without max_entries":         {",starts_at,re_entry_mode", ",now(),'count_limited'"},
			"max_entries on single mode":                {",starts_at,re_entry_mode,max_entries", ",now(),'single',5"},
			"open closure carrying closed_at":           {",starts_at,closure_status,closed_at", ",now(),'open',now()"},
		}
		for name, cv := range bad {
			if err := ins(cv[0], cv[1]); err == nil {
				t.Fatalf("CHECK should have rejected: %s", name)
			}
		}
	})

	t.Run("rollback refuses non-performance slots without partial DDL", func(t *testing.T) {
		db, provider := newDB(t)
		if _, err := provider.Up(ctx); err != nil {
			t.Fatal(err)
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
		if _, err := provider.Down(ctx); err == nil {
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
		// The row is kind 'performance' (default) with starts_at set, so 0004's
		// down rolls back cleanly; the archived-row guard being asserted lives in
		// 0003, one migration further down.
		if _, err := provider.Down(ctx); err != nil {
			t.Fatalf("0004 down should succeed for a performance-kind row: %v", err)
		}
		if _, err := provider.Down(ctx); err == nil {
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
