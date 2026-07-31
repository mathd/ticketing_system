//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// assertReachesVia asserts a plan reaches relation through index and never
// sequentially scans relation.
//
// Both halves are needed. Asserting the index name appears *somewhere* in the plan
// text is weaker than it looks: a regressed query can use the index in one branch
// and still scan the whole relation in another (an Append/UNION arm, say), which is
// precisely the catalog-wide read the assertion claims to exclude — and the test
// would stay green through it. Matching "Seq Scan on <relation>" also catches
// "Parallel Seq Scan on <relation>".
//
// This still asserts on plan *text*, so it is not a proof that every access path to
// the relation is scoped — an exhaustive check would walk EXPLAIN (FORMAT JSON)
// nodes. It is the invariant these tests actually claim; do not describe it as more.
func assertReachesVia(t *testing.T, plan, relation, index string) {
	t.Helper()
	if !strings.Contains(plan, index) {
		t.Fatalf("plan does not reach %s through %s — it scans.\nplan:\n%s", relation, index, plan)
	}
	if strings.Contains(plan, "Seq Scan on "+relation) {
		t.Fatalf("plan sequentially scans %s even though %s appears in it.\nplan:\n%s", relation, index, plan)
	}
}

// planProbeSeq names each PREPARE uniquely. A prepared statement outlives the
// rolled-back transaction that created it, so a fixed name makes a second
// explainGenericPlan call on the same pooled connection die with
// `prepared statement "plan_probe" already exists` (SQLSTATE 42P05) — a confusing
// failure a long way from its cause. DEALLOCATE ALL would fix it and also discard
// the driver's own cached statements, so: a fresh name per call.
var planProbeSeq atomic.Uint64

func seasonSmokeStore(t *testing.T) (context.Context, *sql.DB, *Postgres) {
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
	schema := "catalog_season_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	return ctx, db, &Postgres{db: db}
}

// seedSeasonWithForeignEvent builds a season owning one published event, plus a
// foreign published event belonging to a different organizer. The foreign
// event's name is valid jsonb but a scalar, not an object: the schema accepts
// it (name is jsonb NOT NULL), yet decoding it into LocalizedText fails. So any
// read that *scans* the foreign row errors out, while a correctly scoped read
// never touches it. That asymmetry is what makes this test red before the fix.
func seedSeasonWithForeignEvent(ctx context.Context, t *testing.T, db *sql.DB, st *Postgres) (Season, uuid.UUID) {
	t.Helper()
	orgID, foreignOrgID := uuid.New(), uuid.New()
	venueID, foreignVenueID := uuid.New(), uuid.New()
	eventID, foreignEventID := uuid.New(), uuid.New()
	slotID, foreignSlotID := uuid.New(), uuid.New()

	// One statement per Exec: a multi-command string cannot carry $N parameters
	// (SQLSTATE 42601 — "cannot insert multiple commands into a prepared statement").
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizers(id,name) VALUES($1,'season-owner'),($2,'foreign-org')`,
			[]any{orgID, foreignOrgID}},
		{`INSERT INTO venues(id,organizer_id,name,ga_capacity) VALUES($1,$2,'season-venue',100),($3,$4,'foreign-venue',100)`,
			[]any{venueID, orgID, foreignVenueID, foreignOrgID}},
		// Poison row: the foreign event's name is a schema-valid jsonb scalar
		// ("not-an-object"). It satisfies `name jsonb NOT NULL`, but decoding it
		// into LocalizedText (a map) fails — so any read that scans it errors,
		// while a correctly scoped read never sees it.
		{`INSERT INTO events(id,organizer_id,name) VALUES($1,$2,'{"en":"season event","fr":"season event"}'),($3,$4,'"not-an-object"')`,
			[]any{eventID, orgID, foreignEventID, foreignOrgID}},
		{`INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,starts_at,timezone) VALUES
			($1,$2,$3,$4,'performance',TIMESTAMPTZ '2026-09-01 20:00:00-04','America/Toronto'),
			($5,$6,$7,$8,'performance',TIMESTAMPTZ '2026-09-02 20:00:00-04','America/Toronto')`,
			[]any{slotID, orgID, eventID, venueID, foreignSlotID, foreignOrgID, foreignEventID, foreignVenueID}},
		{`INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency) VALUES
			($1,$2,'{"en":"ga","fr":"ga"}',5000,'CAD'),
			($3,$4,'{"en":"ga","fr":"ga"}',5000,'CAD')`,
			[]any{orgID, slotID, foreignOrgID, foreignSlotID}},
	} {
		if _, err := db.ExecContext(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}

	season, err := st.CreateSeason(ctx, SeasonInput{
		OrganizerID: orgID,
		Name:        LocalizedText{"en": "season", "fr": "saison"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if season, err = st.AttachEventToSeason(ctx, season.ID, eventID); err != nil {
		t.Fatal(err)
	}
	// Both slots published: the foreign one is live inventory the season read
	// must never look at.
	for _, id := range []uuid.UUID{slotID, foreignSlotID} {
		if _, _, err = st.PublishPerformance(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	return season, eventID
}

// TestGetPublishedSeasonDoesNotScanForeignEvents asserts RESULT scoping: the
// season read must not load a published event outside the season. Red before
// TKT-60 (ListPublishedEvents scans the whole catalog and chokes on the poison
// row); green after (the scoped query never selects it).
func TestGetPublishedSeasonDoesNotScanForeignEvents(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	season, eventID := seedSeasonWithForeignEvent(ctx, t, db, st)

	agg, err := st.GetPublishedSeason(ctx, season.ID)
	if err != nil {
		t.Fatalf("season read scanned a foreign event it should never have loaded: %v", err)
	}
	if len(agg.Events) != 1 {
		t.Fatalf("event count = %d, want 1 (the season's own event only)", len(agg.Events))
	}
	if agg.Events[0].Event.ID != eventID {
		t.Fatalf("event = %s, want the season's own %s", agg.Events[0].Event.ID, eventID)
	}
}

// explainGenericPlan returns the TEXT plan that Postgres caches for query — a
// uuid[]-parameterized SELECT — under plan_cache_mode = force_generic_plan.
//
// It goes through a server-side PREPARE/EXECUTE, and that is the entire point.
// `EXPLAIN <query with $1>` sent through the driver does NOT answer the
// generic-plan question: the driver's statement is the EXPLAIN itself, so the
// inner query is planned with the value already bound and you get a *custom*
// plan no matter what plan_cache_mode says. It looks like a passing assertion
// and proves nothing — both predicate shapes return an identical, indexed,
// literal-substituted plan. A real generic plan is recognisable in the output:
// the parameter survives as `$1` instead of appearing as a literal.
//
// PREPARE, the SET, and the EXECUTE must also share one connection — a `SET` on
// *sql.DB can land on a different pooled connection than the statement it means
// to govern — so all three run in one transaction, rolled back to drop both the
// setting and the prepared statement.
//
// scopes are the query's parameters in order, each a uuid.UUID (a scalar scope,
// e.g. the festival read) or a []uuid.UUID (an array scope, e.g. the season
// read). Variadic so a multi-parameter query — the TKT-151 pricing read binds
// six — uses the SAME helper: ADR-019 is explicit that forking it is how one
// copy quietly stops asserting anything. Each is
// interpolated into the EXECUTE as a literal because EXECUTE arguments cannot be
// driver parameters (they would belong to the outer statement) nor subqueries. The
// values are fixture-generated uuid.UUIDs formatted by their own String method —
// there is no injection surface here.
//
// Both catalog subset reads share this helper deliberately: the plan mode and the
// $1 guard below are the discipline ADR-019 rule 2 asks for, and forking them per
// read is how one copy quietly stops asserting anything.
func explainGenericPlan(ctx context.Context, t *testing.T, db *sql.DB, query string, scopes ...any) string {
	t.Helper()
	if len(scopes) == 0 {
		t.Fatal("explainGenericPlan: a query with no parameters has no generic plan to assert")
	}
	paramTypes := make([]string, 0, len(scopes))
	literals := make([]string, 0, len(scopes))
	for i, scope := range scopes {
		switch v := scope.(type) {
		case uuid.UUID:
			paramTypes = append(paramTypes, `uuid`)
			literals = append(literals, `'`+v.String()+`'::uuid`)
		case []uuid.UUID:
			ids := make([]string, 0, len(v))
			for _, id := range v {
				ids = append(ids, id.String())
			}
			paramTypes = append(paramTypes, `uuid[]`)
			literals = append(literals, `'{`+strings.Join(ids, ",")+`}'::uuid[]`)
		default:
			t.Fatalf("explainGenericPlan: unsupported scope type %T at $%d", scope, i+1)
		}
	}
	paramType, literal := strings.Join(paramTypes, ", "), strings.Join(literals, ", ")

	stmt := "plan_probe_" + strconv.FormatUint(planProbeSeq.Add(1), 10)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`PREPARE %s(%s) AS %s`, stmt, paramType, query)); err != nil {
		t.Fatal(err)
	}
	// Set before the first EXECUTE: the cached plan is built then.
	if _, err = tx.ExecContext(ctx, `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	// EXPLAIN returns the plan one row per line — read them all, not just the first.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`EXPLAIN EXECUTE %s(%s)`, stmt, literal))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Guard the trap above: if a parameter was folded to a literal this is a
	// custom plan, and every index assertion below would be vacuous. Every
	// parameter is checked, not just $1 — a multi-parameter query can have one
	// survive while another is substituted.
	got := plan.String()
	for i := range scopes {
		marker := "$" + strconv.Itoa(i+1)
		if !strings.Contains(got, marker) {
			t.Fatalf("not a generic plan — %s was substituted, so plan_cache_mode did not apply "+
				"and this assertion proves nothing.\nplan:\n%s", marker, got)
		}
	}
	return got
}

// TestGetPublishedSeasonIsIndexScoped asserts PHYSICAL scan cost — the claim a
// poison row cannot make. A correct result can still be produced by reading the
// entire catalog and discarding rows; that is exactly the TKT-60 defect. So
// assert the query plan itself: the season read must reach its performances
// through performances_by_event, never a sequential scan.
//
// It EXPLAINs scopedPublicPerformancesQuery itself — the exact SQL the shipped
// season read executes, referenced as a const rather than retyped. Nothing here
// is a surrogate: the four extra joins, the ~24-column projection and the sort
// are all in the plan under assertion, so a scoping regression anywhere in the
// production query text reddens this test.
//
// It used to EXPLAIN a hand-copied, reduced `SELECT p.id` around the predicate,
// which could only prove the predicate was index-*compatible* — production was
// free to drift to a catalog scan with this green (ADR-019 booked that as an
// enforcement gap, TKT-65). Splitting the query into consts (TKT-63) is what made
// binding to the real statement a one-liner.
//
// Only the two scoping indexes are asserted, not every join's — the other joins
// may legitimately change access path without touching the scoping claim.
//
// Both tables are asserted. Under force_generic_plan the old
// `($1 IS NULL OR e.id = ANY($1))` shape kept performances_by_event but lost
// events_pkey — the planner must build one plan valid for a NULL $1 too — so
// performances alone would stay green through exactly the defect TKT-63 fixed.
func TestGetPublishedSeasonIsIndexScoped(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	season, _ := seedSeasonWithForeignEvent(ctx, t, db, st)
	if _, err := st.GetPublishedSeason(ctx, season.ID); err != nil {
		t.Fatal(err)
	}

	// A plan assertion is only meaningful once a seq scan is the *wrong* choice:
	// on a two-row table Postgres rightly ignores any index, and the assertion
	// would fail for a reason that has nothing to do with this change. Seed a
	// catalog large enough that scanning it is the expensive option — which is
	// also the only condition under which TKT-60's defect actually bites.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO events(id,organizer_id,name)
		SELECT gen_random_uuid(), (SELECT organizer_id FROM events LIMIT 1), '{"en":"bulk","fr":"bulk"}'
		FROM generate_series(1,2000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO performances(organizer_id,event_id,venue_id,kind,status,starts_at,timezone)
		SELECT e.organizer_id, e.id, (SELECT id FROM venues LIMIT 1), 'performance', 'published',
		       TIMESTAMPTZ '2026-10-01 20:00:00-04', 'America/Toronto'
		FROM events e, generate_series(1,5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE events, performances`); err != nil {
		t.Fatal(err)
	}
	plan := explainGenericPlan(ctx, t, db, scopedPublicPerformancesQuery, []uuid.UUID{season.EventIDs[0]})

	assertReachesVia(t, plan, "performances", "performances_by_event")
	assertReachesVia(t, plan, "events", "events_pkey")
}

// TestGetPublishedSeasonEmptyScopeDoesNotWidenToCatalog guards the contract that
// splitting the SQL moved out of the query and into Go (TKT-63).
//
// publicPerformances routes on eventIDs == nil: nil means the whole catalog, a
// non-nil *empty* slice means no events. A season with no attached events yields
// the empty slice, so a router that tested len(eventIDs) == 0 instead of nil
// would hand it the entire published catalog — a zero-event season becoming the
// most expensive read in the service. While the two shapes shared one SQL text
// the `$1 IS NULL` branch encoded that distinction; now only the Go does.
//
// The seeded catalog contains a published poison event, so a widened read does
// not merely over-return: it fails to decode and errors. Either way, not ErrNotFound.
func TestGetPublishedSeasonEmptyScopeDoesNotWidenToCatalog(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	season, _ := seedSeasonWithForeignEvent(ctx, t, db, st)

	empty, err := st.CreateSeason(ctx, SeasonInput{
		OrganizerID: season.OrganizerID,
		Name:        LocalizedText{"en": "empty season", "fr": "saison vide"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.EventIDs) != 0 {
		t.Fatalf("fixture is not an empty season: %d events attached", len(empty.EventIDs))
	}

	switch _, err := st.GetPublishedSeason(ctx, empty.ID); {
	case errors.Is(err, ErrNotFound): // the scoped query matched nothing, as it must
	case err == nil:
		t.Fatal("a season with no events returned events — the empty scope widened to the catalog")
	default:
		t.Fatalf("a season with no events read the catalog and hit the poison row: %v", err)
	}
}
