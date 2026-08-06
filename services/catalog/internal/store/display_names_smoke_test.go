//go:build smoke

package store

import (
	"testing"

	"github.com/google/uuid"
)

// The display-name resolver against real Postgres (TKT-222 / US-A3).
//
// This exists because the unit tests could not see either of the two bugs that
// shipped past them, and both were driver-level:
//
//  1. `= ANY($1)` without a `::uuid[]` cast type-checks in Go and fails only when
//     a real driver has to infer the parameter's type;
//  2. `events.name` is jsonb and LocalizedText is a plain map with no
//     sql.Scanner, so scanning straight into it compiles and fails at runtime.
//
// ADR-028 laundered both into "response violates OpenAPI contract" / a generic
// 500, which reads as a contract problem rather than a SQL one. A browser run
// found them; this is the test that would have.
//
// The seeded row is what makes it work: an unknown-ids-only version proves the
// query EXECUTES but never scans a row, so it catches (1) and is blind to (2).
func TestPerformanceDisplayNamesResolveAgainstRealPostgres(t *testing.T) {
	ctx, db, pg := festivalSmokeStore(t)
	_, _, _, day := seedFestivalDay(t, ctx, db, false)

	got, err := pg.PerformanceDisplayNames(ctx, []uuid.UUID{day, uuid.New()})
	if err != nil {
		t.Fatalf("the query did not execute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("resolved %d rows, want exactly the one that exists (the unknown id must be "+
			"omitted, not an error): %+v", len(got), got)
	}
	if got[0].PerformanceID != day {
		t.Fatalf("resolved %s, want %s", got[0].PerformanceID, day)
	}
	// The jsonb half. A scan that failed to unmarshal would leave this nil.
	if got[0].EventName["en"] == "" {
		t.Fatalf("the localized name did not survive the scan: %+v", got[0].EventName)
	}
	// A FESTIVAL DAY has no single instant — an operating date and opening hours
	// instead (ADR-014) — so this fixture's starts_at is legitimately NULL. That
	// is exactly why the fixture is a festival day: a plain time.Time destination
	// fails on precisely the purchases a festival wallet contains, and a fixture
	// with a start time would never have shown it.
	if got[0].StartsAt != nil {
		t.Fatalf("a festival day reported a start instant: %v", *got[0].StartsAt)
	}
}

// A festival day is seeded in DRAFT, which is the point: publication controls
// what may be SOLD, not what may be NAMED, and a wallet is mostly past purchases.
// The test above therefore already proves the resolver ignores publication state
// — this asserts it explicitly so the reason survives a refactor of the fixture.
func TestPerformanceDisplayNamesIgnorePublicationState(t *testing.T) {
	ctx, db, pg := festivalSmokeStore(t)
	_, _, _, day := seedFestivalDay(t, ctx, db, false)

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM performances WHERE id=$1`, day).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "published" {
		t.Fatalf("the fixture is published, so it cannot distinguish a resolver that filters "+
			"from one that does not — status is %q", status)
	}

	got, err := pg.PerformanceDisplayNames(ctx, []uuid.UUID{day})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("an unpublished performance could not be named; a wallet of past purchases would "+
			"go blank exactly where it matters")
	}
}
