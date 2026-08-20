package api

// The public read handlers and the payload shaping they answer with, at the
// ADR-004 cache tiers.

import (
	"fmt"
	"net/http"
	"slices"
	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/store"
	"time"
)

// publicStartsAt is the slot's representative instant on the public path: the
// store COALESCEs a day kind's operating-window opening moment into StartsAt,
// so it is always set here; the guard is defensive against a nil.
func publicStartsAt(p store.Performance) time.Time {
	if p.StartsAt != nil {
		return *p.StartsAt
	}
	return time.Time{}
}

func localeSupported(locale string) bool {
	return slices.Contains(SupportedLocales, locale)
}

// resolve picks the requested locale with defaultLocale fallback for
// optional fields (required fields are complete by construction at create).
func resolve(text store.LocalizedText, locale string) string {
	if v := text[locale]; v != "" {
		return v
	}
	return text[SupportedLocales[0]]
}

func (s *Server) ListPublicEvents(w http.ResponseWriter, r *http.Request, params ListPublicEventsParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.ListPublishedEvents(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	aggs := read.Value
	out := PublicEventList{Events: make([]PublicEventSummary, 0, len(aggs))}
	for _, agg := range aggs {
		out.Events = append(out.Events, eventSummary(agg, params.Locale))
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, out)
}

// ListPublicVenues serves the back-office venue list (US-018): an organizer's
// venues, name-ordered, at the ADR-004 hours tier. organizer_id is a required
// query param (parsed + validated by the generated wrapper); scoping is a store
// predicate (ADR-002). The response is the full Venue payload so the contract
// (ADR-028) is satisfied without hand-shaping.
func (s *Server) ListPublicVenues(w http.ResponseWriter, r *http.Request, params ListPublicVenuesParams) {
	venues, err := s.store.ListVenues(r.Context(), params.OrganizerId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := PublicVenueList{Venues: make([]Venue, 0, len(venues))}
	for _, v := range venues {
		out.Venues = append(out.Venues, Venue{
			Id:          v.ID,
			OrganizerId: v.OrganizerID,
			Name:        v.Name,
			GaCapacity:  v.GACapacity,
			CreatedAt:   v.CreatedAt,
		})
	}
	w.Header().Set("Cache-Control", CacheControlPublicVenueReads)
	writeJSON(w, http.StatusOK, out)
}

func eventSummary(agg store.EventAggregate, locale string) PublicEventSummary {
	sum := PublicEventSummary{
		Id:           agg.Event.ID,
		Name:         resolve(agg.Event.Name, locale),
		Performances: make([]PublicPerformanceSummary, 0, len(agg.Performances)),
	}
	if d := resolve(agg.Event.Description, locale); d != "" {
		sum.Description = &d
	}
	for _, pa := range agg.Performances {
		from := pa.TicketTypes[0]
		for _, tt := range pa.TicketTypes[1:] {
			if tt.PriceAmount < from.PriceAmount {
				from = tt
			}
		}
		sum.Performances = append(sum.Performances, PublicPerformanceSummary{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance),
			Timezone: pa.Performance.Timezone, VenueName: pa.Venue.Name,
			FromPrice: Money{Amount: from.PriceAmount, Currency: from.Currency},
		})
	}
	return sum
}

func (s *Server) GetPublicEvent(w http.ResponseWriter, r *http.Request, eventId EventId, params GetPublicEventParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.GetPublishedEvent(r.Context(), eventId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	agg := read.Value
	detail := publicEventDetail(agg, params.Locale)
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, detail)
}

func publicEventDetail(agg store.EventAggregate, locale string) PublicEventDetail {
	detail := PublicEventDetail{
		Id:           agg.Event.ID,
		OrganizerId:  agg.Event.OrganizerID,
		Name:         resolve(agg.Event.Name, locale),
		Series:       make([]PublicSeriesContext, 0, len(agg.Series)),
		Performances: make([]PublicPerformanceDetail, 0, len(agg.Performances)),
	}
	if d := resolve(agg.Event.Description, locale); d != "" {
		detail.Description = &d
	}
	for _, sa := range agg.Series {
		detail.Series = append(detail.Series, PublicSeriesContext{Id: sa.Series.ID, Name: resolve(sa.Series.Name, locale), PerformanceIds: sa.PerformanceIDs})
	}
	for _, pa := range agg.Performances {
		pd := PublicPerformanceDetail{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance),
			Timezone:    pa.Performance.Timezone,
			Venue:       PublicVenue{Id: pa.Venue.ID, Name: pa.Venue.Name},
			SeatMapId:   pa.Performance.SeatMapID,
			TicketTypes: make([]PublicTicketType, 0, len(pa.TicketTypes)),
		}
		for _, tt := range pa.TicketTypes {
			pd.TicketTypes = append(pd.TicketTypes, PublicTicketType{
				Id: tt.ID, Name: resolve(tt.Name, locale),
				Price: Money{Amount: tt.PriceAmount, Currency: tt.Currency},
			})
		}
		detail.Performances = append(detail.Performances, pd)
	}
	return detail
}

func (s *Server) GetPublicSeason(w http.ResponseWriter, r *http.Request, seasonId SeasonId, params GetPublicSeasonParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.GetPublishedSeason(r.Context(), seasonId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	agg := read.Value
	out := PublicSeasonDetail{Id: agg.Season.ID, OrganizerId: agg.Season.OrganizerID, Name: resolve(agg.Season.Name, params.Locale), Events: make([]PublicEventDetail, 0, len(agg.Events))}
	for _, event := range agg.Events {
		out.Events = append(out.Events, publicEventDetail(event, params.Locale))
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetPublicFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId, params GetPublicFestivalParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.GetPublishedFestival(r.Context(), festivalId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	agg := read.Value
	out := PublicFestivalDetail{
		Id: agg.Festival.ID, OrganizerId: agg.Festival.OrganizerID,
		Name: resolve(agg.Festival.Name, params.Locale), Days: make([]PublicPerformanceDetail, 0, len(agg.Performances)),
	}
	for _, pa := range agg.Performances {
		day := PublicPerformanceDetail{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance), Timezone: pa.Performance.Timezone,
			Venue: PublicVenue{Id: pa.Venue.ID, Name: pa.Venue.Name}, TicketTypes: make([]PublicTicketType, 0, len(pa.TicketTypes)),
		}
		for _, tt := range pa.TicketTypes {
			day.TicketTypes = append(day.TicketTypes, PublicTicketType{
				Id: tt.ID, Name: resolve(tt.Name, params.Locale), Price: Money{Amount: tt.PriceAmount, Currency: tt.Currency},
			})
		}
		out.Days = append(out.Days, day)
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, out)
}

// GetOpenAPISpec serves the committed contract byte-identical (ADR-009 §4).
func (s *Server) GetOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(apispec.Spec)
}
