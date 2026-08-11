package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
	"ticketing/shared/httpx"
)

// Ticket voiding on refund (TKT-157, ADR-038). Internal: the gateway denies
// /internal/* at the edge, and a missing or wrong token reads as 404 — the convention
// commerce already uses for its staff operations, so an unauthenticated caller cannot
// even learn the route exists.
//
// Commerce is the only caller. It drives this from its refund path after the money
// has moved, and replays it on retry.

type ticketRefundRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
	RefundID    uuid.UUID `json:"refund_id"`
	Quantity    int32     `json:"quantity"`
}

func (s *Server) refundTickets(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid order"})
		return
	}
	var in ticketRefundRequest
	if err := httpx.DecodeJSON(w, r, &in, 4<<10); err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if in.OrganizerID == uuid.Nil || in.RefundID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 {
		write(w, http.StatusBadRequest, map[string]string{"error": "organizer_id, refund_id and quantity required"})
		return
	}

	batch, err := s.st.RefundOrderTickets(r.Context(), in.OrganizerID, order, in.RefundID, in.Quantity)
	switch {
	case errors.Is(err, store.ErrTicketsNotIssued):
		// 503, not 404 or 409: issuance is asynchronous, so "not enough tickets" is
		// usually "not yet". The caller must keep the obligation and retry, never read
		// this as "there was nothing to void".
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "tickets not issued yet"})
		return
	case errors.Is(err, store.ErrRefundBatchConflict):
		write(w, http.StatusConflict, map[string]string{"error": "refund id reused with a different request"})
		return
	case err != nil:
		write(w, http.StatusInternalServerError, map[string]string{"error": "void tickets"})
		return
	}
	ids := make([]string, 0, len(batch.TicketIDs))
	for _, id := range batch.TicketIDs {
		ids = append(ids, id.String())
	}
	write(w, http.StatusOK, map[string]any{"ticket_ids": ids, "replay": batch.Replay})
}
