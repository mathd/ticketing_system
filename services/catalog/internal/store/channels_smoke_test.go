//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// DB-backed tests for the sales-channel registry (TKT-235 / epic TKT-17).
//
// channels_test.go proves the pure write gate without a database. What needs one
// is the DDL itself, per-organizer uniqueness through the real unique index,
// code immutability through the real UPDATE, the enabled filter running in SQL
// rather than in Go, and ADR-019's claim that the public read is index-backed.

func seedChannelOrganizer(ctx context.Context, t *testing.T, db *sql.DB, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizers(name) VALUES($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestChannelCodeIsUniquePerOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	orgA := seedChannelOrganizer(ctx, t, db, "org-a")
	orgB := seedChannelOrganizer(ctx, t, db, "org-b")

	if _, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: orgA, Code: "pos", DisplayName: "Box office", Kind: ChannelKindPOS, Enabled: true,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: orgA, Code: "pos", DisplayName: "Duplicate", Kind: ChannelKindPOS, Enabled: true,
	})
	if !errors.Is(err, ErrChannelCodeTaken) {
		t.Fatalf("duplicate for the same organizer = %v, want ErrChannelCodeTaken", err)
	}
	// Tenants do not share a code namespace.
	if _, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: orgB, Code: "pos", DisplayName: "Other box office", Kind: ChannelKindPOS, Enabled: true,
	}); err != nil {
		t.Fatalf("same code for a different organizer: %v", err)
	}
}

// The exactness rule, proved against the real unique index rather than against
// the Go gate.
//
// A case-insensitive index (or a citext column, or a normalizing store) would
// make the second create collide. The Go-level test cannot see that: it never
// reaches Postgres. This is the only place the DDL's case sensitivity is
// actually asserted.
func TestChannelCodesDifferingByCaseOrSpaceCoexistInPostgres(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "exactness")

	codes := []string{"pos", "POS", "Pos", " pos", "pos "}
	for _, code := range codes {
		c, err := st.CreateChannel(ctx, ChannelInput{
			OrganizerID: org, Code: code, DisplayName: "Box office", Kind: ChannelKindPOS, Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateChannel(%q): %v — codes are exact opaque strings (ADR-024); these are distinct channels", code, err)
		}
		if c.Code != code {
			t.Fatalf("stored code = %q, want %q verbatim — nothing may trim or fold", c.Code, code)
		}
	}
	// And all five are really there, as five rows.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM channels WHERE organizer_id = $1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(codes) {
		t.Fatalf("stored %d channels, want %d — a normalizing index collapsed them", n, len(codes))
	}
}

func TestChannelCodeIsImmutableAndTheRefusalDoesNotPartiallyApply(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "immutable")

	created, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: org, Code: "legacy-pos", DisplayName: "Old box office", Kind: ChannelKindPOS, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = st.UpdateChannel(ctx, org, created.ID, ChannelUpdate{
		Code: "pos", DisplayName: "Renamed", Kind: ChannelKindPOS, Enabled: true,
		ExpectedRevision: created.Revision,
	})
	if !errors.Is(err, ErrChannelCodeImmutable) {
		t.Fatalf("rename = %v, want ErrChannelCodeImmutable", err)
	}

	// Nothing was written. The code is compared under the row lock, before the
	// UPDATE runs, and the refusal rolls the transaction back — there is no
	// window in which the display name lands and the code does not.
	after, err := st.GetChannel(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Code != "legacy-pos" || after.DisplayName != "Old box office" {
		t.Fatalf("after a refused rename: code=%q display_name=%q, want legacy-pos / 'Old box office'", after.Code, after.DisplayName)
	}
}

func TestUpdateChannelWritesUpdatedAtAndTheMutableFields(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "update")

	created, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: org, Code: "web", DisplayName: "Website", Kind: ChannelKindWeb, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpdateChannel(ctx, org, created.ID, ChannelUpdate{
		Code: "web", DisplayName: "Main website", Kind: ChannelKindWeb, Enabled: false,
		ExpectedRevision: created.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Main website" || updated.Enabled {
		t.Fatalf("got display_name=%q enabled=%v, want 'Main website' / false", updated.DisplayName, updated.Enabled)
	}
	// Catalog has no updated_at trigger anywhere — the store writes it. If this
	// fails, the UPDATE dropped `updated_at = now()` and every consumer sees a
	// stale timestamp with no other symptom.
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at %v not after the create's %v — the store must set it explicitly",
			updated.UpdatedAt, created.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at moved from %v to %v", created.CreatedAt, updated.CreatedAt)
	}
}

func TestUpdateUnknownChannelIsNotFound(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_ = seedChannelOrganizer(ctx, t, db, "missing")
	_, err := st.UpdateChannel(ctx, uuid.New(), uuid.New(), ChannelUpdate{
		Code: "pos", DisplayName: "Box office", Kind: ChannelKindPOS, Enabled: true,
		ExpectedRevision: 0,
	})
	// Not ErrChannelCodeImmutable: reporting the immutability conflict for an id
	// that does not exist would tell a caller that an id it guessed is real.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of an unknown channel = %v, want ErrNotFound", err)
	}
}

func TestChannelReadsSplitEnabledFromOperatorView(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "reads")
	other := seedChannelOrganizer(ctx, t, db, "reads-other")

	seed := []struct {
		org     uuid.UUID
		code    string
		enabled bool
	}{
		{org, "web", true},
		{org, "pos", true},
		{org, "retired", false},
		{other, "web", true},
	}
	for _, s := range seed {
		if _, err := st.CreateChannel(ctx, ChannelInput{
			OrganizerID: s.org, Code: s.code, DisplayName: s.code, Kind: ChannelKindWeb, Enabled: s.enabled,
		}); err != nil {
			t.Fatalf("seed %q: %v", s.code, err)
		}
	}

	public, err := st.ListEnabledChannels(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	gotPublic := make([]string, 0, len(public))
	for _, c := range public {
		gotPublic = append(gotPublic, c.Code)
	}
	if strings.Join(gotPublic, ",") != "pos,web" {
		t.Fatalf("public = %v, want [pos web] ordered by code, no disabled row, no other tenant", gotPublic)
	}

	ops, err := st.ListChannels(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("operator list = %d rows, want 3 including the disabled one", len(ops))
	}
	for _, c := range ops {
		if c.OrganizerID != org {
			t.Fatalf("operator list leaked organizer %s", c.OrganizerID)
		}
	}
}

// ADR-019, both halves: the result is scoped AND the scan is.
//
// Asserting only the rows would pass against a sequential scan over every
// tenant's channels, which is the no-op this ADR exists to stop. The query is
// the exact const the store runs, not a hand-copied reduction that is free to
// drift from the shipped SQL.
func TestPublicChannelReadIsIndexBacked(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "planner")
	other := seedChannelOrganizer(ctx, t, db, "planner-other")

	// A plan assertion is only meaningful once a seq scan is the WRONG choice —
	// and under `force_generic_plan` (which explainGenericPlan sets, because that
	// is the plan the driver's parameterized statement actually gets) what
	// decides that is **tenant cardinality**, not table size.
	//
	// Two fixtures failed here before this one, and both failures are worth
	// keeping written down because neither is obvious:
	//
	//  1. 400 rows, a quarter of them this organizer's → seq scan. Too small,
	//     and too concentrated.
	//  2. 5020 rows across TWO organizers → still a seq scan, and this is the
	//     instructive one. The generic plan cannot see the literal, so it
	//     estimates from `n_distinct` on organizer_id: with 2 tenants the average
	//     tenant owns half the table, so it predicted ~2008 rows and scanning was
	//     genuinely correct. The same query with a real value planned an index
	//     scan at 16 rows. The index was never the problem.
	//
	// So the fixture models real multi-tenancy: MANY organizers, each holding a
	// small slice. That is what makes `organizer_id = $1` selective in the
	// generic plan, and it is also the only condition under which the missing
	// index would actually hurt in production.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO organizers(name)
		SELECT 'channel-planner-' || g FROM generate_series(1, 200) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channels(organizer_id, code, display_name, kind, enabled)
		SELECT o.id, 'bulk-' || o.id || '-' || g, 'Bulk', 'web', (g % 5 <> 0)
		FROM organizers o, generate_series(1, 25) g
		WHERE o.name LIKE 'channel-planner-%'`); err != nil {
		t.Fatal(err)
	}
	// The two named organizers get a slice the same size as everyone else's, so
	// neither is special to the planner.
	for _, owner := range []uuid.UUID{org, other} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO channels(organizer_id, code, display_name, kind, enabled)
			SELECT $1::uuid, 'mine-' || $1::text || '-' || g, 'Mine', 'web', (g % 5 <> 0)
			FROM generate_series(1, 25) g`, owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `ANALYZE channels`); err != nil {
		t.Fatal(err)
	}

	plan := explainGenericPlan(ctx, t, db, publicChannelsQuery, org)
	assertReachesVia(t, plan, "channels", "channels_enabled_by_organizer")
}

// The registry is a LOOKUP, NOT A CONSTRAINT.
//
// Nothing may make an unregistered channel code unusable. This asserts the
// schema-level half of that guarantee: no foreign key anywhere in catalog
// references `channels`. The inventory half — that a sale on an unregistered
// code still completes — lives in inventory's own smoke suite, because that is
// where the claim path is.
func TestNothingReferencesTheChannelRegistry(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)

	rows, err := db.QueryContext(ctx, `
SELECT tc.table_name, tc.constraint_name
FROM information_schema.table_constraints tc
JOIN information_schema.constraint_column_usage ccu
  ON tc.constraint_name = ccu.constraint_name
 AND tc.table_schema = ccu.table_schema
WHERE tc.constraint_type = 'FOREIGN KEY'
  AND ccu.table_name = 'channels'
  AND tc.table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var offenders []string
	for rows.Next() {
		var table, constraint string
		if err := rows.Scan(&table, &constraint); err != nil {
			t.Fatal(err)
		}
		offenders = append(offenders, table+"."+constraint)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("foreign keys reference channels: %v.\n"+
			"ADR-024 forbids this: historical attribution must survive a channel being retired, "+
			"and an FK would make an unregistered code unsellable — the registry is a lookup, not a constraint.",
			offenders)
	}
}

// The migration is inert with respect to everything that already existed.
func TestChannelMigrationAddsOnlyTheRegistry(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM channels`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("channels has %d rows after migration, want 0 — 0018 must backfill nothing", n)
	}
	// The four columns that store channel codes keep their own bounds and are
	// untouched by 0018. If a later migration "helpfully" adds an FK or a
	// normalization, this and TestNothingReferencesTheChannelRegistry both fail.
	for _, col := range []struct{ table, column string }{
		{"fee_rules", "channel_code"},
		{"split_schedules", "channel_code"},
	} {
		var dataType string
		err := db.QueryRowContext(ctx, `
SELECT data_type FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			col.table, col.column).Scan(&dataType)
		if err != nil {
			t.Fatalf("%s.%s: %v", col.table, col.column, err)
		}
		if dataType != "text" {
			t.Fatalf("%s.%s is %s, want text — 0018 must not alter it", col.table, col.column, dataType)
		}
	}
}

func TestChannelDDLRejectsOutOfBoundsAndUnknownKind(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "ddl")

	// Written as raw SQL deliberately: the Go write gate would refuse these
	// first, so going through the store would prove the gate, not the schema.
	// The CHECKs are the last line of defence against a writer that is not this
	// store, and they are what this test is for.
	cases := []struct {
		name                       string
		code, displayName, kindStr string
	}{
		{"empty code", "", "X", "web"},
		{"code over 100", strings.Repeat("c", 101), "X", "web"},
		{"empty display name", "x", "", "web"},
		{"display name over 200", "x", strings.Repeat("d", 201), "web"},
		{"unknown kind", "x", "X", "partner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx,
				`INSERT INTO channels (organizer_id, code, display_name, kind) VALUES ($1,$2,$3,$4)`,
				org, tc.code, tc.displayName, tc.kindStr)
			if err == nil {
				t.Fatalf("INSERT succeeded, want a CHECK violation")
			}
		})
	}
}

func TestChannelRequiresAKnownOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_ = seedChannelOrganizer(ctx, t, db, "fk")
	_, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: uuid.New(), Code: "pos", DisplayName: "Box office", Kind: ChannelKindPOS, Enabled: true,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("create against an unknown organizer = %v, want ErrNotFound", err)
	}
}

var _ = context.Background

// The tenant boundary on the update, proved against the REAL SQL.
//
// The API-level version of this test passes against the in-memory fake, so it
// proves the handler and the fake agree — not that the UPDATE statement carries
// an organizer predicate. A mutation check made that concrete: deleting
// `AND organizer_id = $2` from the query killed no test at the API tier, because
// the fake does its own scoping in Go.
//
// This is the tier where the predicate exists, so this is the tier that has to
// assert it (TKT-236 ai-review).
func TestUpdateChannelIsScopedToTheOwningOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	victim := seedChannelOrganizer(ctx, t, db, "victim")
	attacker := seedChannelOrganizer(ctx, t, db, "attacker")

	theirs, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: victim, Code: "pos", DisplayName: "Their box office",
		Kind: ChannelKindPOS, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The attacker holds the id AND the exact code — the strongest position a
	// forged request can be in, since the back office takes the id from a form
	// field and the code is visible on the storefront selector.
	_, err = st.UpdateChannel(ctx, attacker, theirs.ID, ChannelUpdate{
		Code: "pos", DisplayName: "Hijacked", Kind: ChannelKindWeb, Enabled: false,
		ExpectedRevision: theirs.Revision,
	})
	// ErrNotFound, not ErrChannelCodeImmutable and not a mismatch error: an
	// answer that distinguished "not yours" from "no such channel" would confirm
	// a guessed id is real.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update = %v, want ErrNotFound — the UPDATE must carry an "+
			"organizer predicate, and the refusal must not confirm the id exists", err)
	}

	// Nothing moved.
	after, err := st.GetChannel(ctx, theirs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DisplayName != "Their box office" || after.Kind != ChannelKindPOS || !after.Enabled {
		t.Fatalf("the victim's channel was mutated: %+v", after)
	}

	// And the owner is unaffected — a boundary, not a wall.
	updated, err := st.UpdateChannel(ctx, victim, theirs.ID, ChannelUpdate{
		Code: "pos", DisplayName: "Counter", Kind: ChannelKindPOS, Enabled: false,
		ExpectedRevision: theirs.Revision,
	})
	if err != nil {
		t.Fatalf("the owning organizer must still update: %v", err)
	}
	if updated.DisplayName != "Counter" || updated.Enabled {
		t.Fatalf("owner update did not apply: %+v", updated)
	}
}

// ---- TKT-325: the channel's optimistic revision (ADR-080) ----
//
// These live at the store tier because the mechanism does: the comparison runs
// under a row lock in UpdateChannel's transaction, and the organizer predicate
// is SQL. The API fake compares in Go, so an API test passes with the lock or the
// predicate deleted (AGENTS.md, TKT-236). Every write that moves a revision here
// goes through UpdateChannel, never a raw UPDATE, so the counter really moves
// (AGENTS.md, TKT-250: a fixture that writes the guarded table directly does not
// move the counter, and the "stale" value then still matches).

// createRevisionChannel creates an enabled web channel and checks it starts at 0.
func createRevisionChannel(ctx context.Context, t *testing.T, st *Postgres, org uuid.UUID, code, name string) Channel {
	t.Helper()
	c, err := st.CreateChannel(ctx, ChannelInput{
		OrganizerID: org, Code: code, DisplayName: name, Kind: ChannelKindWeb, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create %q: %v", code, err)
	}
	if c.Revision != 0 {
		t.Fatalf("create %q returned revision %d, want 0", code, c.Revision)
	}
	return c
}

// assertChannelRow re-reads the row and compares it with want field by field,
// including the revision and the exact updated_at.
func assertChannelRow(ctx context.Context, t *testing.T, st *Postgres, want Channel, why string) {
	t.Helper()
	got, err := st.GetChannel(ctx, want.ID)
	if err != nil {
		t.Fatalf("%s: re-read: %v", why, err)
	}
	if got.Code != want.Code || got.DisplayName != want.DisplayName || got.Kind != want.Kind ||
		got.Enabled != want.Enabled || got.Revision != want.Revision ||
		!got.UpdatedAt.Equal(want.UpdatedAt) || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("%s:\n got  %+v\n want %+v", why, got, want)
	}
}

// Create starts at 0, and every full-row read returns the STORED value. The
// reads are checked after a bump on purpose: at 0 a read that forgot to select
// the column would return the Go zero value and pass.
func TestChannelRevisionStartsAtZeroAndEveryReadReturnsIt(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "revision-reads")
	c := createRevisionChannel(ctx, t, st, org, "web", "Website")

	for _, step := range []struct {
		expected, want int64
	}{{0, 1}, {1, 2}} {
		updated, err := st.UpdateChannel(ctx, org, c.ID, ChannelUpdate{
			Code: "web", DisplayName: "Website", Kind: ChannelKindWeb, Enabled: true,
			ExpectedRevision: step.expected,
		})
		if err != nil {
			t.Fatalf("update with revision %d: %v", step.expected, err)
		}
		if updated.Revision != step.want {
			t.Fatalf("update with revision %d returned revision %d, want %d", step.expected, updated.Revision, step.want)
		}
	}

	got, err := st.GetChannel(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 {
		t.Fatalf("GetChannel revision = %d, want 2", got.Revision)
	}
	list, err := st.ListChannels(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Revision != 2 {
		t.Fatalf("ListChannels = %+v, want one row at revision 2", list)
	}
}

// The reported defect and its mirror image (D1), at the store tier. B writes
// through UpdateChannel with the revision both read; A then sends a full
// replacement built from the same, now stale, read. A is refused, and B's row
// survives exactly: every field, the timestamp and the revision.
func TestStaleChannelUpdateIsRefusedInBothDirections(t *testing.T) {
	cases := []struct {
		name string
		// b is the other writer's change; a is the stale writer's full replacement.
		b, a func(c Channel) ChannelUpdate
	}{
		{
			name: "rename after a concurrent toggle",
			b: func(c Channel) ChannelUpdate {
				return ChannelUpdate{Code: c.Code, DisplayName: c.DisplayName, Kind: c.Kind, Enabled: false, ExpectedRevision: c.Revision}
			},
			// A's stale snapshot still says enabled=true.
			a: func(c Channel) ChannelUpdate {
				return ChannelUpdate{Code: c.Code, DisplayName: "Renamed by A", Kind: c.Kind, Enabled: true, ExpectedRevision: c.Revision}
			},
		},
		{
			name: "toggle after a concurrent rename",
			b: func(c Channel) ChannelUpdate {
				return ChannelUpdate{Code: c.Code, DisplayName: "Renamed by B", Kind: c.Kind, Enabled: c.Enabled, ExpectedRevision: c.Revision}
			},
			// A's stale snapshot still carries the old name.
			a: func(c Channel) ChannelUpdate {
				return ChannelUpdate{Code: c.Code, DisplayName: c.DisplayName, Kind: c.Kind, Enabled: false, ExpectedRevision: c.Revision}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db, st := seasonSmokeStore(t)
			org := seedChannelOrganizer(ctx, t, db, "stale")
			read := createRevisionChannel(ctx, t, st, org, "web", "Website")

			bRow, err := st.UpdateChannel(ctx, org, read.ID, tc.b(read))
			if err != nil {
				t.Fatalf("B's update: %v", err)
			}
			if bRow.Revision != read.Revision+1 {
				t.Fatalf("B's update returned revision %d, want %d", bRow.Revision, read.Revision+1)
			}

			_, err = st.UpdateChannel(ctx, org, read.ID, tc.a(read))
			if !errors.Is(err, ErrChannelRevisionMismatch) {
				t.Fatalf("A's stale update = %v, want ErrChannelRevisionMismatch", err)
			}
			assertChannelRow(ctx, t, st, bRow, "after A's refused update, B's row must survive unchanged")

			// A second click with the same stale token is refused again and
			// still moves nothing.
			_, err = st.UpdateChannel(ctx, org, read.ID, tc.a(read))
			if !errors.Is(err, ErrChannelRevisionMismatch) {
				t.Fatalf("A's repeated stale update = %v, want ErrChannelRevisionMismatch", err)
			}
			assertChannelRow(ctx, t, st, bRow, "after A's second refused update")
		})
	}
}

// Every successful PUT bumps once, including one whose values are unchanged: the
// revision counts writes, not differences. Plus one, not plus two.
func TestUnchangedChannelReplacementBumpsTheRevisionOnce(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "unchanged")
	c := createRevisionChannel(ctx, t, st, org, "web", "Website")

	same := ChannelUpdate{Code: c.Code, DisplayName: c.DisplayName, Kind: c.Kind, Enabled: c.Enabled, ExpectedRevision: 0}
	updated, err := st.UpdateChannel(ctx, org, c.ID, same)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 1 {
		t.Fatalf("an unchanged replacement returned revision %d, want exactly 1", updated.Revision)
	}
	assertChannelRow(ctx, t, st, updated, "the stored row must equal what the update returned")
	// The old token is now stale, even though no value changed.
	if _, err := st.UpdateChannel(ctx, org, c.ID, same); !errors.Is(err, ErrChannelRevisionMismatch) {
		t.Fatalf("revision 0 after an unchanged replacement = %v, want ErrChannelRevisionMismatch", err)
	}
	assertChannelRow(ctx, t, st, updated, "a refusal must not move the revision")
}

// Ownership is decided before the revision and the code. Another organizer gets
// ErrNotFound with the CURRENT revision and code (the strongest forged
// position) and with a stale revision and a wrong code: an answer that
// distinguished a mismatch would confirm the id is real and leak whether the
// guessed revision is current. The victim's row is at revision 1, so "current"
// and "stale" are different values.
func TestChannelRevisionIsCheckedOnlyAfterOwnership(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	victim := seedChannelOrganizer(ctx, t, db, "rev-victim")
	attacker := seedChannelOrganizer(ctx, t, db, "rev-attacker")
	c := createRevisionChannel(ctx, t, st, victim, "pos", "Their box office")
	theirs, err := st.UpdateChannel(ctx, victim, c.ID, ChannelUpdate{
		Code: "pos", DisplayName: "Their box office", Kind: ChannelKindWeb, Enabled: true, ExpectedRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		in   ChannelUpdate
	}{
		{"current revision and code", ChannelUpdate{Code: "pos", DisplayName: "Hijacked", Kind: ChannelKindPOS, Enabled: false, ExpectedRevision: theirs.Revision}},
		{"stale revision and wrong code", ChannelUpdate{Code: "other", DisplayName: "Hijacked", Kind: ChannelKindPOS, Enabled: false, ExpectedRevision: theirs.Revision - 1}},
	} {
		_, err := st.UpdateChannel(ctx, attacker, theirs.ID, tc.in)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: cross-tenant update = %v, want ErrNotFound", tc.name, err)
		}
		assertChannelRow(ctx, t, st, theirs, tc.name+": the victim's row must not move")
	}

	// A boundary, not a wall: the owner still updates with the current token.
	owned, err := st.UpdateChannel(ctx, victim, theirs.ID, ChannelUpdate{
		Code: "pos", DisplayName: "Counter", Kind: ChannelKindPOS, Enabled: false, ExpectedRevision: theirs.Revision,
	})
	if err != nil {
		t.Fatalf("the owner must still update: %v", err)
	}
	if owned.Revision != theirs.Revision+1 || owned.DisplayName != "Counter" || owned.Enabled {
		t.Fatalf("owner update = %+v, want revision %d, Counter, disabled", owned, theirs.Revision+1)
	}
}

// The code-immutability refusal stays distinct when the token is current, and a
// stale token is reported before a wrong code (A5's order: revision, then code).
func TestChannelCodeRefusalIsDistinctFromTheRevisionRefusal(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "code-vs-revision")
	c := createRevisionChannel(ctx, t, st, org, "web", "Website")
	bumped, err := st.UpdateChannel(ctx, org, c.ID, ChannelUpdate{
		Code: "web", DisplayName: "Website", Kind: ChannelKindWeb, Enabled: true, ExpectedRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateChannel(ctx, org, c.ID, ChannelUpdate{
		Code: "renamed", DisplayName: "Website", Kind: ChannelKindWeb, Enabled: true, ExpectedRevision: bumped.Revision,
	})
	if !errors.Is(err, ErrChannelCodeImmutable) {
		t.Fatalf("wrong code, current revision = %v, want ErrChannelCodeImmutable", err)
	}
	_, err = st.UpdateChannel(ctx, org, c.ID, ChannelUpdate{
		Code: "renamed", DisplayName: "Website", Kind: ChannelKindWeb, Enabled: true, ExpectedRevision: 0,
	})
	if !errors.Is(err, ErrChannelRevisionMismatch) {
		t.Fatalf("wrong code, stale revision = %v, want ErrChannelRevisionMismatch", err)
	}
	assertChannelRow(ctx, t, st, bumped, "neither refusal may write")
}

// The comparison is made UNDER THE ROW LOCK (A9). Two writers present the same
// revision at the same moment; exactly one wins and the revision moves once.
//
// The schedule is forced, not hoped for (AGENTS.md, TKT-308): a lock holder on a
// dedicated connection takes the row first, both writers are started, and the
// test waits until PostgreSQL itself reports both blocked behind the holder
// before releasing it. No sleep stands in for "both reached the lock".
//
// The wait counts the holder's TRANSITIVE waiters. A direct
// `$holder = ANY(pg_blocking_pids(pid))` reaches only 1: the second row-lock
// waiter queues on the TUPLE lock the first waiter holds, so pg_blocking_pids
// names the first waiter, not the holder (verified against PostgreSQL 18).
//
// What this catches that the sequential tests cannot: with `FOR UPDATE` removed
// and the Go comparison kept, both writers read revision 0 without waiting, both
// pass the comparison, both then queue on the UPDATE, and both apply. That run
// ends with two successes and revision 2.
func TestConcurrentChannelUpdatesWithOneRevisionApplyExactlyOnce(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	org := seedChannelOrganizer(ctx, t, db, "contention")
	c := createRevisionChannel(ctx, t, st, org, "web", "Website")

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	var holderPID int
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	holdTx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holdTx.Rollback() }()
	if _, err := holdTx.ExecContext(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, c.ID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		row Channel
		err error
	}
	results := make(chan result, 2)
	for _, in := range []ChannelUpdate{
		{Code: "web", DisplayName: "Renamed", Kind: ChannelKindWeb, Enabled: true, ExpectedRevision: 0},
		{Code: "web", DisplayName: "Website", Kind: ChannelKindWeb, Enabled: false, ExpectedRevision: 0},
	} {
		go func(in ChannelUpdate) {
			row, err := st.UpdateChannel(ctx, org, c.ID, in)
			results <- result{row, err}
		}(in)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		if err := db.QueryRowContext(ctx, `
WITH RECURSIVE waiters AS (
    SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
    UNION
    SELECT a.pid FROM pg_stat_activity a JOIN waiters w ON w.pid = ANY(pg_blocking_pids(a.pid))
)
SELECT count(*) FROM waiters`, holderPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked == 2 {
			break
		}
		if blocked > 2 {
			t.Fatalf("%d backends wait behind the holder, want 2 — something else is touching this row", blocked)
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 2 writers blocked behind the lock holder within 10s", blocked)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := holdTx.Rollback(); err != nil {
		t.Fatal(err)
	}

	var won []Channel
	var refused int
	for range 2 {
		r := <-results
		switch {
		case r.err == nil:
			won = append(won, r.row)
		case errors.Is(r.err, ErrChannelRevisionMismatch):
			refused++
		default:
			t.Fatalf("writer failed with %v, want success or ErrChannelRevisionMismatch", r.err)
		}
	}
	if len(won) != 1 || refused != 1 {
		t.Fatalf("%d writers succeeded and %d were refused, want exactly 1 and 1 — "+
			"the revision must be compared under the row lock", len(won), refused)
	}
	if won[0].Revision != 1 {
		t.Fatalf("winner's revision = %d, want 1", won[0].Revision)
	}
	assertChannelRow(ctx, t, st, won[0], "the stored row must be the winner's, at revision 1")
}
