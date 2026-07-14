//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
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

// TestArchiveDoesNotRacePublish proves the fix for the check-then-act race in
// ArchivePerformance: archiving is decided under a FOR UPDATE lock in the same
// transaction as the transition, so a concurrent draft->publish can never let
// archive succeed against a still-published row (which would emit a phantom
// archive event with a nil-timestamp id). Regardless of who wins the lock, the
// archive call stays internally consistent: it either rejects a draft or
// archives a genuinely-published row.
func TestArchiveDoesNotRacePublish(t *testing.T) {
	dsn := os.Getenv("CATALOG_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATALOG_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	// Run the race many times so the interleaving is exercised, not assumed.
	for i := 0; i < 30; i++ {
		schema := "catalog_race_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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

		// Seed a DRAFT performance with a ticket type (so publish can succeed).
		orgID, venueID, eventID, perfID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `
			WITH o AS (INSERT INTO organizers(id,name) VALUES($1,'race') RETURNING id),
			     v AS (INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id),
			     e AS (INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id),
			     p AS (INSERT INTO performances(id,organizer_id,event_id,venue_id,starts_at,timezone,status)
			           SELECT $4,o.id,e.id,v.id,now(),'UTC','draft' FROM o,e,v RETURNING id,organizer_id)
			INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
			SELECT p.organizer_id,p.id,'{"en":"ga","fr":"ga"}',1000,'EUR' FROM p`,
			orgID, venueID, eventID, perfID); err != nil {
			t.Fatal(err)
		}

		st := NewPostgres(db)
		var (
			wg          sync.WaitGroup
			archivePerf Performance
			archiveErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = st.PublishPerformance(ctx, perfID)
		}()
		go func() {
			defer wg.Done()
			archivePerf, _, _, archiveErr = st.ArchivePerformance(ctx, perfID)
		}()
		wg.Wait()

		var finalStatus string
		var archivedAt sql.NullTime
		if err := db.QueryRowContext(ctx,
			`SELECT status, archived_at FROM performances WHERE id=$1`, perfID).Scan(&finalStatus, &archivedAt); err != nil {
			t.Fatal(err)
		}
		switch {
		case archiveErr == nil:
			// Archive reported success: it must be a real archived transition,
			// never a phantom archive of a published/draft row.
			if archivePerf.Status != "archived" || archivePerf.ArchivedAt == nil {
				t.Fatalf("iter %d: archive success but returned perf status=%s archived_at=%v (phantom archive)",
					i, archivePerf.Status, archivePerf.ArchivedAt)
			}
			if finalStatus != "archived" || !archivedAt.Valid {
				t.Fatalf("iter %d: archive success but row status=%s archived_at.valid=%v", i, finalStatus, archivedAt.Valid)
			}
		case errors.Is(archiveErr, ErrIllegalTransition):
			// Archive lost the lock to a still-draft state (publish had not yet
			// committed): the row must NOT be archived.
			if finalStatus == "archived" {
				t.Fatalf("iter %d: archive rejected as draft but row is archived", i)
			}
		default:
			t.Fatalf("iter %d: archive error = %v, want nil or ErrIllegalTransition", i, archiveErr)
		}

		_ = db.Close()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSeriesArchiveDoesNotDeadlockDirectArchive exercises ADR-015's shared-lock
// argument against real PostgreSQL: the series path owns the series row and
// locks two slot rows in UUID order, while the direct path locks only one of
// those slots. Either path may win that slot, but no lock cycle can form and
// both idempotent archive calls must complete with the whole run archived.
func TestSeriesArchiveDoesNotDeadlockDirectArchive(t *testing.T) {
	dsn := os.Getenv("CATALOG_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATALOG_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	schema := "catalog_series_race_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") }()
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	st := NewPostgres(db)

	for i := 0; i < 20; i++ {
		orgID, venueID, eventID := uuid.New(), uuid.New(), uuid.New()
		firstID, secondID, seriesID := uuid.New(), uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `
			WITH o AS (INSERT INTO organizers(id,name) VALUES($1,'series race') RETURNING id),
			     v AS (INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id),
			     e AS (INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id),
			     p1 AS (INSERT INTO performances(id,organizer_id,event_id,venue_id,starts_at,timezone,status,published_at,event_emitted_at)
			            SELECT $4,o.id,e.id,v.id,now(),'UTC','published',now(),now() FROM o,e,v RETURNING id),
			     p2 AS (INSERT INTO performances(id,organizer_id,event_id,venue_id,starts_at,timezone,status,published_at,event_emitted_at)
			            SELECT $5,o.id,e.id,v.id,now() + interval '1 day','UTC','published',now(),now() FROM o,e,v RETURNING id),
			     s AS (INSERT INTO series(id,organizer_id,event_id,name) SELECT $6,o.id,e.id,'{"en":"run","fr":"série"}' FROM o,e RETURNING id)
			INSERT INTO series_performances(series_id,performance_id,position)
			SELECT s.id,p1.id,1 FROM s,p1 UNION ALL SELECT s.id,p2.id,2 FROM s,p2`,
			orgID, venueID, eventID, firstID, secondID, seriesID); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var seriesErr, directErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, seriesErr = st.ArchiveSeries(ctx, seriesID)
		}()
		go func() {
			defer wg.Done()
			_, _, _, directErr = st.ArchivePerformance(ctx, secondID)
		}()
		wg.Wait()
		if seriesErr != nil || directErr != nil {
			t.Fatalf("iter %d: series archive=%v direct archive=%v", i, seriesErr, directErr)
		}
		var archived int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM performances WHERE id IN ($1,$2) AND status='archived' AND archived_at IS NOT NULL`, firstID, secondID).Scan(&archived); err != nil {
			t.Fatal(err)
		}
		if archived != 2 {
			t.Fatalf("iter %d: archived members=%d, want 2", i, archived)
		}
	}
}
