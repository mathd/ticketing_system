//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// seedPublishedMap authors a minimal draft map (one section/row/seat so it is a
// real map) and publishes it, returning the published version's id.
func seedPublishedMap(ctx context.Context, t *testing.T, st *Postgres, name string) SeatMap {
	t.Helper()
	m := seedDraftMap(ctx, t, st, name)
	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "1", Position: 1}); err != nil {
		t.Fatal(err)
	}
	published, needsEmit, err := st.PublishSeatMap(ctx, m.ID)
	if err != nil {
		t.Fatalf("publish seat map: %v", err)
	}
	if published.Status != "published" || !needsEmit {
		t.Fatalf("published map = %q needsEmit=%v, want published + owed", published.Status, needsEmit)
	}
	return published
}

// TestPublishSeatMapMonotonic (TKT-103 COS-1) pins the publish transition as a
// monotonic, lock-free draft->published flip mirroring PublishPerformance
// (ADR-018: a monotonic one-way transition needs no row lock). Idempotent, and
// the owed-marker discipline (needsEmit true then false after mark) matches
// performance publication.
func TestPublishSeatMapMonotonic(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedDraftMap(ctx, t, st, "Main floor")

	published, needsEmit, err := st.PublishSeatMap(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != "published" || published.PublishedAt == nil {
		t.Fatalf("first publish = %q publishedAt=%v, want published with a timestamp", published.Status, published.PublishedAt)
	}
	if !needsEmit {
		t.Fatal("first publish must owe the domain event")
	}

	// Idempotent re-publish: still published, but the event is no longer owed
	// only after we mark it. Before marking, a retry re-owes (at-least-once).
	again, needsEmitAgain, err := st.PublishSeatMap(ctx, m.ID)
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if again.Status != "published" || !needsEmitAgain {
		t.Fatalf("re-publish before mark = %q needsEmit=%v, want published + still owed", again.Status, needsEmitAgain)
	}

	if err := st.MarkSeatMapEventEmitted(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	marked, needsEmitAfterMark, err := st.PublishSeatMap(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if marked.Status != "published" || needsEmitAfterMark {
		t.Fatalf("re-publish after mark = %q needsEmit=%v, want published + not owed", marked.Status, needsEmitAfterMark)
	}
}

// TestPublishSeatMapUnknown pins the not-found path.
func TestPublishSeatMapUnknown(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	if _, _, err := st.PublishSeatMap(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publish unknown map err = %v, want ErrNotFound", err)
	}
}

// TestPublishedSeatMapIsImmutable (TKT-103 COS-1) proves a published version can
// no longer be authored: the existing status='draft' write gate now bites.
func TestPublishedSeatMapIsImmutable(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "Frozen")

	_, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "New", Position: 9})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("adding a section to a published map err = %v, want ErrNotFound (write gate)", err)
	}
}
