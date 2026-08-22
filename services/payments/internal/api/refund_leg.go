package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Read-only refund-leg evidence (TKT-168, ADR-032 §Provider-neutral evidence reads).
//
// The write path (TKT-156, ADR-037) already recorded a leg's amount, currency and
// completion; nothing could read them back. The consequence was that a caller could prove a
// charge had run but could not prove a refund had settled or for how much, so a refund call
// replaced by a successful no-op was indistinguishable from one that moved money.
//
// Strictly a read, in the sense /internal/operations is one and /internal/psp/status is
// NOT: it answers from the stored row alone. It binds nothing, calls no provider and
// appends no fact, so a caller that must not cause provider traffic — a test, a
// reconciliation sweep — can use it.

// refundLeg answers evidence for one leg addressed by (organizer, source key, refund key).
// All three are required: the pair (organizer, source key) identifies a charge that may
// carry many legs, and only the refund key distinguishes them.
func (s *Server) refundLeg(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "valid organizer_id required"})
		return
	}
	sourceKey := strings.TrimSpace(r.URL.Query().Get("source_idempotency_key"))
	if sourceKey == "" || len(sourceKey) > 200 {
		write(w, 400, map[string]string{"error": "source_idempotency_key required"})
		return
	}
	refundKey := strings.TrimSpace(r.URL.Query().Get("refund_idempotency_key"))
	if refundKey == "" || len(refundKey) > 200 {
		write(w, 400, map[string]string{"error": "refund_idempotency_key required"})
		return
	}
	leg, found, err := s.journal.LookupRefundLeg(r.Context(), org, sourceKey, refundKey)
	if err != nil {
		write(w, 500, map[string]string{"error": "lookup refund leg"})
		return
	}
	if !found {
		write(w, 404, map[string]string{"error": "refund leg not found"})
		return
	}
	// Field by field, never the store row: RefundLeg carries ProviderKey and ProviderRef,
	// and marshalling it would publish the provider idempotency key that makes a leg
	// replayable at the provider. Completed is the row's settled state, not merely
	// "a leg exists" — a bound leg is money the buyer has not received back.
	//
	// Amount is the amount the leg BOUND — the same figure the write response and the
	// compensating fact carry. It proves the leg was created for this amount and settled.
	body := map[string]any{
		"completed": leg.Completed,
		"amount":    leg.Amount,
		"currency":  leg.Currency,
	}
	// The provider's confirmation that this money actually came back, published separately
	// and omitted when there is none (TKT-257). Completion now refuses without it, so a leg
	// completed from here on always carries one — but a leg completed BEFORE migration 0006
	// has none and never can. It answers absent rather than having its bound amount promoted
	// to a provider confirmation it never received.
	if leg.ConfirmedAmount != nil {
		body["confirmed_amount"] = *leg.ConfirmedAmount
		body["confirmed_currency"] = leg.ConfirmedCurrency
	}
	write(w, 200, body)
}
