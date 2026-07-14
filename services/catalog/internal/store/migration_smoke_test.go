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
		if _, err := provider.Down(ctx); err == nil {
			t.Fatal("down unexpectedly accepted archived row")
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
