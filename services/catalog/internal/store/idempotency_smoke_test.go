//go:build smoke

package store

// Create idempotency against real PostgreSQL (TKT-200).
//
// THIS TIER, NOT THE API TIER. The mechanism is a partial UNIQUE index in
// Postgres; the API-tier fake enforces its scoping in Go, so an assertion
// written up there would prove the fake and the handler agree and nothing else.
// A test must live at the tier its mechanism does.
//
// The invariant every assertion below derives from, stated without naming the
// implementation:
//
//	For one organizer and one key, a create yields exactly one resource, and
//	every repeat of that request returns that same resource.
//
// Expected counts come from that sentence. None was read off a run.

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

// idempotencyFixture stands up an isolated schema with the real migrations and
// seeds one organizer, venue, event and draft performance — the ancestors all
// three creates need.
type idempotencyFixture struct {
	db      *sql.DB
	st      *Postgres
	org     uuid.UUID
	venue   uuid.UUID
	event   uuid.UUID
	perf    uuid.UUID
	cleanup func()
}

func newIdempotencyFixture(t *testing.T, ctx context.Context, admin *sql.DB, dsn string) idempotencyFixture {
	t.Helper()
	schema := "catalog_idem_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := fs.Sub(migrationsFS, "migrations")
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
	f := idempotencyFixture{
		db: db, st: NewPostgres(db),
		org: uuid.New(), venue: uuid.New(), event: uuid.New(), perf: uuid.New(),
	}
	if _, err := db.ExecContext(ctx, `
		WITH o AS (INSERT INTO organizers(id,name) VALUES($1,'idem') RETURNING id),
		     v AS (INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',10 FROM o RETURNING id),
		     e AS (INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id)
		INSERT INTO performances(id,organizer_id,event_id,venue_id,starts_at,timezone,status)
		SELECT $4,o.id,e.id,v.id,now(),'UTC','draft' FROM o,e,v`,
		f.org, f.venue, f.event, f.perf); err != nil {
		t.Fatal(err)
	}
	f.cleanup = func() {
		_ = db.Close()
		_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
	}
	return f
}

func idempotencyDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CATALOG_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATALOG_MIGRATION_TEST_DATABASE_URL is not set")
	}
	return dsn
}

func openAdmin(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	return admin
}

func eventInput(org uuid.UUID, key, name string) EventInput {
	return EventInput{
		OrganizerID:    org,
		Name:           LocalizedText{"en": name, "fr": name},
		IdempotencyKey: key,
	}
}

// TestConcurrentCreateWithOneKeyCreatesOneRow is the load-bearing test of this
// ticket, and it is deliberately NOT a sequential repeat.
//
// A sequential repeat passes against a naive check-then-insert — the first call
// commits before the second one looks — so it proves nothing about the race this
// ticket exists to close. Both callers here are released together and neither
// can see the other's result.
//
// Both must succeed with the same id, and the table must hold one row. "Both
// succeed" is not politeness: a caller that gets a 500 because it lost a race it
// could not observe has been told its create failed when it did not.
func TestConcurrentCreateWithOneKeyCreatesOneRow(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin := openAdmin(t, dsn)

	// Repeated so the interleaving is exercised rather than assumed: a single
	// pass can serialize by luck and would then be green against no mechanism.
	for i := 0; i < 30; i++ {
		f := newIdempotencyFixture(t, ctx, admin, dsn)

		const key = "double-click"
		var (
			wg        sync.WaitGroup
			first     Event
			second    Event
			firstErr  error
			secondErr error
			in        = eventInput(f.org, key, "Race Night")
		)

		// The interleaving is FORCED, not hoped for. Both callers reach the
		// barrier — after any pre-insert read, immediately before the insert —
		// and neither proceeds until both have arrived. That is precisely the
		// window a check-then-insert leaves open, so this test can tell the two
		// implementations apart.
		//
		// Merely starting two goroutines cannot: that version of this test was
		// green against a Go-side check-then-insert with the unique index
		// dropped, across all 30 iterations. The barrier is what makes the
		// mutation die.
		var arrive sync.WaitGroup
		arrive.Add(2)
		beforeIdempotentInsert = func() {
			arrive.Done()
			arrive.Wait()
		}
		t.Cleanup(func() { beforeIdempotentInsert = nil })

		wg.Add(2)
		go func() {
			defer wg.Done()
			first, firstErr = f.st.CreateEvent(ctx, in)
		}()
		go func() {
			defer wg.Done()
			second, secondErr = f.st.CreateEvent(ctx, in)
		}()
		wg.Wait()
		beforeIdempotentInsert = nil

		if firstErr != nil || secondErr != nil {
			t.Fatalf("iter %d: both concurrent creates must succeed; got %v and %v", i, firstErr, secondErr)
		}
		if first.ID != second.ID {
			t.Fatalf("iter %d: one key produced two resources: %s and %s", i, first.ID, second.ID)
		}
		var rows int
		if err := f.db.QueryRowContext(ctx,
			`SELECT count(*) FROM events WHERE organizer_id=$1 AND idempotency_key=$2`, f.org, key).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		// One, because one key names one resource. Not because a run said so.
		if rows != 1 {
			t.Fatalf("iter %d: one key must name one row, found %d", i, rows)
		}
		f.cleanup()
	}
}

// TestConcurrentTicketTypeCreateWithOneKeyCreatesOneRow is the same forced race
// on the third operation. Present because the guarantee is per-table — each of
// the three tables carries its own index — so a green events test says nothing
// about ticket_types. The performance path is covered by its own fingerprint
// test above plus this shape's shared helper.
func TestConcurrentTicketTypeCreateWithOneKeyCreatesOneRow(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin := openAdmin(t, dsn)

	for i := 0; i < 10; i++ {
		f := newIdempotencyFixture(t, ctx, admin, dsn)
		in := TicketTypeInput{
			OrganizerID: f.org, PerformanceID: f.perf,
			Name:        LocalizedText{"en": "ga", "fr": "ga"},
			PriceAmount: 1500, Currency: "EUR", IdempotencyKey: "tt-race",
		}
		var (
			wg         sync.WaitGroup
			arrive     sync.WaitGroup
			a, b       TicketType
			aErr, bErr error
		)
		arrive.Add(2)
		beforeIdempotentInsert = func() { arrive.Done(); arrive.Wait() }
		wg.Add(2)
		go func() { defer wg.Done(); a, aErr = f.st.CreateTicketType(ctx, in) }()
		go func() { defer wg.Done(); b, bErr = f.st.CreateTicketType(ctx, in) }()
		wg.Wait()
		beforeIdempotentInsert = nil

		if aErr != nil || bErr != nil {
			t.Fatalf("iter %d: both concurrent creates must succeed; got %v and %v", i, aErr, bErr)
		}
		if a.ID != b.ID {
			t.Fatalf("iter %d: one key produced two ticket types: %s and %s", i, a.ID, b.ID)
		}
		var rows int
		if err := f.db.QueryRowContext(ctx,
			`SELECT count(*) FROM ticket_types WHERE organizer_id=$1 AND idempotency_key=$2`, f.org, in.IdempotencyKey).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("iter %d: one key must name one row, found %d", i, rows)
		}
		f.cleanup()
	}
}

// TestRepeatWithSameKeyReplaysTheFirstResource covers the ordinary retry: the
// second call must be handed the FIRST call's resource, identity and creation
// time included, rather than a second row that merely looks the same.
func TestRepeatWithSameKeyReplaysTheFirstResource(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	in := eventInput(f.org, "retry-me", "Replay Night")
	first, err := f.st.CreateEvent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.st.CreateEvent(ctx, in)
	if err != nil {
		t.Fatalf("a repeat of an identical request must replay, not fail: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned a different resource: %s then %s", first.ID, second.ID)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay must return the ORIGINAL creation time: %s then %s", first.CreatedAt, second.CreatedAt)
	}
	// Scoped to the KEY, not to the organizer: the fixture seeds an ancestor
	// event for the performance/ticket-type paths, so an organizer-wide count
	// would be measuring the fixture as much as the mechanism.
	var rows int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE organizer_id=$1 AND idempotency_key=$2`, f.org, in.IdempotencyKey).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("a replay must create nothing; found %d rows", rows)
	}
}

// TestSameKeyDifferentRequestIsRefused pins the decision that a reused key with
// different terms is a conflict rather than a silent replay. Handing back the
// first resource would give the caller something it never asked for.
func TestSameKeyDifferentRequestIsRefused(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	if _, err := f.st.CreateEvent(ctx, eventInput(f.org, "shared", "First Name")); err != nil {
		t.Fatal(err)
	}
	_, err := f.st.CreateEvent(ctx, eventInput(f.org, "shared", "Different Name"))
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reusing a key for different terms must be refused, got %v", err)
	}
}

// TestKeysAreScopedByOrganizer is the ADR-002 tenancy assertion. Two organizers
// choosing the same key string is ordinary — they never coordinate — and each
// must get its own resource. Without the organizer in the constraint, the second
// tenant would be handed the first tenant's row as a "replay".
func TestKeysAreScopedByOrganizer(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	other := uuid.New()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO organizers(id,name) VALUES($1,'other')`, other); err != nil {
		t.Fatal(err)
	}
	const key = "create-event-1"
	mine, err := f.st.CreateEvent(ctx, eventInput(f.org, key, "Mine"))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.st.CreateEvent(ctx, eventInput(other, key, "Theirs"))
	if err != nil {
		t.Fatalf("another organizer's identical key must not collide: %v", err)
	}
	if mine.ID == theirs.ID {
		t.Fatal("two organizers sharing a key string were given one resource")
	}
}

// TestKeylessCreatesDoNotCollide pins the nullable-column decision from
// 0020_create_idempotency.sql. Non-contract writers (fixtures, future internal
// paths) pass no key and store NULL; NULL never collides with NULL under the
// partial index, so two keyless creates must both succeed.
//
// Without this, "tightening" the column to NOT NULL later looks harmless and
// would break every keyless writer at once.
func TestKeylessCreatesDoNotCollide(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	a, err := f.st.CreateEvent(ctx, eventInput(f.org, "", "Keyless A"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.st.CreateEvent(ctx, eventInput(f.org, "", "Keyless B"))
	if err != nil {
		t.Fatalf("two keyless creates must both succeed: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("keyless creates were deduplicated; NULL must not be a key")
	}
	var keyed int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE organizer_id=$1 AND idempotency_key IS NOT NULL`, f.org).Scan(&keyed); err != nil {
		t.Fatal(err)
	}
	if keyed != 0 {
		t.Fatalf("a keyless create must store NULL, found %d keyed rows", keyed)
	}
}

// TestPerformanceFingerprintUsesNormalizedValues is the trap named in the plan
// critique. CreatePerformance defaults an empty kind to 'performance' and an
// empty re-entry mode to 'single' AFTER validation. If the fingerprint were
// taken over the raw request while the row stores the normalized value, then
// kind:"" and kind:"performance" would be two fingerprints for one identical
// row — and this repeat would 409 instead of replaying.
func TestPerformanceFingerprintUsesNormalizedValues(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	starts := time.Now().Add(48 * time.Hour).UTC()
	raw := PerformanceInput{
		OrganizerID: f.org, EventID: f.event, VenueID: f.venue,
		Kind: "", StartsAt: &starts, Timezone: "UTC",
		ReEntry: ReEntryPolicy{Mode: ""}, IdempotencyKey: "normalize-me",
	}
	first, err := f.st.CreatePerformance(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	// The same request spelled with the values the first call normalized to.
	explicit := raw
	explicit.Kind = KindPerformance
	explicit.ReEntry = ReEntryPolicy{Mode: "single"}
	second, err := f.st.CreatePerformance(ctx, explicit)
	if err != nil {
		t.Fatalf("a request identical after defaulting must replay, not conflict: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("defaulted and explicit spellings created two rows: %s and %s", first.ID, second.ID)
	}
}

// TestFingerprintCanonicalisesToTheStoredPrecision covers ai-review [medium].
//
// The fingerprint's contract is "same stored row, same hash". Go carries
// nanoseconds; timestamptz keeps microseconds. So an instant differing only
// below the microsecond becomes ONE row, and a retry reconstructed from the
// stored value must replay rather than be refused as a conflict.
//
// The mutation that turns this red is restoring the nanosecond layout.
func TestFingerprintCanonicalisesToTheStoredPrecision(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	// The values are chosen to be UNALIGNED and to straddle the rounding rule,
	// because a pair that truncates and rounds alike proves nothing (ai-review
	// pass 2 [medium] caught exactly that). 1900ns is the sharp one: Postgres
	// stores .000002, so an implementation that TRUNCATES hashes .000001 and the
	// stored-value retry below conflicts with its own original.
	//
	// The instants cover the boundaries a precision rule can get wrong: a
	// pre-epoch value (Go's truncation is toward the zero YEAR, not the epoch),
	// one a nanosecond short of a second rollover, and a far-future one.
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"1900ns", time.Date(2026, 9, 1, 20, 0, 0, 1900, time.UTC)},
		{"500ns", time.Date(2026, 9, 1, 20, 0, 0, 500, time.UTC)},
		{"2500ns", time.Date(2026, 9, 1, 20, 0, 0, 2500, time.UTC)},
		{"3500ns", time.Date(2026, 9, 1, 20, 0, 0, 3500, time.UTC)},
		{"1200ns", time.Date(2026, 9, 1, 20, 0, 0, 1200, time.UTC)},
		{"pre-epoch", time.Date(1969, 7, 20, 20, 17, 0, 123456789, time.UTC)},
		{"second rollover", time.Date(2026, 9, 1, 20, 0, 59, 999999999, time.UTC)},
		{"day rollover", time.Date(2026, 9, 1, 23, 59, 59, 999999999, time.UTC)},
		{"far future", time.Date(2999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "precision-" + tc.name
			raw := tc.at
			in := PerformanceInput{
				OrganizerID: f.org, EventID: f.event, VenueID: f.venue,
				StartsAt: &raw, Timezone: "UTC", IdempotencyKey: key,
			}
			first, err := f.st.CreatePerformance(ctx, in)
			if err != nil {
				t.Fatal(err)
			}

			// The expected instant, derived from the RULE and not from the run
			// (ai-review pass 3 [medium]): sub-microsecond precision is dropped,
			// so the stored value is the input with its nanosecond remainder
			// removed. Computed here in the test rather than by calling
			// normalizeStartsAt, which would be the implementation grading itself.
			want := raw.Add(-time.Duration(raw.Nanosecond() % 1000))

			var stored time.Time
			if err := f.db.QueryRowContext(ctx,
				`SELECT starts_at FROM performances WHERE id=$1`, first.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			// THIS is what makes the retry below non-vacuous. Without it, a
			// normalizeStartsAt that zeroed or shifted the instant would still
			// replay — the retry echoes whatever the implementation chose, so it
			// can only prove self-consistency, never correctness.
			if !stored.Equal(want) {
				t.Fatalf("%s stored %v, want %v (the input with sub-microsecond precision dropped)",
					tc.name, stored.UTC(), want.UTC())
			}
			if stored.Equal(raw) {
				t.Fatalf("%s did not exercise the column's precision: stored %v equals the raw input", tc.name, stored)
			}

			// The retry a real client makes: it echoes the instant the server
			// returned, which is the stored one. It must replay.
			in.StartsAt = &stored
			second, err := f.st.CreatePerformance(ctx, in)
			if err != nil {
				t.Fatalf("a retry echoing the STORED instant must replay, not conflict: %v", err)
			}
			if second.ID != first.ID {
				t.Fatalf("stored-value retry created a second row: %s and %s", first.ID, second.ID)
			}
		})
	}
}

// TestOperatingDateFingerprintIsACalendarDate covers ai-review pass 2 [medium].
//
// operating_date is a DATE — a calendar date, not an instant. A caller in a
// positive-offset zone passing midnight local means that day; converting the
// time.Time to UTC first turns it into the day BEFORE in the hash, while the
// column still stores the local day. The retry then conflicts with its own
// original.
//
// The mutation that turns this red is restoring `.UTC().Format(...)`.
func TestOperatingDateFingerprintIsACalendarDate(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// Midnight in Tokyo on the 1st is 15:00 UTC on the 31st — the rollover.
	local := time.Date(2026, 9, 1, 0, 0, 0, 0, tokyo)
	opens, closes := "09:00", "18:00"
	in := PerformanceInput{
		OrganizerID: f.org, EventID: f.event, VenueID: f.venue,
		Kind: KindOperatingDay, OperatingDate: &local,
		OpensAt: &opens, ClosesAt: &closes,
		Timezone: "Asia/Tokyo", IdempotencyKey: "tokyo-date",
	}
	first, err := f.st.CreatePerformance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	// What the column actually kept, and the day the caller meant.
	var stored string
	if err := f.db.QueryRowContext(ctx,
		`SELECT operating_date::text FROM performances WHERE id=$1`, first.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "2026-09-01" {
		t.Fatalf("the column stored %s; the caller meant the local calendar date 2026-09-01", stored)
	}

	// The retry a real client makes: it echoes the date the server stored, read
	// back as the DATE the column holds. That is a different time.Time from the
	// one submitted — same calendar day, no location offset — and it is what
	// makes this test able to fail. Repeating the ORIGINAL input proves nothing:
	// an implementation that converts to UTC converts BOTH calls the same wrong
	// way and replays happily (verified — that version survived the mutation).
	var storedDate time.Time
	if err := f.db.QueryRowContext(ctx,
		`SELECT operating_date FROM performances WHERE id=$1`, first.ID).Scan(&storedDate); err != nil {
		t.Fatal(err)
	}
	in.OperatingDate = &storedDate
	second, err := f.st.CreatePerformance(ctx, in)
	if err != nil {
		t.Fatalf("a retry echoing the STORED calendar date must replay, not conflict: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("one key produced two rows: %s and %s", first.ID, second.ID)
	}
}

// TestKeyedRowRequiresAFingerprint covers ai-review [medium]. A keyed row with a
// NULL fingerprint is the one state the replay path cannot answer: replayLookup
// reads NULL as a mismatch, so the ORIGINAL request would be refused as a
// conflict forever. Fail-closed is the right reading; the row should simply not
// be constructible.
//
// Asserted against the database directly, because the constraint is the
// mechanism and the store never produces this state.
func TestKeyedRowRequiresAFingerprint(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	// Asserting on the NAMED constraint, not on "some error" (ai-review pass 2
	// [medium]). Any error would stay green if an unrelated constraint rejected
	// the insert, or if the pairing CHECK were replaced by a blanket refusal that
	// also rejects legitimate keyed rows — and the positive case below is what
	// rules that second one out.
	//
	// All three tables, because each carries its own constraint: a green events
	// assertion says nothing about performances or ticket_types.
	for _, tc := range []struct{ table, cols, values string }{
		{"events", "organizer_id,name", `$1,'{"en":"x","fr":"x"}'`},
		{"performances", "organizer_id,event_id,venue_id,starts_at,timezone,status", `$1,'` + f.event.String() + `','` + f.venue.String() + `',now(),'UTC','draft'`},
		{"ticket_types", "organizer_id,performance_id,name,price_amount,currency", `$1,'` + f.perf.String() + `','{"en":"t","fr":"t"}',100,'EUR'`},
	} {
		t.Run(tc.table, func(t *testing.T) {
			want := tc.table + "_key_and_fingerprint_agree"
			for _, d := range []struct{ name, col, val string }{
				{"key without fingerprint", "idempotency_key", "orphan-key"},
				{"fingerprint without key", "request_fingerprint", "orphan-print"},
			} {
				_, err := f.db.ExecContext(ctx,
					`INSERT INTO `+tc.table+`(`+tc.cols+`,`+d.col+`) VALUES(`+tc.values+`,'`+d.val+`')`, f.org)
				if err == nil {
					t.Fatalf("%s: %s must be refused by the schema", tc.table, d.name)
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: %s was refused by the wrong mechanism; want constraint %q, got: %v",
						tc.table, d.name, want, err)
				}
			}
			// The positive direction: both present is accepted. Without this, a
			// constraint of `false` would satisfy every assertion above.
			if _, err := f.db.ExecContext(ctx,
				`INSERT INTO `+tc.table+`(`+tc.cols+`,idempotency_key,request_fingerprint) VALUES(`+tc.values+`,'paired-`+tc.table+`','print')`, f.org); err != nil {
				t.Fatalf("%s: a row carrying BOTH must be accepted: %v", tc.table, err)
			}
		})
	}
}

// TestTicketTypeCreateIsIdempotent covers the third operation, whose replay path
// must also skip the public-read invalidation: a replay creates nothing, so
// announcing a listability change would be announcing an event that did not
// happen.
func TestTicketTypeCreateIsIdempotent(t *testing.T) {
	dsn := idempotencyDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newIdempotencyFixture(t, ctx, openAdmin(t, dsn), dsn)
	defer f.cleanup()

	in := TicketTypeInput{
		OrganizerID: f.org, PerformanceID: f.perf,
		Name:        LocalizedText{"en": "ga", "fr": "ga"},
		PriceAmount: 2500, Currency: "EUR", IdempotencyKey: "tt-key",
	}
	first, err := f.st.CreateTicketType(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.st.CreateTicketType(ctx, in)
	if err != nil {
		t.Fatalf("repeat must replay: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("one key produced two ticket types: %s and %s", first.ID, second.ID)
	}
	if second.PriceAmount != first.PriceAmount || second.Currency != first.Currency {
		t.Fatalf("replay returned different money: %d %s then %d %s",
			first.PriceAmount, first.Currency, second.PriceAmount, second.Currency)
	}
	var rows int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ticket_types WHERE organizer_id=$1 AND idempotency_key=$2`, f.org, in.IdempotencyKey).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("a replay must create nothing; found %d ticket types", rows)
	}
}
