package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"ticketing/shared/httpx"

	commercestore "ticketing/services/commerce/internal/store"
)

func (s *Server) internalOrderSeats(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid order id"})
		return
	}
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil || org == uuid.Nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "organizer_id required"})
		return
	}
	seats, err := commercestore.ReadOrderSeats(r.Context(), s.db, org, order)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			write(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		slog.Default().ErrorContext(r.Context(), "read internal order seats", "err", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": "read internal order seats"})
		return
	}
	write(w, http.StatusOK, map[string]any{
		"order_id": seats.OrderID, "organizer_id": seats.OrganizerID,
		"slot_id": seats.SlotID, "ticket_type_id": seats.TicketTypeID,
		"quantity": seats.Quantity, "seated": seats.Seated,
		"seat_identities": seats.SeatIdentities,
	})
}
