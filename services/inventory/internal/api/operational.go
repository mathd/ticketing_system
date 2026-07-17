package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/httpx"
)

// Staff/internal surface for operational holds (TKT-77 / ADR-023). Reachable only
// service-to-service: the gateway 404s /internal/, and every handler requires the
// internal credential. Request shapes are enforced by the OpenAPI validator; handlers
// re-check only what the spec cannot express.

func (s *Server) internalOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.credential == "" || r.Header.Get("X-Internal-Token") != s.credential {
			write(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return "", false
	}
	return key, true
}

func (s *Server) opPlace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrganizerID uuid.UUID `json:"organizer_id"`
		SlotID      uuid.UUID `json:"slot_id"`
		Quantity    int32     `json:"quantity"`
		Purpose     string    `json:"purpose"`
		Label       string    `json:"label"`
		Actor       string    `json:"actor"`
		Reason      string    `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil || in.OrganizerID == uuid.Nil || in.SlotID == uuid.Nil {
		write(w, 400, map[string]string{"error": "invalid operational hold request"})
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	h, replay, err := s.st.PlaceOperationalHold(r.Context(), in.OrganizerID, in.SlotID, in.Quantity, in.Purpose, in.Label, in.Actor, in.Reason, key)
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

func (s *Server) opRelease(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid hold id"})
		return
	}
	var in struct {
		OrganizerID uuid.UUID `json:"organizer_id"`
		Quantity    int32     `json:"quantity"`
		Actor       string    `json:"actor"`
		Reason      string    `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil || in.OrganizerID == uuid.Nil {
		write(w, 400, map[string]string{"error": "invalid release request"})
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	h, _, err := s.st.ReleaseOperational(r.Context(), in.OrganizerID, id, in.Quantity, in.Actor, in.Reason, key)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, h)
}

func (s *Server) opConvert(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid hold id"})
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
		write(w, 400, map[string]string{"error": "invalid convert request"})
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	res, replay, err := s.st.ConvertOperational(r.Context(), in.OrganizerID, id, in.TicketTypeID, in.SlotID, in.Quantity, in.UnitAmount, in.Currency, in.Actor, in.Reason, key)
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

func (s *Server) opHistory(w http.ResponseWriter, r *http.Request) {
	id, e1 := parseUUID(chi.URLParam(r, "id"))
	org, e2 := parseUUID(r.URL.Query().Get("organizer_id"))
	if e1 != nil || e2 != nil {
		write(w, 400, map[string]string{"error": "valid hold and organizer required"})
		return
	}
	entries, err := s.st.History(r.Context(), org, id)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, entries)
}

// replaceAllocations atomically replaces the pool's channel allocation set (TKT-78 /
// ADR-024). PUT of the complete desired state — repeating the request is a no-op, so no
// idempotency key is needed.
func (s *Server) replaceAllocations(w http.ResponseWriter, r *http.Request) {
	slot, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid slot id"})
		return
	}
	var in struct {
		OrganizerID uuid.UUID                 `json:"organizer_id"`
		Allocations []store.ChannelAllocation `json:"allocations"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil || in.OrganizerID == uuid.Nil || in.Allocations == nil {
		write(w, 400, map[string]string{"error": "invalid allocation request"})
		return
	}
	seen := map[string]bool{}
	for _, a := range in.Allocations {
		if seen[a.Channel] {
			write(w, 400, map[string]string{"error": "duplicate channel " + a.Channel})
			return
		}
		seen[a.Channel] = true
	}
	out, err := s.st.ReplaceChannelAllocations(r.Context(), in.OrganizerID, slot, in.Allocations)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, map[string]any{"slot_id": slot, "allocations": out})
}

// adjustCapacity is the staff capacity-adjustment operation (TKT-76 / ADR-026): raises
// apply freely; a cut below demand clamps to the invariant floor and blocks new claims
// until demand drains to the target — forward-only, never releasing anything.
func (s *Server) adjustCapacity(w http.ResponseWriter, r *http.Request) {
	slot, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid slot id"})
		return
	}
	var in struct {
		OrganizerID uuid.UUID `json:"organizer_id"`
		Capacity    int32     `json:"capacity"`
		Actor       string    `json:"actor"`
		Reason      string    `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil || in.OrganizerID == uuid.Nil {
		write(w, 400, map[string]string{"error": "invalid capacity adjustment request"})
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	adj, replay, err := s.st.AdjustCapacity(r.Context(), in.OrganizerID, slot, in.Capacity, in.Actor, in.Reason, key)
	if err != nil {
		problem(w, err)
		return
	}
	code := 201
	if replay {
		code = 200
	}
	write(w, code, adj)
}

func (s *Server) capacityHistory(w http.ResponseWriter, r *http.Request) {
	slot, e1 := parseUUID(chi.URLParam(r, "id"))
	org, e2 := parseUUID(r.URL.Query().Get("organizer_id"))
	if e1 != nil || e2 != nil {
		write(w, 400, map[string]string{"error": "valid slot and organizer required"})
		return
	}
	entries, err := s.st.CapacityHistory(r.Context(), org, slot)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, entries)
}

func (s *Server) staffAvailability(w http.ResponseWriter, r *http.Request) {
	slot, e1 := parseUUID(chi.URLParam(r, "id"))
	org, e2 := parseUUID(r.URL.Query().Get("organizer_id"))
	if e1 != nil || e2 != nil {
		write(w, 400, map[string]string{"error": "valid slot and organizer required"})
		return
	}
	a, err := s.st.StaffAvailability(r.Context(), org, slot)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, a)
}
