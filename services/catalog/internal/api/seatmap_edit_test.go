package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// editBody builds a full-replacement geometry for the one-seat map that
// seedPublishedMap authors (Orchestra/A/1), optionally dropping that seat.
func editBody(dropPinnedSeat bool) SeatMapEdit {
	seats := []SeatMapEditSeat{{Label: "1", Position: 1}}
	if dropPinnedSeat {
		seats = []SeatMapEditSeat{{Label: "2", Position: 1}} // renames the seat -> orphans Orchestra/A/1
	}
	return SeatMapEdit{
		Sections: []SeatMapEditSection{{
			Name: "Orchestra", Position: 1,
			Rows: []SeatMapEditRow{{
				Label: "A", Position: 1, Seats: seats,
			}},
		}},
	}
}

// TestEditSeatMapOrphanedPinReturns409 is the first COS-1/COS-4 guard: an edit
// whose new geometry drops a seat identity pinned by a sale/hold is hard-rejected
// with 409 and the actionable {error} body — never a 500 (the store sentinel
// must be mapped). The pinning contract itself is TKT-104's; this only surfaces it.
func TestEditSeatMapOrphanedPinReturns409(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedPublishedMap(t, e, venueID, "Main floor")

	// A sale pins Orchestra/A/1 (TKT-80 does this in prod; the fake seeds it).
	e.store.pinSeat(m.Id, "Orchestra/A/1")

	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", editBody(true))
	if rec.Code != http.StatusConflict {
		t.Fatalf("orphaning edit must be 409, got %d %s", rec.Code, rec.Body.String())
	}
	// The pin message, exactly. The seatless message must not stand in for it: the
	// two refusals are different facts, and the operator fixes each differently.
	if body := decode[Error](t, rec); body.Error != orphanPinMessage {
		t.Fatalf("409 error = %q, want the pin message %q", body.Error, orphanPinMessage)
	}
}

// orphanPinMessage is the 409 body for an edit that drops a pinned seat identity (TKT-105).
const orphanPinMessage = "edit would orphan a seat identity pinned by a sale or hold; the new geometry must keep every pinned seat (same section/row/seat labels)"

// TestEditSeatMapCreatesNewPublishedVersion is COS-1/COS-2: a valid edit mints a
// new published version (version+1) that keeps the pinned seat, leaves the
// predecessor untouched, and is a write (no-store).
func TestEditSeatMapCreatesNewPublishedVersion(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedPublishedMap(t, e, venueID, "Main floor")
	e.store.pinSeat(m.Id, "Orchestra/A/1")

	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", editBody(false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid edit must be 201, got %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("edit is a write, must be no-store, got %q", cc)
	}
	nv := decode[SeatMap](t, rec)
	if nv.Version != m.Version+1 {
		t.Fatalf("edit must bump version to %d, got %d", m.Version+1, nv.Version)
	}
	if nv.Status != "published" || nv.PublishedAt == nil {
		t.Fatalf("new version must be published with a timestamp, got %q %v", nv.Status, nv.PublishedAt)
	}
	if nv.Id == m.Id {
		t.Fatalf("edit must create a NEW row, not mutate the predecessor (same id %s)", nv.Id)
	}
	// The new version emits its own seat_map.published.
	if len(e.pub.seatMapsPub) == 0 || e.pub.seatMapsPub[len(e.pub.seatMapsPub)-1].Version != nv.Version {
		t.Fatalf("new version must emit seat_map.published for version %d, got %+v", nv.Version, e.pub.seatMapsPub)
	}
}

// TestEditSeatMapRefusesEmptyGeometry (TKT-318 COS-2) answers an edit with no seats with
// 409 and the seatless message. The map keeps its one version, and nothing is emitted.
func TestEditSeatMapRefusesEmptyGeometry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sections []SeatMapEditSection
	}{
		{"no sections", []SeatMapEditSection{}},
		{"a section with no rows", []SeatMapEditSection{{Name: "Orchestra", Position: 1, Rows: []SeatMapEditRow{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			venueID := seedVenue(t, e, "La Grande Salle")
			m := seedPublishedMap(t, e, venueID, "Main floor")
			before, emitted := len(e.store.seatMaps), len(e.pub.seatMapsPub)

			rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", SeatMapEdit{Sections: tc.sections})
			if rec.Code != http.StatusConflict {
				t.Fatalf("empty edit must be 409, got %d %s", rec.Code, rec.Body.String())
			}
			if got := decode[Error](t, rec).Error; got != seatlessMessage {
				t.Fatalf("error = %q, want %q", got, seatlessMessage)
			}
			if n := len(e.store.seatMaps); n != before {
				t.Fatalf("refused edit left %d version(s), want none", n-before)
			}
			if n := len(e.pub.seatMapsPub); n != emitted {
				t.Fatalf("refused edit emitted %d event(s), want none", n-emitted)
			}
		})
	}
}

// TestEditSeatMapEmptyWithPinsIsSeatlessNotPin (TKT-318 COS-3) keeps the order of the two
// refusals: an empty edit of a pinned map gets the seatless message, not the pin message.
func TestEditSeatMapEmptyWithPinsIsSeatlessNotPin(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedPublishedMap(t, e, venueID, "Main floor")
	e.store.pinSeat(m.Id, "Orchestra/A/1")

	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", SeatMapEdit{Sections: []SeatMapEditSection{}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("empty edit of a pinned map must be 409, got %d %s", rec.Code, rec.Body.String())
	}
	if got := decode[Error](t, rec).Error; got != seatlessMessage {
		t.Fatalf("error = %q, want the seatless message, not the pin message", got)
	}
}

func TestEditSeatMapRejectsOverlongIdentityBeforeCreatingVersion(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Identity bound venue")
	m := seedPublishedMap(t, e, venueID, "Identity bound map")
	before := len(e.store.seatMaps)
	body := SeatMapEdit{Sections: []SeatMapEditSection{{
		Name: strings.Repeat("S", 196), Position: 1,
		Rows: []SeatMapEditRow{{
			Label: "R", Position: 1,
			Seats: []SeatMapEditSeat{{Label: "12", Position: 1}},
		}},
	}}}

	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong edit status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := len(e.store.seatMaps); got != before {
		t.Fatalf("rejected edit left %d map versions, want %d", got, before)
	}
}

func TestEditSeatMapUnknownMap(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/seat-maps/"+orgID.String()+"/edit", editBody(false)) // not a map id
	if rec.Code != http.StatusNotFound {
		t.Fatalf("editing an unknown map must be 404, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestEditSeatMapEmitFailureRetries mirrors TestPublishSeatMapEmitFailureRetries:
// a failed emission of the new version's event leaves it owed (500), and the
// edit is NOT retried (that would mint yet another version) — recovery is
// re-POSTing publish of the new version. Here we just assert the 500 surface.
func TestEditSeatMapEmitFailureReturns500(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedPublishedMap(t, e, venueID, "Floor")
	e.store.pinSeat(m.Id, "Orchestra/A/1")

	e.pub.failSeatMapNext = true
	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", editBody(false))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("emit failure on edit must 500, got %d %s", rec.Code, rec.Body.String())
	}
	// The recovery hint must survive the production ResponseValidator (TKT-108).
	if !strings.Contains(rec.Body.String(), "retry by publishing the new version") {
		t.Fatalf("recovery body lost, got: %s", rec.Body.String())
	}
}

// TestListSeatMapVersionsHistoryAndCurrent is COS-3: the version-history read
// returns every version of the family newest-first, each with published_at, and
// current_version = the highest published version. Both versions here are
// published, so under TKT-107 it earned the cacheable branch — but TKT-141
// demoted that branch to the MINUTES tier for the two list reads, because this
// is exactly the read whose membership an ADR-029 edit changes: the successor
// this test creates is a new row in the very list being cached. The status-driven
// tier is proven by TestSeatMapReadCacheTierByStatus.
func TestListSeatMapVersionsHistoryAndCurrent(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedPublishedMap(t, e, venueID, "Main floor")
	e.store.pinSeat(m.Id, "Orchestra/A/1")
	nv := decode[SeatMap](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/edit", editBody(false)))

	rec := e.do("GET", "/public/seat-maps/"+m.Id.String()+"/versions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions read: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300, s-maxage=300" {
		t.Fatalf("versions read must be ADR-004 minutes tier, got %q", cc)
	}
	h := decode[SeatMapVersionHistory](t, rec)
	if len(h.Versions) != 2 {
		t.Fatalf("family must have 2 versions, got %d", len(h.Versions))
	}
	if h.Versions[0].Version != nv.Version {
		t.Fatalf("versions must be newest-first, got first=%d", h.Versions[0].Version)
	}
	for _, v := range h.Versions {
		if v.PublishedAt == nil {
			t.Fatalf("every published version must carry published_at, v%d had none", v.Version)
		}
	}
	if h.CurrentVersion == nil || *h.CurrentVersion != nv.Version {
		t.Fatalf("current_version must be the highest published (%d), got %v", nv.Version, h.CurrentVersion)
	}
	// Resolving by ANY version id yields the same family history.
	rec2 := e.do("GET", "/public/seat-maps/"+nv.Id.String()+"/versions", nil)
	if h2 := decode[SeatMapVersionHistory](t, rec2); len(h2.Versions) != 2 {
		t.Fatalf("history must resolve from any version id, got %d versions", len(h2.Versions))
	}
}

func TestListSeatMapVersionsUnknown(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/public/seat-maps/"+orgID.String()+"/versions", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("versions of an unknown map must be 404, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateVenueGaCapacity is COS-5: staff set a venue's GA capacity; the write
// echoes the updated venue and is no-store.
func TestUpdateVenueGaCapacity(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle") // seeded with GaCapacity 1000

	rec := e.do("POST", "/venues/"+venueID.String()+"/ga-capacity",
		VenueGaCapacityUpdate{GaCapacity: 250})
	if rec.Code != http.StatusOK {
		t.Fatalf("GA update: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GA update is a write, must be no-store, got %q", cc)
	}
	v := decode[Venue](t, rec)
	if v.GaCapacity != 250 {
		t.Fatalf("GA capacity must be updated to 250, got %d", v.GaCapacity)
	}
	if v.Id != venueID {
		t.Fatalf("must echo the same venue, got %s", v.Id)
	}
}

func TestUpdateVenueGaCapacityUnknown(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/venues/"+uuid.NewString()+"/ga-capacity",
		VenueGaCapacityUpdate{GaCapacity: 100})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GA update on unknown venue must be 404, got %d %s", rec.Code, rec.Body.String())
	}
}
