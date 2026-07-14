// Package api implements the generated ServerInterface (openapi_gen.go —
// regenerate with `make generate`, never edit) against the store and event
// ports. Shape validation is the spec middleware's job; this layer owns
// business rules (locale completeness, timezone validity, tenancy mapping)
// and the ADR-004 cache tier on every public read.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	oapimiddleware "github.com/oapi-codegen/nethttp-middleware"
	openapi_types "github.com/oapi-codegen/runtime/types"

	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"
)

// SupportedLocales is the platform's live locale set — data, not schema
// (TKT-36): extending it is a code change here, not a contract change.
var SupportedLocales = []string{"en", "fr"}

// CacheControlPublicReads is the ADR-004 minutes tier carried by both
// aggregated public reads (event list, event detail).
const CacheControlPublicReads = "public, max-age=300, s-maxage=300"

type Server struct {
	store              store.Store
	pub                events.Publisher
	log                *slog.Logger
	internalCredential string
}

func NewServer(st store.Store, pub events.Publisher, log *slog.Logger, internalCredential string) *Server {
	return &Server{store: st, pub: pub, log: log, internalCredential: internalCredential}
}

// NewRouter mounts the generated routes wrapped in spec request validation
// (ADR-009 §3) on a fresh chi router. /healthz stays outside, in main.
func NewRouter(s *Server) (http.Handler, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		return nil, fmt.Errorf("load openapi spec: %w", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return nil, fmt.Errorf("invalid openapi spec: %w", err)
	}
	validator := oapimiddleware.OapiRequestValidatorWithOptions(doc, &oapimiddleware.Options{
		ErrorHandler: func(w http.ResponseWriter, message string, statusCode int) {
			writeJSON(w, statusCode, Error{Error: message})
		},
	})
	r := chi.NewRouter()
	r.Get("/internal/ticket-types/{id}", s.getTicketType)
	r.Get("/internal/performances/{id}", s.getPublishedPerformance)
	// Unauthenticated public surface: bound request bodies before any read.
	limitBody := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
			next.ServeHTTP(w, req)
		})
	}
	return HandlerWithOptions(s, ChiServerOptions{
		BaseRouter:  r,
		Middlewares: []MiddlewareFunc{validator, limitBody},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		},
	}), nil
}

func (s *Server) getTicketType(w http.ResponseWriter, r *http.Request) {
	if s.internalCredential == "" || r.Header.Get("X-Internal-Token") != s.internalCredential {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid ticket type id"})
		return
	}
	tt, err := s.store.GetTicketType(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": tt.ID, "organizer_id": tt.OrganizerID, "performance_id": tt.PerformanceID,
		"price": map[string]any{"amount": tt.PriceAmount, "currency": tt.Currency},
	})
}

func (s *Server) getPublishedPerformance(w http.ResponseWriter, r *http.Request) {
	if s.internalCredential == "" || r.Header.Get("X-Internal-Token") != s.internalCredential {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid performance id"})
		return
	}
	perf, err := s.store.GetPublishedPerformance(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": perf.ID, "organizer_id": perf.OrganizerID, "capacity": perf.Capacity,
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, Error{Error: "referenced entity not found"})
	case errors.Is(err, store.ErrOrganizerMismatch):
		writeJSON(w, http.StatusBadRequest, Error{Error: "entities must belong to the same organizer"})
	case errors.Is(err, store.ErrNotSellable):
		writeJSON(w, http.StatusConflict, Error{Error: "performance has no ticket type; create one before publishing"})
	case errors.Is(err, store.ErrIllegalTransition):
		writeJSON(w, http.StatusConflict, Error{Error: "illegal performance lifecycle transition"})
	case errors.Is(err, store.ErrClosurePending):
		writeJSON(w, http.StatusConflict, Error{Error: "previous closure event still owed; retry that transition first"})
	default:
		s.log.ErrorContext(r.Context(), "store error", "err", err)
		writeJSON(w, http.StatusInternalServerError, Error{Error: "internal error"})
	}
}

// validateLocalized enforces the i18n-from-birth rule (owner decision,
// 2026-07-12): required localized fields carry every supported locale.
func validateLocalized(field string, text LocalizedString) error {
	for _, loc := range SupportedLocales {
		if text[loc] == "" {
			return fmt.Errorf("%s must include non-empty %q text", field, loc)
		}
	}
	return nil
}

func (s *Server) CreateVenue(w http.ResponseWriter, r *http.Request) {
	var in VenueCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	v, err := s.store.CreateVenue(r.Context(), store.VenueInput{
		OrganizerID: in.OrganizerId,
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

func (s *Server) CreateEvent(w http.ResponseWriter, r *http.Request) {
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
	ev, err := s.store.CreateEvent(r.Context(), store.EventInput{
		OrganizerID: in.OrganizerId,
		Name:        store.LocalizedText(in.Name),
		Description: desc,
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

func (s *Server) CreatePerformance(w http.ResponseWriter, r *http.Request) {
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
	input := store.PerformanceInput{
		OrganizerID: in.OrganizerId,
		EventID:     in.EventId,
		VenueID:     in.VenueId,
		Kind:        kind,
		StartsAt:    in.StartsAt,
		OpensAt:     in.OpensAt,
		ClosesAt:    in.ClosesAt,
		Timezone:    in.Timezone,
		ReEntry:     re,
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
		ArchivedAt: p.ArchivedAt, CreatedAt: p.CreatedAt,
	}
	if p.OperatingDate != nil {
		out.OperatingDate = &openapi_types.Date{Time: *p.OperatingDate}
	}
	return out
}

// PublishPerformance is idempotent on the resource and at-least-once on the
// event: the domain event is emitted only while unacknowledged
// (event_emitted_at null), so a failed emission is retried by re-POSTing
// publish. Crash between DB commit and ack remains the recorded US-004
// deferral (ADR-009).
func (s *Server) PublishPerformance(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	p, needsEmit, err := s.store.PublishPerformance(r.Context(), performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.PerformancePublished(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "domain event emission failed; re-POST publish to retry",
				"performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "performance is published but the domain event was not emitted; retry publish"})
			return
		}
		if err := s.store.MarkPerformanceEventEmitted(r.Context(), p.ID); err != nil {
			// Ack'd but unmarked: the next publish retry may re-emit — that
			// is the at-least-once contract, consumers de-duplicate on id.
			s.log.ErrorContext(r.Context(), "event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

// ArchivePerformance is resource-idempotent and event-at-least-once. If the
// publication marker is still null, publication is emitted and marked before
// the archive event so the lifecycle cannot silently drop a domain event.
func (s *Server) ArchivePerformance(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	p, publishNeedsEmit, archiveNeedsEmit, err := s.store.ArchivePerformance(r.Context(), performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if publishNeedsEmit {
		if err := s.pub.PerformancePublished(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "owed publication event emission failed", "performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, Error{Error: "performance is archived but its publication event was not emitted; retry archive"})
			return
		}
	}
	if archiveNeedsEmit {
		if err := s.pub.PerformanceArchived(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "archive event emission failed", "performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, Error{Error: "performance is archived but the archive event was not emitted; retry archive"})
			return
		}
	}
	// Mark only after every owed event has been emitted. A failure between
	// emissions therefore retries the already-emitted publication too; its
	// deterministic id makes that safe at the stream.
	if publishNeedsEmit {
		if err := s.store.MarkPerformanceEventEmitted(r.Context(), p.ID); err != nil {
			s.log.ErrorContext(r.Context(), "publication event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	if archiveNeedsEmit {
		if err := s.store.MarkPerformanceArchiveEmitted(r.Context(), p.ID); err != nil {
			s.log.ErrorContext(r.Context(), "archive event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

// CloseSlot sets the orthogonal closure attribute to closed and emits the
// closed event at least once while owed (deterministic id per closure_version,
// so retried or raced emissions de-duplicate). Resource-idempotent: closing an
// already-closed slot returns 200 and only re-emits if the event is still owed.
func (s *Server) CloseSlot(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	var in SlotCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	p, needsEmit, err := s.store.CloseSlot(r.Context(), performanceId, in.Reason)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.SlotClosed(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "closure event emission failed; re-POST close to retry",
				"performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "slot is closed but the domain event was not emitted; retry close"})
			return
		}
		if err := s.store.MarkClosureEmitted(r.Context(), p.ID, p.Closure.Version); err != nil {
			s.log.ErrorContext(r.Context(), "closure event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

// ReopenSlot mirrors CloseSlot for the reverse transition.
func (s *Server) ReopenSlot(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	p, needsEmit, err := s.store.ReopenSlot(r.Context(), performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.SlotReopened(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "reopen event emission failed; re-POST reopen to retry",
				"performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "slot is open but the domain event was not emitted; retry reopen"})
			return
		}
		if err := s.store.MarkClosureEmitted(r.Context(), p.ID, p.Closure.Version); err != nil {
			s.log.ErrorContext(r.Context(), "reopen event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

func (s *Server) CreateTicketType(w http.ResponseWriter, r *http.Request) {
	var in TicketTypeCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	tt, err := s.store.CreateTicketType(r.Context(), store.TicketTypeInput{
		OrganizerID:   in.OrganizerId,
		PerformanceID: in.PerformanceId,
		Name:          store.LocalizedText(in.Name),
		PriceAmount:   in.Price.Amount,
		Currency:      in.Price.Currency,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, TicketType{
		Id: tt.ID, OrganizerId: tt.OrganizerID, PerformanceId: tt.PerformanceID,
		Name:      LocalizedString(tt.Name),
		Price:     Money{Amount: tt.PriceAmount, Currency: tt.Currency},
		CreatedAt: tt.CreatedAt,
	})
}

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
	aggs, err := s.store.ListPublishedEvents(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := PublicEventList{Events: make([]PublicEventSummary, 0, len(aggs))}
	for _, agg := range aggs {
		out.Events = append(out.Events, eventSummary(agg, params.Locale))
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
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
	agg, err := s.store.GetPublishedEvent(r.Context(), eventId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	detail := PublicEventDetail{
		Id:           agg.Event.ID,
		OrganizerId:  agg.Event.OrganizerID,
		Name:         resolve(agg.Event.Name, params.Locale),
		Performances: make([]PublicPerformanceDetail, 0, len(agg.Performances)),
	}
	if d := resolve(agg.Event.Description, params.Locale); d != "" {
		detail.Description = &d
	}
	for _, pa := range agg.Performances {
		pd := PublicPerformanceDetail{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance),
			Timezone:    pa.Performance.Timezone,
			Venue:       PublicVenue{Id: pa.Venue.ID, Name: pa.Venue.Name},
			TicketTypes: make([]PublicTicketType, 0, len(pa.TicketTypes)),
		}
		for _, tt := range pa.TicketTypes {
			pd.TicketTypes = append(pd.TicketTypes, PublicTicketType{
				Id: tt.ID, Name: resolve(tt.Name, params.Locale),
				Price: Money{Amount: tt.PriceAmount, Currency: tt.Currency},
			})
		}
		detail.Performances = append(detail.Performances, pd)
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	writeJSON(w, http.StatusOK, detail)
}

// GetOpenAPISpec serves the committed contract byte-identical (ADR-009 §4).
func (s *Server) GetOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(apispec.Spec)
}
