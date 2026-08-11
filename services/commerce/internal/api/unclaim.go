package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"

	"ticketing/shared/httpx"
)

// unclaimOrder detaches a completed order from the account that claimed it
// (TKT-225 / ADR-052).
//
// Guarded by the shared internal token compared INLINE, which is the idiom for
// every internal operation except the refund (ADR-043; staff_credential_test.go
// enumerates the exception to keep it deliberate). Not `staffOrInternal`: this
// slice ships no back-office surface, so accepting the staff credential would
// widen what that credential reaches for nothing. Whoever adds the form widens it
// then, with their own review — narrowing a credential after something depends on
// it is the hard direction.
//
// 404 rather than 401 for a bad credential, matching every other internal
// operation here: the service does not confirm the route exists.
func (s *Server) unclaimOrder(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid order id"})
		return
	}

	// Required, and load-bearing rather than ceremony: a detached order is
	// immediately re-claimable, so a retried request without a key could detach
	// whoever claimed it in between (ai-review [high]).
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key required"})
		return
	}

	var body struct {
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	detached, err := commercestore.DetachOrderAttribution(r.Context(), s.db, order, key, body.Actor, body.Reason)
	switch {
	case errors.Is(err, commercestore.ErrDetachmentNotDescribed):
		// 400, not 404: the caller's request is malformed, and saying so discloses
		// nothing about the order — the check runs before the order is looked at.
		write(w, http.StatusBadRequest, map[string]string{"error": "actor and reason are required"})
		return
	case errors.Is(err, commercestore.ErrOrderNotDetachable):
		// One answer for no-such-order, not-completed and already-unattributed.
		// Not for secrecy — this caller holds the service credential — but because
		// the operator's next step is the same in all three: look at the order.
		write(w, http.StatusNotFound, map[string]string{"error": "order is not detachable"})
		return
	case err != nil:
		slog.Default().ErrorContext(r.Context(), "unclaim order", "err", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	write(w, http.StatusOK, map[string]string{
		"order_id":             order.String(),
		"detached_customer_id": detached.String(),
	})
}
