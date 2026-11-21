package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// Read-only terminal-failure proof for one post-purchase refund leg (TKT-525, ADR-087).
//
// Commerce's refunded_outside command calls this before it records that a buyer was refunded
// outside the platform. The proof must establish two things that Commerce cannot see: payments
// still holds the leg bound and unmarked (so no settlement or abandonment raced the operator),
// and the provider's own stamped refund for the leg terminally failed (so no platform money
// left). Only then may money have moved by another route without the platform moving it too.
//
// No lock is held across the provider call (ADR-037 §4). The leg and its marker are read, the
// provider is asked, and the leg and its marker are read AGAIN; a change between the two reads
// refuses rather than reporting a proof about a row that no longer looks as it did.

func (s *Server) pspPartialRefundFailureProof(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	q := r.URL.Query()
	org, err := uuid.Parse(q.Get("organizer_id"))
	if err != nil || org == uuid.Nil {
		write(w, 400, map[string]string{"error": "valid organizer_id required"})
		return
	}
	sourceKey := strings.TrimSpace(q.Get("idempotency_key"))
	if sourceKey == "" || len(sourceKey) > 200 {
		write(w, 400, map[string]string{"error": "idempotency_key required"})
		return
	}
	// Canonical only: the refund key of a post-purchase refund IS Commerce's refund id, written
	// with uuid.UUID.String(). Accepting another spelling of the same UUID would address a
	// different leg row, and an exchange downgrade key (`exchange-refund:<id>`) is not a UUID.
	refundKey := q.Get("refund_key")
	if parsed, err := uuid.Parse(refundKey); err != nil || parsed.String() != refundKey {
		write(w, 400, map[string]string{"error": "refund_key must be a canonical UUID"})
		return
	}

	before, ok := s.proofState(w, r, org, sourceKey, refundKey)
	if !ok {
		return
	}
	if before.ProviderPaymentRef == "" {
		write(w, 502, map[string]string{"error": "refund failure proof unavailable"})
		return
	}
	prover, ok := s.psp.(psp.RefundFailureProver)
	if !ok {
		write(w, 502, map[string]string{"error": "refund failure proof unavailable"})
		return
	}
	proof, err := prover.ProveRefundFailure(r.Context(), before.ProviderPaymentRef, before.ProviderKey, before.Amount, before.Currency)
	switch {
	case errors.Is(err, psp.ErrRefundProofContradicted):
		write(w, 409, map[string]string{"error": "provider evidence contradicts a failed refund"})
		return
	case err != nil:
		write(w, 502, map[string]string{"error": "refund failure proof unavailable"})
		return
	}
	if !strings.HasPrefix(proof.ProviderRef, "re_") || len(proof.ProviderRef) <= len("re_") || len(proof.ProviderRef) > 255 {
		write(w, 502, map[string]string{"error": "refund failure proof unavailable"})
		return
	}

	// The second read. The proof is about the leg as it was BEFORE the provider call; it is
	// only worth anything if the leg, its charge and its marker still look exactly like that.
	after, ok := s.proofState(w, r, org, sourceKey, refundKey)
	if !ok {
		return
	}
	if after != before {
		write(w, 409, map[string]string{"error": "refund leg changed during the proof"})
		return
	}
	write(w, 200, map[string]any{"provider_ref": proof.ProviderRef, "amount": before.Amount, "currency": before.Currency})
}

// proofState reads the leg with its charge and marker and answers every state that refuses.
// ok=false means a response was written.
func (s *Server) proofState(w http.ResponseWriter, r *http.Request, org uuid.UUID, sourceKey, refundKey string) (store.RefundLegProofState, bool) {
	state, found, err := s.journal.LookupRefundLegProofState(r.Context(), org, sourceKey, refundKey)
	switch {
	case err != nil:
		write(w, 500, map[string]string{"error": "lookup refund leg"})
		return state, false
	case !found:
		write(w, 404, map[string]string{"error": "refund leg not found"})
		return state, false
	case state.Completed:
		write(w, 409, map[string]string{"error": "refund leg completed"})
		return state, false
	case state.Abandoned:
		write(w, 409, map[string]string{"error": "refund leg abandoned"})
		return state, false
	}
	return state, true
}
