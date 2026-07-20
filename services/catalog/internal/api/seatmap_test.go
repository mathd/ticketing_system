package api

import (
	"net/http"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

// seedVenue creates a venue for orgID and returns its id.
func seedVenue(t *testing.T, e *env, name string) openapi_types.UUID {
	t.Helper()
	v := decode[Venue](t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: name, GaCapacity: 1000}))
	return v.Id
}

func seedDraftMap(t *testing.T, e *env, venueID openapi_types.UUID, name string) SeatMap {
	t.Helper()
	rec := e.do("POST", "/venues/"+venueID.String()+"/seat-maps",
		SeatMapCreate{OrganizerId: orgID, Name: name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create seat map: %d %s", rec.Code, rec.Body.String())
	}
	return decode[SeatMap](t, rec)
}

func TestCreateSeatMapDraft(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")

	rec := e.do("POST", "/venues/"+venueID.String()+"/seat-maps",
		SeatMapCreate{OrganizerId: orgID, Name: "Main floor"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("write must be no-store, got %q", cc)
	}
	m := decode[SeatMap](t, rec)
	if m.Status != "draft" {
		t.Fatalf("new map must start draft, got %q", m.Status)
	}
	if m.Version != 1 {
		t.Fatalf("new map must be version 1, got %d", m.Version)
	}
	if m.VenueId != venueID || m.OrganizerId != orgID || m.Name != "Main floor" {
		t.Fatalf("map payload wrong: %+v", m)
	}
}

func TestCreateSeatMapUnknownVenue(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/venues/"+orgID.String()+"/seat-maps", // orgID is not a venue id
		SeatMapCreate{OrganizerId: orgID, Name: "Nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown venue, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestSeatMapAuthoringChain is the COS-1/COS-2 round trip: map -> section ->
// row -> seat, then read the geometry back nested and ordered.
func TestSeatMapAuthoringChain(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedDraftMap(t, e, venueID, "Main floor")

	sec := decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{OrganizerId: orgID, SectionId: sec.Id, Label: "A", Position: 1}))
	seat := decode[Seat](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{OrganizerId: orgID, RowId: row.Id, Label: "12", Position: 1}))

	if seat.SeatIdentity != "Orchestra/A/12" {
		t.Fatalf("seat identity must be composed section/row/seat, got %q", seat.SeatIdentity)
	}

	// Geometry read: nested + hours tier.
	rec := e.do("GET", "/public/seat-maps/"+m.Id.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("geometry read: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600, s-maxage=3600" {
		t.Fatalf("geometry read must be ADR-004 hours tier, got %q", cc)
	}
	g := decode[SeatMapGeometry](t, rec)
	if g.Map.Id != m.Id || len(g.Sections) != 1 {
		t.Fatalf("geometry wrong: %+v", g)
	}
	s := g.Sections[0]
	if s.Name != "Orchestra" || s.Rows == nil || len(*s.Rows) != 1 {
		t.Fatalf("section geometry wrong: %+v", s)
	}
	r := (*s.Rows)[0]
	if r.Label != "A" || r.Seats == nil || len(*r.Seats) != 1 {
		t.Fatalf("row geometry wrong: %+v", r)
	}
	if (*r.Seats)[0].SeatIdentity != "Orchestra/A/12" {
		t.Fatalf("seat geometry wrong: %+v", (*r.Seats)[0])
	}
}

func TestGeometryOrderedByPosition(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedDraftMap(t, e, venueID, "Floor")
	// Insert sections out of position order; the read must sort ascending.
	_ = decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Balcony", Position: 2}))
	_ = decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1}))
	g := decode[SeatMapGeometry](t, e.do("GET", "/public/seat-maps/"+m.Id.String(), nil))
	if len(g.Sections) != 2 || g.Sections[0].Name != "Orchestra" || g.Sections[1].Name != "Balcony" {
		t.Fatalf("sections must be position-ordered, got %+v", g.Sections)
	}
}

func TestAddSectionUnknownMap(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/seat-maps/"+orgID.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestAddRowRejectsCrossMapSection: a section belonging to another map cannot be
// referenced under this map (parent scoping) — COS-5.
func TestAddRowRejectsCrossMapSection(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	mapA := seedDraftMap(t, e, venueID, "A")
	mapB := seedDraftMap(t, e, venueID, "B")
	secA := decode[SeatSection](t, e.do("POST", "/seat-maps/"+mapA.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1}))
	// Try to add a row under mapB referencing mapA's section.
	rec := e.do("POST", "/seat-maps/"+mapB.Id.String()+"/rows",
		SeatMapRowCreate{OrganizerId: orgID, SectionId: secA.Id, Label: "A", Position: 1})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-map section must be rejected 404, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAddSeatRejectsCrossMapRow(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	mapA := seedDraftMap(t, e, venueID, "A")
	mapB := seedDraftMap(t, e, venueID, "B")
	secA := decode[SeatSection](t, e.do("POST", "/seat-maps/"+mapA.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1}))
	rowA := decode[SeatRow](t, e.do("POST", "/seat-maps/"+mapA.Id.String()+"/rows",
		SeatMapRowCreate{OrganizerId: orgID, SectionId: secA.Id, Label: "A", Position: 1}))
	rec := e.do("POST", "/seat-maps/"+mapB.Id.String()+"/seats",
		SeatMapSeatCreate{OrganizerId: orgID, RowId: rowA.Id, Label: "1", Position: 1})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-map row must be rejected 404, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestDuplicateSeatIdentity: adding the same section/row/seat twice is a 409 —
// the composed identity collides. COS-5.
func TestDuplicateSeatIdentity(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedDraftMap(t, e, venueID, "Floor")
	sec := decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{OrganizerId: orgID, SectionId: sec.Id, Label: "A", Position: 1}))
	first := e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{OrganizerId: orgID, RowId: row.Id, Label: "12", Position: 1})
	if first.Code != http.StatusCreated {
		t.Fatalf("first seat: %d %s", first.Code, first.Body.String())
	}
	dup := e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{OrganizerId: orgID, RowId: row.Id, Label: "12", Position: 2})
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate seat identity must be 409, got %d %s", dup.Code, dup.Body.String())
	}
}

func TestDuplicateSectionNameIsConflict(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedDraftMap(t, e, venueID, "Floor")
	_ = e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1})
	dup := e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 2})
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate section name must be 409, got %d %s", dup.Code, dup.Body.String())
	}
}

func TestGetGeometryUnknownMap(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/public/seat-maps/"+orgID.String(), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestListVenueSeatMaps(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	other := seedVenue(t, e, "Other")
	_ = seedDraftMap(t, e, venueID, "Floor 1")
	_ = seedDraftMap(t, e, venueID, "Floor 2")
	_ = seedDraftMap(t, e, other, "Elsewhere")

	rec := e.do("GET", "/public/venues/"+venueID.String()+"/seat-maps", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600, s-maxage=3600" {
		t.Fatalf("list must be hours tier, got %q", cc)
	}
	out := decode[SeatMapList](t, rec)
	if len(out.SeatMaps) != 2 {
		t.Fatalf("list must be venue-scoped: want 2, got %d", len(out.SeatMaps))
	}
	for _, sm := range out.SeatMaps {
		if sm.VenueId != venueID {
			t.Fatalf("scoping leak: %+v", sm)
		}
	}
}
