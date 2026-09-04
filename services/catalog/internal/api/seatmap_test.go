package api

import (
	"encoding/json"
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
		VenueCreate{Name: name, GaCapacity: 1000}))
	return v.Id
}

func seedDraftMap(t *testing.T, e *env, venueID openapi_types.UUID, name string) SeatMap {
	t.Helper()
	rec := e.do("POST", "/venues/"+venueID.String()+"/seat-maps",
		SeatMapCreate{Name: name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create seat map: %d %s", rec.Code, rec.Body.String())
	}
	return decode[SeatMap](t, rec)
}

func TestCreateSeatMapDraft(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")

	rec := e.do("POST", "/venues/"+venueID.String()+"/seat-maps",
		SeatMapCreate{Name: "Main floor"})
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
		SeatMapCreate{Name: "Nope"})
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
		SeatMapSectionCreate{Name: "Orchestra", Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: sec.Id, Label: "A", Position: 1}))
	seat := decode[Seat](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: row.Id, Label: "12", Position: 1}))

	if seat.SeatIdentity != "Orchestra/A/12" {
		t.Fatalf("seat identity must be composed section/row/seat, got %q", seat.SeatIdentity)
	}

	// Geometry read: nested + no-store, because this map is still a draft
	// (TKT-107: draft geometry is mutable, so it never gets a shared-cache tier).
	rec := e.do("GET", "/public/seat-maps/"+m.Id.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("geometry read: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("draft geometry read must be no-store (TKT-107), got %q", cc)
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

// Exercise the longest identity Catalog may persist through its real router.
// The cross-contract test in services/catalog/api checks the same boundary
// against the Commerce and Inventory request schemas.
func TestSeatMapAuthoringReturnsMaximumIdentity(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Long identity venue")
	m := seedDraftMap(t, e, venueID, "Long identity map")
	sectionName := strings.Repeat("🎟", 196)

	section := decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{Name: sectionName, Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: section.Id, Label: "R", Position: 1}))
	seat := decode[Seat](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: row.Id, Label: "1", Position: 1}))

	want := sectionName + "/R/1"
	if got := len([]rune(want)); got != 200 {
		t.Fatalf("fixture identity has %d characters, want 200", got)
	}
	if seat.SeatIdentity != want {
		t.Fatalf("seat identity = %q, want %q", seat.SeatIdentity, want)
	}

	geometry := decode[SeatMapGeometry](t, e.do("GET", "/public/seat-maps/"+m.Id.String(), nil))
	if geometry.Sections[0].Rows == nil || (*geometry.Sections[0].Rows)[0].Seats == nil {
		t.Fatalf("long-identity seat missing from geometry: %+v", geometry)
	}
	got := (*(*geometry.Sections[0].Rows)[0].Seats)[0].SeatIdentity
	if got != want {
		t.Fatalf("geometry seat identity = %q, want %q", got, want)
	}

}

func TestSeatMapAuthoringRejectsOverlongComposedIdentityBeforeWrite(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "Overlong identity venue")
	m := seedDraftMap(t, e, venueID, "Overlong identity map")
	section := decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{Name: strings.Repeat("🎟", 196), Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: section.Id, Label: "R", Position: 1}))
	before := len(e.store.seatSeats)

	rec := e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: row.Id, Label: "12", Position: 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong identity status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := len(e.store.seatSeats); got != before {
		t.Fatalf("rejected identity wrote %d seat(s), want %d", got, before)
	}
}

// seedPublishedMap authors a one-seat draft map and publishes it via the API,
// returning the published version.
func seedPublishedMap(t *testing.T, e *env, venueID openapi_types.UUID, name string) SeatMap {
	t.Helper()
	m := seedDraftMap(t, e, venueID, name)
	sec := decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{Name: "Orchestra", Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: sec.Id, Label: "A", Position: 1}))
	e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: row.Id, Label: "1", Position: 1})
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
		SeatMapSectionCreate{Name: "New", Position: 9})
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
		Name: LocalizedString{"fr": "Récital", "en": "Recital"},
	}))
	startsAt := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	seatedRec := e.do("POST", "/performances", PerformanceCreate{
		EventId: event.Id, VenueId: venueID,
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
		EventId: event.Id, VenueId: venueID,
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
		Name: LocalizedString{"fr": "R", "en": "R"},
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
				EventId: event.Id, VenueId: venueID,
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
		SeatMapSectionCreate{Name: "Balcony", Position: 2}))
	_ = decode[SeatSection](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{Name: "Orchestra", Position: 1}))
	g := decode[SeatMapGeometry](t, e.do("GET", "/public/seat-maps/"+m.Id.String(), nil))
	if len(g.Sections) != 2 || g.Sections[0].Name != "Orchestra" || g.Sections[1].Name != "Balcony" {
		t.Fatalf("sections must be position-ordered, got %+v", g.Sections)
	}
}

func TestAddSectionUnknownMap(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/seat-maps/"+orgID.String()+"/sections",
		SeatMapSectionCreate{Name: "Orchestra", Position: 1})
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
		SeatMapSectionCreate{Name: "Orchestra", Position: 1}))
	// Try to add a row under mapB referencing mapA's section.
	rec := e.do("POST", "/seat-maps/"+mapB.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: secA.Id, Label: "A", Position: 1})
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
		SeatMapSectionCreate{Name: "Orchestra", Position: 1}))
	rowA := decode[SeatRow](t, e.do("POST", "/seat-maps/"+mapA.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: secA.Id, Label: "A", Position: 1}))
	rec := e.do("POST", "/seat-maps/"+mapB.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: rowA.Id, Label: "1", Position: 1})
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
		SeatMapSectionCreate{Name: "Orchestra", Position: 1}))
	row := decode[SeatRow](t, e.do("POST", "/seat-maps/"+m.Id.String()+"/rows",
		SeatMapRowCreate{SectionId: sec.Id, Label: "A", Position: 1}))
	first := e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: row.Id, Label: "12", Position: 1})
	if first.Code != http.StatusCreated {
		t.Fatalf("first seat: %d %s", first.Code, first.Body.String())
	}
	dup := e.do("POST", "/seat-maps/"+m.Id.String()+"/seats",
		SeatMapSeatCreate{RowId: row.Id, Label: "12", Position: 2})
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
		SeatMapSectionCreate{Name: "Orchestra", Position: 1})
	dup := e.do("POST", "/seat-maps/"+m.Id.String()+"/sections",
		SeatMapSectionCreate{Name: "Orchestra", Position: 2})
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
		SeatMapSectionCreate{Name: "A/B", Position: 1})
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
	// Every map in this fixture is a draft, so the whole response is no-store
	// (TKT-107). The status-driven tier itself is proven by
	// TestSeatMapReadCacheTierByStatus.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("draft-only list must be no-store (TKT-107), got %q", cc)
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

// addDraftSuccessor seeds a draft version into an existing map's family, which no
// public write path can produce: publish moves draft -> published, and an ADR-029
// edit inserts a *published* successor. A mixed-status version history is
// nonetheless reachable in principle (nothing forbids the row), so TKT-107's
// least-cacheable-member rule has to hold for it — hence the fixture.
func addDraftSuccessor(t *testing.T, e *env, anyVersionID openapi_types.UUID) {
	t.Helper()
	successor, ok := e.store.seatMaps[anyVersionID]
	if !ok {
		t.Fatalf("addDraftSuccessor: %s is not a seeded map", anyVersionID)
	}
	successor.ID = uuid.New()
	successor.Version++
	successor.Status = "draft"
	successor.PublishedAt = nil
	successor.CreatedAt = time.Now().UTC()
	e.store.seatMaps[successor.ID] = successor
	e.store.families[successor.ID] = e.store.families[anyVersionID]
}

// TestSeatMapReadCacheTierByStatus is TKT-107's whole rule, across all three
// public seat-map reads: a response gets the ADR-004 hours tier only when it is
// non-empty and every seat map in it is published; anything else is no-store.
// Draft geometry is mutable, so an hour of shared-cache lifetime would make an
// authoring write look lost; published versions are immutable by ADR-029, which
// is what makes the hours branch correct rather than merely inherited.
func TestSeatMapReadCacheTierByStatus(t *testing.T) {
	// Two tiers, two literals. TKT-141 split them: list MEMBERSHIP is mutable
	// (authoring a map, or an ADR-029 edit inserting a published successor,
	// changes the list), so an hours tier on a list promises something untrue;
	// a single published version genuinely is immutable, so the by-id read keeps
	// it. Written out rather than read from CacheControlPublicVenueReads /
	// CacheControlPublicReads on purpose: an expectation derived from the
	// implementation pins whatever the code does, not the rule (TKT-239).
	const (
		hoursTier   = "public, max-age=3600, s-maxage=3600" // ADR-004, by-id geometry
		minutesTier = "public, max-age=300, s-maxage=300"   // ADR-004, list reads
	)

	// -- GET /public/seat-maps/{id} : one map, so draft or published, never mixed --
	t.Run("geometry/published", func(t *testing.T) {
		e := newEnv(t)
		m := seedPublishedMap(t, e, seedVenue(t, e, "Hall"), "Floor")
		rec := e.do("GET", "/public/seat-maps/"+m.Id.String(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("geometry: %d %s", rec.Code, rec.Body.String())
		}
		if g := decode[SeatMapGeometry](t, rec); g.Map.Status != "published" {
			t.Fatalf("fixture must be published, got %q", g.Map.Status)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != hoursTier {
			t.Fatalf("published geometry must keep the hours tier, got %q", cc)
		}
	})
	t.Run("geometry/draft", func(t *testing.T) {
		e := newEnv(t)
		m := seedDraftMap(t, e, seedVenue(t, e, "Hall"), "Floor")
		rec := e.do("GET", "/public/seat-maps/"+m.Id.String(), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("geometry: %d %s", rec.Code, rec.Body.String())
		}
		if g := decode[SeatMapGeometry](t, rec); g.Map.Status != "draft" {
			t.Fatalf("fixture must be draft, got %q", g.Map.Status)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("draft geometry must be no-store, got %q", cc)
		}
	})

	// -- GET /public/venues/{id}/seat-maps : the mixed and empty cases live here --
	t.Run("venue-list/published-only", func(t *testing.T) {
		e := newEnv(t)
		venueID := seedVenue(t, e, "Hall")
		_ = seedPublishedMap(t, e, venueID, "Floor 1")
		_ = seedPublishedMap(t, e, venueID, "Floor 2")
		rec := e.do("GET", "/public/venues/"+venueID.String()+"/seat-maps", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		out := decode[SeatMapList](t, rec)
		if len(out.SeatMaps) != 2 {
			t.Fatalf("fixture must list 2 maps, got %d", len(out.SeatMaps))
		}
		if cc := rec.Header().Get("Cache-Control"); cc != minutesTier {
			t.Fatalf("all-published list must take the minutes tier, got %q", cc)
		}
	})
	t.Run("venue-list/mixed", func(t *testing.T) {
		e := newEnv(t)
		venueID := seedVenue(t, e, "Hall")
		_ = seedPublishedMap(t, e, venueID, "Floor 1")
		_ = seedDraftMap(t, e, venueID, "Floor 2")
		rec := e.do("GET", "/public/venues/"+venueID.String()+"/seat-maps", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		out := decode[SeatMapList](t, rec)
		if len(out.SeatMaps) != 2 {
			t.Fatalf("mixed fixture must list both maps, got %d", len(out.SeatMaps))
		}
		// One response carries one header, so the least-cacheable member decides.
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("one draft row must make the whole list no-store, got %q", cc)
		}
	})
	t.Run("venue-list/empty", func(t *testing.T) {
		e := newEnv(t)
		venueID := seedVenue(t, e, "Empty hall")
		rec := e.do("GET", "/public/venues/"+venueID.String()+"/seat-maps", nil)
		// The status assertion is load-bearing here and not decoration: an error
		// body unmarshals into SeatMapList as a nil slice, so "empty" alone would
		// also be satisfied by a 500 (which carries no-store as well).
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		if out := decode[SeatMapList](t, rec); len(out.SeatMaps) != 0 {
			t.Fatalf("fixture must be empty, got %d", len(out.SeatMaps))
		}
		// Fail closed: no published row justifies caching, and caching "no maps"
		// for an hour would hide the venue's first seat map.
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("empty list must be no-store, got %q", cc)
		}
	})

	// -- GET /public/seat-maps/{id}/versions : never empty (404s instead) --
	t.Run("versions/draft-only", func(t *testing.T) {
		e := newEnv(t)
		m := seedDraftMap(t, e, seedVenue(t, e, "Hall"), "Floor")
		rec := e.do("GET", "/public/seat-maps/"+m.Id.String()+"/versions", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
		}
		out := decode[SeatMapVersionHistory](t, rec)
		if len(out.Versions) != 1 || out.Versions[0].Status != "draft" || out.CurrentVersion != nil {
			t.Fatalf("fixture must be one draft version with no current_version, got %+v", out)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("draft-only history must be no-store, got %q", cc)
		}
	})
	t.Run("versions/mixed", func(t *testing.T) {
		e := newEnv(t)
		m := seedPublishedMap(t, e, seedVenue(t, e, "Hall"), "Floor")
		addDraftSuccessor(t, e, m.Id)
		rec := e.do("GET", "/public/seat-maps/"+m.Id.String()+"/versions", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
		}
		out := decode[SeatMapVersionHistory](t, rec)
		if len(out.Versions) != 2 {
			t.Fatalf("mixed history must carry both versions, got %d", len(out.Versions))
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("a draft version must make the whole history no-store, got %q", cc)
		}
	})
	// The all-published history is the SECOND list read TKT-141 demotes, and it
	// gets its own case here rather than relying on the venue-list case above:
	// the two reads call the tier rule through different handlers, so one
	// assertion could not see a regression that moved only the other.
	t.Run("versions/published-only", func(t *testing.T) {
		e := newEnv(t)
		m := seedPublishedMap(t, e, seedVenue(t, e, "Hall"), "Floor")
		rec := e.do("GET", "/public/seat-maps/"+m.Id.String()+"/versions", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
		}
		out := decode[SeatMapVersionHistory](t, rec)
		if len(out.Versions) != 1 || out.Versions[0].Status != "published" {
			t.Fatalf("fixture must be one published version, got %+v", out)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != minutesTier {
			t.Fatalf("all-published history must take the minutes tier, got %q", cc)
		}
	})
}

// TestPublicPerformanceDetailCarriesSeatMapID is TKT-172 AC1: the public event
// detail says which published seat-map version a performance is seated against,
// so a storefront can tell a seated performance from a GA one and know which map
// to render. Before this, `performances.seat_map_id` existed in the database and
// on the back-office response but was invisible to every public reader.
//
// The GA half is asserted on the RAW JSON, not the decoded struct: an optional
// field decodes to nil whether it was absent or explicitly null, so a struct
// assertion passes even when the payload grew a `"seat_map_id": null` key. The AC
// is that a GA performance's bytes are unchanged, and only the bytes show that.
func TestPublicPerformanceDetailCarriesSeatMapID(t *testing.T) {
	e := newEnv(t)
	venueID := seedVenue(t, e, "La Grande Salle")
	m := seedPublishedMap(t, e, venueID, "Main floor")
	event := decode[Event](t, e.do("POST", "/events", EventCreate{
		Name: LocalizedString{"fr": "Récital", "en": "Recital"},
	}))
	startsAt := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)

	for _, seatMap := range []*openapi_types.UUID{&m.Id, nil} {
		perf := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{
			EventId: event.Id, VenueId: venueID,
			StartsAt: &startsAt, Timezone: "Europe/Paris", SeatMapId: seatMap,
		}))
		e.do("POST", "/ticket-types", TicketTypeCreate{
			PerformanceId: perf.Id,
			Name:          LocalizedString{"fr": "Place", "en": "Seat"},
			Price:         Money{Amount: 5000, Currency: "EUR"},
		})
		if rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil); rec.Code != http.StatusOK {
			t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := e.do("GET", "/public/events/"+event.Id.String()+"?locale=fr", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public detail: %d %s", rec.Code, rec.Body.String())
	}
	detail := decode[PublicEventDetail](t, rec)
	if len(detail.Performances) != 2 {
		t.Fatalf("want both performances listed, got %d", len(detail.Performances))
	}

	var seated, ga int
	for _, p := range detail.Performances {
		if p.SeatMapId != nil {
			seated++
			if *p.SeatMapId != m.Id {
				t.Fatalf("seated performance names map %v, want the published version %v", *p.SeatMapId, m.Id)
			}
		} else {
			ga++
		}
	}
	if seated != 1 || ga != 1 {
		t.Fatalf("want exactly one seated and one GA performance, got %d seated / %d GA", seated, ga)
	}

	// The GA object must not have grown a key at all — not even a null one.
	var raw struct {
		Performances []map[string]json.RawMessage `json:"performances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var sawGA bool
	for _, p := range raw.Performances {
		if _, ok := p["seat_map_id"]; ok {
			continue
		}
		sawGA = true
	}
	if !sawGA {
		t.Fatalf("a GA performance must OMIT seat_map_id, not carry it as null — payload: %s", rec.Body.String())
	}
}
