package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

func correctorFor(f *fakeReemit) orphanCorrector {
	return orphanCorrector{list: f.list, publisher: f}
}

func enabledSeatedRow() store.Performance {
	p := perfRow("single")
	mapID := uuid.New()
	p.SeatMapID = &mapID
	p.OrphanPreventionEnabled = true
	return p
}

// TestOrphanCorrectionPublishesEveryCandidate verifies that each listed row reaches
// the correction publisher with the fields needed to build the event.
func TestOrphanCorrectionPublishesEveryCandidate(t *testing.T) {
	rows := []store.Performance{enabledSeatedRow(), enabledSeatedRow()}
	f := &fakeReemit{rows: rows}
	n, err := correctorFor(f).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != len(rows) || len(f.corrected) != len(rows) {
		t.Fatalf("corrected=%d published=%d, want %d", n, len(f.corrected), len(rows))
	}
	for i, p := range f.corrected {
		if p != rows[i] {
			t.Fatalf("corrected[%d]=%+v, want %+v", i, p, rows[i])
		}
	}
}

// TestOrphanCorrectionDrainsAcrossPagesAndCanRerun verifies pagination and that
// each invocation processes the full current candidate set.
func TestOrphanCorrectionDrainsAcrossPagesAndCanRerun(t *testing.T) {
	var rows []store.Performance
	for i := 0; i < reemitBatchSize+2; i++ {
		rows = append(rows, enabledSeatedRow())
	}
	f := &fakeReemit{rows: rows}
	n, err := correctorFor(f).run(context.Background())
	if err != nil || n != len(rows) {
		t.Fatalf("corrected=%d err=%v, want the full candidate set %d", n, err, len(rows))
	}
	if len(f.pageSizes) < 2 {
		t.Fatalf("expected keyset pagination across pages, got %v", f.pageSizes)
	}
	f2 := &fakeReemit{rows: rows}
	again, err := correctorFor(f2).run(context.Background())
	if err != nil || again != len(rows) {
		t.Fatalf("rerun corrected=%d err=%v, want the same set re-emitted", again, err)
	}
}

// TestOrphanCorrectionPublishFailureAborts: the run must never report a repair it
// cannot prove. An operator reads that count and stops worrying.
func TestOrphanCorrectionPublishFailureAborts(t *testing.T) {
	f := &fakeReemit{rows: []store.Performance{enabledSeatedRow()}, pubErr: errors.New("broker down")}
	if _, err := correctorFor(f).run(context.Background()); err == nil {
		t.Fatal("a failed publish must surface as an error")
	}
}
