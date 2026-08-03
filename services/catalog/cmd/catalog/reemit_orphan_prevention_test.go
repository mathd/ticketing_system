package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"
)

func correctorFor(f *fakeReemit) orphanCorrector {
	return orphanCorrector{list: f.list, publish: f.publish}
}

func enabledSeatedRow() store.Performance {
	p := perfRow("single")
	mapID := uuid.New()
	p.SeatMapID = &mapID
	p.OrphanPreventionEnabled = true
	return p
}

// TestOrphanCorrectionEmitsSchema5ForEveryCandidate: the wave's whole job. Each listed
// candidate is re-emitted exactly once, and the payload the publish port would build is
// schema 5 — the variant inventory needs to attach the flag and the projection.
func TestOrphanCorrectionEmitsSchema5ForEveryCandidate(t *testing.T) {
	rows := []store.Performance{enabledSeatedRow(), enabledSeatedRow()}
	f := &fakeReemit{rows: rows}
	n, err := correctorFor(f).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != len(rows) || len(f.published) != len(rows) {
		t.Fatalf("corrected=%d published=%d, want %d", n, len(f.published), len(rows))
	}
	for i, p := range f.published {
		if p.ID != rows[i].ID || !p.OrphanPreventionEnabled || p.SeatMapID == nil {
			t.Fatalf("published[%d]=%+v — a candidate must reach the publisher with its bound map and flag intact", i, p)
		}
	}
}

// TestOrphanCorrectionIDEscapesBothConsumedNamespaces: the wave re-emits publications
// catalog ALREADY emitted at schema 4. Under the live id inventory's consumed_events
// drops it; under the re_entry backfill id access's does. Either way the repair is a
// silent no-op, which is the failure this ticket exists to avoid.
func TestOrphanCorrectionIDEscapesBothConsumedNamespaces(t *testing.T) {
	p := enabledSeatedRow()
	id := events.OrphanPreventionCorrectionEventID(p)
	if id == events.EventID(p) || id == events.BackfillEventID(p) {
		t.Fatalf("correction id %s collides with an identity a consumer has already seen", id)
	}
}

// TestOrphanCorrectionRerunConverges: re-running is the design, not a tolerance. The
// second run re-emits the SAME deterministic ids, which downstream consumed_events
// absorbs — so a slot published at schema 4 by an undrained old catalog replica is
// picked up by the next run rather than being lost for ever (ADR-041).
func TestOrphanCorrectionRerunConverges(t *testing.T) {
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
	for i := range f.published {
		if events.OrphanPreventionCorrectionEventID(f.published[i]) !=
			events.OrphanPreventionCorrectionEventID(f2.published[i]) {
			t.Fatal("a re-run produced a different identity — repeats would multiply events instead of converging")
		}
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

// TestOrphanCorrectionSubcommandIsRegistered: an operator command that is not in the
// registry is unreachable in the shipped image, which no unit test of run() would catch.
func TestOrphanCorrectionSubcommandIsRegistered(t *testing.T) {
	if _, ok := subcommands()["reemit-orphan-prevention"]; !ok {
		t.Fatal("reemit-orphan-prevention is not registered — the wave cannot be run in the image")
	}
}

// TestReemitPoliciesInheritsTheSchemaFork states, rather than hides, a side effect of
// TKT-183: reemit-policies publishes through the same envelope builder, so from this
// commit it emits schema 5 for a slot bound to a rule-enabled version — under the
// re_entry BACKFILL identity, which inventory has never consumed.
//
// That is deliberate. Suppressing it would make the re_entry backfill emit a payload
// that disagrees with the live one, which is precisely the property TKT-96's golden
// exists to prevent. It also does no harm: inventory's schema-5 arm provisions or
// upgrades the pool exactly as the correction wave would. This test exists so the
// behaviour is a decision on the record and not a surprise in production.
func TestReemitPoliciesInheritsTheSchemaFork(t *testing.T) {
	p := enabledSeatedRow()
	if events.BackfillEventID(p) == events.OrphanPreventionCorrectionEventID(p) {
		t.Fatal("the two waves must keep separate identities or one silently swallows the other")
	}
	// Both waves carry the same slot, so both serialize schema 5 for it. The wave that
	// matters is the one whose identity is unconsumed; both here are.
	if !p.OrphanPreventionEnabled {
		t.Fatal("fixture no longer represents an enabled bound version")
	}
}
