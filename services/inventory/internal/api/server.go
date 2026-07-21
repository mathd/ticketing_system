package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/inventory/api"
	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
	"ticketing/shared/contract"
	"ticketing/shared/httpx"
)

// SeatPinner pins/unpins a seat-hold's seats against catalog (TKT-80). It is the
// inventory→catalog boundary; *consumer.CatalogResolver satisfies it. The store never
// calls it (ADR-010: inventory never touches the catalog DB) — the handler does, after
// the hold commits (hold-then-pin).
type SeatPinner interface {
	PinSeats(ctx context.Context, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error
	UnpinSeats(ctx context.Context, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error
}

type Server struct {
	st         *store.Postgres
	credential string
	pinner     SeatPinner
}

func New(st *store.Postgres, credential string, pinner SeatPinner) *Server {
	return &Server{st: st, credential: credential, pinner: pinner}
}

func (s *Server) Router(log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Post("/holds", s.create)
	r.Post("/holds/seats", s.createSeatHold)
	r.Post("/holds/{id}/confirm", s.transition("confirmed"))
	r.Post("/holds/{id}/finalize", s.transition("finalizing"))
	r.Post("/holds/{id}/release", s.transition("released"))
	r.Get("/slots/{id}/availability", s.availability)
	r.Post("/internal/operational-holds", s.internalOnly(s.opPlace))
	r.Post("/internal/operational-holds/{id}/release", s.internalOnly(s.opRelease))
	r.Post("/internal/operational-holds/{id}/convert", s.internalOnly(s.opConvert))
	r.Get("/internal/operational-holds/{id}/history", s.internalOnly(s.opHistory))
	r.Post("/internal/group-reservations", s.internalOnly(s.grpPlace))
	r.Post("/internal/group-reservations/{id}/draw-down", s.internalOnly(s.grpDrawDown))
	r.Get("/internal/group-reservations/{id}/history", s.internalOnly(s.opHistory))
	r.Get("/internal/slots/{id}/availability", s.internalOnly(s.staffAvailability))
	r.Post("/internal/slots/{id}/capacity-adjustments", s.internalOnly(s.adjustCapacity))
	r.Get("/internal/slots/{id}/capacity-adjustments", s.internalOnly(s.capacityHistory))
	r.Put("/internal/slots/{id}/channel-allocations", s.internalOnly(s.replaceAllocations))
	validated, err := contract.RequestValidator(apispec.Spec, r, log)
	if err != nil {
		panic(err)
	}
	return validated
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = 404
	// A dead slot is not a full slot: clients need to tell "sold out, retry later /
	// join waitlist" from "stop offering this slot" (TKT-75 AC2), so these two carry
	// a machine-readable code alongside the message.
	case errors.Is(err, store.ErrSlotArchived):
		write(w, 409, map[string]string{"error": err.Error(), "code": "slot_archived"})
		return
	case errors.Is(err, store.ErrSlotClosed):
		write(w, 409, map[string]string{"error": err.Error(), "code": "slot_closed"})
		return
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrIdempotency):
		code = 409
	}
	write(w, code, map[string]string{"error": err.Error()})
}
func parseUUID(v string) (uuid.UUID, error) { return uuid.Parse(strings.TrimSpace(v)) }
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrganizerID  uuid.UUID `json:"organizer_id"`
		SlotID       uuid.UUID `json:"slot_id"`
		Quantity     int32     `json:"quantity"`
		TicketTypeID uuid.UUID `json:"ticket_type_id"`
		UnitAmount   int64     `json:"unit_amount"`
		Currency     string    `json:"currency"`
		Channel      string    `json:"channel"`
	}
	err := httpx.DecodeJSON(w, r, &in, 1<<20)
	legacy := in.TicketTypeID == uuid.Nil && in.Currency == ""
	if err != nil || in.OrganizerID == uuid.Nil || in.SlotID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 || in.UnitAmount < 0 || len(in.Channel) > 100 || (!legacy && (in.TicketTypeID == uuid.Nil || in.Currency != "EUR")) {
		write(w, 400, map[string]string{"error": "invalid hold request"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	c, replay, err := s.st.CreateHold(r.Context(), in.OrganizerID, in.SlotID, in.TicketTypeID, in.Quantity, in.UnitAmount, in.Currency, in.Channel, key)
	if err != nil {
		problem(w, err)
		return
	}
	code := 201
	if replay {
		code = 200
	}
	write(w, code, c)
}
// seatHoldResponse is a buyer hold plus the seats it holds.
type seatHoldResponse struct {
	store.Claim
	Seats []string `json:"seats"`
}

// createSeatHold holds a specific seat set on a seated slot (TKT-80). It commits the
// inventory hold first, then pins the seats in catalog (hold-then-pin): a success
// response is returned ONLY after the pins exist, and an idempotent replay re-asserts
// them — so commerce can never hold a claim whose seats were not pinned (AC3). A
// deterministic pin rejection releases the hold; a transient one leaves the hold for a
// same-key retry to re-pin (the hold self-expires via TTL if abandoned).
func (s *Server) createSeatHold(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrganizerID    uuid.UUID `json:"organizer_id"`
		SlotID         uuid.UUID `json:"slot_id"`
		SeatIdentities []string  `json:"seat_identities"`
		TicketTypeID   uuid.UUID `json:"ticket_type_id"`
		UnitAmount     int64     `json:"unit_amount"`
		Currency       string    `json:"currency"`
	}
	err := httpx.DecodeJSON(w, r, &in, 1<<20)
	if err != nil || in.OrganizerID == uuid.Nil || in.SlotID == uuid.Nil || len(in.SeatIdentities) == 0 ||
		len(in.SeatIdentities) > store.MaxSeatsPerHold || in.UnitAmount < 0 ||
		in.TicketTypeID == uuid.Nil || in.Currency != "EUR" {
		write(w, 400, map[string]string{"error": "invalid seat hold request"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	ctx := r.Context()
	sh, err := s.st.CreateSeatHold(ctx, in.OrganizerID, in.SlotID, in.TicketTypeID, in.SeatIdentities, in.UnitAmount, in.Currency, key)
	if err != nil {
		if errors.Is(err, store.ErrSeatTaken) {
			write(w, 409, map[string]string{"error": err.Error(), "code": "seat_taken"})
			return
		}
		problem(w, err)
		return
	}
	// Clean up pins left by holds this transaction swept-expired (best-effort — a leaked
	// pin fails safe, blocking a now-safe edit, never orphaning; ADR-031).
	for _, ref := range sh.ExpiredPins {
		_ = s.pinner.UnpinSeats(ctx, in.OrganizerID, ref.SeatMapID, ref.Seats, ref.PinnedBy)
	}
	// Hold-then-pin (+ replay-re-pin): always pin before returning success, replay too.
	if err := s.pinner.PinSeats(ctx, in.OrganizerID, sh.SeatMapID, sh.Seats, sh.PinnedBy); err != nil {
		// The hold exists but its seats are not (fully) pinned — never return success.
		_, _ = s.st.Transition(ctx, in.OrganizerID, sh.Claim.ID, "released")
		if errors.Is(err, consumer.ErrSeatPinRejected) {
			// Deterministic: a named seat is not in the current published map. Retrying
			// cannot help; the hold is released, report conflict.
			write(w, 409, map[string]string{"error": "one or more seats are not available in the current seat map", "code": "seat_unavailable"})
			return
		}
		// Transient: released to keep the invariant; the client may retry the same key.
		write(w, 503, map[string]string{"error": "seat pin temporarily unavailable, retry", "code": "pin_unavailable"})
		return
	}
	code := 201
	if sh.Replay {
		code = 200
	}
	write(w, code, seatHoldResponse{Claim: sh.Claim, Seats: sh.Seats})
}

func (s *Server) transition(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.credential == "" || r.Header.Get("X-Internal-Token") != s.credential {
			write(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		id, err := parseUUID(chi.URLParam(r, "id"))
		if err != nil {
			write(w, 400, map[string]string{"error": "invalid hold id"})
			return
		}
		org, err := parseUUID(r.URL.Query().Get("organizer_id"))
		if err != nil {
			write(w, 400, map[string]string{"error": "organizer_id required"})
			return
		}
		c, err := s.st.Transition(r.Context(), org, id, target)
		if err != nil {
			problem(w, err)
			return
		}
		// A released seated hold frees its seats in-txn; clear their catalog pins too
		// (best-effort — a leaked pin fails safe, ADR-031). Confirm/finalize keep the
		// pins (the seat is still sold/held). No-op for a GA claim.
		if target == "released" {
			if ref, ok, refErr := s.st.SeatPinRef(r.Context(), org, id); refErr == nil && ok {
				_ = s.pinner.UnpinSeats(r.Context(), org, ref.SeatMapID, ref.Seats, ref.PinnedBy)
			}
		}
		write(w, 200, c)
	}
}
func (s *Server) availability(w http.ResponseWriter, r *http.Request) {
	slot, e1 := parseUUID(chi.URLParam(r, "id"))
	org, e2 := parseUUID(r.URL.Query().Get("organizer_id"))
	if e1 != nil || e2 != nil {
		write(w, 400, map[string]string{"error": "valid slot and organizer required"})
		return
	}
	a, err := s.st.Availability(r.Context(), org, slot, r.URL.Query().Get("channel"))
	if err != nil {
		problem(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=5, s-maxage=5")
	write(w, 200, a)
}
