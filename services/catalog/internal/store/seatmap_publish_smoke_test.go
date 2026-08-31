//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"
	"time"

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
	published, needsEmit, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
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

	published, needsEmit, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
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
	again, needsEmitAgain, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if again.Status != "published" || !needsEmitAgain {
		t.Fatalf("re-publish before mark = %q needsEmit=%v, want published + still owed", again.Status, needsEmitAgain)
	}

	if err := st.MarkSeatMapEventEmitted(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	marked, needsEmitAfterMark, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
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
	if _, _, err := st.PublishSeatMap(ctx, seatMapOrg, uuid.New()); !errors.Is(err, ErrNotFound) {
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

// TestPublicEventReadCarriesSeatMapID is TKT-172 AC1 against real Postgres.
//
// It exists because the public read scans POSITIONALLY: publicPerformancesSelect
// lists columns and rows.Scan lists targets, and adding one to either without the
// other is a runtime failure, not a compile error. The API-level test runs against
// a fake store and cannot see that class of bug at all — only a real query can.
//
// Both slots live under one published event so the assertion is about hydration,
// not about which rows the predicate returns: the seated one carries the exact
// published version, the GA one carries nil.
func TestPublicEventReadCarriesSeatMapID(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)
	m := seedPublishedMap(ctx, t, st, "Main floor")

	event, err := st.CreateEvent(ctx, EventInput{
		OrganizerID: seatMapOrg,
		Name:        LocalizedText{"en": "Recital", "fr": "Récital"},
	})
	if err != nil {
		t.Fatal(err)
	}
	startsAt := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	seatMaps := map[bool]*uuid.UUID{true: &m.ID, false: nil}
	perfIDs := map[bool]uuid.UUID{}
	for _, seated := range []bool{true, false} {
		at := startsAt
		if !seated {
			at = startsAt.Add(24 * time.Hour) // distinct start times keep the ordering stable
		}
		perf, err := st.CreatePerformance(ctx, PerformanceInput{
			OrganizerID: seatMapOrg, EventID: event.ID, VenueID: seatMapVenue,
			StartsAt: &at, Timezone: "Europe/Paris", SeatMapID: seatMaps[seated],
		})
		if err != nil {
			t.Fatalf("create performance (seated=%v): %v", seated, err)
		}
		if _, err = st.CreateTicketType(ctx, TicketTypeInput{
			OrganizerID: seatMapOrg, PerformanceID: perf.ID,
			Name: LocalizedText{"en": "Seat", "fr": "Place"}, PriceAmount: 5000, Currency: "EUR",
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.PublishPerformance(ctx, seatMapOrg, perf.ID); err != nil {
			t.Fatalf("publish (seated=%v): %v", seated, err)
		}
		perfIDs[seated] = perf.ID
	}

	agg, err := st.GetPublishedEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("published event read: %v", err)
	}
	if len(agg.Performances) != 2 {
		t.Fatalf("want both slots in the public read, got %d", len(agg.Performances))
	}
	for _, pa := range agg.Performances {
		switch pa.Performance.ID {
		case perfIDs[true]:
			if pa.Performance.SeatMapID == nil || *pa.Performance.SeatMapID != m.ID {
				t.Fatalf("seated slot hydrated SeatMapID = %v, want %v — the public projection "+
					"or its positional scan target is missing p.seat_map_id", pa.Performance.SeatMapID, m.ID)
			}
		case perfIDs[false]:
			if pa.Performance.SeatMapID != nil {
				t.Fatalf("GA slot hydrated SeatMapID = %v, want nil", pa.Performance.SeatMapID)
			}
		default:
			t.Fatalf("unexpected performance %v in the aggregate", pa.Performance.ID)
		}
	}
}

// TestPublishSeatMapCarriesOrphanPrevention closes an ai-review finding on TKT-179.
//
// The post-publish read is a POSITIONAL scan, so omitting the new column from its
// projection left the returned SeatMap with Go's `false` zero value while the stored
// row said true. That value is not decorative: it is what `SeatMapPublished` is handed
// and what the API returns, so the map would have published — and, once TKT-181 puts
// the flag on the wire, EMITTED — as rule-off while the database said otherwise. The
// required response field would have passed validation the whole time, lying.
//
// Re-publish is asserted too: it is idempotent and takes the same read path.
func TestPublishSeatMapCarriesOrphanPrevention(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	m, err := st.CreateSeatMap(ctx, SeatMapInput{
		OrganizerID: seatMapOrg, VenueID: seatMapVenue, Name: "Strict", OrphanPreventionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, RowID: row.ID, Label: "1", Position: 1}); err != nil {
		t.Fatal(err)
	}

	published, needsEmit, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !needsEmit {
		t.Fatal("a first publish owes an event")
	}
	if !published.OrphanPreventionEnabled {
		t.Fatal("publish returned a rule-OFF map for a rule-ON version — this value is handed to " +
			"SeatMapPublished and returned to the caller, so it would publish a lie")
	}

	republished, _, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !republished.OrphanPreventionEnabled {
		t.Fatal("re-publish is idempotent and takes the same read path; it must not drop the setting")
	}
}

// TKT-306 item 3, SPLIT OUT as TKT-318 and PINNED here.
//
// A seat map with NO SEATS publishes, and the resulting published version satisfies
// CreatePerformance's seated check — producing a slot that can sell nothing. Contrast
// ErrNotSellable, which gates performance publish on having a ticket type; the analogous
// "no sellable offer" gate is absent from the seat-map lifecycle.
//
// THIS TEST ASSERTS THE GAP, deliberately, following ADR-021's rollback-gap pattern: the
// alternative is a ticket that says "we noticed" in prose nobody greps. If it goes RED,
// the gap has been closed — update it to assert the refusal and close TKT-318; do not
// delete it.
//
// Why it was split rather than fixed here: it is the only item in TKT-306 that changes
// behaviour rather than aligning copies, and WHERE the gate belongs is a real design
// question with three candidate homes and different consequences each way — gating
// PublishSeatMap makes an existing seatless published map unrepairable, gating
// CreatePerformance leaves it publishable but unusable, gating EditSeatMap catches the
// edit and not the initial publish.
func TestASeatlessSeatMapStillPublishes_TKT318(t *testing.T) {
	ctx, _, st, _ := seatMapSmokeStore(t)

	// A draft map with a section and a row but NO seats. Not the degenerate
	// nothing-at-all case: a map that looks authored and sells nothing is the shape an
	// operator actually produces, by deleting the last seat or stopping halfway.
	m := seedDraftMap(ctx, t, st, "Seatless")
	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{
		OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSeatMapRow(ctx, SeatMapRowInput{
		OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1}); err != nil {
		t.Fatal(err)
	}

	published, _, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if err != nil {
		t.Fatalf("publishing a seatless map failed with %v.\n\n"+
			"If this is a deliberate new refusal, the gap TKT-318 tracks is CLOSED: change "+
			"this test to assert the refusal and close that ticket. Do not delete it.", err)
	}
	if published.Status != "published" {
		t.Fatalf("status = %q, want published — see the message above", published.Status)
	}

	// And the geometry really is empty, so this is not passing for some other reason.
	geo, err := st.GetSeatMapGeometry(ctx, published.ID)
	if err != nil {
		t.Fatal(err)
	}
	seats := 0
	for _, s := range geo.Sections {
		for _, r := range s.Rows {
			seats += len(r.Seats)
		}
	}
	if seats != 0 {
		t.Fatalf("fixture seeded %d seats; this test is about a map with none", seats)
	}
}
