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
		// Through problem(), like every other handler here (TKT-305). This used to
		// flatten EVERY store error to 404, which answers "no such claim" when the
		// truth is "inventory could not look" — and the store already draws that line
		// for us, returning ErrNotFound for no rows and the driver's error otherwise.
		//
		// It matters because of what consumes this: an exchange refuses a SEATED source
		// before money moves. A 404 during an outage happens to fail safe today, since
		// commerce reads it as "not seated" and proceeds — correct by accident, and one
		// caller away from an exchange settling against a seated line on the strength of
		// a fact nobody established.
		problem(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"hold_id": id, "seated": seated})
}
