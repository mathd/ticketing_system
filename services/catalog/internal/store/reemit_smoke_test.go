//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// seedPublishedSlot inserts an organizer/venue/event and one ungrouped
// performance in the given status with the given re_entry policy, plus a
// ticket_type so it is a real (sellable) publication. Returns the performance id.
func seedPublishedSlot(t *testing.T, ctx context.Context, db *sql.DB, status, mode string, maxEntries *int32) uuid.UUID {
	t.Helper()
	orgID, venueID, eventID, perfID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `WITH o AS (
		INSERT INTO organizers(id,name) VALUES($1,'reemit') RETURNING id
	), v AS (
		INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',250 FROM o RETURNING id
	), e AS (
		INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
	)
	INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,starts_at,timezone,
		re_entry_mode,max_entries,requires_exit,status,published_at)
	SELECT $4,o.id,e.id,v.id,'performance',TIMESTAMPTZ '2026-09-01T20:00:00Z','America/Toronto',
		$5,$6,false,$7,CASE WHEN $7='published' THEN now() ELSE NULL END
	FROM o,e,v`,
		orgID, venueID, eventID, perfID, mode, maxEntries, status); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
		VALUES($1,$2,'{"en":"ga","fr":"ga"}',7500,'CAD')`, orgID, perfID); err != nil {
		t.Fatal(err)
	}
	return perfID
}

// seedPublishedGroupedDay inserts a published festival_day member whose
// capacity_group_id references a real festival — the only kind allowed a
// capacity_group_id (constraint performances_capacity_group_kind + the festivals
// FK). The backfill must EXCLUDE it (ADR-018 rule 2). Returns the member id.
func seedPublishedGroupedDay(t *testing.T, ctx context.Context, db *sql.DB) uuid.UUID {
	t.Helper()
	orgID, venueID, eventID, festivalID, dayID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `WITH o AS (
		INSERT INTO organizers(id,name) VALUES($1,'reemit-grouped') RETURNING id
	), v AS (
		INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',250 FROM o RETURNING id
	), e AS (
		INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
	), f AS (
		INSERT INTO festivals(id,organizer_id,name,shared_capacity)
		SELECT $4,id,'{"en":"f","fr":"f"}',1000 FROM o RETURNING id
	)
	INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone,
		re_entry_mode,capacity_group_id,status,published_at)
	SELECT $5,o.id,e.id,v.id,'festival_day',DATE '2026-08-01','12:00','23:00','America/Toronto',
		'multi',f.id,'published',now()
	FROM o,e,v,f`,
		orgID, venueID, eventID, festivalID, dayID); err != nil {
		t.Fatal(err)
	}
	return dayID
}

// TestListPublishedUngroupedPerformances pins the backfill's read surface
// (TKT-96): it returns fully-hydrated Performance rows for published, ungrouped
// slots — with re_entry AND the publication-time Capacity snapshot reconstructed
// (via the same v.ga_capacity join getPerformanceFrom uses) — and excludes draft
// slots and grouped festival-day members (ADR-018 rule 2: grouped members are
// out of scope; re-emitting them would assert festival-shared capacity from a
// per-member backfill). Keyset pagination drains across pages.
func TestListPublishedUngroupedPerformances(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)

	maxEntries := int32(3)
	multiID := seedPublishedSlot(t, ctx, db, "published", "multi", nil)
	countID := seedPublishedSlot(t, ctx, db, "published", "count_limited", &maxEntries)
	singleID := seedPublishedSlot(t, ctx, db, "published", "single", nil)
	_ = seedPublishedSlot(t, ctx, db, "draft", "multi", nil) // excluded: not published
	_ = seedPublishedGroupedDay(t, ctx, db)                  // excluded: grouped festival-day member

	got := map[uuid.UUID]Performance{}
	var cursor *uuid.UUID
	for {
		batch, err := st.ListPublishedUngroupedPerformances(ctx, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		for _, p := range batch {
			if _, dup := got[p.ID]; dup {
				t.Fatalf("keyset pagination returned %s twice", p.ID)
			}
			got[p.ID] = p
		}
		last := batch[len(batch)-1].ID
		cursor = &last
	}

	if len(got) != 3 {
		t.Fatalf("returned %d slots, want 3 published ungrouped (multi, count_limited, single)", len(got))
	}
	for _, id := range []uuid.UUID{multiID, countID, singleID} {
		if _, ok := got[id]; !ok {
			t.Fatalf("published ungrouped slot %s missing from results", id)
		}
	}

	// re_entry and Capacity are reconstructed identically to the single-row read
	// path, so the re-emission carries the same payload the live publish would.
	if m := got[multiID]; m.ReEntry.Mode != "multi" || m.Capacity != 250 {
		t.Fatalf("multi slot hydrated as mode=%s capacity=%d, want multi/250", m.ReEntry.Mode, m.Capacity)
	}
	if c := got[countID]; c.ReEntry.Mode != "count_limited" || c.ReEntry.MaxEntries == nil || *c.ReEntry.MaxEntries != 3 {
		t.Fatalf("count_limited slot hydrated as %+v, want count_limited max=3", c.ReEntry)
	}
	// The list-path hydration must match the single-row read path exactly.
	single, err := st.GetPublishedPerformance(ctx, singleID)
	if err != nil {
		t.Fatal(err)
	}
	if got[singleID].Capacity != single.Capacity {
		t.Fatalf("list Capacity=%d != single-read Capacity=%d — payload would differ from live publish",
			got[singleID].Capacity, single.Capacity)
	}
}
