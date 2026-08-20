package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/httpx"
)

// Staff/internal surface for group/agency reservations (TKT-79 / ADR-027). Same posture
// as the operational-hold surface: gateway 404s /internal/, internal credential required,
// request shapes enforced by the OpenAPI validator.

func (s *Server) grpPlace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrganizerID  uuid.UUID `json:"organizer_id"`
		SlotID       uuid.UUID `json:"slot_id"`
		Quantity     int32     `json:"quantity"`
		Counterparty string    `json:"counterparty"`
		ExpiresAt    time.Time `json:"expires_at"`
		Channel      string    `json:"channel"`
		// Exact, unnormalized, like the hold path's (ADR-064).
		PresaleCode string `json:"presale_code"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil || in.OrganizerID == uuid.Nil || in.SlotID == uuid.Nil || in.ExpiresAt.IsZero() {
		write(w, 400, map[string]string{"error": "invalid group reservation request"})
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	h, replay, err := s.st.PlaceGroupReservation(r.Context(), in.OrganizerID, in.SlotID, in.Quantity, in.Counterparty, in.ExpiresAt, in.Channel, in.Actor, in.Reason, key, store.WithPresaleCode(in.PresaleCode))
	if err != nil {
		problem(w, err)
		return
	}
	code := 201
	if replay {
		code = 200
	}
	write(w, code, h)
}

func (s *Server) grpDrawDown(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid reservation id"})
		return
	}
	var in struct {
		OrganizerID  uuid.UUID `json:"organizer_id"`
		SlotID       uuid.UUID `json:"slot_id"`
		Quantity     int32     `json:"quantity"`
		TicketTypeID uuid.UUID `json:"ticket_type_id"`
		UnitAmount   int64     `json:"unit_amount"`
		Currency     string    `json:"currency"`
		Actor        string    `json:"actor"`
		Reason       string    `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil || in.OrganizerID == uuid.Nil || in.SlotID == uuid.Nil || in.TicketTypeID == uuid.Nil || in.UnitAmount < 0 || in.Currency != "EUR" {
		write(w, 400, map[string]string{"error": "invalid draw-down request"})
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	res, replay, err := s.st.DrawDownGroupReservation(r.Context(), in.OrganizerID, id, in.TicketTypeID, in.SlotID, in.Quantity, in.UnitAmount, in.Currency, in.Actor, in.Reason, key)
	if err != nil {
		problem(w, err)
		return
	}
	code := 201
	if replay {
		code = 200
	}
	write(w, code, res)
}
