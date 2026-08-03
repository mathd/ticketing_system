//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// seedSeatedSlot inserts a published seated performance bound to a NEW seat-map version
// with the given rule setting, and returns (performance id, bound map id, family id).
// A second version of the same family can be added with seedMapVersion.
func seedSeatedSlot(t *testing.T, ctx context.Context, db *sql.DB, rule bool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	orgID, venueID, eventID, perfID, mapID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `WITH o AS (
		INSERT INTO organizers(id,name) VALUES($1,'orphan-wave') RETURNING id
	), v AS (
		INSERT INTO venues(id,organizer_id,name,ga_capacity) SELECT $2,id,'v',250 FROM o RETURNING id
	), e AS (
		INSERT INTO events(id,organizer_id,name) SELECT $3,id,'{"en":"e","fr":"e"}' FROM o RETURNING id
	), m AS (
		INSERT INTO seat_maps(id,organizer_id,venue_id,name,version,status,orphan_prevention_enabled)
		SELECT $5,o.id,v.id,'stalls',1,'published',$6 FROM o,v RETURNING id
	)
	INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,starts_at,timezone,
		re_entry_mode,requires_exit,status,published_at,seat_map_id)
	SELECT $4,o.id,e.id,v.id,'performance',TIMESTAMPTZ '2026-09-01T20:00:00Z','America/Toronto',
		'single',false,'published',now(),m.id
	FROM o,e,v,m`, orgID, venueID, eventID, perfID, mapID, rule); err != nil {
		t.Fatal(err)
	}
	return perfID, mapID, venueID
}

// TestListOrphanPreventionCandidates pins the wave's read surface: published slots bound
// to a rule-ENABLED seat-map version, and nothing else. A wave that over-selects emits
// schema 5 for a map whose organizer never turned the rule on.
func TestListOrphanPreventionCandidates(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)

	enabled, _, _ := seedSeatedSlot(t, ctx, db, true)
	ruleOff, _, _ := seedSeatedSlot(t, ctx, db, false)
	_ = seedPublishedSlot(t, ctx, db, "published", "multi", nil) // GA: no seat map at all

	// A draft slot bound to an enabled map: the rule is on, but there is no publication
	// to correct, so emitting one would invent inventory for an unpublished slot.
	draft, _, _ := seedSeatedSlot(t, ctx, db, true)
	if _, err := db.ExecContext(ctx,
		`UPDATE performances SET status='draft', published_at=NULL WHERE id=$1`, draft); err != nil {
		t.Fatal(err)
	}

	got := map[uuid.UUID]Performance{}
	var cursor *uuid.UUID
	for {
		batch, err := st.ListOrphanPreventionCandidates(ctx, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		for _, p := range batch {
			got[p.ID] = p
		}
		last := batch[len(batch)-1].ID
		cursor = &last
	}

	if _, ok := got[enabled]; !ok {
		t.Fatal("the enabled seated slot is the whole candidate set and it was not returned")
	}
	if !got[enabled].OrphanPreventionEnabled || got[enabled].SeatMapID == nil {
		t.Fatalf("candidate hydrated as %+v — the flag and the bound map are what the emission needs", got[enabled])
	}
	for _, id := range []uuid.UUID{ruleOff, draft} {
		if _, bad := got[id]; bad {
			t.Fatalf("%s must not be a candidate", id)
		}
	}
}

// TestHydratedFlagFollowsTheBoundVersionNotTheFamily is AC4 at the layer that decides it.
// A published seat-map version is immutable and a slot is bound to exactly one (ADR-029),
// so publishing a NEWER version of the same family with a different setting must not
// change what this slot emits. Joining the family's current version instead would look
// identical until the first edit, and then every emission would silently describe a
// version the slot is not bound to.
func TestHydratedFlagFollowsTheBoundVersionNotTheFamily(t *testing.T) {
	ctx, db, st := festivalSmokeStore(t)
	perfID, boundMapID, venueID := seedSeatedSlot(t, ctx, db, true)

	// v2 of the same family, in the same venue, rule OFF and published later.
	var orgID uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT organizer_id FROM seat_maps WHERE id=$1`, boundMapID).
		Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO seat_maps(id,organizer_id,venue_id,name,version,status,orphan_prevention_enabled)
		 VALUES($1,$2,$3,'stalls',2,'published',false)`,
		uuid.New(), orgID, venueID); err != nil {
		t.Fatal(err)
	}

	batch, err := st.ListOrphanPreventionCandidates(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range batch {
		if p.ID != perfID {
			continue
		}
		found = true
		if !p.OrphanPreventionEnabled || p.SeatMapID == nil || *p.SeatMapID != boundMapID {
			t.Fatalf("slot hydrated as %+v — it is bound to v1 (%s) and a later v2 must not change that",
				p, boundMapID)
		}
	}
	if !found {
		t.Fatal("the slot bound to the enabled v1 stopped being a candidate when a rule-off v2 was published")
	}
}
