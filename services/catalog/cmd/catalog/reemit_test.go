package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

func TestCommandRegistryInvokesEveryCatalogCallback(t *testing.T) {
	var invoked string
	withoutArgs := func(name string) func() error {
		return func() error { invoked = name; return nil }
	}
	withArgs := func(name string) func([]string) error {
		return func([]string) error { invoked = name; return nil }
	}
	callbacks := commandCallbacks{
		migrate: withoutArgs("migrate"), healthcheck: func() int { invoked = "healthcheck"; return 7 },
		reemitPolicies: withArgs("reemit-policies"), reemitOrphanPrevention: withArgs("reemit-orphan-prevention"),
		provisionStaff: withArgs("provision-staff"), validateRules: withArgs("validate-rules"),
	}
	registry := commandRegistry(callbacks)
	names := []string{
		"migrate", "healthcheck", "reemit-policies", "reemit-orphan-prevention",
		"provision-staff", "validate-rules",
	}
	if len(registry) != len(names) {
		t.Fatalf("registry has %d commands, test names %d", len(registry), len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			invoked = ""
			got := execute([]string{name, "tail"}, callbacks, func() error {
				t.Fatal("server ran after a command was selected")
				return nil
			})
			wantExit := 0
			if name == "healthcheck" {
				wantExit = 7
			}
			if got.Name != name || got.Err != nil || got.ExitCode != wantExit {
				t.Fatalf("dispatch result = %+v, want %s with exit %d", got, name, wantExit)
			}
			if invoked != name {
				t.Fatalf("invoked %q, want %q", invoked, name)
			}
		})
	}
}

func TestReemitPoliciesRejectsArguments(t *testing.T) {
	if err := reemitPolicies([]string{"--all"}); err == nil {
		t.Fatal("unexpected arguments must be a usage error, not silently ignored")
	}
}

type fakeReemit struct {
	rows       []store.Performance
	backfilled []store.Performance
	corrected  []store.Performance
	pubErr     error
	pageSizes  []int
}

func (f *fakeReemit) list(_ context.Context, after *uuid.UUID, limit int) ([]store.Performance, error) {
	f.pageSizes = append(f.pageSizes, limit)
	start := 0
	if after != nil {
		for i, r := range f.rows {
			if r.ID == *after {
				start = i + 1
			}
		}
	}
	end := start + limit
	if end > len(f.rows) {
		end = len(f.rows)
	}
	return f.rows[start:end], nil
}

func (f *fakeReemit) PerformancePublishedBackfill(_ context.Context, p store.Performance) error {
	if f.pubErr != nil {
		return f.pubErr
	}
	f.backfilled = append(f.backfilled, p)
	return nil
}

func (f *fakeReemit) PerformancePublishedOrphanCorrection(_ context.Context, p store.Performance) error {
	if f.pubErr != nil {
		return f.pubErr
	}
	f.corrected = append(f.corrected, p)
	return nil
}

func reemitterFor(f *fakeReemit) policyReemitter {
	return policyReemitter{list: f.list, publisher: f}
}

func perfRow(mode string) store.Performance {
	pubAt := time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC)
	return store.Performance{
		ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(),
		Kind: store.KindPerformance, Capacity: 250, PublishedAt: &pubAt,
		ReEntry: store.ReEntryPolicy{Mode: mode},
	}
}

// TestReemitPublishesEachListedSlot: every published ungrouped slot the list
// returns is re-emitted exactly once, through the backfill publish port.
func TestReemitPublishesEachListedSlot(t *testing.T) {
	rows := []store.Performance{perfRow("multi"), perfRow("count_limited"), perfRow("single")}
	f := &fakeReemit{rows: rows}
	n, err := reemitterFor(f).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != len(rows) || len(f.backfilled) != len(rows) {
		t.Fatalf("reemitted=%d backfilled=%d, want %d", n, len(f.backfilled), len(rows))
	}
	for i, p := range f.backfilled {
		if p != rows[i] {
			t.Fatalf("backfilled[%d]=%+v, want %+v", i, p, rows[i])
		}
	}
}

// TestReemitDrainsAcrossPagesAndReruns: a backlog larger than one page fully
// drains via the id keyset, and a re-run publishes the same set again (source is
// stateless; downstream consumed_events dedup makes the second run a no-op).
func TestReemitDrainsAcrossPagesAndReruns(t *testing.T) {
	var rows []store.Performance
	for i := 0; i < reemitBatchSize+3; i++ {
		rows = append(rows, perfRow("multi"))
	}
	f := &fakeReemit{rows: rows}
	n, err := reemitterFor(f).run(context.Background())
	if err != nil || n != len(rows) {
		t.Fatalf("reemitted=%d err=%v, want the full backlog %d", n, err, len(rows))
	}
	if len(f.pageSizes) < 2 {
		t.Fatalf("expected multiple pages, got page sizes %v", f.pageSizes)
	}
	f2 := &fakeReemit{rows: rows}
	again, err := reemitterFor(f2).run(context.Background())
	if err != nil || again != len(rows) {
		t.Fatalf("rerun reemitted=%d err=%v, want the same backlog re-emitted (downstream dedups)", again, err)
	}
}

// TestReemitPublishFailureAborts: a publish error surfaces and halts the run;
// the backfill must never claim a success it cannot prove.
func TestReemitPublishFailureAborts(t *testing.T) {
	f := &fakeReemit{rows: []store.Performance{perfRow("multi")}, pubErr: errors.New("broker down")}
	if _, err := reemitterFor(f).run(context.Background()); err == nil {
		t.Fatal("a failed publish must surface as an error")
	}
}
