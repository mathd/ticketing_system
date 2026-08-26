package api

// Authoring writes at the trusted root: venues, events, the grouping aggregates
// and performances. Every handler takes its organizer from the verified
// assertion via organizerFor, never from the request body.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"ticketing/services/catalog/internal/store"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

func (s *Server) CreateVenue(w http.ResponseWriter, r *http.Request) {
	var in VenueCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	v, err := s.store.CreateVenue(r.Context(), store.VenueInput{
		OrganizerID: organizerID,
		Name:        in.Name,
		GACapacity:  in.GaCapacity,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, Venue{
		Id: v.ID, OrganizerId: v.OrganizerID, Name: v.Name,
		GaCapacity: v.GACapacity, CreatedAt: v.CreatedAt,
	})
}

// CreateEvent, CreatePerformance and CreateTicketType take their idempotency key
// from the generated params rather than reading the header themselves (TKT-200).
//
// The generated wrapper binds and REFUSES an absent Idempotency-Key before this
// handler runs — catalog generates chi-server, unlike commerce, which generates
// models only and therefore checks the header by hand in eight places. That
// ordering is load-bearing and is asserted by a test: the wrapper's refusal
// precedes the security check, so a request with neither the key nor a valid
// credential answers 400, not 401. See TestCreateRefusesMissingIdempotencyKey.
func (s *Server) CreateEvent(w http.ResponseWriter, r *http.Request, params CreateEventParams) {
	var in EventCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	var desc store.LocalizedText
	if in.Description != nil {
		desc = store.LocalizedText(*in.Description)
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	ev, err := s.store.CreateEvent(r.Context(), store.EventInput{
		OrganizerID:    organizerID,
		Name:           store.LocalizedText(in.Name),
		Description:    desc,
		IdempotencyKey: string(params.IdempotencyKey),
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, eventToAPI(ev))
}

func eventToAPI(ev store.Event) Event {
	out := Event{
		Id: ev.ID, OrganizerId: ev.OrganizerID,
		Name: LocalizedString(ev.Name), CreatedAt: ev.CreatedAt,
	}
	if len(ev.Description) > 0 {
		d := LocalizedString(ev.Description)
		out.Description = &d
	}
	return out
}

func seriesToAPI(s store.Series) Series {
	members := make([]SeriesMember, 0, len(s.Members))
	for _, m := range s.Members {
		members = append(members, SeriesMember{PerformanceId: m.PerformanceID, Position: m.Position})
	}
	return Series{Id: s.ID, OrganizerId: s.OrganizerID, EventId: s.EventID, Name: LocalizedString(s.Name), Members: members, CreatedAt: s.CreatedAt}
}

func seasonToAPI(s store.Season) Season {
	return Season{Id: s.ID, OrganizerId: s.OrganizerID, Name: LocalizedString(s.Name), SeriesIds: s.SeriesIDs, EventIds: s.EventIDs, CreatedAt: s.CreatedAt}
}

func festivalToAPI(f store.Festival) Festival {
	return Festival{
		Id: f.ID, OrganizerId: f.OrganizerID, Name: LocalizedString(f.Name),
		SharedCapacity: f.SharedCapacity, Status: FestivalStatus(f.Status),
		MemberIds: f.MemberIDs, CreatedAt: f.CreatedAt,
	}
}

func (s *Server) CreateSeries(w http.ResponseWriter, r *http.Request) {
	var in SeriesCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.CreateSeries(r.Context(), store.SeriesInput{OrganizerID: organizerID, EventID: in.EventId, Name: store.LocalizedText(in.Name)})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, seriesToAPI(out))
}

func (s *Server) AttachPerformanceToSeries(w http.ResponseWriter, r *http.Request, seriesId SeriesId) {
	var in SeriesPerformanceAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.AttachPerformanceToSeries(r.Context(), organizerID, seriesId, in.PerformanceId, in.Position)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seriesToAPI(out))
}

func (s *Server) CreateSeason(w http.ResponseWriter, r *http.Request) {
	var in SeasonCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.CreateSeason(r.Context(), store.SeasonInput{OrganizerID: organizerID, Name: store.LocalizedText(in.Name)})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, seasonToAPI(out))
}

func (s *Server) AttachSeriesToSeason(w http.ResponseWriter, r *http.Request, seasonId SeasonId) {
	var in SeasonSeriesAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.AttachSeriesToSeason(r.Context(), organizerID, seasonId, in.SeriesId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seasonToAPI(out))
}

func (s *Server) AttachEventToSeason(w http.ResponseWriter, r *http.Request, seasonId SeasonId) {
	var in SeasonEventAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.AttachEventToSeason(r.Context(), organizerID, seasonId, in.EventId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seasonToAPI(out))
}

func (s *Server) CreateFestival(w http.ResponseWriter, r *http.Request) {
	var in FestivalCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	if in.SharedCapacity <= 0 {
		writeJSON(w, http.StatusBadRequest, Error{Error: "shared_capacity must be positive"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.CreateFestival(r.Context(), store.FestivalInput{
		OrganizerID: organizerID, Name: store.LocalizedText(in.Name), SharedCapacity: in.SharedCapacity,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, festivalToAPI(out))
}

func (s *Server) AttachDayToFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId) {
	var in FestivalDayAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	out, err := s.store.AttachDayToFestival(r.Context(), organizerID, festivalId, in.PerformanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, festivalToAPI(out))
}

func (s *Server) CreatePerformance(w http.ResponseWriter, r *http.Request, params CreatePerformanceParams) {
	var in PerformanceCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unknown timezone %q", in.Timezone)})
		return
	}
	kind := store.KindPerformance
	if in.Kind != nil {
		kind = string(*in.Kind)
	}
	// Per-kind temporal invariant: the spec can't express "required-if-kind",
	// so it is enforced here (the DB CHECK is the backstop). A performance
	// carries an instant; a day kind carries the operating window.
	switch kind {
	case store.KindPerformance:
		if in.StartsAt == nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "kind 'performance' requires starts_at"})
			return
		}
		if in.OperatingDate != nil || in.OpensAt != nil || in.ClosesAt != nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "kind 'performance' must not carry an operating window"})
			return
		}
	case store.KindFestivalDay, store.KindOperatingDay:
		if in.StartsAt != nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "day kinds must not carry starts_at"})
			return
		}
		if in.OperatingDate == nil || in.OpensAt == nil || in.ClosesAt == nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "day kinds require operating_date, opens_at and closes_at"})
			return
		}
	}
	re := store.ReEntryPolicy{Mode: "single"}
	if in.ReEntry != nil {
		re = store.ReEntryPolicy{Mode: string(in.ReEntry.Mode), MaxEntries: in.ReEntry.MaxEntries, RequiresExit: in.ReEntry.RequiresExit}
	}
	if re.Mode == "count_limited" && re.MaxEntries == nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "re_entry mode 'count_limited' requires max_entries"})
		return
	}
	if re.Mode != "count_limited" && re.MaxEntries != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "max_entries is only valid for re_entry mode 'count_limited'"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	input := store.PerformanceInput{
		OrganizerID: organizerID,
		EventID:     in.EventId,
		VenueID:     in.VenueId,
		Kind:        kind,
		StartsAt:    in.StartsAt,
		OpensAt:     in.OpensAt,
		ClosesAt:    in.ClosesAt,
		Timezone:    in.Timezone,
		ReEntry:     re,
		SeatMapID:   in.SeatMapId,

		IdempotencyKey: string(params.IdempotencyKey),
	}
	if in.OperatingDate != nil {
		d := in.OperatingDate.Time
		input.OperatingDate = &d
	}
	p, err := s.store.CreatePerformance(r.Context(), input)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, performanceToAPI(p))
}

func performanceToAPI(p store.Performance) Performance {
	out := Performance{
		Id: p.ID, OrganizerId: p.OrganizerID, EventId: p.EventID, VenueId: p.VenueID,
		Kind: SlotKind(p.Kind), StartsAt: p.StartsAt, OpensAt: p.OpensAt, ClosesAt: p.ClosesAt,
		Timezone: p.Timezone,
		ReEntry: ReEntryPolicy{
			Mode: ReEntryPolicyMode(p.ReEntry.Mode), MaxEntries: p.ReEntry.MaxEntries,
			RequiresExit: p.ReEntry.RequiresExit,
		},
		Closure: Closure{
			Status: ClosureStatus(p.Closure.Status), ClosedAt: p.Closure.ClosedAt, Reason: p.Closure.Reason,
		},
		Status: PerformanceStatus(p.Status), PublishedAt: p.PublishedAt,
		ArchivedAt: p.ArchivedAt, CapacityGroupId: p.CapacityGroupID, SeatMapId: p.SeatMapID,
		CreatedAt: p.CreatedAt,
	}
	if p.OperatingDate != nil {
		out.OperatingDate = &openapi_types.Date{Time: *p.OperatingDate}
	}
	return out
}
