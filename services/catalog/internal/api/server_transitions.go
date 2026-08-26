package api

// Slot and group lifecycle handlers: publish, archive, close and reopen, plus
// ticket-type creation. Resource-idempotent and at-least-once on the event
// (ADR-018); a grouped member refuses its own transition.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"ticketing/services/catalog/internal/store"

	"github.com/google/uuid"
)

// PublishPerformance is idempotent on the resource and at-least-once on the
// event: the domain event is emitted only while unacknowledged
// (event_emitted_at null), so a failed emission is retried by re-POSTing
// publish. Crash between DB commit and ack remains the recorded US-004
// deferral (ADR-009).
//
// **Organizer-scoped since TKT-251 — TKT-199 is closed.** The organizer comes
// from the verified assertion via organizerFor, never from the request, and the
// store's UPDATE carries `organizer_id = $2`. A cross-tenant caller gets
// ErrNotFound, indistinguishable from "no such slot": the store re-asserts
// ownership before classifying the no-op, because the classification is itself
// an information channel (see PublishPerformance in store/postgres.go).
// Pinned by TestPerformanceTransitionsAreScopedToTheOwningOrganizer — deleting
// the predicate makes the attacker's publish succeed and only that test fails.
func (s *Server) PublishPerformance(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	p, needsEmit, err := s.store.PublishPerformance(r.Context(), organizerID, performanceId)
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
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	p, publishNeedsEmit, archiveNeedsEmit, err := s.store.ArchivePerformance(r.Context(), organizerID, performanceId)
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

func (s *Server) PublishSeries(w http.ResponseWriter, r *http.Request, seriesId SeriesId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	items, err := s.store.PublishSeries(r.Context(), organizerID, seriesId)
	if err != nil {
		s.writeSeriesTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "series is published but a member event is still owed; retry publish"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "series publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeSeriesResult(w, seriesId, items)
}

func (s *Server) ArchiveSeries(w http.ResponseWriter, r *http.Request, seriesId SeriesId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	items, err := s.store.ArchiveSeries(r.Context(), organizerID, seriesId)
	if err != nil {
		s.writeSeriesTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "series is archived but a member publication event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "series publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
		if item.ArchiveNeedsEmit {
			if err = s.pub.PerformanceArchived(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "series is archived but a member archive event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceArchiveEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "series archive emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeSeriesResult(w, seriesId, items)
}

func (s *Server) writeSeriesResult(w http.ResponseWriter, id uuid.UUID, items []store.SeriesTransition) {
	performances := make([]Performance, 0, len(items))
	for _, item := range items {
		performances = append(performances, performanceToAPI(item.Performance))
	}
	writeJSON(w, http.StatusOK, SeriesLifecycleResult{SeriesId: id, Performances: performances})
}

func (s *Server) writeSeriesTransitionError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *store.SeriesTransitionConflict
	if errors.As(err, &conflict) {
		id := conflict.PerformanceID
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "series transition blocked", Reason: conflict.Reason, BlockingPerformanceId: &id})
		return
	}
	if errors.Is(err, store.ErrEmptySeries) {
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "series transition blocked", Reason: "series has no members"})
		return
	}
	s.writeStoreError(w, r, err)
}

func (s *Server) PublishFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	items, err := s.store.PublishFestival(r.Context(), organizerID, festivalId)
	if err != nil {
		s.writeFestivalTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "festival is published but a member event is still owed; retry publish"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "festival publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeFestivalResult(w, festivalId, items)
}

func (s *Server) ArchiveFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	items, err := s.store.ArchiveFestival(r.Context(), organizerID, festivalId)
	if err != nil {
		s.writeFestivalTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "festival is archived but a member publication event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "festival publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
		if item.ArchiveNeedsEmit {
			if err = s.pub.PerformanceArchived(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "festival is archived but a member archive event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceArchiveEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "festival archive emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeFestivalResult(w, festivalId, items)
}

func (s *Server) writeFestivalResult(w http.ResponseWriter, id uuid.UUID, items []store.SeriesTransition) {
	performances := make([]Performance, 0, len(items))
	for _, item := range items {
		performances = append(performances, performanceToAPI(item.Performance))
	}
	writeJSON(w, http.StatusOK, FestivalLifecycleResult{FestivalId: id, Performances: performances})
}

func (s *Server) writeFestivalTransitionError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *store.FestivalTransitionConflict
	if errors.As(err, &conflict) {
		id := conflict.PerformanceID
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "festival transition blocked", Reason: conflict.Reason, BlockingPerformanceId: &id})
		return
	}
	if errors.Is(err, store.ErrEmptyFestival) {
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "festival transition blocked", Reason: "festival has no members"})
		return
	}
	s.writeStoreError(w, r, err)
}

// CloseSlot sets the orthogonal closure attribute to closed and emits the
// closed event at least once while owed (deterministic id per closure_version,
// so retried or raced emissions de-duplicate). Any publication event still owed
// for this slot is emitted first, so a closure never overtakes the slot's
// publication. Resource-idempotent: closing an already-closed slot returns 200
// and only re-emits while an event is still owed.
func (s *Server) CloseSlot(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	var in SlotCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	p, publishNeedsEmit, closureNeedsEmit, err := s.store.CloseSlot(r.Context(), organizerID, performanceId, in.Reason)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.emitClosure(w, r, p, publishNeedsEmit, closureNeedsEmit, s.pub.SlotClosed, "close")
}

// ReopenSlot mirrors CloseSlot for the reverse transition.
func (s *Server) ReopenSlot(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	p, publishNeedsEmit, closureNeedsEmit, err := s.store.ReopenSlot(r.Context(), organizerID, performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.emitClosure(w, r, p, publishNeedsEmit, closureNeedsEmit, s.pub.SlotReopened, "reopen")
}

// emitClosure emits any owed publication first, then the closure event, marking
// each only after it is emitted — the publication-before-closure ordering and
// at-least-once discipline that ArchivePerformance already uses. A failure
// between emissions retries the already-emitted publication too; its
// deterministic id makes that safe at the stream.
func (s *Server) emitClosure(w http.ResponseWriter, r *http.Request, p store.Performance,
	publishNeedsEmit, closureNeedsEmit bool, emitClosureEvent func(context.Context, store.Performance) error, verb string) {
	if publishNeedsEmit {
		if err := s.pub.PerformancePublished(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "owed publication event emission failed", "performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "slot state changed but its publication event was not emitted; retry " + verb})
			return
		}
		if err := s.store.MarkPerformanceEventEmitted(r.Context(), p.ID); err != nil {
			s.log.ErrorContext(r.Context(), "publication event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	if closureNeedsEmit {
		if err := emitClosureEvent(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "closure event emission failed; retry to re-emit",
				"performance_id", p.ID, "verb", verb, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "slot state changed but the closure event was not emitted; retry " + verb})
			return
		}
		if err := s.store.MarkClosureEmitted(r.Context(), p.ID, p.Closure.Version); err != nil {
			s.log.ErrorContext(r.Context(), "closure event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

func (s *Server) CreateTicketType(w http.ResponseWriter, r *http.Request, params CreateTicketTypeParams) {
	var in TicketTypeCreate
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
	tt, err := s.store.CreateTicketType(r.Context(), store.TicketTypeInput{
		OrganizerID:    organizerID,
		PerformanceID:  in.PerformanceId,
		Name:           store.LocalizedText(in.Name),
		PriceAmount:    in.Price.Amount,
		Currency:       in.Price.Currency,
		IdempotencyKey: string(params.IdempotencyKey),
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
