package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Comped-order voids (TKT-171). A void reverses an order with no money leg: it
// voids the tickets and returns the capacity, and it moves no money.
//
// A SEPARATE endpoint from the refund rather than a mode of it, because the
// owner's decision was that a comped reversal is not a refund. Two consequences
// visible here: this request body has no quantity, amount or currency to carry,
// and the response has no money fields to report. A caller cannot instruct a
// zero-amount refund through this route because there is nothing on the wire to
// instruct it with (AGENTS.md — make it unsubmittable, not validated).
//
// Same 404-not-401 convention as the refund route: it does not confirm the route
// exists to a caller without a credential.

// voidRequest carries only attribution. The quantity is whole-order and comes
// from the reservation under the order row lock in BindOrderVoid — a client that
// cannot state one cannot forge one.
type voidRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
}

// voidProblem maps a store error onto the status the contract declares. Kept
// separate from the handler so the whole mapping is table-testable without a
// database, mirroring refundProblem.
func voidProblem(err error) (int, string) {
	switch {
	case errors.Is(err, commercestore.ErrVoidHasMoney):
		// The routing answer: this order IS reversible, just not here. 409 rather
		// than 400 because nothing about the request is malformed — the order is
		// simply the refund path's, and telling the caller which path to take is
		// the useful answer.
		return http.StatusConflict, "order has captured money and must be refunded, not voided"
	case errors.Is(err, commercestore.ErrOrderNotVoidable):
		return http.StatusConflict, "only a completed, unexchanged order can be voided"
	case errors.Is(err, commercestore.ErrRefundConflict):
		return http.StatusConflict, "void conflicts with an existing request"
	default:
		return http.StatusInternalServerError, "persist void"
	}
}

func (s *Server) voidOrder(w http.ResponseWriter, r *http.Request) {
	if !s.staffOrInternal(r) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	// The `cancel:` namespace belongs to the cancellation runner, as on the refund
	// route. The reason is narrower here but real: the void's IDENTITY is derived
	// from the order rather than the key, so a borrowed key does not collide on
	// identity — it collides on FINGERPRINT, which covers actor and reason. A
	// staff void under a cancellation key would bind first with a human actor, and
	// every later cancellation run would then read its own void row, disagree with
	// the fingerprint, and report the order failed forever.
	if strings.HasPrefix(key, commercestore.CancellationRefundKeyPrefix) {
		write(w, 400, map[string]string{"error": "Idempotency-Key uses a reserved prefix"})
		return
	}
	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid order"})
		return
	}
	var in voidRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || strings.TrimSpace(in.Actor) == "" || strings.TrimSpace(in.Reason) == "" {
		write(w, 400, map[string]string{"error": "invalid void"})
		return
	}

	result, err := s.refunds.Void(r.Context(), commercestore.VoidRequest{
		OrderID: order, OrganizerID: in.OrganizerID,
		IdempotencyKey: key, Actor: in.Actor, Reason: in.Reason,
	})
	if err != nil {
		code, message := voidProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "void order", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	// The two obligations are reported independently and honestly: a void whose
	// tickets are void but whose seat has not come back is a real state, and
	// rounding it up to "done" is what would hide an outstanding obligation.
	write(w, http.StatusOK, map[string]any{
		"void_id":           result.Void.ID.String(),
		"order_id":          result.Void.OrderID.String(),
		"quantity":          result.Void.Quantity,
		"tickets_voided":    result.Void.TicketsVoided,
		"capacity_returned": result.Void.CapacityReturned,
		"replay":            result.Replay,
	})
}
