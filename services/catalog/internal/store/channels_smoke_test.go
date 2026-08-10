//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

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
	})
	if !errors.Is(err, ErrChannelCodeImmutable) {
		t.Fatalf("rename = %v, want ErrChannelCodeImmutable", err)
	}

	// Nothing was written. The UPDATE carries the code comparison in its WHERE
	// clause, so a mismatch matches no row — there is no window in which the
	// display name lands and the code does not.
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
	})
	if err != nil {
		t.Fatalf("the owning organizer must still update: %v", err)
	}
	if updated.DisplayName != "Counter" || updated.Enabled {
		t.Fatalf("owner update did not apply: %+v", updated)
	}
}
