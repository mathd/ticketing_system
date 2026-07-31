package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/httpx"
)

// Refund capacity return (TKT-161, ADR-038). Internal: commerce is the only caller, and
// it drives this after the money has moved and the tickets have been voided.

type refundCapacityRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
	RefundID    uuid.UUID `json:"refund_id"`
	Quantity    int32     `json:"quantity"`
}

// refundCapacityProblem maps a store error onto a status this operation declares. Separate
// from the handler so the whole mapping is table-testable without a database — and so an
// undeclared status cannot slip in (ADR-028).
func refundCapacityProblem(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrPartialSeatedReturn):
		return http.StatusConflict, "a partial return of a seated claim cannot identify which seats to free"
	case errors.Is(err, store.ErrRefundReturnExceedsClaim):
		return http.StatusConflict, "refund would return more than the claim's unreturned quantity"
	case errors.Is(err, store.ErrRefundReturnNotConfirmed):
		return http.StatusConflict, "only a confirmed buyer claim can return capacity"
	case errors.Is(err, store.ErrRefundReturnConflict):
		return http.StatusConflict, "refund identity reused with a different request"
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not found"
	default:
		return http.StatusInternalServerError, "return refunded capacity"
	}
}

func (s *Server) refundCapacity(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid hold id"})
		return
	}
	var in refundCapacityRequest
	if err := httpx.DecodeJSON(w, r, &in, 1<<20); err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if in.OrganizerID == uuid.Nil || in.RefundID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 {
		write(w, http.StatusBadRequest, map[string]string{"error": "organizer_id, refund_id and quantity required"})
		return
	}

	// SeatPinRef is read BEFORE the return, because the return releases the claim_seats
	// rows it reads from — afterwards there is nothing left to name in the unpin.
	pin, seated, pinErr := s.st.SeatPinRef(r.Context(), in.OrganizerID, id)

	out, err := s.st.ReturnRefundedCapacity(r.Context(), in.OrganizerID, id, in.RefundID, in.Quantity)
	if err != nil {
		code, message := refundCapacityProblem(err)
		write(w, code, map[string]string{"error": message})
		return
	}

	// Unpin AFTER the commit, never inside it: a catalog HTTP call cannot join the
	// PostgreSQL transaction, and holding the hot pool lock across another service is the
	// thing ADR-010 exists to prevent. Best-effort, and the failure direction is
	// deliberate (ADR-031): a leaked pin blocks a seat-map edit, which is recoverable,
	// while unpinning a live seat orphans a sold one.
	//
	// A failure here does NOT make the response retryable, and the 200 is not a lie: the
	// capacity IS back and the seats ARE released — both committed above. Only the
	// catalog-side pin is stale, and a pin is not capacity. Reporting the whole return as
	// outstanding would make commerce re-drive a mutation that already happened, to fix
	// something it cannot see.
	//
	// The remedy is `reconcile-pins`, and THIS ticket is what makes it work here: a fully
	// returned confirmed claim now reads as dead (ReconcileSeatClaimStates), so the
	// command can reclaim the leak. Before that change the claim never reached a terminal
	// status and the leak was permanent.
	//
	// An earlier version of this comment claimed a commerce replay would retry the unpin.
	// It would not — commerce marks capacity_returned_at on the 200 and its replay path
	// exits early — and the adversarial review was right to call that out.
	if seated && pinErr == nil && len(pin.Seats) > 0 {
		_ = s.pinner.UnpinSeats(r.Context(), in.OrganizerID, pin.SeatMapID, pin.Seats, pin.PinnedBy)
	}
	write(w, http.StatusOK, map[string]any{
		"hold_id": out.ClaimID, "quantity": out.Quantity,
		"unreturned_quantity": out.UnreturnedQuantity, "replay": out.Replay,
	})
}
