package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"
)

type orderingFake struct {
	rows       []store.Performance
	emitted    []store.Performance
	emittedIDs []string
	pageSizes  []int
	err        error
}

func (f *orderingFake) list(_ context.Context, after *uuid.UUID, limit int) ([]store.Performance, error) {
	f.pageSizes = append(f.pageSizes, limit)
	if f.err != nil {
		return nil, f.err
	}
	start := 0
	if after != nil {
		for i, row := range f.rows {
			if row.ID == *after {
				start = i + 1
			}
		}
	}
	end := min(start+limit, len(f.rows))
	return f.rows[start:end], nil
}

func (f *orderingFake) PerformancePublishedBestAvailableOrderingCorrection(_ context.Context, perf store.Performance) error {
	if f.err != nil {
		return f.err
	}
	f.emitted = append(f.emitted, perf)
	f.emittedIDs = append(f.emittedIDs, events.BestAvailableOrderingCorrectionEventID(perf))
	return nil
}

func TestBestAvailableOrderingCorrectionPaginatesAndReruns(t *testing.T) {
	f := &orderingFake{}
	for range reemitBatchSize + 2 {
		row := perfRow("single")
		row.SeatMapID = new(uuid.UUID)
		*row.SeatMapID = uuid.New()
		f.rows = append(f.rows, row)
	}
	c := orderingCorrector{list: f.list, publisher: f}

	n, err := c.run(context.Background())
	if err != nil || n != len(f.rows) || len(f.emitted) != len(f.rows) {
		t.Fatalf("corrected=%d emitted=%d err=%v, want %d", n, len(f.emitted), err, len(f.rows))
	}
	if len(f.pageSizes) < 2 {
		t.Fatalf("pages = %v, want keyset pagination", f.pageSizes)
	}
	firstRunIDs := append([]string(nil), f.emittedIDs...)
	secondCount, err := c.run(context.Background())
	if err != nil || secondCount != len(f.rows) {
		t.Fatalf("second run corrected=%d err=%v, want %d", secondCount, err, len(f.rows))
	}
	if len(f.emittedIDs) != 2*len(firstRunIDs) {
		t.Fatalf("total emitted ids = %d, want %d", len(f.emittedIDs), 2*len(firstRunIDs))
	}
	for i, id := range firstRunIDs {
		if got := f.emittedIDs[len(firstRunIDs)+i]; got != id {
			t.Fatalf("second run id[%d] = %s, want deterministic id %s", i, got, id)
		}
	}
}

func TestBestAvailableOrderingCorrectionStopsOnPublishFailure(t *testing.T) {
	f := &orderingFake{rows: []store.Performance{perfRow("single")}}
	c := orderingCorrector{list: f.list, publisher: failingOrderingPublisher{}}
	if _, err := c.run(context.Background()); err == nil {
		t.Fatal("publish failure must stop the wave")
	}
}

type failingOrderingPublisher struct{}

func (failingOrderingPublisher) PerformancePublishedBestAvailableOrderingCorrection(context.Context, store.Performance) error {
	return errors.New("publish failed")
}

func TestReemitBestAvailableOrderingRejectsArguments(t *testing.T) {
	if err := reemitBestAvailableOrdering([]string{"--all"}); err == nil {
		t.Fatal("unexpected arguments must return a usage error")
	}
}
