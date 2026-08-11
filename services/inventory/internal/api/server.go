package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apispec "ticketing/services/inventory/api"
	"ticketing/services/inventory/internal/availability"
	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
	"ticketing/shared/cachetier"
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

// CacheControlPublicAvailability is ADR-004 rule 1's seconds tier, carried by the
// public availability read — the tier where staleness is most load-bearing during
// an on-sale. TKT-110 declared it in the OpenAPI document, so it is now a
// contract value enforced at runtime on both sides (the '200' response declares
// it required, with this string as its only allowed value): changing it here
// without changing the spec fails the contract, and vice versa.
// Derived from the tier registry rather than written out (TKT-204), so the
// lifetime a future in-memory availability cache honours (TKT-205) and the
// lifetime this header advertises are one number. A var rather than a const only
// because Go cannot make a function-derived string constant.
var CacheControlPublicAvailability = cachetier.Seconds.CacheControl()

// CacheControlPublicSeatOccupancy is the same ADR-004 seconds tier for the seat
// occupancy read (TKT-172). Same value, separate constant and separate header
// declaration (SeatOccupancyCacheControl): ADR-028 fails closed per declaration,
// so sharing one would let a handler that stopped emitting the tier on one
// operation hide behind the other still emitting it.
var CacheControlPublicSeatOccupancy = cachetier.Seconds.CacheControl()

// availabilityReader is the public display read, served from memory (TKT-205).
//
// Deliberately narrow, and deliberately NOT the way Server reaches the store for
// anything else: `st` stays the concrete *store.Postgres for every write, every
// claim and the staff read. Two paths to truth, and which is which is visible at
// the call site — the claim path cannot reach a cached number even by accident,
// which is ADR-002's rule that correctness lives at claim time.
type availabilityReader interface {
	Read(ctx context.Context, org, slot uuid.UUID, channel string) (availability.Read, error)
	// SetEnabled and Status are the operator kill-switch (TKT-210). They are on
	// THIS interface, not a separate controller, so the switch cannot address a
	// different object than the read path consults.
	SetEnabled(bool)
	Status() availability.Status
}

type Server struct {
	st         *store.Postgres
	credential string
	pinner     SeatPinner
	avail      availabilityReader
}

func New(st *store.Postgres, credential string, pinner SeatPinner) *Server {
	return &Server{st: st, credential: credential, pinner: pinner, avail: availability.New(st)}
}

// NewWithAvailability injects the display-read collaborator. Tests use it to
// count loads and to prove the claim path never touches it.
func NewWithAvailability(st *store.Postgres, credential string, pinner SeatPinner, avail availabilityReader) *Server {
	return &Server{st: st, credential: credential, pinner: pinner, avail: avail}
}

func (s *Server) Router(log *slog.Logger, validateResponses bool) http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Post("/holds", s.create)
	r.Post("/holds/seats", s.createSeatHold)
	r.Get("/slots/{id}/availability", s.availability)
	r.Get("/slots/{id}/seat-occupancy", s.seatOccupancy)
	r.Post("/internal/holds/{id}/confirm", s.internalOnly(s.transition("confirmed")))
	r.Post("/internal/holds/{id}/finalize", s.internalOnly(s.transition("finalizing")))
	r.Post("/internal/holds/{id}/release", s.internalOnly(s.transition("released")))
	r.Post("/internal/holds/{id}/refund-capacity", s.internalOnly(s.refundCapacity))
	r.Get("/internal/holds/{id}/seating", s.internalOnly(s.holdSeating))
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
	r.Get("/internal/cache-control", s.internalOnly(s.cacheControlStatus))
	r.Put("/internal/cache-control", s.internalOnly(s.cacheControlSet))
	validated, err := contract.RequestValidator(apispec.Spec, r, log, validateResponses)
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
	// A closed sales window is not a sold-out channel and not a dead slot: the
	// caller should wait for the window, not join a waitlist (TKT-238). It carries
	// a code for the same reason the two above do, and `slot_closed` deliberately
	// is not reused — that mirrors catalog's offering closure on the whole SLOT,
	// while this is one allocation row being temporally shut.
	case errors.Is(err, store.ErrChannelWindowClosed):
		write(w, 409, map[string]string{"error": err.Error(), "code": "channel_window_closed"})
		return
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrIdempotency), errors.Is(err, store.ErrPoolKindMismatch):
		// ErrPoolKindMismatch: a quantity claim hit a seated pool (or a seat claim a GA
		// pool) — a 409 conflict, not a 500.
		code = 409
	case errors.Is(err, store.ErrSeatSetInvalid):
		// Empty/oversized/whitespace seat set that slipped past the shape checks — a 400.
		code = 400
	}
	// Only a MAPPED error may speak. Everything reaching the default is an
	// unrecognized error on a public, credential-free route (/holds is reachable
	// through the gateway by anyone), and in this store that is a wrapped pgx
	// error — table, column and constraint names handed to the caller. The
	// sentinels above are hand-written text and stay verbatim; the rest gets
	// catalog's answer (writeStoreError): a static body, the real error logged.
	if code == http.StatusInternalServerError {
		slog.Error("inventory store error", "error", err)
		write(w, code, map[string]string{"error": "internal error"})
		return
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
		// A contended seat set names the seats that actually lost (TKT-173). The
		// caller is a buyer's reservation, and re-rendering a picker needs the
		// identities, not the fact — see SeatTakenError. The typed error still
		// unwraps to ErrSeatTaken, so this branch is checked first.
		// A stranded seat is FREE and was never requested — the opposite relationship to
		// the request from seat_taken, which is why it needs its own code (ADR-041).
		var orphaned *store.SeatOrphanedError
		if errors.As(err, &orphaned) {
			write(w, 409, map[string]any{
				"error": err.Error(), "code": "orphaned_seats", "seat_identities": orphaned.Seats,
			})
			return
		}
		var taken *store.SeatTakenError
		if errors.As(err, &taken) {
			write(w, 409, map[string]any{
				"error": err.Error(), "code": "seat_taken", "seat_identities": taken.Seats,
			})
			return
		}
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
		if errors.Is(err, consumer.ErrSeatPinRejected) {
			// Deterministic: a named seat is not in the current published map. Retrying
			// cannot help, and the batch pin is all-or-nothing so nothing landed — release
			// the invalid hold and report conflict.
			_, _ = s.st.Transition(ctx, in.OrganizerID, sh.Claim.ID, "released")
			write(w, 409, map[string]string{"error": "one or more seats are not available in the current seat map", "code": "seat_unavailable"})
			return
		}
		// Transient: do NOT release — the hold stays held (unpinned), a same-key retry
		// re-pins it, and the TTL reclaims it if the client abandons. Releasing here would
		// both break the replay-re-pin retry and, under a concurrent same-key retry that
		// pinned successfully, free seats out from under a request that returned success.
		write(w, 503, map[string]string{"error": "seat pin temporarily unavailable, retry", "code": "pin_unavailable"})
		return
	}
	code := 201
	if sh.Replay {
		code = 200
	}
	write(w, code, seatHoldResponse{Claim: sh.Claim, Seats: sh.Seats})
}

// transition is mounted behind internalOnly (TKT-124) — it does not re-check the
// credential itself. Mount it anywhere else and it is unguarded.
func (s *Server) transition(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	read, err := s.avail.Read(r.Context(), org, slot, r.URL.Query().Get("channel"))
	if err != nil {
		problem(w, err)
		return
	}
	w.Header().Set("Cache-Control", CacheControlPublicAvailability)
	// Age is what stops the two staleness budgets stacking. The tier declares this
	// response publicly cacheable for five seconds; without Age, an entry already
	// four seconds old inside this process would hand a conformant client another
	// full five, doubling what a buyer can observe and breaking the tier the epic
	// promises. Rounded UP, so the number is never optimistic.
	//
	// Varying Cache-Control by remaining freshness — what the storefront
	// middleware does for pages — is not available here: ADR-028 makes this header
	// a required single-valued enum on this operation, so a third value is a 500.
	w.Header().Set("Age", strconv.Itoa(int(math.Ceil(read.Age.Seconds()))))
	write(w, 200, read.Value)
}

// seatOccupancy answers which seats a seated slot cannot sell right now (TKT-172).
// The two refusals stay distinguishable through the already-shipped problem()
// branches: an unknown or wrong-organizer slot is ErrNotFound → 404, a GA slot is
// ErrPoolKindMismatch → 409. Collapsing them would leave a caller unable to tell
// "no such slot" from "this slot has no seats".
func (s *Server) seatOccupancy(w http.ResponseWriter, r *http.Request) {
	slot, e1 := parseUUID(chi.URLParam(r, "id"))
	org, e2 := parseUUID(r.URL.Query().Get("organizer_id"))
	if e1 != nil || e2 != nil {
		write(w, 400, map[string]string{"error": "valid slot and organizer required"})
		return
	}
	occ, err := s.st.SeatOccupancy(r.Context(), org, slot)
	if err != nil {
		problem(w, err)
		return
	}
	w.Header().Set("Cache-Control", CacheControlPublicSeatOccupancy)
	write(w, 200, occ)
}
