//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TKT-251: the 13 path-id transition writes take only an id, so until this
// ticket a caller holding CATALOG_STAFF_WRITE_TOKEN could publish, archive,
// close, reopen or re-wire ANOTHER organizer's resources by id — and public
// catalog responses expose those ids.
//
// These assertions live at the STORE tier deliberately. The API-tier fake
// (`fakeStore` in internal/api/server_test.go) looks its rows up in a Go map, so
// an organizer check written there passes with the SQL predicate deleted — the
// exact shape AGENTS.md records from TKT-236. The mechanism is the SQL
// predicate, so this is the tier that can prove it.
//
// The refusal is ErrNotFound, never ErrOrganizerMismatch. An answer that
// distinguished "not yours" from "no such row" would confirm a guessed id is
// real; `writeStoreError` maps the mismatch to a 400 naming the reason and
// ErrNotFound to a 404. TestUpdateChannelIsScopedToTheOwningOrganizer
// (channels_smoke_test.go, TKT-236) is the precedent this copies.
//
// Every case asserts three things, because any one alone can pass while the
// boundary is broken: the attacker is refused, the victim's row did NOT move,
// and the OWNER can still perform the same operation. The third is what keeps a
// predicate from being a wall instead of a boundary — a `WHERE false` would
// satisfy the first two.

// tenantPair seeds two organizers, each owning a venue and an event. It returns
// the victim's ids first, then the attacker's.
type tenantPair struct {
	victimOrg, victimVenue, victimEvent       uuid.UUID
	attackerOrg, attackerVenue, attackerEvent uuid.UUID
}

func seedTenantPair(ctx context.Context, t *testing.T, db *sql.DB) tenantPair {
	t.Helper()
	p := tenantPair{
		victimOrg: uuid.New(), victimVenue: uuid.New(), victimEvent: uuid.New(),
		attackerOrg: uuid.New(), attackerVenue: uuid.New(), attackerEvent: uuid.New(),
	}
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizers(id,name) VALUES($1,'victim'),($2,'attacker')`,
			[]any{p.victimOrg, p.attackerOrg}},
		{`INSERT INTO venues(id,organizer_id,name,ga_capacity) VALUES($1,$2,'victim-venue',100),($3,$4,'attacker-venue',100)`,
			[]any{p.victimVenue, p.victimOrg, p.attackerVenue, p.attackerOrg}},
		{`INSERT INTO events(id,organizer_id,name) VALUES($1,$2,'{"en":"victim","fr":"victime"}'),($3,$4,'{"en":"attacker","fr":"attaquant"}')`,
			[]any{p.victimEvent, p.victimOrg, p.attackerEvent, p.attackerOrg}},
	} {
		if _, err := db.ExecContext(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// seedSlot inserts one performance owned by org, sellable so publish is legal.
//
// The two kinds carry different temporal shapes (constraint
// performances_kind_temporal): a 'performance' has starts_at, a 'festival_day'
// has the operating window instead. `at` is a date for a festival day and a
// timestamp for a performance.
func seedSlot(ctx context.Context, t *testing.T, db *sql.DB, org, event, venue uuid.UUID, kind string, at string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var err error
	if kind == KindFestivalDay {
		_, err = db.ExecContext(ctx,
			`INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,operating_date,opens_at,closes_at,timezone)
			 VALUES($1,$2,$3,$4,$5,$6::date,'12:00','23:00','America/Toronto')`,
			id, org, event, venue, kind, at)
	} else {
		_, err = db.ExecContext(ctx,
			`INSERT INTO performances(id,organizer_id,event_id,venue_id,kind,starts_at,timezone)
			 VALUES($1,$2,$3,$4,$5,$6::timestamptz,'America/Toronto')`,
			id, org, event, venue, kind, at)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
		 VALUES($1,$2,'{"en":"ga","fr":"ga"}',5000,'CAD')`, org, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func slotStatus(ctx context.Context, t *testing.T, db *sql.DB, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(ctx, `SELECT status FROM performances WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPerformanceTransitionsAreScopedToTheOwningOrganizer covers publish,
// archive, close and reopen — the four /performances/{id}/* transitions.
func TestPerformanceTransitionsAreScopedToTheOwningOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	p := seedTenantPair(ctx, t, db)

	t.Run("publish", func(t *testing.T) {
		slot := seedSlot(ctx, t, db, p.victimOrg, p.victimEvent, p.victimVenue, "performance", "2026-09-01 20:00:00-04")

		_, _, err := st.PublishPerformance(ctx, p.attackerOrg, slot)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant publish = %v, want ErrNotFound", err)
		}
		if got := slotStatus(ctx, t, db, slot); got != "draft" {
			t.Fatalf("the victim's slot moved to %q", got)
		}
		// A boundary, not a wall.
		if _, _, err := st.PublishPerformance(ctx, p.victimOrg, slot); err != nil {
			t.Fatalf("the owning organizer must still publish: %v", err)
		}
		if got := slotStatus(ctx, t, db, slot); got != "published" {
			t.Fatalf("owner publish did not apply: %q", got)
		}
	})

	t.Run("archive", func(t *testing.T) {
		slot := seedSlot(ctx, t, db, p.victimOrg, p.victimEvent, p.victimVenue, "performance", "2026-09-02 20:00:00-04")
		if _, _, err := st.PublishPerformance(ctx, p.victimOrg, slot); err != nil {
			t.Fatal(err)
		}

		_, _, _, err := st.ArchivePerformance(ctx, p.attackerOrg, slot)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant archive = %v, want ErrNotFound", err)
		}
		if got := slotStatus(ctx, t, db, slot); got != "published" {
			t.Fatalf("the victim's slot moved to %q", got)
		}
		if _, _, _, err := st.ArchivePerformance(ctx, p.victimOrg, slot); err != nil {
			t.Fatalf("the owning organizer must still archive: %v", err)
		}
	})

	t.Run("close and reopen", func(t *testing.T) {
		slot := seedSlot(ctx, t, db, p.victimOrg, p.victimEvent, p.victimVenue, "performance", "2026-09-03 20:00:00-04")
		if _, _, err := st.PublishPerformance(ctx, p.victimOrg, slot); err != nil {
			t.Fatal(err)
		}

		reason := "attacker closure"
		_, _, _, err := st.CloseSlot(ctx, p.attackerOrg, slot, &reason)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant close = %v, want ErrNotFound", err)
		}
		var closure string
		if err := db.QueryRowContext(ctx, `SELECT closure_status FROM performances WHERE id=$1`, slot).Scan(&closure); err != nil {
			t.Fatal(err)
		}
		if closure != "open" {
			t.Fatalf("the victim's slot was closed: %q", closure)
		}

		// The owner closes, and only then can the attacker's reopen be tested
		// against a genuinely closed row — otherwise a refusal proves nothing,
		// because reopening an open slot is a no-op anyway (the fixture-too-
		// small trap AGENTS.md names).
		ownerReason := "owner closure"
		closed, _, _, err := st.CloseSlot(ctx, p.victimOrg, slot, &ownerReason)
		if err != nil {
			t.Fatalf("the owning organizer must still close: %v", err)
		}
		// The opposite toggle is refused while the closure event is still owed
		// (ErrClosurePending), so ack it the way the handler does. Without this
		// the reopen below fails for a reason that has nothing to do with
		// tenancy, and the test would be asserting the wrong refusal.
		if err := st.MarkClosureEmitted(ctx, slot, closed.Closure.Version); err != nil {
			t.Fatal(err)
		}

		_, _, _, err = st.ReopenSlot(ctx, p.attackerOrg, slot)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant reopen = %v, want ErrNotFound", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT closure_status FROM performances WHERE id=$1`, slot).Scan(&closure); err != nil {
			t.Fatal(err)
		}
		if closure != "closed" {
			t.Fatalf("the victim's slot was reopened: %q", closure)
		}
		if _, _, _, err := st.ReopenSlot(ctx, p.victimOrg, slot); err != nil {
			t.Fatalf("the owning organizer must still reopen: %v", err)
		}
	})
}

// TestSeriesTransitionsAreScopedToTheOwningOrganizer covers publishSeries,
// archiveSeries and attachPerformanceToSeries.
//
// The attach case is the sharpest in the ticket: AttachPerformanceToSeries
// compares organizer_id from BOTH rows to EACH OTHER (targetOrg != orgID), never
// to a caller, so two resources of the same victim tenant satisfy it. The
// attacker here holds both victim ids — the strongest forged position — which is
// precisely the input the old comparison accepts.
func TestSeriesTransitionsAreScopedToTheOwningOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	p := seedTenantPair(ctx, t, db)

	newSeries := func(t *testing.T) (uuid.UUID, uuid.UUID) {
		t.Helper()
		s, err := st.CreateSeries(ctx, SeriesInput{
			OrganizerID: p.victimOrg, EventID: p.victimEvent,
			Name: LocalizedText{"en": "series", "fr": "serie"},
		})
		if err != nil {
			t.Fatal(err)
		}
		slot := seedSlot(ctx, t, db, p.victimOrg, p.victimEvent, p.victimVenue, "performance", "2026-10-01 20:00:00-04")
		return s.ID, slot
	}

	t.Run("attach — attacker holds BOTH victim ids", func(t *testing.T) {
		seriesID, slot := newSeries(t)

		_, err := st.AttachPerformanceToSeries(ctx, p.attackerOrg, seriesID, slot, 1)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant attach = %v, want ErrNotFound — the two-row organizer "+
				"comparison is not authorization; both lookups need a caller predicate", err)
		}
		var members int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM series_performances WHERE series_id=$1`, seriesID).Scan(&members); err != nil {
			t.Fatal(err)
		}
		if members != 0 {
			t.Fatalf("the victim's series gained %d member(s)", members)
		}
		if _, err := st.AttachPerformanceToSeries(ctx, p.victimOrg, seriesID, slot, 1); err != nil {
			t.Fatalf("the owning organizer must still attach: %v", err)
		}
	})

	t.Run("publish and archive", func(t *testing.T) {
		seriesID, slot := newSeries(t)
		if _, err := st.AttachPerformanceToSeries(ctx, p.victimOrg, seriesID, slot, 1); err != nil {
			t.Fatal(err)
		}

		_, err := st.PublishSeries(ctx, p.attackerOrg, seriesID)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant series publish = %v, want ErrNotFound", err)
		}
		if got := slotStatus(ctx, t, db, slot); got != "draft" {
			t.Fatalf("the victim's member moved to %q", got)
		}
		if _, err := st.PublishSeries(ctx, p.victimOrg, seriesID); err != nil {
			t.Fatalf("the owning organizer must still publish: %v", err)
		}

		if _, err := st.ArchiveSeries(ctx, p.attackerOrg, seriesID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant series archive = %v, want ErrNotFound", err)
		}
		if got := slotStatus(ctx, t, db, slot); got != "published" {
			t.Fatalf("the victim's member moved to %q", got)
		}
		if _, err := st.ArchiveSeries(ctx, p.victimOrg, seriesID); err != nil {
			t.Fatalf("the owning organizer must still archive: %v", err)
		}
	})
}

// TestFestivalTransitionsAreScopedToTheOwningOrganizer covers publishFestival,
// archiveFestival and attachDayToFestival. AttachDayToFestival carries the same
// two-row comparison as the series attach (festivalOrg != performanceOrg).
func TestFestivalTransitionsAreScopedToTheOwningOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	p := seedTenantPair(ctx, t, db)

	newFestival := func(t *testing.T, at string) (uuid.UUID, uuid.UUID) {
		t.Helper()
		f, err := st.CreateFestival(ctx, FestivalInput{
			OrganizerID:    p.victimOrg,
			Name:           LocalizedText{"en": "festival", "fr": "festival"},
			SharedCapacity: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		day := seedSlot(ctx, t, db, p.victimOrg, p.victimEvent, p.victimVenue, KindFestivalDay, at)
		return f.ID, day
	}

	t.Run("attach day — attacker holds BOTH victim ids", func(t *testing.T) {
		festivalID, day := newFestival(t, "2026-11-01")

		_, err := st.AttachDayToFestival(ctx, p.attackerOrg, festivalID, day)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant day attach = %v, want ErrNotFound", err)
		}
		var group uuid.NullUUID
		if err := db.QueryRowContext(ctx, `SELECT capacity_group_id FROM performances WHERE id=$1`, day).Scan(&group); err != nil {
			t.Fatal(err)
		}
		if group.Valid {
			t.Fatalf("the victim's day was grouped into %v", group.UUID)
		}
		if _, err := st.AttachDayToFestival(ctx, p.victimOrg, festivalID, day); err != nil {
			t.Fatalf("the owning organizer must still attach: %v", err)
		}
	})

	t.Run("publish and archive", func(t *testing.T) {
		festivalID, day := newFestival(t, "2026-11-02")
		if _, err := st.AttachDayToFestival(ctx, p.victimOrg, festivalID, day); err != nil {
			t.Fatal(err)
		}

		if _, err := st.PublishFestival(ctx, p.attackerOrg, festivalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant festival publish = %v, want ErrNotFound", err)
		}
		if got := slotStatus(ctx, t, db, day); got != "draft" {
			t.Fatalf("the victim's day moved to %q", got)
		}
		if _, err := st.PublishFestival(ctx, p.victimOrg, festivalID); err != nil {
			t.Fatalf("the owning organizer must still publish: %v", err)
		}

		if _, err := st.ArchiveFestival(ctx, p.attackerOrg, festivalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant festival archive = %v, want ErrNotFound", err)
		}
		if got := slotStatus(ctx, t, db, day); got != "published" {
			t.Fatalf("the victim's day moved to %q", got)
		}
		if _, err := st.ArchiveFestival(ctx, p.victimOrg, festivalID); err != nil {
			t.Fatalf("the owning organizer must still archive: %v", err)
		}
	})
}

// TestSeasonAttachIsScopedToTheOwningOrganizer covers attachSeriesToSeason and
// attachEventToSeason, both backed by attachSeasonMember.
func TestSeasonAttachIsScopedToTheOwningOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	p := seedTenantPair(ctx, t, db)

	season, err := st.CreateSeason(ctx, SeasonInput{
		OrganizerID: p.victimOrg,
		Name:        LocalizedText{"en": "season", "fr": "saison"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Membership lives in two tables, one per member kind; count both so an
	// attach that landed in either is visible.
	countMembers := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT (SELECT count(*) FROM season_events WHERE season_id=$1)
			      + (SELECT count(*) FROM season_series WHERE season_id=$1)`, season.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("attach event", func(t *testing.T) {
		before := countMembers(t)
		if _, err := st.AttachEventToSeason(ctx, p.attackerOrg, season.ID, p.victimEvent); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant event attach = %v, want ErrNotFound", err)
		}
		if got := countMembers(t); got != before {
			t.Fatalf("the victim's season gained a member: %d -> %d", before, got)
		}
		if _, err := st.AttachEventToSeason(ctx, p.victimOrg, season.ID, p.victimEvent); err != nil {
			t.Fatalf("the owning organizer must still attach: %v", err)
		}
	})

	t.Run("attach series", func(t *testing.T) {
		s, err := st.CreateSeries(ctx, SeriesInput{
			OrganizerID: p.victimOrg, EventID: p.victimEvent,
			Name: LocalizedText{"en": "series", "fr": "serie"},
		})
		if err != nil {
			t.Fatal(err)
		}
		before := countMembers(t)
		if _, err := st.AttachSeriesToSeason(ctx, p.attackerOrg, season.ID, s.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant series attach = %v, want ErrNotFound", err)
		}
		if got := countMembers(t); got != before {
			t.Fatalf("the victim's season gained a member: %d -> %d", before, got)
		}
		if _, err := st.AttachSeriesToSeason(ctx, p.victimOrg, season.ID, s.ID); err != nil {
			t.Fatalf("the owning organizer must still attach: %v", err)
		}
	})
}

// TestPublishSeatMapIsScopedToTheOwningOrganizer covers the 13th operation.
//
// Note this one is NOT the ADR-029 family-lock shape: publish flips one specific
// draft row by id and resolves no "current version", so it takes the predicate
// on its conditional UPDATE and on the canonical re-read. The family advisory
// lock governs EditSeatMap/PinSeat, which resolve the family's current published
// version and were already organizer-scoped.
func TestPublishSeatMapIsScopedToTheOwningOrganizer(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	p := seedTenantPair(ctx, t, db)

	m, err := st.CreateSeatMap(ctx, SeatMapInput{
		OrganizerID: p.victimOrg, VenueID: p.victimVenue, Name: "victim map",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.PublishSeatMap(ctx, p.attackerOrg, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant seat-map publish = %v, want ErrNotFound", err)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM seat_maps WHERE id=$1`, m.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Fatalf("the victim's map was published: %q", status)
	}

	published, _, err := st.PublishSeatMap(ctx, p.victimOrg, m.ID)
	if err != nil {
		t.Fatalf("the owning organizer must still publish: %v", err)
	}
	if published.Status != "published" {
		t.Fatalf("owner publish did not apply: %q", published.Status)
	}
}
