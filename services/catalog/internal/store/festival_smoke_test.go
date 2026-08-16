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
	go func() { defer wg.Done(); _, errs[0] = st.AttachDayToFestival(ctx, orgID, first.ID, dayID) }()
	go func() { defer wg.Done(); _, errs[1] = st.AttachDayToFestival(ctx, orgID, second.ID, dayID) }()
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
		if _, err = st.AttachDayToFestival(ctx, orgID, festival.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.PublishFestival(ctx, orgID, festival.ID); !errors.Is(err, ErrNotSellable) {
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
	if _, err = st.AttachDayToFestival(ctx, orgID, festival.ID, dayID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var publishErr, archiveErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, publishErr = st.PublishFestival(ctx, orgID, festival.ID) }()
	go func() { defer wg.Done(); _, archiveErr = st.ArchiveFestival(ctx, orgID, festival.ID) }()
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
	if _, err = st.AttachDayToFestival(ctx, orgID, festival.ID, dayID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var publishErr, archiveErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, publishErr = st.PublishFestival(ctx, orgID, festival.ID) }()
	go func() { defer wg.Done(); _, _, _, archiveErr = st.ArchivePerformance(ctx, orgID, dayID) }()
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
	// One statement per Exec: a multi-command string cannot carry $N parameters
	// (SQLSTATE 42601). This seed used to be a single multi-statement Exec, so the
	// test failed on its first line and had never passed — the -run allowlist in
	// scripts/smoke.sh meant it never ran to reveal that (fixed in TKT-60).
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizers(id,name) VALUES($1,'festival')`, []any{orgID}},
		{`INSERT INTO venues(id,organizer_id,name,ga_capacity) VALUES($1,$2,'v',250)`, []any{venueID, orgID}},
		{`INSERT INTO events(id,organizer_id,name) VALUES($1,$3,'{"en":"first","fr":"first"}'),($2,$3,'{"en":"second","fr":"second"}')`,
			[]any{firstEventID, secondEventID, orgID}},
		{`INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone) VALUES
			($1,$5,$6,$4,'festival_day',DATE '2026-08-02','12:00','23:00','America/Toronto'),
			($2,$5,$7,$4,'festival_day',DATE '2026-08-03','12:00','23:00','America/Toronto'),
			($3,$5,$7,$4,'festival_day',DATE '2026-08-01','12:00','23:00','America/Toronto')`,
			[]any{firstDayID, secondDayID, unrelatedID, venueID, orgID, firstEventID, secondEventID}},
		{`INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency) VALUES
			($1,$2,'{"en":"ga","fr":"ga"}',7500,'CAD'),
			($1,$3,'{"en":"ga","fr":"ga"}',7500,'CAD'),
			($1,$4,'{"en":"ga","fr":"ga"}',7500,'CAD')`,
			[]any{orgID, firstDayID, secondDayID, unrelatedID}},
	} {
		if _, err := db.ExecContext(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	festival, err := st.CreateFestival(ctx, FestivalInput{OrganizerID: orgID, Name: LocalizedText{"en": "f", "fr": "f"}, SharedCapacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{firstDayID, secondDayID} {
		if _, err = st.AttachDayToFestival(ctx, orgID, festival.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.PublishFestival(ctx, orgID, festival.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.PublishPerformance(ctx, orgID, unrelatedID); err != nil {
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

// seedPublishedFestival builds one festival owning a single sellable, published day
// and returns it. The day is the only row a scoped festival read may ever touch.
func seedPublishedFestival(ctx context.Context, t *testing.T, db *sql.DB, st *Postgres) (Festival, uuid.UUID) {
	t.Helper()
	orgID, _, _, dayID := seedFestivalDay(t, ctx, db, true)
	festival, err := st.CreateFestival(ctx, FestivalInput{
		OrganizerID: orgID, Name: LocalizedText{"en": "f", "fr": "f"}, SharedCapacity: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if festival, err = st.AttachDayToFestival(ctx, orgID, festival.ID, dayID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.PublishFestival(ctx, orgID, festival.ID); err != nil {
		t.Fatal(err)
	}
	return festival, dayID
}

// TestGetPublishedFestivalDoesNotScanForeignDays asserts RESULT scope — the first of
// the two claims ADR-019 rule 2 says a scoped read owes.
//
// The existing ordering test seeds an unattached day and excludes it, but that test is
// *about* chronology; exclusion is incidental to it and it asserts nothing if the read
// widens in some other direction. This one is about scope and nothing else.
//
// The poison is the foreign day's ticket_types.name: a schema-valid jsonb scalar
// ("not-an-object") that the column accepts but LocalizedText cannot decode. So a read
// that *scans* the foreign row errors, while a correctly scoped read never sees it —
// the asymmetry is what makes the test able to fail. Note the poison must sit on the
// ticket type, not the event: the festival query projects t.name and never joins
// events, so a poisoned event name would go unread and the test would pass vacuously.
func TestGetPublishedFestivalDoesNotScanForeignDays(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	festival, dayID := seedPublishedFestival(ctx, t, db, st)

	// A foreign published day, in no capacity group, owned by someone else.
	foreignOrg, _, _, foreignDayID := seedFestivalDay(t, ctx, db, true)
	if _, err := db.ExecContext(ctx,
		`UPDATE ticket_types SET name='"not-an-object"' WHERE performance_id=$1`, foreignDayID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PublishPerformance(ctx, foreignOrg, foreignDayID); err != nil {
		t.Fatal(err)
	}

	agg, err := st.GetPublishedFestival(ctx, festival.ID)
	if err != nil {
		t.Fatalf("festival read scanned a foreign day it should never have loaded: %v", err)
	}
	if len(agg.Performances) != 1 {
		t.Fatalf("day count = %d, want 1 (the festival's own day only)", len(agg.Performances))
	}
	if agg.Performances[0].Performance.ID != dayID {
		t.Fatalf("day = %s, want the festival's own %s", agg.Performances[0].Performance.ID, dayID)
	}
}

// TestGetPublishedFestivalIsIndexScoped asserts PHYSICAL scan cost — the second rule 2
// claim, and the one a poison row cannot make. A correct result can still be produced
// by reading every published slot and discarding rows; that is the TKT-53/TKT-60 defect,
// and it returns the right answer while doing it.
//
// It EXPLAINs publishedFestivalPerformancesQuery itself — the statement the shipped read
// executes — so a copy cannot silently drift away from production. What it asserts is
// narrower than "the read is scoped": that the plan reaches performances through the
// scoping index and does not sequentially scan performances, under the fixture's
// statistics and a blind plan. It is not a proof about every access path, and says
// nothing about production's data or plan choice (ADR-019 Consequences).
//
// The plan is asserted under force_generic_plan even though production runs auto. That is a
// robustness check on predicate shape, NOT a simulation of production — auto compares the
// generic plan's cost against the average custom plan's and may go on choosing custom plans
// forever, so this is not the plan production necessarily runs (ADR-019 Consequences).
//
// The reason to force it is that it is the only mode in which this assertion can fail when the
// predicate is wrong. A value-bound custom plan uses the index whether the predicate is
// `capacity_group_id = $1` or a nullable `(… OR $1 IS NULL)` — measured, on this very query —
// so a custom-plan assertion is green either way and proves nothing. Planning blind is what
// distinguishes the shapes, so planning blind is what this test does.
func TestGetPublishedFestivalIsIndexScoped(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	festival, dayID := seedPublishedFestival(ctx, t, db, st)

	// A plan assertion is only meaningful once a sequential scan is the *wrong* choice:
	// on a handful of rows Postgres rightly ignores any index and the assertion fails for
	// a reason unrelated to scoping. Seed enough published, ungrouped slots that scanning
	// them is the expensive option — which is also the only condition under which an
	// unscoped festival read actually hurts. Each carries a ticket type: without one the
	// planner may start from a tiny ticket_types relation and reach performances by
	// primary key, never consulting the scoping index, and the assertion would pass for
	// the wrong reason.
	if _, err := db.ExecContext(ctx, `
		WITH bulk AS (
			INSERT INTO performances(organizer_id,event_id,venue_id,kind,status,starts_at,timezone)
			SELECT p.organizer_id, p.event_id, p.venue_id, 'performance', 'published',
			       TIMESTAMPTZ '2026-10-01 20:00:00-04', 'America/Toronto'
			FROM performances p, generate_series(1,2000)
			WHERE p.id = $1
			RETURNING id, organizer_id
		)
		INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
		SELECT b.organizer_id, b.id, '{"en":"ga","fr":"ga"}', 5000, 'CAD' FROM bulk b`,
		dayID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE performances, ticket_types`); err != nil {
		t.Fatal(err)
	}

	plan := explainGenericPlan(ctx, t, db, publishedFestivalPerformancesQuery, festival.ID)

	// Only the relation carrying the scoping claim. The venue/ticket-type/sort access
	// paths may legitimately change without touching what this test is about.
	assertReachesVia(t, plan, "performances", "performances_capacity_group_idx")
}
