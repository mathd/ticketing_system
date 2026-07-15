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

func festivalSmokeStore(t *testing.T) (context.Context, *sql.DB, *Postgres) {
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
	schema := "catalog_festival_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	return ctx, db, NewPostgres(db)
}

func seedFestivalDay(t *testing.T, ctx context.Context, db *sql.DB, sellable bool) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	orgID, venueID, eventID, dayID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `WITH o AS (
		INSERT INTO organizers(id,name) VALUES($1,'festival') RETURNING id
	), v AS (
		INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',250 FROM o RETURNING id
	), e AS (
		INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
	)
	INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone)
	SELECT $4,o.id,e.id,v.id,'festival_day',DATE '2026-08-01','12:00','23:00','America/Toronto' FROM o,e,v`,
		orgID, venueID, eventID, dayID); err != nil {
		t.Fatal(err)
	}
	if sellable {
		if _, err := db.ExecContext(ctx, `INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
			VALUES($1,$2,'{"en":"ga","fr":"ga"}',7500,'CAD')`, orgID, dayID); err != nil {
			t.Fatal(err)
		}
	}
	return orgID, venueID, eventID, dayID
}

func TestConcurrentFestivalAttachChoosesOneGroup(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	orgID, _, _, dayID := seedFestivalDay(t, ctx, db, true)
	first, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "a", "fr": "a"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "b", "fr": "b"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = st.AttachDayToFestival(ctx, first.ID, dayID) }()
	go func() { defer wg.Done(); _, errs[1] = st.AttachDayToFestival(ctx, second.ID, dayID) }()
	wg.Wait()
	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrAlreadyGrouped) {
			t.Fatalf("attach error = %v, want ErrAlreadyGrouped", err)
		}
	}
	if winners != 1 {
		t.Fatalf("attach winners=%d errors=%v", winners, errs)
	}
}

func TestFestivalPublishPreflightRollsBackAllMembers(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	orgID, venueID, eventID, sellableID := seedFestivalDay(t, ctx, db, true)
	blockingID := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone)
		VALUES($1,$2,$3,$4,'festival_day',DATE '2026-08-02','12:00','23:00','America/Toronto')`, blockingID, orgID, eventID, venueID); err != nil {
		t.Fatal(err)
	}
	festival, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "f", "fr": "f"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{sellableID, blockingID} {
		if _, err = st.AttachDayToFestival(ctx, festival.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.PublishFestival(ctx, festival.ID); !errors.Is(err, ErrNotSellable) {
		t.Fatalf("publish error=%v, want ErrNotSellable", err)
	}
	var drafts int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM performances WHERE id IN ($1,$2) AND status='draft' AND published_at IS NULL`, sellableID, blockingID).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if drafts != 2 {
		t.Fatalf("draft members after failed preflight=%d", drafts)
	}
}

func TestFestivalPublishArchiveRaceIsConsistent(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	orgID, _, _, dayID := seedFestivalDay(t, ctx, db, true)
	festival, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "f", "fr": "f"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AttachDayToFestival(ctx, festival.ID, dayID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var publishErr, archiveErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, publishErr = st.PublishFestival(ctx, festival.ID) }()
	go func() { defer wg.Done(); _, archiveErr = st.ArchiveFestival(ctx, festival.ID) }()
	wg.Wait()
	if publishErr != nil {
		t.Fatalf("publish: %v", publishErr)
	}
	var festivalStatus, dayStatus string
	if err = db.QueryRowContext(ctx, `SELECT f.status,p.status FROM festivals f JOIN performances p ON p.capacity_group_id=f.id WHERE f.id=$1`, festival.ID).Scan(&festivalStatus, &dayStatus); err != nil {
		t.Fatal(err)
	}
	if archiveErr == nil {
		if festivalStatus != "archived" || dayStatus != "archived" {
			t.Fatalf("archive succeeded with festival=%s day=%s", festivalStatus, dayStatus)
		}
	} else if !errors.Is(archiveErr, ErrIllegalTransition) {
		t.Fatalf("archive error=%v, want nil or ErrIllegalTransition", archiveErr)
	} else if festivalStatus != "published" || dayStatus != "published" {
		t.Fatalf("archive rejected with festival=%s day=%s", festivalStatus, dayStatus)
	}
}

func TestDirectArchiveRacingFestivalPublishCannotDesync(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	orgID, _, _, dayID := seedFestivalDay(t, ctx, db, true)
	festival, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "f", "fr": "f"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AttachDayToFestival(ctx, festival.ID, dayID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var publishErr, archiveErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, publishErr = st.PublishFestival(ctx, festival.ID) }()
	go func() { defer wg.Done(); _, _, _, archiveErr = st.ArchivePerformance(ctx, dayID) }()
	wg.Wait()
	if publishErr != nil {
		t.Fatalf("festival publish: %v", publishErr)
	}
	if !errors.Is(archiveErr, ErrGroupedSlotLifecycle) {
		t.Fatalf("direct archive error=%v, want ErrGroupedSlotLifecycle", archiveErr)
	}
	var festivalStatus, dayStatus string
	if err = db.QueryRowContext(ctx, `SELECT f.status,p.status FROM festivals f JOIN performances p ON p.capacity_group_id=f.id WHERE f.id=$1`, festival.ID).Scan(&festivalStatus, &dayStatus); err != nil {
		t.Fatal(err)
	}
	if festivalStatus != "published" || dayStatus != "published" {
		t.Fatalf("festival=%s day=%s", festivalStatus, dayStatus)
	}
}

func TestGetPublishedFestivalOrdersDaysAcrossEventsChronologically(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	orgID, venueID := uuid.New(), uuid.New()
	firstEventID, secondEventID := uuid.New(), uuid.New()
	firstDayID, secondDayID, unrelatedID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO organizers(id,name) VALUES($1,'festival');
		INSERT INTO venues(id,organizer_id,name,ga_capacity) VALUES($2,$1,'v',250);
		INSERT INTO events(id,organizer_id,name) VALUES
			($3,$1,'{"en":"first","fr":"first"}'),
			($4,$1,'{"en":"second","fr":"second"}');
		INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone) VALUES
			($5,$1,$3,$2,'festival_day',DATE '2026-08-02','12:00','23:00','America/Toronto'),
			($6,$1,$4,$2,'festival_day',DATE '2026-08-03','12:00','23:00','America/Toronto'),
			($7,$1,$4,$2,'festival_day',DATE '2026-08-01','12:00','23:00','America/Toronto');
		INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency) VALUES
			($1,$5,'{"en":"ga","fr":"ga"}',7500,'CAD'),
			($1,$6,'{"en":"ga","fr":"ga"}',7500,'CAD'),
			($1,$7,'{"en":"ga","fr":"ga"}',7500,'CAD')`,
		orgID, venueID, firstEventID, secondEventID, firstDayID, secondDayID, unrelatedID); err != nil {
		t.Fatal(err)
	}
	festival, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "f", "fr": "f"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{firstDayID, secondDayID} {
		if _, err = st.AttachDayToFestival(ctx, festival.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.PublishFestival(ctx, festival.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.PublishPerformance(ctx, unrelatedID); err != nil {
		t.Fatal(err)
	}

	agg, err := st.GetPublishedFestival(ctx, festival.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agg.Performances) != 2 {
		t.Fatalf("festival day count = %d", len(agg.Performances))
	}
	if agg.Performances[0].Performance.ID != firstDayID || agg.Performances[1].Performance.ID != secondDayID {
		t.Fatalf("festival day order = %v", []uuid.UUID{agg.Performances[0].Performance.ID, agg.Performances[1].Performance.ID})
	}
}
