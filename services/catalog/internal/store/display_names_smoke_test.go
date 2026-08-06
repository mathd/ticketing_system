//go:build smoke

package store

import (
	"testing"

	"github.com/google/uuid"
)

// The display-name resolver against real Postgres (TKT-222 / US-A3).
//
// This exists because the unit tests could not see the bug that actually
// shipped: `= ANY($1)` without a `::uuid[]` cast type-checks in Go, passes every
// fake-store test, and fails only when a real driver has to infer the parameter's
// type — where ADR-028 then launders it into "response violates OpenAPI
// contract", a symptom a long way from its cause. A browser run found it; this
// test is what would have.
func TestPerformanceDisplayNamesResolveAgainstRealPostgres(t *testing.T) {
	ctx, _, pg := festivalSmokeStore(t)

	// Ids that resolve to nothing are the cheap half: they prove the query RUNS —
	// which is the part that was broken — without needing a seeded catalog.
	got, err := pg.PerformanceDisplayNames(ctx, []uuid.UUID{uuid.New(), uuid.New()})
	if err != nil {
		t.Fatalf("the query did not execute: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown ids resolved to %d rows, want none", len(got))
	}
}
