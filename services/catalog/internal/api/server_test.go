package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/store"
)

// fakeStore is an in-memory Store. It mirrors the referential/tenancy checks
// the SQL enforces; the real queries are exercised by the smoke suite.
type fakeStore struct {
	venues         map[uuid.UUID]store.Venue
	events         map[uuid.UUID]store.Event
	performances   map[uuid.UUID]store.Performance
	ticketTypes    map[uuid.UUID]store.TicketType
	series         map[uuid.UUID]store.Series
	seasons        map[uuid.UUID]store.Season
	festivals      map[uuid.UUID]store.Festival
	emitted        map[uuid.UUID]bool // performance id -> event_emitted_at set
	archiveEmitted map[uuid.UUID]bool
	closureEmitted map[uuid.UUID]int32 // performance id -> closure_emitted_version
	seatMaps       map[uuid.UUID]store.SeatMap
	seatMapEmitted map[uuid.UUID]bool            // seat map id -> seat_map.published event_emitted_at set
	families       map[uuid.UUID]uuid.UUID       // seat map id -> its map_family_id (TKT-105)
	pins           map[uuid.UUID]map[string]bool // map_family_id -> pinned seat identities (TKT-104/105)
	seatSections   map[uuid.UUID]fakeSection
	seatRows       map[uuid.UUID]fakeRow
	seatSeats      map[uuid.UUID]fakeSeat
}

// fakeSection/fakeRow/fakeSeat carry the parent linkage the SQL enforces via
// FKs, so the in-memory store can mirror the same referential/uniqueness checks.
type fakeSection struct {
	store.SeatMapSection
	seatMapID uuid.UUID
}
type fakeRow struct {
	store.SeatMapRow
	seatMapID   uuid.UUID
	sectionID   uuid.UUID
	sectionName string
}
type fakeSeat struct {
	store.SeatMapSeat
	seatMapID uuid.UUID
	rowID     uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		venues:         map[uuid.UUID]store.Venue{},
		events:         map[uuid.UUID]store.Event{},
		performances:   map[uuid.UUID]store.Performance{},
		ticketTypes:    map[uuid.UUID]store.TicketType{},
		series:         map[uuid.UUID]store.Series{},
		seasons:        map[uuid.UUID]store.Season{},
		festivals:      map[uuid.UUID]store.Festival{},
		emitted:        map[uuid.UUID]bool{},
		archiveEmitted: map[uuid.UUID]bool{},
		closureEmitted: map[uuid.UUID]int32{},
		seatMaps:       map[uuid.UUID]store.SeatMap{},
		families:       map[uuid.UUID]uuid.UUID{},
		pins:           map[uuid.UUID]map[string]bool{},
		seatSections:   map[uuid.UUID]fakeSection{},
		seatRows:       map[uuid.UUID]fakeRow{},
		seatSeats:      map[uuid.UUID]fakeSeat{},
	}
}

// --- Seat-map authoring (US-019). Mirrors the SQL parent-scoping + draft
// requirement + UNIQUE constraints so the handler tests exercise the same
// referential surface the smoke suite proves against real Postgres. ---

func (f *fakeStore) CreateSeatMap(_ context.Context, in store.SeatMapInput) (store.SeatMap, error) {
	v, ok := f.venues[in.VenueID]
	if !ok || v.OrganizerID != in.OrganizerID {
		return store.SeatMap{}, fmt.Errorf("venue: %w", store.ErrNotFound)
	}
	m := store.SeatMap{ID: uuid.New(), OrganizerID: in.OrganizerID, VenueID: in.VenueID,
		Name: in.Name, Version: 1, Status: "draft", CreatedAt: time.Now().UTC()}
	f.seatMaps[m.ID] = m
	f.families[m.ID] = m.ID // a new draft map starts its own family (id == family key)
	return m, nil
}

func (f *fakeStore) draftMap(id, org uuid.UUID) (store.SeatMap, bool) {
	m, ok := f.seatMaps[id]
	return m, ok && m.OrganizerID == org && m.Status == "draft"
}

func (f *fakeStore) PublishSeatMap(_ context.Context, id uuid.UUID) (store.SeatMap, bool, error) {
	m, ok := f.seatMaps[id]
	if !ok {
		return store.SeatMap{}, false, fmt.Errorf("seat map: %w", store.ErrNotFound)
	}
	if m.Status == "archived" {
		return store.SeatMap{}, false, store.ErrIllegalTransition
	}
	if m.Status != "published" {
		now := time.Now().UTC()
		m.Status = "published"
		m.PublishedAt = &now
		f.seatMaps[id] = m
	}
	if f.seatMapEmitted == nil {
		f.seatMapEmitted = map[uuid.UUID]bool{}
	}
	return m, !f.seatMapEmitted[id], nil
}

func (f *fakeStore) MarkSeatMapEventEmitted(_ context.Context, id uuid.UUID) error {
	if f.seatMapEmitted == nil {
		f.seatMapEmitted = map[uuid.UUID]bool{}
	}
	f.seatMapEmitted[id] = true
	return nil
}

// currentPublishedInFamily returns the highest published version of the family
// that mapID belongs to (mirrors lockCurrentPublishedVersion). ok is false when
// mapID is unknown or the family has no published version.
func (f *fakeStore) currentPublishedInFamily(mapID, org uuid.UUID) (store.SeatMap, bool) {
	seed, ok := f.seatMaps[mapID]
	if !ok || seed.OrganizerID != org {
		return store.SeatMap{}, false
	}
	fam := f.families[mapID] // family key; seeded on create, inherited on edit
	var best store.SeatMap
	found := false
	for id, m := range f.seatMaps {
		if f.families[id] == fam && m.Status == "published" && (!found || m.Version > best.Version) {
			best, found = m, true
		}
	}
	return best, found
}

// pinSeat seeds a pin on a family (test helper; TKT-80 does this in prod). The
// pin is family-scoped and version-independent, keyed by the seat identity.
func (f *fakeStore) pinSeat(anyVersionID uuid.UUID, identity string) {
	if f.pins == nil {
		f.pins = map[uuid.UUID]map[string]bool{}
	}
	fam := f.families[anyVersionID]
	if f.pins[fam] == nil {
		f.pins[fam] = map[string]bool{}
	}
	f.pins[fam][identity] = true
}

// EditSeatMap mirrors the store contract (TKT-104/ADR-029) in memory: resolve
// the family's current published version, reject if the submitted geometry drops
// any pinned identity, else create a new published version (version+1) with the
// new geometry. The authoritative behaviour is proven by the store smoke tests;
// this fake exists so the HTTP handler can be tested without Postgres.
func (f *fakeStore) EditSeatMap(_ context.Context, in store.EditSeatMapInput) (store.SeatMap, bool, error) {
	cur, ok := f.currentPublishedInFamily(in.SeatMapID, in.OrganizerID)
	if !ok {
		return store.SeatMap{}, false, fmt.Errorf("seat map: %w", store.ErrNotFound)
	}
	// Compose the submitted geometry's identities; a duplicate is a conflict.
	newIdentities := map[string]bool{}
	for _, sec := range in.Sections {
		for _, row := range sec.Rows {
			for _, seat := range row.Seats {
				id := sec.Name + "/" + row.Label + "/" + seat.Label
				if newIdentities[id] {
					return store.SeatMap{}, false, fmt.Errorf("seat identity: %w", store.ErrSeatMapConflict)
				}
				newIdentities[id] = true
			}
		}
	}
	// Every currently-pinned identity must survive.
	fam := f.families[in.SeatMapID]
	for identity := range f.pins[fam] {
		if !newIdentities[identity] {
			return store.SeatMap{}, false, store.ErrSeatMapEditOrphansPinned
		}
	}
	// Create the new published version.
	now := time.Now().UTC()
	nv := store.SeatMap{
		ID: uuid.New(), OrganizerID: cur.OrganizerID, VenueID: cur.VenueID, Name: cur.Name,
		Version: cur.Version + 1, Status: "published", PublishedAt: &now, CreatedAt: now,
	}
	f.seatMaps[nv.ID] = nv
	f.families[nv.ID] = fam
	// Materialize the new version's seats (so a subsequent edit/pin sees them).
	for _, sec := range in.Sections {
		for _, row := range sec.Rows {
			for _, seat := range row.Seats {
				sid := sec.Name + "/" + row.Label + "/" + seat.Label
				f.seatSeats[uuid.New()] = fakeSeat{
					SeatMapSeat: store.SeatMapSeat{ID: uuid.New(), SeatIdentity: sid, Label: seat.Label, Position: seat.Position},
					seatMapID:   nv.ID,
				}
			}
		}
	}
	if f.seatMapEmitted == nil {
		f.seatMapEmitted = map[uuid.UUID]bool{}
	}
	return nv, !f.seatMapEmitted[nv.ID], nil
}

func (f *fakeStore) ListSeatMapVersions(_ context.Context, seatMapID uuid.UUID) ([]store.SeatMap, error) {
	fam, ok := f.families[seatMapID]
	if !ok {
		return nil, fmt.Errorf("seat map: %w", store.ErrNotFound)
	}
	var out []store.SeatMap
	for id, m := range f.seatMaps {
		if f.families[id] == fam {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version }) // newest first
	if len(out) == 0 {
		return nil, fmt.Errorf("seat map: %w", store.ErrNotFound)
	}
	return out, nil
}

func (f *fakeStore) UpdateVenueGACapacity(_ context.Context, in store.VenueGACapacityInput) (store.Venue, error) {
	v, ok := f.venues[in.VenueID]
	if !ok || v.OrganizerID != in.OrganizerID {
		return store.Venue{}, fmt.Errorf("venue: %w", store.ErrNotFound)
	}
	v.GACapacity = in.GACapacity
	f.venues[in.VenueID] = v
	return v, nil
}

func (f *fakeStore) PinSeat(_ context.Context, _ store.PinSeatInput) error {
	return fmt.Errorf("seat map: %w", store.ErrNotFound)
}

func (f *fakeStore) UnpinSeat(_ context.Context, _ store.PinSeatInput) error { return nil }

func (f *fakeStore) PinSeats(_ context.Context, _ store.BatchPinInput) error {
	return fmt.Errorf("seat map: %w", store.ErrNotFound)
}

func (f *fakeStore) UnpinSeats(_ context.Context, _ store.BatchPinInput) error { return nil }

func (f *fakeStore) AddSeatMapSection(_ context.Context, in store.SeatMapSectionInput) (store.SeatMapSection, error) {
	if _, ok := f.draftMap(in.SeatMapID, in.OrganizerID); !ok {
		return store.SeatMapSection{}, fmt.Errorf("seat map: %w", store.ErrNotFound)
	}
	for _, s := range f.seatSections {
		if s.seatMapID == in.SeatMapID && (s.Name == in.Name || s.Position == in.Position) {
			return store.SeatMapSection{}, fmt.Errorf("section: %w", store.ErrSeatMapConflict)
		}
	}
	sec := store.SeatMapSection{ID: uuid.New(), Name: in.Name, Position: in.Position}
	f.seatSections[sec.ID] = fakeSection{SeatMapSection: sec, seatMapID: in.SeatMapID}
	return sec, nil
}

func (f *fakeStore) AddSeatMapRow(_ context.Context, in store.SeatMapRowInput) (store.SeatMapRow, error) {
	sec, ok := f.seatSections[in.SectionID]
	if !ok || sec.seatMapID != in.SeatMapID {
		return store.SeatMapRow{}, fmt.Errorf("section: %w", store.ErrNotFound)
	}
	if _, ok := f.draftMap(in.SeatMapID, in.OrganizerID); !ok {
		return store.SeatMapRow{}, fmt.Errorf("section: %w", store.ErrNotFound)
	}
	for _, r := range f.seatRows {
		if r.sectionID == in.SectionID && (r.Label == in.Label || r.Position == in.Position) {
			return store.SeatMapRow{}, fmt.Errorf("row: %w", store.ErrSeatMapConflict)
		}
	}
	row := store.SeatMapRow{ID: uuid.New(), Label: in.Label, Position: in.Position}
	f.seatRows[row.ID] = fakeRow{SeatMapRow: row, seatMapID: in.SeatMapID, sectionID: in.SectionID, sectionName: sec.Name}
	return row, nil
}

func (f *fakeStore) AddSeatMapSeat(_ context.Context, in store.SeatMapSeatInput) (store.SeatMapSeat, error) {
	row, ok := f.seatRows[in.RowID]
	if !ok || row.seatMapID != in.SeatMapID {
		return store.SeatMapSeat{}, fmt.Errorf("row: %w", store.ErrNotFound)
	}
	if _, ok := f.draftMap(in.SeatMapID, in.OrganizerID); !ok {
		return store.SeatMapSeat{}, fmt.Errorf("row: %w", store.ErrNotFound)
	}
	identity := row.sectionName + "/" + row.Label + "/" + in.Label
	for _, s := range f.seatSeats {
		if s.seatMapID == in.SeatMapID && s.SeatIdentity == identity {
			return store.SeatMapSeat{}, fmt.Errorf("seat identity: %w", store.ErrSeatMapConflict)
		}
		if s.rowID == in.RowID && s.Position == in.Position {
			return store.SeatMapSeat{}, fmt.Errorf("seat position: %w", store.ErrSeatMapConflict)
		}
	}
	seat := store.SeatMapSeat{ID: uuid.New(), SeatIdentity: identity, Label: in.Label, Position: in.Position}
	f.seatSeats[seat.ID] = fakeSeat{SeatMapSeat: seat, seatMapID: in.SeatMapID, rowID: in.RowID}
	return seat, nil
}

func (f *fakeStore) GetSeatMapGeometry(_ context.Context, seatMapID uuid.UUID) (store.SeatMapGeometry, error) {
	m, ok := f.seatMaps[seatMapID]
	if !ok {
		return store.SeatMapGeometry{}, store.ErrNotFound
	}
	g := store.SeatMapGeometry{Map: m, Sections: []store.SeatMapSection{}}
	var secs []fakeSection
	for _, s := range f.seatSections {
		if s.seatMapID == seatMapID {
			secs = append(secs, s)
		}
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i].Position < secs[j].Position })
	for _, fs := range secs {
		sec := fs.SeatMapSection
		sec.Rows = []store.SeatMapRow{}
		var rws []fakeRow
		for _, r := range f.seatRows {
			if r.sectionID == fs.ID {
				rws = append(rws, r)
			}
		}
		sort.Slice(rws, func(i, j int) bool { return rws[i].Position < rws[j].Position })
		for _, fr := range rws {
			row := fr.SeatMapRow
			row.Seats = []store.SeatMapSeat{}
			var sts []fakeSeat
			for _, st := range f.seatSeats {
				if st.rowID == fr.ID {
					sts = append(sts, st)
				}
			}
			sort.Slice(sts, func(i, j int) bool { return sts[i].Position < sts[j].Position })
			for _, fst := range sts {
				row.Seats = append(row.Seats, fst.SeatMapSeat)
			}
			sec.Rows = append(sec.Rows, row)
		}
		g.Sections = append(g.Sections, sec)
	}
	return g, nil
}

func (f *fakeStore) ListVenueSeatMaps(_ context.Context, venueID uuid.UUID) ([]store.SeatMap, error) {
	out := make([]store.SeatMap, 0)
	for _, m := range f.seatMaps {
		if m.VenueID == venueID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (f *fakeStore) CreateFestival(_ context.Context, in store.FestivalInput) (store.Festival, error) {
	festival := store.Festival{
		ID: uuid.New(), OrganizerID: in.OrganizerID, Name: in.Name,
		SharedCapacity: in.SharedCapacity, Status: "draft", MemberIDs: []uuid.UUID{}, CreatedAt: time.Now().UTC(),
	}
	f.festivals[festival.ID] = festival
	return festival, nil
}

func (f *fakeStore) AttachDayToFestival(_ context.Context, festivalID, performanceID uuid.UUID) (store.Festival, error) {
	festival, ok := f.festivals[festivalID]
	if !ok {
		return store.Festival{}, store.ErrNotFound
	}
	performance, ok := f.performances[performanceID]
	if !ok {
		return store.Festival{}, store.ErrNotFound
	}
	if festival.OrganizerID != performance.OrganizerID {
		return store.Festival{}, store.ErrOrganizerMismatch
	}
	if performance.Kind != store.KindFestivalDay {
		return store.Festival{}, store.ErrSlotKindMismatch
	}
	if festival.Status != "draft" {
		return store.Festival{}, store.ErrFestivalNotDraft
	}
	if performance.Status != "draft" {
		return store.Festival{}, store.ErrMembershipFrozen
	}
	if performance.CapacityGroupID != nil {
		return store.Festival{}, store.ErrAlreadyGrouped
	}
	performance.CapacityGroupID = &festivalID
	f.performances[performanceID] = performance
	festival.MemberIDs = append(festival.MemberIDs, performanceID)
	sort.Slice(festival.MemberIDs, func(i, j int) bool { return festival.MemberIDs[i].String() < festival.MemberIDs[j].String() })
	f.festivals[festivalID] = festival
	return festival, nil
}

func (f *fakeStore) CreateSeries(_ context.Context, in store.SeriesInput) (store.Series, error) {
	ev, ok := f.events[in.EventID]
	if !ok {
		return store.Series{}, store.ErrNotFound
	}
	if ev.OrganizerID != in.OrganizerID {
		return store.Series{}, store.ErrOrganizerMismatch
	}
	s := store.Series{ID: uuid.New(), OrganizerID: in.OrganizerID, EventID: in.EventID, Name: in.Name, Members: []store.SeriesMember{}, CreatedAt: time.Now().UTC()}
	f.series[s.ID] = s
	return s, nil
}
func (f *fakeStore) AttachPerformanceToSeries(_ context.Context, seriesID, performanceID uuid.UUID, position int32) (store.Series, error) {
	s, ok := f.series[seriesID]
	if !ok {
		return store.Series{}, store.ErrNotFound
	}
	p, ok := f.performances[performanceID]
	if !ok {
		return store.Series{}, store.ErrNotFound
	}
	if p.OrganizerID != s.OrganizerID || p.EventID != s.EventID {
		return store.Series{}, store.ErrOrganizerMismatch
	}
	if p.Status != "draft" {
		return store.Series{}, store.ErrMembershipFrozen
	}
	for _, member := range s.Members {
		if f.performances[member.PerformanceID].Status != "draft" {
			return store.Series{}, store.ErrMembershipFrozen
		}
	}
	for _, other := range f.series {
		for _, m := range other.Members {
			if m.PerformanceID == performanceID || other.ID == seriesID && m.Position == position {
				return store.Series{}, store.ErrMembershipConflict
			}
		}
	}
	s.Members = append(s.Members, store.SeriesMember{PerformanceID: performanceID, Position: position})
	sort.Slice(s.Members, func(i, j int) bool { return s.Members[i].Position < s.Members[j].Position })
	f.series[s.ID] = s
	return s, nil
}
func (f *fakeStore) CreateSeason(_ context.Context, in store.SeasonInput) (store.Season, error) {
	s := store.Season{ID: uuid.New(), OrganizerID: in.OrganizerID, Name: in.Name, SeriesIDs: []uuid.UUID{}, EventIDs: []uuid.UUID{}, CreatedAt: time.Now().UTC()}
	f.seasons[s.ID] = s
	return s, nil
}
func (f *fakeStore) AttachSeriesToSeason(_ context.Context, seasonID, seriesID uuid.UUID) (store.Season, error) {
	s, ok := f.seasons[seasonID]
	if !ok {
		return store.Season{}, store.ErrNotFound
	}
	series, ok := f.series[seriesID]
	if !ok {
		return store.Season{}, store.ErrNotFound
	}
	if s.OrganizerID != series.OrganizerID {
		return store.Season{}, store.ErrOrganizerMismatch
	}
	for _, id := range s.SeriesIDs {
		if id == seriesID {
			return store.Season{}, store.ErrMembershipConflict
		}
	}
	s.SeriesIDs = append(s.SeriesIDs, seriesID)
	f.seasons[s.ID] = s
	return s, nil
}
func (f *fakeStore) AttachEventToSeason(_ context.Context, seasonID, eventID uuid.UUID) (store.Season, error) {
	s, ok := f.seasons[seasonID]
	if !ok {
		return store.Season{}, store.ErrNotFound
	}
	ev, ok := f.events[eventID]
	if !ok {
		return store.Season{}, store.ErrNotFound
	}
	if s.OrganizerID != ev.OrganizerID {
		return store.Season{}, store.ErrOrganizerMismatch
	}
	for _, id := range s.EventIDs {
		if id == eventID {
			return store.Season{}, store.ErrMembershipConflict
		}
	}
	s.EventIDs = append(s.EventIDs, eventID)
	f.seasons[s.ID] = s
	return s, nil
}

func (f *fakeStore) CreateVenue(_ context.Context, in store.VenueInput) (store.Venue, error) {
	v := store.Venue{ID: uuid.New(), OrganizerID: in.OrganizerID, Name: in.Name,
		GACapacity: in.GACapacity, CreatedAt: time.Now().UTC()}
	f.venues[v.ID] = v
	return v, nil
}

func (f *fakeStore) ListVenues(_ context.Context, organizerID uuid.UUID) ([]store.Venue, error) {
	out := make([]store.Venue, 0)
	for _, v := range f.venues {
		if v.OrganizerID == organizerID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeStore) CreateEvent(_ context.Context, in store.EventInput) (store.Event, error) {
	e := store.Event{ID: uuid.New(), OrganizerID: in.OrganizerID, Name: in.Name,
		Description: in.Description, CreatedAt: time.Now().UTC()}
	f.events[e.ID] = e
	return e, nil
}

func (f *fakeStore) CreatePerformance(_ context.Context, in store.PerformanceInput) (store.Performance, error) {
	ev, ok := f.events[in.EventID]
	if !ok {
		return store.Performance{}, fmt.Errorf("event: %w", store.ErrNotFound)
	}
	v, ok := f.venues[in.VenueID]
	if !ok {
		return store.Performance{}, fmt.Errorf("venue: %w", store.ErrNotFound)
	}
	if ev.OrganizerID != in.OrganizerID || v.OrganizerID != in.OrganizerID {
		return store.Performance{}, store.ErrOrganizerMismatch
	}
	kind := in.Kind
	if kind == "" {
		kind = store.KindPerformance
	}
	// Seated validation mirrors the real store (TKT-103): the map must exist,
	// be published, and share the slot's organizer AND venue; a festival day
	// cannot be seated.
	if in.SeatMapID != nil {
		if kind == store.KindFestivalDay {
			return store.Performance{}, fmt.Errorf("festival day cannot be seated: %w", store.ErrIllegalTransition)
		}
		m, ok := f.seatMaps[*in.SeatMapID]
		if !ok {
			return store.Performance{}, fmt.Errorf("seat map: %w", store.ErrNotFound)
		}
		if m.OrganizerID != in.OrganizerID || m.VenueID != in.VenueID {
			return store.Performance{}, store.ErrOrganizerMismatch
		}
		if m.Status != "published" {
			return store.Performance{}, store.ErrSeatMapNotPublished
		}
	}
	re := in.ReEntry
	if re.Mode == "" {
		re.Mode = "single"
	}
	p := store.Performance{ID: uuid.New(), OrganizerID: in.OrganizerID, EventID: in.EventID,
		VenueID: in.VenueID, Kind: kind, StartsAt: in.StartsAt, OperatingDate: in.OperatingDate,
		OpensAt: in.OpensAt, ClosesAt: in.ClosesAt, Timezone: in.Timezone, ReEntry: re,
		Closure:   store.Closure{Status: "open"},
		SeatMapID: in.SeatMapID,
		Status:    "draft", Capacity: v.GACapacity, CreatedAt: time.Now().UTC()}
	f.performances[p.ID] = p
	return p, nil
}

func (f *fakeStore) CreateTicketType(_ context.Context, in store.TicketTypeInput) (store.TicketType, error) {
	p, ok := f.performances[in.PerformanceID]
	if !ok {
		return store.TicketType{}, fmt.Errorf("performance: %w", store.ErrNotFound)
	}
	if p.OrganizerID != in.OrganizerID {
		return store.TicketType{}, store.ErrOrganizerMismatch
	}
	tt := store.TicketType{ID: uuid.New(), OrganizerID: in.OrganizerID,
		PerformanceID: in.PerformanceID, Name: in.Name,
		PriceAmount: in.PriceAmount, Currency: in.Currency, CreatedAt: time.Now().UTC()}
	f.ticketTypes[tt.ID] = tt
	return tt, nil
}

func (f *fakeStore) GetTicketType(_ context.Context, id uuid.UUID) (store.TicketType, error) {
	tt, ok := f.ticketTypes[id]
	if !ok {
		return store.TicketType{}, store.ErrNotFound
	}
	return tt, nil
}

func (f *fakeStore) GetPublishedPerformance(_ context.Context, id uuid.UUID) (store.Performance, error) {
	p, ok := f.performances[id]
	if !ok || p.Status != "published" {
		return store.Performance{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) GetPoolOfferState(_ context.Context, id uuid.UUID) (store.PoolOfferState, error) {
	if p, ok := f.performances[id]; ok {
		return store.PoolOfferState{Kind: "performance", Lifecycle: p.Status, Closure: p.Closure}, nil
	}
	if _, ok := f.festivals[id]; ok {
		return store.PoolOfferState{Kind: "festival"}, nil
	}
	return store.PoolOfferState{}, store.ErrNotFound
}

func (f *fakeStore) PublishPerformance(_ context.Context, id uuid.UUID) (store.Performance, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, store.ErrNotFound
	}
	if p.CapacityGroupID != nil {
		return store.Performance{}, false, store.ErrGroupedSlotLifecycle
	}
	if p.Status == "draft" && !f.hasTicketType(id) {
		return store.Performance{}, false, store.ErrNotSellable
	}
	if p.Status == "archived" {
		return store.Performance{}, false, store.ErrIllegalTransition
	}
	if p.Status == "draft" {
		now := time.Now().UTC()
		p.Status = "published"
		p.PublishedAt = &now
		f.performances[id] = p
	}
	return p, !f.emitted[id], nil
}

func (f *fakeStore) ArchivePerformance(_ context.Context, id uuid.UUID) (store.Performance, bool, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, false, store.ErrNotFound
	}
	if p.CapacityGroupID != nil {
		return store.Performance{}, false, false, store.ErrGroupedSlotLifecycle
	}
	if p.Status == "draft" {
		return store.Performance{}, false, false, store.ErrIllegalTransition
	}
	if p.Status == "published" {
		if f.closureEmitted[id] < p.Closure.Version {
			return store.Performance{}, false, false, store.ErrClosurePending
		}
		now := time.Now().UTC()
		p.Status = "archived"
		p.ArchivedAt = &now
		f.performances[id] = p
	}
	return p, !f.emitted[id], !f.archiveEmitted[id], nil
}

func (f *fakeStore) hasTicketType(performanceID uuid.UUID) bool {
	for _, tt := range f.ticketTypes {
		if tt.PerformanceID == performanceID {
			return true
		}
	}
	return false
}

func (f *fakeStore) MarkPerformanceEventEmitted(_ context.Context, id uuid.UUID) error {
	f.emitted[id] = true
	return nil
}

func (f *fakeStore) MarkPerformanceArchiveEmitted(_ context.Context, id uuid.UUID) error {
	f.archiveEmitted[id] = true
	return nil
}

func (f *fakeStore) CloseSlot(_ context.Context, id uuid.UUID, reason *string) (store.Performance, bool, bool, error) {
	return f.toggleClosure(id, "closed", reason)
}

func (f *fakeStore) ReopenSlot(_ context.Context, id uuid.UUID) (store.Performance, bool, bool, error) {
	return f.toggleClosure(id, "open", nil)
}

func (f *fakeStore) toggleClosure(id uuid.UUID, target string, reason *string) (store.Performance, bool, bool, error) {
	p, ok := f.performances[id]
	if !ok {
		return store.Performance{}, false, false, store.ErrNotFound
	}
	if p.Status != "published" {
		return store.Performance{}, false, false, store.ErrIllegalTransition
	}
	if p.Closure.Status == target {
		return p, !f.emitted[id], f.closureEmitted[id] < p.Closure.Version, nil
	}
	if f.closureEmitted[id] < p.Closure.Version {
		return store.Performance{}, false, false, store.ErrClosurePending
	}
	now := time.Now().UTC()
	p.Closure.Version++
	p.Closure.ChangedAt = &now
	if target == "closed" {
		p.Closure.Status = "closed"
		p.Closure.ClosedAt = &now
		p.Closure.Reason = reason
	} else {
		p.Closure.Status = "open"
		p.Closure.ClosedAt = nil
		p.Closure.Reason = nil
	}
	f.performances[id] = p
	return p, !f.emitted[id], true, nil
}

func (f *fakeStore) MarkClosureEmitted(_ context.Context, id uuid.UUID, version int32) error {
	if version > f.closureEmitted[id] {
		f.closureEmitted[id] = version
	}
	return nil
}

func (f *fakeStore) PublishSeries(ctx context.Context, id uuid.UUID) ([]store.SeriesTransition, error) {
	return f.transitionSeries(ctx, id, "published")
}
func (f *fakeStore) ArchiveSeries(ctx context.Context, id uuid.UUID) ([]store.SeriesTransition, error) {
	return f.transitionSeries(ctx, id, "archived")
}
func (f *fakeStore) transitionSeries(_ context.Context, id uuid.UUID, target string) ([]store.SeriesTransition, error) {
	s, ok := f.series[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if len(s.Members) == 0 {
		return nil, store.ErrEmptySeries
	}
	for _, m := range s.Members {
		p := f.performances[m.PerformanceID]
		if target == "published" && p.Status == "archived" {
			return nil, &store.SeriesTransitionConflict{PerformanceID: p.ID, Reason: "archived member cannot be published", Cause: store.ErrIllegalTransition}
		}
		if target == "published" && !f.hasTicketType(p.ID) {
			return nil, &store.SeriesTransitionConflict{PerformanceID: p.ID, Reason: "member has no ticket type", Cause: store.ErrNotSellable}
		}
		if target == "archived" && p.Status == "draft" {
			return nil, &store.SeriesTransitionConflict{PerformanceID: p.ID, Reason: "draft member cannot be archived", Cause: store.ErrIllegalTransition}
		}
		if target == "archived" && f.closureEmitted[p.ID] < p.Closure.Version {
			return nil, &store.SeriesTransitionConflict{PerformanceID: p.ID, Reason: "member has an owed closure event", Cause: store.ErrClosurePending}
		}
	}
	out := []store.SeriesTransition{}
	for _, m := range s.Members {
		p := f.performances[m.PerformanceID]
		if target == "published" && p.Status == "draft" {
			now := time.Now().UTC()
			p.Status = "published"
			p.PublishedAt = &now
		}
		if target == "archived" && p.Status == "published" {
			now := time.Now().UTC()
			p.Status = "archived"
			p.ArchivedAt = &now
		}
		f.performances[p.ID] = p
		out = append(out, store.SeriesTransition{Performance: p, PublishNeedsEmit: !f.emitted[p.ID], ArchiveNeedsEmit: target == "archived" && !f.archiveEmitted[p.ID]})
	}
	return out, nil
}

func (f *fakeStore) PublishFestival(ctx context.Context, id uuid.UUID) ([]store.SeriesTransition, error) {
	return f.transitionFestival(ctx, id, "published")
}

func (f *fakeStore) ArchiveFestival(ctx context.Context, id uuid.UUID) ([]store.SeriesTransition, error) {
	return f.transitionFestival(ctx, id, "archived")
}

func (f *fakeStore) transitionFestival(_ context.Context, id uuid.UUID, target string) ([]store.SeriesTransition, error) {
	festival, ok := f.festivals[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if len(festival.MemberIDs) == 0 {
		return nil, store.ErrEmptyFestival
	}
	for _, memberID := range festival.MemberIDs {
		p := f.performances[memberID]
		if target == "published" && p.Status == "archived" {
			return nil, &store.FestivalTransitionConflict{PerformanceID: p.ID, Reason: "archived member cannot be published", Cause: store.ErrIllegalTransition}
		}
		if target == "published" && p.Status == "draft" && !f.hasTicketType(p.ID) {
			return nil, &store.FestivalTransitionConflict{PerformanceID: p.ID, Reason: "member has no ticket type", Cause: store.ErrNotSellable}
		}
		if target == "archived" && p.Status == "draft" {
			return nil, &store.FestivalTransitionConflict{PerformanceID: p.ID, Reason: "draft member cannot be archived", Cause: store.ErrIllegalTransition}
		}
		if target == "archived" && p.Status == "published" && f.closureEmitted[p.ID] < p.Closure.Version {
			return nil, &store.FestivalTransitionConflict{PerformanceID: p.ID, Reason: "member has an owed closure event", Cause: store.ErrClosurePending}
		}
	}
	out := make([]store.SeriesTransition, 0, len(festival.MemberIDs))
	for _, memberID := range festival.MemberIDs {
		p := f.performances[memberID]
		if target == "published" && p.Status == "draft" {
			now := time.Now().UTC()
			p.Status, p.PublishedAt = "published", &now
		}
		if target == "archived" && p.Status == "published" {
			now := time.Now().UTC()
			p.Status, p.ArchivedAt = "archived", &now
		}
		capacity := festival.SharedCapacity
		p.SharedCapacity = &capacity
		f.performances[p.ID] = p
		out = append(out, store.SeriesTransition{
			Performance: p, PublishNeedsEmit: !f.emitted[p.ID],
			ArchiveNeedsEmit: target == "archived" && !f.archiveEmitted[p.ID],
		})
	}
	festival.Status = target
	f.festivals[id] = festival
	return out, nil
}

func (f *fakeStore) aggregates() []store.EventAggregate {
	var aggs []store.EventAggregate
	for _, ev := range f.events {
		agg := store.EventAggregate{Event: ev}
		for _, p := range f.performances {
			if p.EventID != ev.ID || p.Status != "published" {
				continue
			}
			pa := store.PerformanceAggregate{Performance: p, Venue: f.venues[p.VenueID]}
			for _, tt := range f.ticketTypes {
				if tt.PerformanceID == p.ID {
					pa.TicketTypes = append(pa.TicketTypes, tt)
				}
			}
			if len(pa.TicketTypes) > 0 { // no sellable offer, no listing
				agg.Performances = append(agg.Performances, pa)
			}
		}
		if len(agg.Performances) > 0 {
			for _, s := range f.series {
				if s.EventID != ev.ID {
					continue
				}
				sa := store.SeriesAggregate{Series: s}
				for _, m := range s.Members {
					if p := f.performances[m.PerformanceID]; p.Status == "published" {
						sa.PerformanceIDs = append(sa.PerformanceIDs, m.PerformanceID)
					}
				}
				if len(sa.PerformanceIDs) > 0 {
					agg.Series = append(agg.Series, sa)
				}
			}
			aggs = append(aggs, agg)
		}
	}
	return aggs
}

func (f *fakeStore) ListPublishedEvents(_ context.Context) ([]store.EventAggregate, error) {
	return f.aggregates(), nil
}

func (f *fakeStore) GetPublishedEvent(_ context.Context, id uuid.UUID) (store.EventAggregate, error) {
	for _, agg := range f.aggregates() {
		if agg.Event.ID == id {
			return agg, nil
		}
	}
	return store.EventAggregate{}, store.ErrNotFound
}

func (f *fakeStore) GetPublishedSeason(_ context.Context, id uuid.UUID) (store.SeasonAggregate, error) {
	season, ok := f.seasons[id]
	if !ok {
		return store.SeasonAggregate{}, store.ErrNotFound
	}
	ids := map[uuid.UUID]bool{}
	for _, eventID := range season.EventIDs {
		ids[eventID] = true
	}
	for _, seriesID := range season.SeriesIDs {
		ids[f.series[seriesID].EventID] = true
	}
	out := store.SeasonAggregate{Season: season, Events: []store.EventAggregate{}}
	for _, agg := range f.aggregates() {
		if ids[agg.Event.ID] {
			out.Events = append(out.Events, agg)
		}
	}
	if len(out.Events) == 0 {
		return store.SeasonAggregate{}, store.ErrNotFound
	}
	return out, nil
}

func (f *fakeStore) GetPublishedFestival(_ context.Context, id uuid.UUID) (store.FestivalAggregate, error) {
	festival, ok := f.festivals[id]
	if !ok || festival.Status != "published" {
		return store.FestivalAggregate{}, store.ErrNotFound
	}
	out := store.FestivalAggregate{Festival: festival, Performances: []store.PerformanceAggregate{}}
	for _, memberID := range festival.MemberIDs {
		p := f.performances[memberID]
		if p.Status != "published" || p.Kind != store.KindFestivalDay {
			continue
		}
		pa := store.PerformanceAggregate{Performance: p, Venue: f.venues[p.VenueID]}
		for _, tt := range f.ticketTypes {
			if tt.PerformanceID == p.ID {
				pa.TicketTypes = append(pa.TicketTypes, tt)
			}
		}
		if len(pa.TicketTypes) > 0 {
			out.Performances = append(out.Performances, pa)
		}
	}
	if len(out.Performances) == 0 {
		return store.FestivalAggregate{}, store.ErrNotFound
	}
	return out, nil
}

type fakePublisher struct {
	published        []store.Performance
	backfilled       []store.Performance
	archived         []store.Performance
	closed           []store.Performance
	reopened         []store.Performance
	seatMapsPub      []store.SeatMap
	calls            []string // ordered emission log: "published"|"backfilled"|"archived"|"closed"|"reopened"|"seat_map_published"
	failNext         bool
	failBackfillNext bool
	failArchiveNext  bool
	failClosureNext  bool
	failSeatMapNext  bool
}

func (f *fakePublisher) SeatMapPublished(_ context.Context, m store.SeatMap) error {
	if f.failSeatMapNext {
		f.failSeatMapNext = false
		return errors.New("nats down")
	}
	f.seatMapsPub = append(f.seatMapsPub, m)
	f.calls = append(f.calls, "seat_map_published")
	return nil
}

func (f *fakePublisher) SlotClosed(_ context.Context, p store.Performance) error {
	if f.failClosureNext {
		f.failClosureNext = false
		return errors.New("nats down")
	}
	f.closed = append(f.closed, p)
	f.calls = append(f.calls, "closed")
	return nil
}

func (f *fakePublisher) SlotReopened(_ context.Context, p store.Performance) error {
	if f.failClosureNext {
		f.failClosureNext = false
		return errors.New("nats down")
	}
	f.reopened = append(f.reopened, p)
	f.calls = append(f.calls, "reopened")
	return nil
}

func (f *fakePublisher) PerformanceArchived(_ context.Context, p store.Performance) error {
	if f.failArchiveNext {
		f.failArchiveNext = false
		return errors.New("nats down")
	}
	f.archived = append(f.archived, p)
	f.calls = append(f.calls, "archived")
	return nil
}

func (f *fakePublisher) PerformancePublished(_ context.Context, p store.Performance) error {
	if f.failNext {
		f.failNext = false
		return errors.New("nats down")
	}
	f.published = append(f.published, p)
	f.calls = append(f.calls, "published")
	return nil
}

func (f *fakePublisher) PerformancePublishedBackfill(_ context.Context, p store.Performance) error {
	if f.failBackfillNext {
		f.failBackfillNext = false
		return errors.New("nats down")
	}
	f.backfilled = append(f.backfilled, p)
	f.calls = append(f.calls, "backfilled")
	return nil
}

type env struct {
	store   *fakeStore
	pub     *fakePublisher
	handler http.Handler
	router  routers.Router // spec router for response validation
	t       *testing.T
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := newFakeStore()
	pub := &fakePublisher{}
	h, err := NewRouter(NewServer(st, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-internal-token"))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("spec router: %v", err)
	}
	return &env{store: st, pub: pub, handler: h, router: router, t: t}
}

func TestInternalTicketTypeRequiresCredential(t *testing.T) {
	st := newFakeStore()
	pub := &fakePublisher{}
	h, err := NewRouter(NewServer(st, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), "secret"))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	for _, tt := range []struct {
		name, token string
		want        int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", token: "wrong", want: http.StatusUnauthorized},
		{name: "valid", token: "secret", want: http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/ticket-types/"+id.String(), nil)
			req.Header.Set("X-Internal-Token", tt.token)
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d want=%d", res.Code, tt.want)
			}
		})
	}
}

// TestInternalRoutesRefuseUncredentialedCalls covers every route mounted in the
// /internal group with one assertion: no credential, no service. The group
// middleware is what makes this true, so removing it fails all five subtests at
// once rather than only the routes someone remembered to test individually.
// A new /internal route belongs in this list.
func TestInternalRoutesRefuseUncredentialedCalls(t *testing.T) {
	e := newEnv(t)
	id := uuid.New().String()
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/internal/ticket-types/" + id},
		{http.MethodGet, "/internal/performances/" + id},
		{http.MethodGet, "/internal/pools/" + id + "/offer-state"},
		{http.MethodPost, "/internal/seat-maps/" + id + "/pins"},
		{http.MethodPost, "/internal/seat-maps/" + id + "/unpins"},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{}`))
			res := httptest.NewRecorder()
			e.handler.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401 — internal route is not guarded: %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestInternalPublishedPerformanceLookup(t *testing.T) {
	e := newEnv(t)
	_, performanceID := e.createFixture(true)
	for _, tt := range []struct {
		name, token string
		want        int
	}{
		{name: "missing credential", want: http.StatusUnauthorized},
		{name: "valid credential", token: "test-internal-token", want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/performances/"+performanceID.String(), nil)
			req.Header.Set("X-Internal-Token", tt.token)
			res := httptest.NewRecorder()
			e.handler.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d want=%d", res.Code, tt.want)
			}
			if tt.want == http.StatusOK && !bytes.Contains(res.Body.Bytes(), []byte(`"capacity":500`)) {
				t.Fatalf("lookup response %s", res.Body.String())
			}
		})
	}
}

// TestInternalPoolOfferState covers the reconciliation read (TKT-90): a per-id
// answer for whatever the pool id is — a performance in ANY lifecycle (archived
// must be 200, not 404 — reconciliation acts only on positive assertions), a
// festival capacity group, or unknown.
func TestInternalPoolOfferState(t *testing.T) {
	e := newEnv(t)
	_, performanceID := e.createFixture(true)
	closed := e.store.performances[performanceID]
	closed.Closure = store.Closure{Status: "closed", Version: 2}
	e.store.performances[performanceID] = closed

	archivedID := uuid.New()
	e.store.performances[archivedID] = store.Performance{ID: archivedID, OrganizerID: orgID, Status: "archived"}

	festivalID := uuid.New()
	e.store.festivals[festivalID] = store.Festival{ID: festivalID, OrganizerID: orgID}

	for _, tt := range []struct {
		name, id, token string
		want            int
		wantBody        []string
	}{
		{name: "missing credential", id: performanceID.String(), want: http.StatusUnauthorized},
		{name: "closed performance", id: performanceID.String(), token: "test-internal-token", want: http.StatusOK,
			wantBody: []string{`"kind":"performance"`, `"lifecycle":"published"`, `"closure_status":"closed"`, `"closure_version":2`}},
		{name: "archived performance is a positive answer", id: archivedID.String(), token: "test-internal-token", want: http.StatusOK,
			wantBody: []string{`"kind":"performance"`, `"lifecycle":"archived"`}},
		{name: "festival capacity group", id: festivalID.String(), token: "test-internal-token", want: http.StatusOK,
			wantBody: []string{`"kind":"festival"`}},
		{name: "unknown id", id: uuid.New().String(), token: "test-internal-token", want: http.StatusNotFound},
		{name: "invalid id", id: "not-a-uuid", token: "test-internal-token", want: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/pools/"+tt.id+"/offer-state", nil)
			req.Header.Set("X-Internal-Token", tt.token)
			res := httptest.NewRecorder()
			e.handler.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d want=%d body=%s", res.Code, tt.want, res.Body.String())
			}
			for _, frag := range tt.wantBody {
				if !bytes.Contains(res.Body.Bytes(), []byte(frag)) {
					t.Fatalf("body %s missing %s", res.Body.String(), frag)
				}
			}
		})
	}
}

// do performs a request and validates the response against the spec
// (ADR-009 §3: conformance is tested in both directions).
func (e *env) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "http://catalog.local"+path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	e.validateResponse(req, rec)
	return rec
}

func (e *env) validateResponse(req *http.Request, rec *httptest.ResponseRecorder) {
	e.t.Helper()
	if req.URL.Path == "/openapi.yaml" {
		recordCoverage("getOpenAPISpec", rec.Code)
		return // the YAML document is asserted byte-identical, not schema-validated
	}
	route, pathParams, err := e.router.FindRoute(req)
	if err != nil {
		return // route not in spec (spec middleware already rejected it)
	}
	recordCoverage(route.Operation.OperationID, rec.Code)
	// The production ResponseValidator wraps every routed handler (NewRouter),
	// so an undocumented status is laundered into a documented generic 500
	// before this helper ever sees it. Detect the mask itself: a handler
	// response rewritten by ADR-028 fail-closed is always a test failure here.
	if strings.Contains(rec.Body.String(), "response violates OpenAPI contract") {
		e.t.Fatalf("production validator masked the response for %s %s (status %d) — handler drifted from the spec", req.Method, req.URL.Path, rec.Code)
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: pathParams, Route: route,
		},
		Status: rec.Code,
		Header: rec.Header(),
		Body:   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		// Mirror production (shared/go/contract): an undocumented status is
		// drift too — without this, tests pass on statuses production rejects.
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		e.t.Fatalf("response for %s %s violates the contract: %v", req.Method, req.URL.Path, err)
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %T: %v (body: %s)", out, err, rec.Body.String())
	}
	return out
}

var orgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func (e *env) createFixture(publish bool) (eventID, performanceID uuid.UUID) {
	e.t.Helper()
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Le Zénith", GaCapacity: 500}))
	desc := LocalizedString{"fr": "Une soirée électro.", "en": "An electro night."}
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID,
		Name:        LocalizedString{"fr": "Nuit Électrique", "en": "Electric Night"},
		Description: &desc,
	}))
	startsAt := time.Date(2026, 9, 18, 19, 30, 0, 0, time.UTC)
	perf := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: &startsAt, Timezone: "Europe/Paris",
	}))
	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: perf.Id,
		Name:  LocalizedString{"fr": "Admission générale", "en": "General admission"},
		Price: Money{Amount: 4550, Currency: "EUR"},
	})
	if publish {
		rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
		}
	}
	return event.Id, perf.Id
}

func TestCreateVenue(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/venues", VenueCreate{OrganizerId: orgID, Name: "Halle A", GaCapacity: 1200})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("write endpoints must be no-store, got %q", cc)
	}
}

func TestCreateVenueRejectsInvalidBody(t *testing.T) {
	e := newEnv(t)
	// ga_capacity below minimum: rejected by the spec middleware, not handler code.
	rec := e.do("POST", "/venues", VenueCreate{OrganizerId: orgID, Name: "Halle A", GaCapacity: 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEventRequiresAllSupportedLocales(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Sans anglais"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decode[Error](t, rec).Error; !strings.Contains(msg, `"en"`) {
		t.Fatalf("error should name the missing locale: %q", msg)
	}
}

func TestCreatePerformanceValidations(t *testing.T) {
	e := newEnv(t)
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Halle A", GaCapacity: 100}))
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "F", "en": "E"},
	}))

	startsAt := time.Now().UTC()
	base := PerformanceCreate{OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: &startsAt, Timezone: "Europe/Paris"}

	unknownEvent := base
	unknownEvent.EventId = uuid.New()
	if rec := e.do("POST", "/performances", unknownEvent); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown event: status %d", rec.Code)
	}

	badTZ := base
	badTZ.Timezone = "Mars/Olympus"
	if rec := e.do("POST", "/performances", badTZ); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad timezone: status %d", rec.Code)
	}

	crossOrg := base
	crossOrg.OrganizerId = uuid.New()
	if rec := e.do("POST", "/performances", crossOrg); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-organizer: status %d", rec.Code)
	}

	if rec := e.do("POST", "/performances", base); rec.Code != http.StatusCreated {
		t.Fatalf("valid: status %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublishEmitsExactlyOnceOnTransition(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	if len(e.pub.published) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(e.pub.published))
	}
	// Idempotent re-publish: 200, no second emission.
	rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-publish: %d", rec.Code)
	}
	if len(e.pub.published) != 1 {
		t.Fatalf("re-publish must not re-emit, got %d emissions", len(e.pub.published))
	}
	if got := e.pub.published[0].ID; got != perfID {
		t.Fatalf("emitted performance %s, want %s", got, perfID)
	}
}

func TestPublishRetriesEmissionAfterFailure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false)
	e.pub.failNext = true
	rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed emission should 500, got %d", rec.Code)
	}
	// The recovery hint must survive the production ResponseValidator (TKT-108).
	if !strings.Contains(rec.Body.String(), "performance is published but the domain event was not emitted; retry publish") {
		t.Fatalf("recovery body lost, got: %s", rec.Body.String())
	}
	// The performance is published but the event is still owed: retry emits.
	rec = e.do("POST", "/performances/"+perfID.String()+"/publish", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: %d", rec.Code)
	}
	if len(e.pub.published) != 1 {
		t.Fatalf("retry should emit exactly once, got %d", len(e.pub.published))
	}
}

func TestPublishUnknownPerformance(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/performances/"+uuid.NewString()+"/publish", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestArchiveEmitsOnceAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[Performance](t, rec)
	if p.Status != PerformanceStatusArchived || p.ArchivedAt == nil {
		t.Fatalf("archived response = %+v", p)
	}
	if len(e.pub.archived) != 1 {
		t.Fatalf("expected 1 archive emission, got %d", len(e.pub.archived))
	}
	if rec = e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("re-archive: %d", rec.Code)
	}
	if len(e.pub.archived) != 1 {
		t.Fatalf("re-archive must not re-emit, got %d", len(e.pub.archived))
	}
}

func TestArchiveRejectsDraftUnknownAndRepublish(t *testing.T) {
	e := newEnv(t)
	_, draftID := e.createFixture(false)
	if rec := e.do("POST", "/performances/"+draftID.String()+"/archive", nil); rec.Code != http.StatusConflict {
		t.Fatalf("archive draft: want 409, got %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+uuid.NewString()+"/archive", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("archive unknown: want 404, got %d", rec.Code)
	}
	_, publishedID := e.createFixture(true)
	if rec := e.do("POST", "/performances/"+publishedID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive published: %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+publishedID.String()+"/publish", nil); rec.Code != http.StatusConflict {
		t.Fatalf("republish archived: want 409, got %d", rec.Code)
	}
}

func TestArchiveRetriesEmissionAfterFailure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	e.pub.failArchiveNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed archive emission: want 500, got %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "performance is archived but the archive event was not emitted; retry archive") {
		t.Fatalf("recovery body lost, got: %s", rec.Body.String())
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive retry: %d", rec.Code)
	}
	if len(e.pub.archived) != 1 {
		t.Fatalf("retry should emit archive once, got %d", len(e.pub.archived))
	}
}

func TestArchiveEmitsOwedPublishBeforeArchive(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false)
	e.pub.failNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed publish emission: want 500, got %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.published) != 1 || len(e.pub.archived) != 1 {
		t.Fatalf("owed events: published=%d archived=%d", len(e.pub.published), len(e.pub.archived))
	}
	// The owed publication must be emitted BEFORE the archive event — asserting
	// on the ordered call log, not just slice lengths, so a reversal is caught.
	if want := []string{"published", "archived"}; !slices.Equal(e.pub.calls, want) {
		t.Fatalf("emission order = %v, want %v", e.pub.calls, want)
	}
}

// TestArchiveRetryReplaysOwedPublishBeforeArchive covers the interleaving where
// the owed publication emits but the archive emission then fails: the retry
// must replay the still-owed publication (same deterministic id) again before
// the archive, never emitting the archive first.
func TestArchiveRetryReplaysOwedPublishBeforeArchive(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false)
	// Publish with a failed emission so the publication stays owed.
	e.pub.failNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed publish emission: want 500, got %d", rec.Code)
	}
	// First archive: the owed publication emits, then the archive emission fails.
	e.pub.failArchiveNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed archive emission: want 500, got %d", rec.Code)
	}
	// Retry archive: the archive emission failed before the publish marker was
	// written, so the still-owed publication is replayed (safe: its deterministic
	// id de-duplicates at the stream) and only then is the archive emitted. The
	// contract is at-least-once emission, NOT invocation-exactly-once — so the
	// invariant is ordering, not call count: no archive event is ever emitted
	// before its owed publication.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive retry: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.archived) < 1 {
		t.Fatalf("archive event never emitted: calls=%v", e.pub.calls)
	}
	// The final emission is the archive; every archive in the log is preceded by
	// at least one publication (publication is always emitted first).
	if got := e.pub.calls[len(e.pub.calls)-1]; got != "archived" {
		t.Fatalf("last emission = %q, want archived; calls=%v", got, e.pub.calls)
	}
	seenPublished := false
	for _, c := range e.pub.calls {
		switch c {
		case "published":
			seenPublished = true
		case "archived":
			if !seenPublished {
				t.Fatalf("archive emitted before any publication: calls=%v", e.pub.calls)
			}
		}
	}
}

func TestArchivedPerformanceExcludedFromPublicReads(t *testing.T) {
	e := newEnv(t)
	eventID, perfID := e.createFixture(true)
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive: %d", rec.Code)
	}
	list := decode[PublicEventList](t, e.do("GET", "/public/events?locale=en", nil))
	if len(list.Events) != 0 {
		t.Fatalf("archived performance remains listed: %+v", list.Events)
	}
	if rec := e.do("GET", "/public/events/"+eventID.String()+"?locale=en", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("all-archived detail: want 404, got %d", rec.Code)
	}
}

func TestPublicListIsLocalizedAndCacheTiered(t *testing.T) {
	e := newEnv(t)
	e.createFixture(true)

	for locale, wantName := range map[string]string{"fr": "Nuit Électrique", "en": "Electric Night"} {
		rec := e.do("GET", "/public/events?locale="+locale, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("[%s] status %d: %s", locale, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != CacheControlPublicReads {
			t.Fatalf("[%s] public reads carry the minutes tier, got %q", locale, cc)
		}
		list := decode[PublicEventList](t, rec)
		if len(list.Events) != 1 {
			t.Fatalf("[%s] want 1 event, got %d", locale, len(list.Events))
		}
		ev := list.Events[0]
		if ev.Name != wantName {
			t.Fatalf("[%s] name %q, want %q", locale, ev.Name, wantName)
		}
		if len(ev.Performances) != 1 {
			t.Fatalf("[%s] want 1 performance, got %d", locale, len(ev.Performances))
		}
		p := ev.Performances[0]
		if p.FromPrice.Amount != 4550 || p.FromPrice.Currency != "EUR" {
			t.Fatalf("[%s] from_price %+v", locale, p.FromPrice)
		}
		if p.VenueName != "Le Zénith" {
			t.Fatalf("[%s] venue %q", locale, p.VenueName)
		}
	}
}

func TestPublicListExcludesDraftsAndPublishRequiresPrice(t *testing.T) {
	e := newEnv(t)
	e.createFixture(false) // draft: not listed

	// Publishing without a ticket type is refused (409): the publication
	// event and public visibility must never disagree.
	venue := decode[Venue](e.t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Halle B", GaCapacity: 50}))
	event := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Brouillon", "en": "Draft"},
	}))
	startsAt := time.Now().UTC()
	perf := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: event.Id, VenueId: venue.Id,
		StartsAt: &startsAt, Timezone: "Europe/Paris",
	}))
	if rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil); rec.Code != http.StatusConflict {
		t.Fatalf("unpriced publish: want 409, got %d", rec.Code)
	}
	if len(e.pub.published) != 0 {
		t.Fatalf("refused publish must not emit, got %d emissions", len(e.pub.published))
	}

	list := decode[PublicEventList](t, e.do("GET", "/public/events?locale=en", nil))
	if len(list.Events) != 0 {
		t.Fatalf("draft performances must not be listed, got %d events", len(list.Events))
	}
}

func TestPublicListRejectsUnsupportedLocale(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", "/public/events?locale=xx", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	// Missing locale: rejected by the spec middleware (required param).
	if rec := e.do("GET", "/public/events", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing locale: status %d", rec.Code)
	}
}

func TestPublicDetail(t *testing.T) {
	e := newEnv(t)
	eventID, _ := e.createFixture(true)

	rec := e.do("GET", "/public/events/"+eventID.String()+"?locale=fr", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != CacheControlPublicReads {
		t.Fatalf("Cache-Control %q", cc)
	}
	detail := decode[PublicEventDetail](t, rec)
	if detail.Name != "Nuit Électrique" {
		t.Fatalf("name %q", detail.Name)
	}
	tts := detail.Performances[0].TicketTypes
	if len(tts) != 1 || tts[0].Name != "Admission générale" || tts[0].Price.Amount != 4550 {
		t.Fatalf("ticket types %+v", tts)
	}

	if rec := e.do("GET", "/public/events/"+uuid.NewString()+"?locale=fr", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown event: status %d", rec.Code)
	}
}

func TestOpenAPISpecServedVerbatim(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/openapi.yaml", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), apispec.Spec) {
		t.Fatal("served spec must be byte-identical to the committed contract (ADR-009)")
	}
}

func TestSeriesSeasonLifecycleAndPublicGrouping(t *testing.T) {
	e := newEnv(t)
	eventID, firstID := e.createFixture(false)
	first := e.store.performances[firstID]
	startsAt := first.StartsAt.Add(24 * time.Hour)
	second := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{OrganizerId: orgID, EventId: eventID, VenueId: first.VenueID, StartsAt: &startsAt, Timezone: first.Timezone}))
	e.do("POST", "/ticket-types", TicketTypeCreate{OrganizerId: orgID, PerformanceId: second.Id, Name: LocalizedString{"en": "Second", "fr": "Deuxième"}, Price: Money{Amount: 5000, Currency: "EUR"}})

	series := decode[Series](t, e.do("POST", "/series", SeriesCreate{OrganizerId: orgID, EventId: eventID, Name: LocalizedString{"en": "Autumn run", "fr": "Série automne"}}))
	series = decode[Series](t, e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: second.Id, Position: 2}))
	series = decode[Series](t, e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: firstID, Position: 1}))
	if len(series.Members) != 2 || series.Members[0].PerformanceId != firstID {
		t.Fatalf("series members = %+v", series.Members)
	}
	season := decode[Season](t, e.do("POST", "/seasons", SeasonCreate{OrganizerId: orgID, Name: LocalizedString{"en": "2026 season", "fr": "Saison 2026"}}))
	e.do("POST", "/seasons/"+season.Id.String()+"/series", SeasonSeriesAttach{SeriesId: series.Id})
	e.do("POST", "/seasons/"+season.Id.String()+"/events", SeasonEventAttach{EventId: eventID})

	result := decode[SeriesLifecycleResult](t, e.do("POST", "/series/"+series.Id.String()+"/publish", nil))
	if len(result.Performances) != 2 || len(e.pub.published) != 2 {
		t.Fatalf("publish result=%d events=%d", len(result.Performances), len(e.pub.published))
	}
	e.do("POST", "/series/"+series.Id.String()+"/publish", nil)
	if len(e.pub.published) != 2 {
		t.Fatal("idempotent series publish re-emitted")
	}

	detail := decode[PublicEventDetail](t, e.do("GET", "/public/events/"+eventID.String()+"?locale=fr", nil))
	if len(detail.Series) != 1 || detail.Series[0].Name != "Série automne" || len(detail.Series[0].PerformanceIds) != 2 || detail.Series[0].PerformanceIds[0] != firstID {
		t.Fatalf("series context = %+v", detail.Series)
	}
	publicSeason := decode[PublicSeasonDetail](t, e.do("GET", "/public/seasons/"+season.Id.String()+"?locale=en", nil))
	if len(publicSeason.Events) != 1 {
		t.Fatalf("season duplicate event count = %d", len(publicSeason.Events))
	}

	conflictRec := e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: uuid.New(), Position: 3})
	if conflictRec.Code != http.StatusNotFound {
		t.Fatalf("attach unknown = %d", conflictRec.Code)
	}
}

func (e *env) createFestivalDay(venueID, eventID uuid.UUID, day int) Performance {
	e.t.Helper()
	kind := SlotKind(store.KindFestivalDay)
	opens, closes := "12:00", "23:00"
	date := openapi_types.Date{Time: time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC)}
	p := decode[Performance](e.t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: venueID, Kind: &kind,
		OperatingDate: &date, OpensAt: &opens, ClosesAt: &closes, Timezone: "America/Toronto",
	}))
	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: p.Id,
		Name:  LocalizedString{"en": fmt.Sprintf("Day %d", day), "fr": fmt.Sprintf("Jour %d", day)},
		Price: Money{Amount: 7500, Currency: "CAD"},
	})
	return p
}

func TestFestivalCreateAttachDaysAndSharedCapacity(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	first, second := e.createFestivalDay(venueID, eventID, 1), e.createFestivalDay(venueID, eventID, 2)
	festival := decode[Festival](t, e.do("POST", "/festivals", FestivalCreate{
		OrganizerId: orgID, Name: LocalizedString{"en": "Summer Fest", "fr": "Festival d'été"}, SharedCapacity: 1000,
	}))
	festival = decode[Festival](t, e.do("POST", "/festivals/"+festival.Id.String()+"/days", FestivalDayAttach{PerformanceId: first.Id}))
	festival = decode[Festival](t, e.do("POST", "/festivals/"+festival.Id.String()+"/days", FestivalDayAttach{PerformanceId: second.Id}))
	if festival.SharedCapacity != 1000 || len(festival.MemberIds) != 2 {
		t.Fatalf("festival = %+v", festival)
	}
	for _, id := range []uuid.UUID{first.Id, second.Id} {
		if group := e.store.performances[id].CapacityGroupID; group == nil || *group != festival.Id {
			t.Fatalf("day %s capacity group = %v", id, group)
		}
	}

	_, performanceID := e.createFixture(false)
	if rec := e.do("POST", "/festivals/"+festival.Id.String()+"/days", FestivalDayAttach{PerformanceId: performanceID}); rec.Code != http.StatusConflict {
		t.Fatalf("performance-kind attach: %d", rec.Code)
	}
	crossOrg := e.store.performances[first.Id]
	crossOrg.CapacityGroupID = nil
	crossOrg.OrganizerID = uuid.New()
	e.store.performances[first.Id] = crossOrg
	otherFestival := decode[Festival](t, e.do("POST", "/festivals", FestivalCreate{OrganizerId: orgID, Name: LocalizedString{"en": "Other", "fr": "Autre"}, SharedCapacity: 50}))
	if rec := e.do("POST", "/festivals/"+otherFestival.Id.String()+"/days", FestivalDayAttach{PerformanceId: first.Id}); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-organizer attach: %d", rec.Code)
	}
	crossOrg.OrganizerID = orgID
	crossOrg.CapacityGroupID = &festival.Id
	e.store.performances[first.Id] = crossOrg
	if rec := e.do("POST", "/festivals/"+otherFestival.Id.String()+"/days", FestivalDayAttach{PerformanceId: first.Id}); rec.Code != http.StatusConflict {
		t.Fatalf("already-grouped attach: %d", rec.Code)
	}
	launched := e.store.festivals[otherFestival.Id]
	launched.Status = "published"
	e.store.festivals[otherFestival.Id] = launched
	third := e.createFestivalDay(venueID, eventID, 3)
	if rec := e.do("POST", "/festivals/"+otherFestival.Id.String()+"/days", FestivalDayAttach{PerformanceId: third.Id}); rec.Code != http.StatusConflict {
		t.Fatalf("non-draft festival attach: %d", rec.Code)
	}
}

func TestFestivalPublishCascadesAndEmitsSharedCapacity(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	days := []Performance{e.createFestivalDay(venueID, eventID, 1), e.createFestivalDay(venueID, eventID, 2)}
	festival := decode[Festival](t, e.do("POST", "/festivals", FestivalCreate{OrganizerId: orgID, Name: LocalizedString{"en": "Summer Fest", "fr": "Festival d'été"}, SharedCapacity: 1000}))
	for _, day := range days {
		e.do("POST", "/festivals/"+festival.Id.String()+"/days", FestivalDayAttach{PerformanceId: day.Id})
	}
	result := decode[FestivalLifecycleResult](t, e.do("POST", "/festivals/"+festival.Id.String()+"/publish", nil))
	if len(result.Performances) != 2 || len(e.pub.published) != 2 {
		t.Fatalf("publish result=%d events=%d", len(result.Performances), len(e.pub.published))
	}
	for _, p := range e.pub.published {
		if p.Status != "published" || p.CapacityGroupID == nil || *p.CapacityGroupID != festival.Id || p.SharedCapacity == nil || *p.SharedCapacity != 1000 {
			t.Fatalf("published festival day = %+v", p)
		}
	}
	e.do("POST", "/festivals/"+festival.Id.String()+"/publish", nil)
	if len(e.pub.published) != 2 {
		t.Fatal("idempotent festival publish re-emitted")
	}
	for _, day := range days {
		e.store.emitted[day.Id] = false
	}
	e.pub.calls = nil
	e.do("POST", "/festivals/"+festival.Id.String()+"/archive", nil)
	if !slices.Equal(e.pub.calls, []string{"published", "archived", "published", "archived"}) {
		t.Fatalf("owed publication/archive order = %v", e.pub.calls)
	}
}

func TestGroupedFestivalDayLifecycleMustUseFestivalCascade(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	day := e.createFestivalDay(venueID, eventID, 1)
	festival := decode[Festival](t, e.do("POST", "/festivals", FestivalCreate{
		OrganizerId: orgID, Name: LocalizedString{"en": "Summer Fest", "fr": "Festival d'été"}, SharedCapacity: 1000,
	}))
	e.do("POST", "/festivals/"+festival.Id.String()+"/days", FestivalDayAttach{PerformanceId: day.Id})

	rec := e.do("POST", "/performances/"+day.Id.String()+"/publish", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("direct grouped publish: %d", rec.Code)
	}
	if body := decode[Error](t, rec); body.Error != "grouped festival day must be published/archived via its festival" {
		t.Fatalf("direct grouped publish error = %q", body.Error)
	}
	if rec := e.do("POST", "/festivals/"+festival.Id.String()+"/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("festival publish: %d", rec.Code)
	}
	if got := e.store.performances[day.Id].Status; got != "published" {
		t.Fatalf("day after festival publish = %q", got)
	}

	rec = e.do("POST", "/performances/"+day.Id.String()+"/archive", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("direct grouped archive: %d", rec.Code)
	}
	if body := decode[Error](t, rec); body.Error != "grouped festival day must be published/archived via its festival" {
		t.Fatalf("direct grouped archive error = %q", body.Error)
	}
	if rec := e.do("POST", "/festivals/"+festival.Id.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("festival archive: %d", rec.Code)
	}
	if got := e.store.performances[day.Id].Status; got != "archived" {
		t.Fatalf("day after festival archive = %q", got)
	}
}

func TestFestivalPublicGroupedRead(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	festival := decode[Festival](t, e.do("POST", "/festivals", FestivalCreate{OrganizerId: orgID, Name: LocalizedString{"en": "Summer Fest", "fr": "Festival d'été"}, SharedCapacity: 1000}))
	if rec := e.do("GET", "/public/festivals/"+festival.Id.String()+"?locale=en", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("draft festival public read: %d", rec.Code)
	}
	for _, day := range []Performance{e.createFestivalDay(venueID, eventID, 1), e.createFestivalDay(venueID, eventID, 2)} {
		e.do("POST", "/festivals/"+festival.Id.String()+"/days", FestivalDayAttach{PerformanceId: day.Id})
	}
	e.do("POST", "/festivals/"+festival.Id.String()+"/publish", nil)
	rec := e.do("GET", "/public/festivals/"+festival.Id.String()+"?locale=en", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != CacheControlPublicReads {
		t.Fatalf("public festival: %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	detail := decode[PublicFestivalDetail](t, rec)
	if detail.Name != "Summer Fest" || len(detail.Days) != 2 {
		t.Fatalf("public festival = %+v", detail)
	}
}

func TestSeriesPublishConflictNamesBlockingSlotAndIsAtomic(t *testing.T) {
	e := newEnv(t)
	eventID, sellableID := e.createFixture(false)
	sellable := e.store.performances[sellableID]
	startsAt := sellable.StartsAt.Add(48 * time.Hour)
	blocking := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: sellable.VenueID,
		StartsAt: &startsAt, Timezone: sellable.Timezone,
	}))
	series := decode[Series](t, e.do("POST", "/series", SeriesCreate{
		OrganizerId: orgID, EventId: eventID,
		Name: LocalizedString{"en": "Run", "fr": "Série"},
	}))
	e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: sellableID, Position: 1})
	e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: blocking.Id, Position: 2})

	rec := e.do("POST", "/series/"+series.Id.String()+"/publish", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish conflict: %d %s", rec.Code, rec.Body.String())
	}
	conflict := decode[SeriesTransitionConflict](t, rec)
	if conflict.BlockingPerformanceId == nil || *conflict.BlockingPerformanceId != blocking.Id {
		t.Fatalf("blocking performance = %v, want %s", conflict.BlockingPerformanceId, blocking.Id)
	}
	if e.store.performances[sellableID].Status != "draft" || e.store.performances[blocking.Id].Status != "draft" || len(e.pub.published) != 0 {
		t.Fatal("failed all-or-nothing preflight mutated or emitted a member")
	}

	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: blocking.Id,
		Name:  LocalizedString{"en": "Admission", "fr": "Admission"},
		Price: Money{Amount: 2500, Currency: "CAD"},
	})
	if rec = e.do("POST", "/series/"+series.Id.String()+"/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish after repair: %d %s", rec.Code, rec.Body.String())
	}
	newStart := startsAt.Add(24 * time.Hour)
	late := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: sellable.VenueID,
		StartsAt: &newStart, Timezone: sellable.Timezone,
	}))
	if rec = e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: late.Id, Position: 3}); rec.Code != http.StatusConflict {
		t.Fatalf("membership after launch: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSeriesArchiveBlocksOwedClosureThenEmitsInOrder(t *testing.T) {
	e := newEnv(t)
	eventID, firstID := e.createFixture(false)
	first := e.store.performances[firstID]
	startsAt := first.StartsAt.Add(24 * time.Hour)
	second := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: first.VenueID,
		StartsAt: &startsAt, Timezone: first.Timezone,
	}))
	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: second.Id,
		Name:  LocalizedString{"en": "Second", "fr": "Deuxième"},
		Price: Money{Amount: 5000, Currency: "EUR"},
	})
	series := decode[Series](t, e.do("POST", "/series", SeriesCreate{
		OrganizerId: orgID, EventId: eventID,
		Name: LocalizedString{"en": "Run", "fr": "Série"},
	}))
	e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: firstID, Position: 1})
	e.do("POST", "/series/"+series.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: second.Id, Position: 2})
	e.do("POST", "/series/"+series.Id.String()+"/publish", nil)

	owed := e.store.performances[firstID]
	owed.Closure.Version = 1
	e.store.performances[firstID] = owed
	rec := e.do("POST", "/series/"+series.Id.String()+"/archive", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("owed closure archive: %d %s", rec.Code, rec.Body.String())
	}
	conflict := decode[SeriesTransitionConflict](t, rec)
	if conflict.BlockingPerformanceId == nil || *conflict.BlockingPerformanceId != firstID {
		t.Fatalf("blocking performance = %v, want %s", conflict.BlockingPerformanceId, firstID)
	}
	if e.store.performances[firstID].Status != "published" || e.store.performances[second.Id].Status != "published" {
		t.Fatal("failed archive preflight partially mutated the series")
	}

	e.store.closureEmitted[firstID] = 1
	e.store.emitted[firstID] = false // exercise owed publication before archive
	e.pub.calls = nil
	rec = e.do("POST", "/series/"+series.Id.String()+"/archive", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(e.pub.calls, []string{"published", "archived", "archived"}) {
		t.Fatalf("event order = %v", e.pub.calls)
	}
	if e.store.performances[firstID].Status != "archived" || e.store.performances[second.Id].Status != "archived" {
		t.Fatal("series members were not archived")
	}
	e.do("POST", "/series/"+series.Id.String()+"/archive", nil)
	if !slices.Equal(e.pub.calls, []string{"published", "archived", "archived"}) {
		t.Fatalf("idempotent archive re-emitted: %v", e.pub.calls)
	}
}

func TestSeriesEmptyFrozenAndOrganizerMismatchContracts(t *testing.T) {
	e := newEnv(t)
	eventID, performanceID := e.createFixture(true)
	empty := decode[Series](t, e.do("POST", "/series", SeriesCreate{
		OrganizerId: orgID, EventId: eventID,
		Name: LocalizedString{"en": "Empty", "fr": "Vide"},
	}))
	for _, action := range []string{"publish", "archive"} {
		if rec := e.do("POST", "/series/"+empty.Id.String()+"/"+action, nil); rec.Code != http.StatusConflict {
			t.Fatalf("empty series %s: %d %s", action, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do("POST", "/series/"+empty.Id.String()+"/performances", SeriesPerformanceAttach{PerformanceId: performanceID, Position: 1}); rec.Code != http.StatusConflict {
		t.Fatalf("attach published target: %d %s", rec.Code, rec.Body.String())
	}

	otherOrg, otherEventID, otherPerformanceID, otherSeriesID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	otherEvent := e.store.events[eventID]
	otherEvent.ID, otherEvent.OrganizerID = otherEventID, otherOrg
	e.store.events[otherEventID] = otherEvent
	otherPerformance := e.store.performances[performanceID]
	otherPerformance.ID, otherPerformance.OrganizerID, otherPerformance.EventID, otherPerformance.Status = otherPerformanceID, otherOrg, otherEventID, "draft"
	e.store.performances[otherPerformanceID] = otherPerformance
	otherSeries := store.Series{ID: otherSeriesID, OrganizerID: otherOrg, EventID: otherEventID, Name: store.LocalizedText{"en": "Other", "fr": "Autre"}, Members: []store.SeriesMember{}, CreatedAt: time.Now().UTC()}
	e.store.series[otherSeriesID] = otherSeries
	season := decode[Season](t, e.do("POST", "/seasons", SeasonCreate{OrganizerId: orgID, Name: LocalizedString{"en": "Season", "fr": "Saison"}}))

	cases := []struct {
		path string
		body any
	}{
		{"/series/" + empty.Id.String() + "/performances", SeriesPerformanceAttach{PerformanceId: otherPerformanceID, Position: 1}},
		{"/seasons/" + season.Id.String() + "/series", SeasonSeriesAttach{SeriesId: otherSeriesID}},
		{"/seasons/" + season.Id.String() + "/events", SeasonEventAttach{EventId: otherEventID}},
	}
	for _, tc := range cases {
		if rec := e.do("POST", tc.path, tc.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("organizer mismatch %s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}

// --- US-009: typed dated slot (kinds, attributes, closure) ---

// dayEnv creates a venue + event and returns their ids for slot-kind tests.
func (e *env) dayEnv() (venueID, eventID uuid.UUID) {
	e.t.Helper()
	v := decode[Venue](e.t, e.do("POST", "/venues", VenueCreate{OrganizerId: orgID, Name: "La Ronde", GaCapacity: 800}))
	ev := decode[Event](e.t, e.do("POST", "/events", EventCreate{
		OrganizerId: orgID, Name: LocalizedString{"fr": "Journée parc", "en": "Park day"},
	}))
	return v.Id, ev.Id
}

func TestCreateOperatingDaySlot(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	kind := SlotKind("operating_day")
	opens, closes := "10:00", "02:00" // spans midnight
	opDate := openapi_types.Date{Time: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)}
	max := int32(3)
	rec := e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: venueID, Kind: &kind,
		OperatingDate: &opDate, OpensAt: &opens, ClosesAt: &closes, Timezone: "America/Toronto",
		ReEntry: &ReEntryPolicy{Mode: "count_limited", MaxEntries: &max, RequiresExit: true},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create operating_day: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[Performance](t, rec)
	if p.Kind != "operating_day" || p.StartsAt != nil || p.OperatingDate == nil ||
		p.OpensAt == nil || *p.OpensAt != "10:00" || *p.ClosesAt != "02:00" {
		t.Fatalf("operating_day attributes not persisted: %+v", p)
	}
	if p.ReEntry.Mode != "count_limited" || p.ReEntry.MaxEntries == nil || *p.ReEntry.MaxEntries != 3 || !p.ReEntry.RequiresExit {
		t.Fatalf("re_entry not persisted: %+v", p.ReEntry)
	}
	if p.Closure.Status != "open" {
		t.Fatalf("new slot must be open, got %q", p.Closure.Status)
	}
}

func TestCreateSlotKindValidations(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	base := func() PerformanceCreate {
		return PerformanceCreate{OrganizerId: orgID, EventId: eventID, VenueId: venueID, Timezone: "America/Toronto"}
	}
	opens, closes := "09:00", "17:00"
	opDate := openapi_types.Date{Time: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)}
	instant := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	festival := SlotKind("festival_day")
	perfKind := SlotKind("performance")
	max := int32(2)

	cases := []struct {
		name string
		body PerformanceCreate
	}{
		{"performance without starts_at", func() PerformanceCreate { b := base(); b.Kind = &perfKind; return b }()},
		{"performance with operating window", func() PerformanceCreate {
			b := base()
			b.Kind = &perfKind
			b.StartsAt = &instant
			b.OpensAt = &opens
			return b
		}()},
		{"day kind with starts_at", func() PerformanceCreate {
			b := base()
			b.Kind = &festival
			b.StartsAt = &instant
			b.OperatingDate = &opDate
			b.OpensAt = &opens
			b.ClosesAt = &closes
			return b
		}()},
		{"day kind missing closes_at", func() PerformanceCreate {
			b := base()
			b.Kind = &festival
			b.OperatingDate = &opDate
			b.OpensAt = &opens
			return b
		}()},
		{"count_limited without max", func() PerformanceCreate {
			b := base()
			b.StartsAt = &instant
			b.ReEntry = &ReEntryPolicy{Mode: "count_limited"}
			return b
		}()},
		{"max on non-count_limited", func() PerformanceCreate {
			b := base()
			b.StartsAt = &instant
			b.ReEntry = &ReEntryPolicy{Mode: "single", MaxEntries: &max}
			return b
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := e.do("POST", "/performances", c.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPublishEmitsSlotKind(t *testing.T) {
	e := newEnv(t)
	venueID, eventID := e.dayEnv()
	kind := SlotKind("festival_day")
	opens, closes := "12:00", "23:00"
	opDate := openapi_types.Date{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	perf := decode[Performance](t, e.do("POST", "/performances", PerformanceCreate{
		OrganizerId: orgID, EventId: eventID, VenueId: venueID, Kind: &kind,
		OperatingDate: &opDate, OpensAt: &opens, ClosesAt: &closes, Timezone: "Europe/Paris",
	}))
	e.do("POST", "/ticket-types", TicketTypeCreate{
		OrganizerId: orgID, PerformanceId: perf.Id,
		Name: LocalizedString{"fr": "Pass jour", "en": "Day pass"}, Price: Money{Amount: 9000, Currency: "EUR"},
	})
	if rec := e.do("POST", "/performances/"+perf.Id.String()+"/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.published) != 1 || e.pub.published[0].Kind != "festival_day" {
		t.Fatalf("publication event must carry the slot kind, got %+v", e.pub.published)
	}
}

func TestClosureToggleLifecycle(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true) // published performance
	reason := "storm"

	// close
	rec := e.do("POST", "/performances/"+perfID.String()+"/close", SlotCloseRequest{Reason: &reason})
	if rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[Performance](t, rec)
	if p.Closure.Status != "closed" || p.Closure.Reason == nil || *p.Closure.Reason != "storm" || p.Closure.ClosedAt == nil {
		t.Fatalf("closure not applied: %+v", p.Closure)
	}
	if len(e.pub.closed) != 1 {
		t.Fatalf("close must emit once, got %d", len(e.pub.closed))
	}

	// idempotent re-close: no new emission
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("re-close: %d", rec.Code)
	}
	if len(e.pub.closed) != 1 {
		t.Fatalf("idempotent re-close must not re-emit, got %d", len(e.pub.closed))
	}

	// reopen
	rec = e.do("POST", "/performances/"+perfID.String()+"/reopen", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reopen: %d %s", rec.Code, rec.Body.String())
	}
	p = decode[Performance](t, rec)
	if p.Closure.Status != "open" || p.Closure.ClosedAt != nil {
		t.Fatalf("reopen not applied: %+v", p.Closure)
	}
	if len(e.pub.reopened) != 1 {
		t.Fatalf("reopen must emit once, got %d", len(e.pub.reopened))
	}
}

// Archive stays legal from a closed slot (spike §Case 3): closure is orthogonal
// to the lifecycle, so a closed day can be archived without first reopening.
func TestArchiveLegalFromClosed(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("close: %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive from closed must be legal, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestClosureRejectsUnpublished(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false) // draft
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusConflict {
		t.Fatalf("closing a draft must be 409, got %d %s", rec.Code, rec.Body.String())
	}
}

// A closure must never overtake the slot's publication event. If publish
// commits but its emission fails, a subsequent close emits the owed publication
// first, then the closure — consumers see publish before closed.
func TestCloseEmitsOwedPublishBeforeClosure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(false) // draft + priced (publishable)
	// publish, but the emission fails: status becomes published, its event owed.
	e.pub.failNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/publish", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("publish emission failure must be 500, got %d", rec.Code)
	}
	if len(e.pub.calls) != 0 {
		t.Fatalf("failed publish must record nothing, got %v", e.pub.calls)
	}
	// close: owed publication emitted before the closure event.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body.String())
	}
	if want := []string{"published", "closed"}; !slices.Equal(e.pub.calls, want) {
		t.Fatalf("emission order = %v, want %v", e.pub.calls, want)
	}
	// idempotent re-close emits nothing new.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("re-close: %d", rec.Code)
	}
	if len(e.pub.calls) != 2 {
		t.Fatalf("idempotent re-close must not re-emit, got %v", e.pub.calls)
	}
}

// Archiving must not strand an owed closure event: while the closed event is
// unemitted, archive is refused (409) so the toggle can still re-emit it. Once
// emitted, archive-from-closed proceeds (spike §Case 3).
func TestArchiveRefusedWhileClosureOwed(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	e.pub.failClosureNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("close emission failure: %d", rec.Code)
	}
	// closure event owed (emitted=0 < version=1): archive must refuse.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusConflict {
		t.Fatalf("archive while closure owed must be 409, got %d %s", rec.Code, rec.Body.String())
	}
	// re-emit the owed closure, then archive succeeds.
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("retry close: %d", rec.Code)
	}
	if rec := e.do("POST", "/performances/"+perfID.String()+"/archive", nil); rec.Code != http.StatusOK {
		t.Fatalf("archive after closure emitted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCloseRetriesEmissionAfterFailure(t *testing.T) {
	e := newEnv(t)
	_, perfID := e.createFixture(true)
	e.pub.failClosureNext = true
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("emission failure must surface as 500, got %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "slot state changed but the closure event was not emitted; retry close") {
		t.Fatalf("recovery body lost, got: %s", rec.Body.String())
	}
	if len(e.pub.closed) != 0 {
		t.Fatalf("failed emission must not record, got %d", len(e.pub.closed))
	}
	// retry re-emits the still-owed transition (same deterministic id at the stream)
	if rec := e.do("POST", "/performances/"+perfID.String()+"/close", nil); rec.Code != http.StatusOK {
		t.Fatalf("retry close: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.pub.closed) != 1 {
		t.Fatalf("retry must emit the owed closure, got %d", len(e.pub.closed))
	}
}

// TestListPublicVenues covers the US-018 back-office venue read: organizer-scoped,
// hours-tier Cache-Control (ADR-004, distinct from the minutes tier), full Venue
// payload conforming to the contract (ADR-028), and a 400 on a missing/invalid
// organizer_id. Response validation runs through env.do (spec conformance).
func TestListPublicVenues(t *testing.T) {
	e := newEnv(t)
	otherOrg := uuid.MustParse("00000000-0000-0000-0000-0000000000ff")
	// Two venues for orgID, one for another organizer — the read must scope.
	_ = decode[Venue](t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Zed Hall", GaCapacity: 900}))
	_ = decode[Venue](t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: orgID, Name: "Alpha Room", GaCapacity: 200}))
	_ = decode[Venue](t, e.do("POST", "/venues",
		VenueCreate{OrganizerId: otherOrg, Name: "Not Ours", GaCapacity: 100}))

	rec := e.do("GET", "/public/venues?organizer_id="+orgID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list venues: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600, s-maxage=3600" {
		t.Fatalf("venue read must carry the ADR-004 hours tier, got %q", got)
	}
	out := decode[PublicVenueList](t, rec)
	if len(out.Venues) != 2 {
		t.Fatalf("read must be organizer-scoped: want 2, got %d", len(out.Venues))
	}
	// Deterministic order (ORDER BY name) for the smoke assertion + screenshot.
	if out.Venues[0].Name != "Alpha Room" || out.Venues[1].Name != "Zed Hall" {
		t.Fatalf("venues must be name-ordered, got %q, %q", out.Venues[0].Name, out.Venues[1].Name)
	}
	for _, v := range out.Venues {
		if v.OrganizerId != orgID {
			t.Fatalf("scoping leak: got organizer %s", v.OrganizerId)
		}
		if v.GaCapacity == 0 || v.Id == (uuid.UUID{}) {
			t.Fatalf("venue payload incomplete: %+v", v)
		}
	}
}

func TestListPublicVenuesRejectsBadOrganizer(t *testing.T) {
	e := newEnv(t)
	for _, tt := range []struct {
		name, query string
	}{
		{"missing", ""},
		{"malformed", "?organizer_id=not-a-uuid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do("GET", "/public/venues"+tt.query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
