//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedDraftWithSeat authors a draft map with one section, row and seat (Orchestra/A/1)
// and leaves it unpublished. It is the success fixture for publish. seedDraftMap
// stays seatless, for the negative tests.
func seedDraftWithSeat(ctx context.Context, t *testing.T, st *Postgres, name string) SeatMap {
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
	return m
}

// seedSeatlessDraft authors a draft with one section (Orchestra) and one row (A)
// but no seats. An operator can leave a draft in this shape by stopping halfway
// through authoring. It is a negative fixture: do not add seats to it.
func seedSeatlessDraft(ctx context.Context, t *testing.T, st *Postgres, name string) SeatMap {
	t.Helper()
	m := seedDraftMap(ctx, t, st, name)
	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: m.ID, SectionID: sec.ID, Label: "A", Position: 1}); err != nil {
		t.Fatal(err)
	}
	return m
}

// markLegacyPublished puts a version into the published state by direct SQL. It
// reproduces a map that was published before TKT-318 refused seatless publish.
// Normal authoring cannot reach this state now: PublishSeatMap refuses a draft
// with no seats, and EditSeatMap refuses an edit with no seats.
func markLegacyPublished(ctx context.Context, t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`UPDATE seat_maps SET status = 'published', published_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// familyVersionCount counts every version in the map family that id belongs to.
func familyVersionCount(ctx context.Context, t *testing.T, db *sql.DB, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_maps
		 WHERE map_family_id = (SELECT map_family_id FROM seat_maps WHERE id = $1)`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seatCount counts the seat rows of one version, read straight from the table.
func seatCount(ctx context.Context, t *testing.T, db *sql.DB, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM seat_map_seats WHERE seat_map_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedPublishedMap authors a minimal draft map (one section/row/seat so it is a
// real map) and publishes it, returning the published version's id.
func seedPublishedMap(ctx context.Context, t *testing.T, st *Postgres, name string) SeatMap {
	t.Helper()
	m := seedDraftWithSeat(ctx, t, st, name)
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
	m := seedDraftWithSeat(ctx, t, st, "Main floor")

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

// TKT-306 item 3, SPLIT OUT as TKT-318 and PINNED here. TKT-318 closed the gap, so
// this test now asserts the refusal. Do not delete it: it is the regression.
//
// A seat map with NO SEATS used to publish, and the published version then passed
// CreatePerformance's seated check. That produced a slot that could sell nothing.
// TKT-318 (ADR-029 amendment) refuses the publish of a draft with no seats. A
// published seatless version that already exists is left alone (see the legacy
// tests below).
func TestASeatlessSeatMapCannotPublish_TKT318(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)

	// A draft map with a section and a row but NO seats. Not the degenerate
	// nothing-at-all case: a map that looks authored and sells nothing is the shape an
	// operator can leave behind by stopping halfway through authoring.
	m := seedSeatlessDraft(ctx, t, st, "Seatless")

	// A populated map in the same venue. A check that asks "does ANY seat exist?"
	// would see this seat and let the seatless draft through.
	seedPublishedMap(ctx, t, st, "Populated neighbour")

	published, needsEmit, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if !errors.Is(err, ErrSeatMapSeatless) {
		t.Fatalf("publishing a seatless draft err = %v, want ErrSeatMapSeatless", err)
	}
	if needsEmit || published.ID != uuid.Nil {
		t.Fatalf("refused publish returned map %+v needsEmit=%v, want zero value and no event owed", published, needsEmit)
	}

	// The refused write left no trace: still a draft, no publication, no event.
	var status string
	var publishedAtNull, emittedAtNull bool
	if err := db.QueryRowContext(ctx,
		`SELECT status, published_at IS NULL, event_emitted_at IS NULL FROM seat_maps WHERE id = $1`, m.ID).
		Scan(&status, &publishedAtNull, &emittedAtNull); err != nil {
		t.Fatal(err)
	}
	if status != "draft" || !publishedAtNull || !emittedAtNull {
		t.Fatalf("refused publish changed the row: status=%q publishedAtNull=%v emittedAtNull=%v, want draft, null, null",
			status, publishedAtNull, emittedAtNull)
	}
	if n := seatCount(ctx, t, db, m.ID); n != 0 {
		t.Fatalf("fixture seeded %d seats; this test is about a map with none", n)
	}
}

// An archived seatless map gets the lifecycle refusal, not the seatless one. The
// seatless check applies to drafts only, so it must not shadow the lifecycle check.
func TestArchivedSeatlessMapGetsLifecycleRefusal(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	m := seedSeatlessDraft(ctx, t, st, "Archived")
	if _, err := db.ExecContext(ctx, `UPDATE seat_maps SET status = 'archived' WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("publishing an archived seatless map err = %v, want ErrIllegalTransition", err)
	}
}

// A legacy seatless version (published before TKT-318) still answers a publish retry.
// The event is owed until it is marked, and not owed after. The refusal covers drafts only.
func TestLegacySeatlessPublishedMapStillRepublishes(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	m := seedSeatlessDraft(ctx, t, st, "Legacy")
	markLegacyPublished(ctx, t, db, m.ID)

	// Readable: the geometry read answers for the legacy version, with no seats.
	geo, err := st.GetSeatMapGeometry(ctx, m.ID)
	if err != nil {
		t.Fatalf("reading a legacy seatless version: %v", err)
	}
	if geo.Map.Status != "published" || len(geo.Sections) != 1 {
		t.Fatalf("legacy read = %q with %d section(s), want published with 1", geo.Map.Status, len(geo.Sections))
	}

	published, needsEmit, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if err != nil {
		t.Fatalf("republishing a legacy seatless version err = %v, want nil", err)
	}
	if published.Status != "published" || !needsEmit {
		t.Fatalf("legacy republish = %q needsEmit=%v, want published + owed", published.Status, needsEmit)
	}
	if err := st.MarkSeatMapEventEmitted(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	_, needsEmitAfterMark, err := st.PublishSeatMap(ctx, seatMapOrg, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if needsEmitAfterMark {
		t.Fatal("after the event is marked, a legacy republish must not owe it again")
	}
}

// seatedPerformance is a minimal performance input for one seat map reference.
func seatedPerformance(eventID, venueID uuid.UUID, startsAt time.Time, mapID *uuid.UUID) PerformanceInput {
	return PerformanceInput{
		OrganizerID: seatMapOrg, EventID: eventID, VenueID: venueID,
		StartsAt: &startsAt, Timezone: "Europe/Paris", SeatMapID: mapID,
	}
}

// seedEvent creates one event for the seat-map organizer.
func seedEvent(ctx context.Context, t *testing.T, st *Postgres) uuid.UUID {
	t.Helper()
	event, err := st.CreateEvent(ctx, EventInput{
		OrganizerID: seatMapOrg,
		Name:        LocalizedText{"en": "Recital", "fr": "Récital"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return event.ID
}

// TestCreatePerformanceRefusesSeatlessPublishedVersion (TKT-318 COS-4) pins the seated
// create check against the exact version id a slot is bound to.
//
// The fixture holds a populated neighbour in the same venue. A check that asks whether
// ANY seat exists would accept the seatless legacy version through the neighbour's
// seats. The legacy id is tested again after a repair, because a repair creates a new
// version in the same family and a family-wide check would accept the old id.
func TestCreatePerformanceRefusesSeatlessPublishedVersion(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	event := seedEvent(ctx, t, st)
	seedPublishedMap(ctx, t, st, "Neighbour with seats")

	legacy := seedSeatlessDraft(ctx, t, st, "Legacy seatless")
	markLegacyPublished(ctx, t, db, legacy.ID)
	legacyID := legacy.ID

	countPerformances := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM performances WHERE event_id = $1`, event).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	at := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePerformance(ctx, seatedPerformance(event, seatMapVenue, at, &legacyID)); !errors.Is(err, ErrSeatMapSeatless) {
		t.Fatalf("seated slot on a seatless version err = %v, want ErrSeatMapSeatless", err)
	}
	if n := countPerformances(); n != 0 {
		t.Fatalf("refused create left %d performance(s), want none", n)
	}

	// A GA slot never reads the seat map, so the refusal does not touch it.
	ga := seatedPerformance(event, seatMapVenue, at.Add(24*time.Hour), nil)
	if _, err := st.CreatePerformance(ctx, ga); err != nil {
		t.Fatalf("GA slot err = %v, want success", err)
	}

	// Repair: an edit adds seats and mints a new version in the same family.
	repaired, _, err := st.EditSeatMap(ctx, EditSeatMapInput{OrganizerID: seatMapOrg, SeatMapID: legacy.ID,
		Sections: []EditSectionInput{sect("Orchestra", 1, rw("A", 1, st1("1", 1)))}})
	if err != nil {
		t.Fatalf("repair edit: %v", err)
	}
	if _, err := st.CreatePerformance(ctx, seatedPerformance(event, seatMapVenue, at.Add(48*time.Hour), &repaired.ID)); err != nil {
		t.Fatalf("seated slot on the repaired version err = %v, want success", err)
	}
	if _, err := st.CreatePerformance(ctx, seatedPerformance(event, seatMapVenue, at.Add(72*time.Hour), &legacyID)); !errors.Is(err, ErrSeatMapSeatless) {
		t.Fatalf("seated slot on the legacy id after repair err = %v, want ErrSeatMapSeatless", err)
	}
}

// TestCreatePerformanceReplaysKeyedRetryOnSeatlessVersion (TKT-318 review item 1) keeps
// the Idempotency-Key contract for a version that lost its seats after a keyed create.
// The create succeeded while the version still had seats. The seats are then removed by
// direct SQL, which leaves the state of a version published seatless before TKT-318. A
// retry with the same key and terms returns the original row. The same key with other
// terms conflicts. A new key, or no key, is refused and inserts nothing.
func TestCreatePerformanceReplaysKeyedRetryOnSeatlessVersion(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	event := seedEvent(ctx, t, st)
	m := seedPublishedMap(ctx, t, st, "Lost its seats")
	mapID := m.ID
	at := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	keyed := seatedPerformance(event, seatMapVenue, at, &mapID)
	keyed.IdempotencyKey = "tkt318-keyed-retry"
	original, err := st.CreatePerformance(ctx, keyed)
	if err != nil {
		t.Fatalf("keyed create while the version has seats: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM seat_map_seats WHERE seat_map_id = $1`, mapID); err != nil {
		t.Fatal(err)
	}
	if n := seatCount(ctx, t, db, mapID); n != 0 {
		t.Fatalf("fixture left %d seats on the version, want none", n)
	}

	const wantRows = 1
	countPerformances := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM performances WHERE event_id = $1`, event).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("same key and terms return the original performance", func(t *testing.T) {
		replayed, err := st.CreatePerformance(ctx, keyed)
		if err != nil {
			t.Fatalf("keyed retry err = %v, want the original performance", err)
		}
		if replayed.ID != original.ID {
			t.Fatalf("keyed retry returned %s, want the original %s", replayed.ID, original.ID)
		}
		if n := countPerformances(); n != wantRows {
			t.Fatalf("keyed retry left %d performance(s), want %d", n, wantRows)
		}
	})
	t.Run("same key with other terms is a conflict", func(t *testing.T) {
		other := keyed
		later := at.Add(time.Hour)
		other.StartsAt = &later
		if _, err := st.CreatePerformance(ctx, other); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("same key, other terms err = %v, want ErrIdempotencyConflict", err)
		}
		if n := countPerformances(); n != wantRows {
			t.Fatalf("conflicting retry left %d performance(s), want %d", n, wantRows)
		}
	})
	t.Run("a new key is refused and inserts nothing", func(t *testing.T) {
		fresh := keyed
		fresh.IdempotencyKey = "tkt318-new-key"
		if _, err := st.CreatePerformance(ctx, fresh); !errors.Is(err, ErrSeatMapSeatless) {
			t.Fatalf("new key on a seatless version err = %v, want ErrSeatMapSeatless", err)
		}
		if n := countPerformances(); n != wantRows {
			t.Fatalf("refused new key left %d performance(s), want %d", n, wantRows)
		}
	})
	t.Run("no key is refused and inserts nothing", func(t *testing.T) {
		plain := keyed
		plain.IdempotencyKey = ""
		if _, err := st.CreatePerformance(ctx, plain); !errors.Is(err, ErrSeatMapSeatless) {
			t.Fatalf("keyless create on a seatless version err = %v, want ErrSeatMapSeatless", err)
		}
		if n := countPerformances(); n != wantRows {
			t.Fatalf("refused keyless create left %d performance(s), want %d", n, wantRows)
		}
	})
}

// TestCreatePerformanceSeatMapRefusals keeps the existing seat-map create checks pinned
// one at a time. Each case violates exactly one rule, so a removed check shows up as
// the wrong answer for its own case.
func TestCreatePerformanceSeatMapRefusals(t *testing.T) {
	ctx, db, st, _ := seatMapSmokeStore(t)
	event := seedEvent(ctx, t, st)
	at := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	draft := seedDraftWithSeat(ctx, t, st, "Still a draft")
	otherVenue := uuid.New()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO venues (id, organizer_id, name, ga_capacity) VALUES ($1, $2, 'second hall', 100)`,
		otherVenue, seatMapOrg); err != nil {
		t.Fatal(err)
	}
	// A published, seated map in the second hall. The slot below is booked in the first.
	elsewhere, err := st.CreateSeatMap(ctx, SeatMapInput{OrganizerID: seatMapOrg, VenueID: otherVenue, Name: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.AddSeatMapSection(ctx, SeatMapSectionInput{OrganizerID: seatMapOrg, SeatMapID: elsewhere.ID, Name: "Orchestra", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.AddSeatMapRow(ctx, SeatMapRowInput{OrganizerID: seatMapOrg, SeatMapID: elsewhere.ID, SectionID: sec.ID, Label: "A", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSeatMapSeat(ctx, SeatMapSeatInput{OrganizerID: seatMapOrg, SeatMapID: elsewhere.ID, RowID: row.ID, Label: "1", Position: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PublishSeatMap(ctx, seatMapOrg, elsewhere.ID); err != nil {
		t.Fatal(err)
	}
	unknown := uuid.New()

	for _, tc := range []struct {
		name  string
		mapID uuid.UUID
		want  error
	}{
		{"unknown map is not found", unknown, ErrNotFound},
		{"draft map is not published", draft.ID, ErrSeatMapNotPublished},
		{"published map in another venue is a mismatch", elsewhere.ID, ErrOrganizerMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.mapID
			if _, err := st.CreatePerformance(ctx, seatedPerformance(event, seatMapVenue, at, &id)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
