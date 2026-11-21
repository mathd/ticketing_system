package api

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/store"
)

// The payments half of the operator outcome `abandoned` (TKT-525 S2, ADR-087).
//
// Commerce's `record-refund-outcome ... abandoned` command calls this before it records its own
// outcome. It writes one append-only marker for the exact leg. The marker releases ONLY that
// leg's allowance in the captured-money ceiling (BindRefundLeg); the leg stays bound with its
// provider key, the whole-refund exclusion still counts it, and ordinary dispatch refuses it.
//
// The order is the correctness argument, the same as the failure proof's:
//
//  1. Read the leg, its charge and any marker. A committed marker answers as a replay here,
//     with no provider call: a caller that lost the first response must always be able to
//     finish, even with the provider down.
//  2. Ask the provider, with NO lock held and no transaction open (D10, ADR-037 §4).
//  3. One short transaction locks the operation, then the leg, rechecks what step 1 read, and
//     inserts the marker (store.AbandonRefundLeg).

type abandonRequest struct {
	OrganizerID    uuid.UUID `json:"organizer_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	RefundKey      string    `json:"refund_key"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
}

var isoCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *Server) pspPartialRefundAbandon(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in abandonRequest
	if !decode(w, r, &in) {
		return
	}
	sourceKey := strings.TrimSpace(in.IdempotencyKey)
	if in.OrganizerID == uuid.Nil || sourceKey == "" || len(sourceKey) > 200 ||
		in.Amount <= 0 || !isoCurrency.MatchString(in.Currency) {
		write(w, 400, map[string]string{"error": "organizer_id, idempotency_key, positive amount and currency required"})
		return
	}
	// Canonical only, as for the failure proof: the refund key of a post-purchase refund IS
	// Commerce's refund id, and an exchange downgrade key is not a UUID.
	refundKey := in.RefundKey
	if parsed, err := uuid.Parse(refundKey); err != nil || parsed.String() != refundKey {
		write(w, 400, map[string]string{"error": "refund_key must be a canonical UUID"})
		return
	}

	state, found, err := s.journal.LookupRefundLegProofState(r.Context(), in.OrganizerID, sourceKey, refundKey)
	switch {
	case err != nil:
		write(w, 500, map[string]string{"error": "lookup refund leg"})
		return
	case !found:
		write(w, 404, map[string]string{"error": "refund leg not found"})
		return
	case state.Completed:
		write(w, 409, map[string]string{"error": "refund leg completed"})
		return
	case state.Amount != in.Amount || state.Currency != in.Currency:
		write(w, 409, map[string]string{"error": "refund leg money does not match"})
		return
	}
	if state.Abandoned {
		m, found, err := s.journal.LookupRefundLegAbandonment(r.Context(), in.OrganizerID, sourceKey, refundKey)
		if err != nil || !found {
			// The marker is append-only: a state read that saw it and a read that cannot
			// find it is a database fault, not an answer.
			write(w, 500, map[string]string{"error": "lookup refund leg abandonment"})
			return
		}
		writeAbandonment(w, m.ProviderRef, state)
		return
	}

	providerRef, ok := s.proveTerminalFailure(w, r, state)
	if !ok {
		return
	}

	m, _, err := s.journal.AbandonRefundLeg(r.Context(), in.OrganizerID, sourceKey, refundKey, state, providerRef)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		write(w, 404, map[string]string{"error": "refund leg not found"})
		return
	case errors.Is(err, store.ErrRefundLegCompleted):
		write(w, 409, map[string]string{"error": "refund leg completed"})
		return
	case errors.Is(err, store.ErrRefundLegChanged):
		write(w, 409, map[string]string{"error": "refund leg changed during the proof"})
		return
	case errors.Is(err, store.ErrRefundLegAbandonedOtherwise):
		write(w, 409, map[string]string{"error": "refund leg abandoned under other evidence"})
		return
	case err != nil:
		write(w, 500, map[string]string{"error": "persist refund leg abandonment"})
		return
	}
	writeAbandonment(w, m.ProviderRef, state)
}

// writeAbandonment answers with the marker's provider reference and the leg's stored money,
// the same shape as the failure proof.
func writeAbandonment(w http.ResponseWriter, providerRef string, state store.RefundLegProofState) {
	write(w, 200, map[string]any{"provider_ref": providerRef, "amount": state.Amount, "currency": state.Currency})
}
