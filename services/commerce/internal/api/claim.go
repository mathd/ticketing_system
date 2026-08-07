package api

// Claiming a past guest order (TKT-223 / US-A4). See ADR-049 § TKT-223.

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// claimGuestOrderFn is the store seam, so the handler's authorization can be
// tested without a database.
var claimGuestOrderFn = commercestore.ClaimGuestOrder

// notClaimable is the ONE refusal body. Nonexistent, not completed, and claimed
// by somebody else share it: telling them apart hands a caller probing references
// an oracle for which are real, complete and unclaimed.
const notClaimable = "not found"

type orderClaimRequest struct {
	GuestOrderRef string `json:"guest_order_ref"`
}

func (s *Server) claimGuestOrder(w http.ResponseWriter, r *http.Request) {
	var in orderClaimRequest
	if !decode(w, r, &in) {
		return
	}
	ref, err := uuid.Parse(in.GuestOrderRef)
	if err != nil {
		// 400 and not 404: this is request validation, not a statement about
		// whether any order exists. A caller who cannot spell a uuid has learned
		// nothing about the order book.
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid order reference"})
		return
	}

	// Identity comes from the assertion and only from it. The body deliberately
	// carries no customer id — accepting one would be attribution forgery.
	caller, err := customerFromRequest(s.assertionKey, r.Header.Get(assertionHeader), time.Now())
	if err != nil || !caller.Valid {
		write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	// Keyed on the CALLER, not on the order reference (TKT-224). A guesser varies
	// the reference and keeps one identity, so a per-reference bucket would hand
	// every guess a fresh budget — no bound at all — while filling the key map.
	// The caller is the one thing that stays fixed across an attempt sequence.
	//
	// After the assertion check because it needs the caller, and that ordering is
	// safe here in a way it would not be for the address-keyed operations: this
	// refusal is about who is asking, not about whether the order exists, so a 429
	// discloses nothing about the order book.
	if !s.allowSubject(w, "customer:"+caller.UUID.String()) {
		return
	}

	order, err := claimGuestOrderFn(r.Context(), s.db, ref, caller.UUID)
	switch {
	case errors.Is(err, commercestore.ErrOrderNotClaimable):
		write(w, http.StatusNotFound, map[string]string{"error": notClaimable})
		return
	case err != nil:
		// The customer is safe to log; the order reference is not (ADR-012).
		slog.Default().ErrorContext(r.Context(), "claim guest order", "customer_id", caller.UUID, "err", err)
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}

	write(w, http.StatusOK, map[string]any{
		"order_id":        order,
		"guest_order_ref": ref,
		"customer_id":     caller.UUID,
	})
}
