package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/shared/httpx"
)

func (s *Server) orderAdmission(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid order"})
		return
	}
	organizerID, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil || organizerID == uuid.Nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid organizer_id"})
		return
	}

	result, err := s.st.OrderAdmission(r.Context(), organizerID, orderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			write(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		write(w, http.StatusInternalServerError, map[string]string{"error": "read order admission"})
		return
	}
	write(w, http.StatusOK, map[string]any{"admitted": result.Admitted, "issued_count": result.IssuedCount})
}
