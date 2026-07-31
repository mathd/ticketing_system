package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/psp"
	"ticketing/services/payments/internal/store"
)

// The post-purchase partial-refund leg (TKT-156, ADR-037).
//
// This is the ONE place the refund amount comes from the caller, and ADR-032's
// "never from a caller-supplied value" rule is amended for it rather than bent: the
// caller-supplied amount is validated against the operation's own durable captured
// evidence under a row lock before any provider call, so it can only ever be a
// SUBSET of money payments already knows moved. The recovery path
// (/internal/psp/refund) still derives its amount from the stored row and is
// untouched.

type partialRefundRequest struct {
	OrganizerID    uuid.UUID `json:"organizer_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	RefundKey      string    `json:"refund_key"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
}

// refundLegFactID derives the deterministic compensating-fact id for one leg, in its own
// namespace so it can never collide with the whole-refund compensation's fact id for the
// same charge.
func refundLegFactID(org uuid.UUID, sourceKey, refundKey string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment-leg:"+org.String()+":"+sourceKey+":"+refundKey))
}

func (s *Server) pspPartialRefund(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		write(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in partialRefundRequest
	if !decode(w, r, &in) {
		return
	}
	key, refundKey := strings.TrimSpace(in.IdempotencyKey), strings.TrimSpace(in.RefundKey)
	if in.OrganizerID == uuid.Nil || key == "" || len(key) > 200 || refundKey == "" || len(refundKey) > 200 ||
		in.Amount <= 0 || len(in.Currency) != 3 {
		write(w, 400, map[string]string{"error": "organizer_id, idempotency_key, refund_key, positive amount and currency required"})
		return
	}
	op, found, err := s.journal.LookupOperation(r.Context(), in.OrganizerID, key)
	if err != nil {
		write(w, 500, map[string]string{"error": "lookup operation"})
		return
	}
	if !found {
		write(w, 404, map[string]string{"error": "operation not found"})
		return
	}

	leg, err := s.journal.BindRefundLeg(r.Context(), in.OrganizerID, key, refundKey, in.Amount, in.Currency)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		write(w, 404, map[string]string{"error": "operation not found"})
		return
	case errors.Is(err, store.ErrRefundExceedsCapture), errors.Is(err, store.ErrWholeRefundBound):
		write(w, 409, map[string]string{"error": err.Error()})
		return
	case err != nil:
		// Eligibility failures (no captured money, currency mismatch) land here as 409:
		// the request is coherent, the operation's evidence does not support it.
		write(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if leg.Completed {
		write(w, 200, map[string]any{"status": leg.Status, "fact_id": leg.FactID, "amount": leg.Amount, "currency": leg.Currency, "replay": true})
		return
	}
	// A replayed leg re-submits under the SAME derived provider key. The Stripe adapter
	// resolves before it submits (ADR-032 §Refund, TKT-116) — it lists the PaymentIntent's
	// refunds and adopts the one carrying this key's compensation stamp — so a settled leg
	// whose response was lost is adopted rather than refunded twice. That is why no
	// provider reference needs recording on the still-bound row.
	result, err := s.psp.Refund(r.Context(), op.ProviderPaymentRef, leg.ProviderKey, leg.Amount, leg.Currency)
	if err != nil {
		// Recoverable, never terminal: the leg stays bound and its allowance stays
		// reserved, so nothing else can spend the money this leg may yet take.
		write(w, 502, map[string]string{"error": "provider refund unresolved"})
		return
	}
	if err := result.Validate(); err != nil {
		write(w, 500, map[string]string{"error": "invalid provider result"})
		return
	}
	if result.Outcome != psp.Refunded {
		write(w, 502, map[string]string{"error": "provider refund unresolved"})
		return
	}
	factID := refundLegFactID(in.OrganizerID, key, refundKey)
	// OccurredAt is the leg's stable bound_at, never the clock: the fact id is
	// deterministic and the journal compares the full canonical fact on replay, so a fresh
	// timestamp would fail "fact id reused with different content" and wedge the leg.
	if _, _, err := s.journal.Append(r.Context(), store.Fact{
		ID: factID, OrganizerID: in.OrganizerID, Type: "payment.refunded", OccurredAt: leg.BoundAt,
		BuyerID: op.BuyerID, Amount: leg.Amount, Currency: leg.Currency,
		Payload: map[string]string{"order_id": op.OrderID.String(), "refund_key": refundKey},
	}); err != nil {
		write(w, 500, map[string]string{"error": "journal append failed"})
		return
	}
	if err := s.journal.CompleteRefundLeg(r.Context(), in.OrganizerID, key, refundKey, result.ProviderRef, factID); err != nil {
		write(w, 500, map[string]string{"error": "persist refund leg result"})
		return
	}
	write(w, 200, map[string]any{"status": "refunded", "fact_id": factID, "amount": leg.Amount, "currency": leg.Currency, "replay": false})
}
