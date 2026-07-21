package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// seedPublishedMap authors a one-seat draft map and publishes it via the API,
// returning the published version.
func seedPublishedMap(t *testing.T, e *env, venueID openapi_types.UUID, name string) SeatMap {
	t.Helper()
	m := seedDraftMap(t, e, venueID, name)
	sec := decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "Orchestra", Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{OrganizerId: orgID, SectionId: sec.Id, Label: "A", Position: 1}))
	e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{OrganizerId: orgID, RowId: row.Id, Label: "1", Position: 1})
	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish seat map: %d %s", rec.Code, rec.Body.String())
	}
	return decode[SeatMap](t, rec)
}

// TestPublishSeatMap (TKT-103 COS-1) exercises the publish endpoint: draft ->
// published emits the seat_map.published event once, is idempotent, and marks
// the outbox so a re-publish does not re-emit.
func TestPublishSeatMap(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedDraftMap(t, e, venueID, "Main floor")

	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	published := decode[SeatMap](t, rec)
	if published.Status != "published" || published.PublishedAt == nil {
		t.Fatalf("published map = %q publishedAt=%v, want published with a timestamp", published.Status, published.PublishedAt)
	}
	if len(e.pub.seatMapsPub) != 1 || e.pub.seatMapsPub[0].ID != m.Id {
		t.Fatalf("expected exactly one seat_map.published emission for %s, got %+v", m.Id, e.pub.seatMapsPub)
	}

	// Idempotent re-publish: still 200 published, and NO second emission (marked).
	rec2 := e.do("POST", "/seat-maps/"+m.Id.String()+"/publish", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-publish: %d %s", rec2.Code, rec2.Body.String())
	}
	if len(e.pub.seatMapsPub) != 1 {
		t.Fatalf("re-publish must not re-emit once marked, got %d emissions", len(e.pub.seatMapsPub))
	}
}

// TestPublishSeatMapEmitFailureRetries (COS-1 at-least-once): a failed emission
// leaves the event owed, so a retry re-emits — the marker is set only after ack.
func TestPublishSeatMapEmitFailureRetries(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedDraftMap(t, e, venueID, "Floor")

	e.pub.failSeatMapNext = true
	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/publish", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("emit failure must 500, got %d %s", rec.Code, rec.Body.String())
	}
	// The recovery hint must survive the production ResponseValidator (TKT-108):
	// an undocumented 500 would be rewritten to the generic contract-violation body.
	if !strings.Contains(rec.Body.String(), "the domain event was not emitted; retry publish") {
		t.Fatalf("recovery body lost, got: %s", rec.Body.String())
	}
	if len(e.pub.seatMapsPub) != 0 {
		t.Fatalf("failed emission must not record a publish, got %+v", e.pub.seatMapsPub)
	}
	// Retry succeeds and emits (the event was still owed).
	rec = e.do("POST", "/seat-maps/"+m.Id.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry publish: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.seatMapsPub) != 1 {
		t.Fatalf("retry must emit the owed event once, got %d", len(e.pub.seatMapsPub))
	}
}

func TestPublishSeatMapUnknown(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/seat-maps/"+orgID.String()+"/publish", nil) // not a map id
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown map, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestPublishedSeatMapRejectsAuthoring (COS-1 immutability): a published version
// is frozen — further section authoring is a 404 (the draft-only write gate).
func TestPublishedSeatMapRejectsAuthoring(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedPublishedMap(t, e, venueID, "Frozen")
	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "New", Position: 9})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("authoring a published map must be refused (404), got %d %s", rec.Code, rec.Body.String())
	}
}

// TestCreateSeatedPerformance (TKT-103 COS-2): a performance referencing a
// published map in the same venue is created seated; the response carries the
// map reference. A GA performance (no reference) is unchanged.
func TestCreateSeatedPerformance(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedPublishedMap(t, e, venueID, "Main floor")
	event := decode[Event](t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Récital", "en": "Recital"},
	}))
	startsAt := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	seatedRec := e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venueID,
		StartsAt: &startsAt, Timezone: "Europe/Paris", SeatMapId: &m.Id,
	})
	if seatedRec.Code != http.StatusCreated {
		t.Fatalf("create seated: %d %s", seatedRec.Code, seatedRec.Body.String())
	}
	seated := decode[Performance](t, seatedRec)
	if seated.SeatMapId == nil || *seated.SeatMapId != m.Id {
		t.Fatalf("seated performance must carry the map reference, got %+v", seated.SeatMapId)
	}

	// GA performance at the same venue is unchanged: no reference, coexists.
	gaRec := e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venueID,
		StartsAt: &startsAt, Timezone: "Europe/Paris",
	})
	ga := decode[Performance](t, gaRec)
	if ga.SeatMapId != nil {
		t.Fatalf("GA performance must not carry a map reference, got %+v", ga.SeatMapId)
	}
}

// TestCreateSeatedPerformanceRejectsUnpublishedOrCrossTenant (COS-2 validation):
// a seated reference must be to a published map in the same venue/organizer.
func TestCreateSeatedPerformanceRejectsUnpublishedOrCrossTenant(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	otherVenueID := seedVenue(t, e, "Petit Théâtre")
	draft := seedDraftMap(t, e, venueID, "Draft map")
	publishedElsewhere := seedPublishedMap(t, e, otherVenueID, "Other venue map")
	event := decode[Event](t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "R", "en": "R"},
	}))
	startsAt := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	unknown := uuid.New()

	for _, tc := range []struct {
		name  string
		mapID uuid.UUID
		want  int
	}{
		{"draft map is not seatable", draft.Id, http.StatusConflict},
		{"published map in another venue", publishedElsewhere.Id, http.StatusBadRequest},
		{"unknown map", unknown, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.mapID
			rec := e.do("POST", "/performances", PerformanceCreate{
				OrganizerId: orgID, EventId: event.Id, VenueId: venueID,
				StartsAt: &startsAt, Timezone: "Europe/Paris", SeatMapId: &id,
			})
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d %s", tc.want, rec.Code, rec.Body.String())
			}
		})
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
	// The 409 body must name the seat-identity cause (TKT-105 broadened it so an
	// edit's identity conflict is not misdescribed as "name or position").
	if body := decode[Error](t, dup); !strings.Contains(body.Error, "seat identity") {
		t.Fatalf("conflict body must name the seat-identity cause, got %q", body.Error)
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

// TestSeatMapRejectsSlashInLabel: '/' is the seat-identity delimiter, so a
// component carrying one is rejected at the contract (400) — otherwise distinct
// seats could compose to the same identity.
func TestSeatMapRejectsSlashInLabel(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Hall")
	m := seedDraftMap(t, e, venueID, "Floor")
	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{OrganizerId: orgID, Name: "A/B", Position: 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("section name with '/' must be 400, got %d %s", rec.Code, rec.Body.String())
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
