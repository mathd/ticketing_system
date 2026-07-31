package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Seating lookup (TKT-158). Commerce asks whether a claim holds seats, because seatedness
// is a property inventory owns and commerce cannot infer.
//
// It exists so an exchange can refuse a seated line BEFORE any money moves. An exchange
// has no safe partial state — a settled delta plus a half-done exchange leaves the buyer
// holding the wrong thing — which is why this refusal is worth a round trip here, where
// ADR-038 §9 judged the same round trip not worth it for a refund.
func (s *Server) holdSeating(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid hold id"})
		return
	}
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "valid organizer_id required"})
		return
	}
	seated, err := s.st.ClaimIsSeated(r.Context(), org, id)
	if err != nil {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	write(w, http.StatusOK, map[string]any{"hold_id": id, "seated": seated})
}
