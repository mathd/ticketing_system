package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Post-purchase refunds (TKT-156, ADR-037). Staff-facing and internal: the gateway denies
// /internal/* at the edge, and a missing or wrong token reads as 404 here, matching
// staffSale and deliveryEmail rather than inventing a 401 convention for commerce.
//
// The handler is deliberately thin. Every decision that needs the database — the
// completed-order check, the quantity ceiling, the idempotent replay — lives in
// store.BindOrderRefund under the order row lock, which is also where it is tested.

type refundRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
	Quantity    int32     `json:"quantity"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
}

// refundProblem maps a store error onto the status the contract declares. Kept separate
// from the handler so the whole mapping is table-testable without a database.
func refundProblem(err error) (int, string) {
	switch {
	case errors.Is(err, commercestore.ErrOrderNotRefundable):
		return http.StatusConflict, "only a completed order can be refunded"
	case errors.Is(err, commercestore.ErrRefundExceedsOrder):
		return http.StatusConflict, "refund would exceed the order quantity"
	case errors.Is(err, commercestore.ErrRefundNoMoney):
		return http.StatusConflict, "order has no captured money to refund"
	case errors.Is(err, commercestore.ErrRefundConflict):
		return http.StatusConflict, "refund conflicts with an existing request"
	case errors.Is(err, sql.ErrNoRows):
		return http.StatusNotFound, "not found"
	default:
		return http.StatusInternalServerError, "persist refund"
	}
}

func (s *Server) refundOrder(w http.ResponseWriter, r *http.Request) {
	if s.token == "" || r.Header.Get("X-Internal-Token") != s.token {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid order"})
		return
	}
	var in refundRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 ||
		strings.TrimSpace(in.Actor) == "" || strings.TrimSpace(in.Reason) == "" {
		write(w, 400, map[string]string{"error": "invalid refund"})
		return
	}

	refund, err := commercestore.BindOrderRefund(r.Context(), s.db, commercestore.RefundRequest{
		OrderID: order, OrganizerID: in.OrganizerID, Quantity: in.Quantity,
		IdempotencyKey: key, Actor: in.Actor, Reason: in.Reason,
	})
	if err != nil {
		code, message := refundProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "bind order refund", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	if refund.Completed {
		// The money is done, but the reversal may not be: a replay is how outstanding
		// ticket voiding gets retried (TKT-157). Drive it, then answer.
		refund = s.driveReversal(r, refund)
		writeRefund(w, refund, true)
		return
	}

	// Money first, then the trail. The refund row is already durable, so a crash between
	// here and the completion leaves a pending refund a replay resolves — never money
	// moved with no record that it was owed.
	factID, code, err := s.refundPayment(r, refund)
	if err != nil {
		write(w, code, map[string]string{"error": "refund unresolved"})
		return
	}
	if err := s.refundFact(r, refund); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	if err := commercestore.CompleteOrderRefund(r.Context(), s.db, in.OrganizerID, refund.ID, factID); err != nil {
		slog.Default().ErrorContext(r.Context(), "complete order refund", "err", err)
		write(w, 500, map[string]string{"error": "persist refund"})
		return
	}
	// The money is durable from here on. The reversal is a SEPARATE obligation: if it
	// fails, the refund stays valid and outstanding rather than the whole request
	// failing and inviting a retry that would re-examine money that already moved.
	refund = s.driveReversal(r, refund)

	// Re-read the projection so the response reports the state this refund produced, not
	// the one it was bound against. A read failure here costs the caller nothing that
	// matters — the money moved and the refund is durable — so it answers with the
	// pre-completion projection rather than turning a successful refund into an error.
	if qty, amount, state, err := commercestore.OrderRefundProjection(r.Context(), s.db, order); err == nil {
		refund.RefundedQty, refund.RefundedAmount, refund.OrderRefundState = qty, amount, state
	}
	writeRefund(w, refund, false)
}

func writeRefund(w http.ResponseWriter, r commercestore.Refund, replay bool) {
	write(w, 200, map[string]any{
		"refund_id": r.ID, "order_id": r.OrderID, "quantity": r.Quantity,
		"amount": r.Amount, "currency": r.Currency,
		"refund_status": r.OrderRefundState, "refunded_quantity": r.RefundedQty,
		"refunded_amount": r.RefundedAmount, "replay": replay,
		// The reversal is reported separately from the money, and reported as FALSE
		// when it is outstanding rather than omitted: a caller that cannot see the
		// difference between "tickets voided" and "we did not get to it" will assume
		// the first (TKT-157).
		"tickets_voided": r.TicketsVoided,
		// Reported separately, and reported as FALSE when outstanding rather than omitted,
		// for the same reason as tickets_voided (TKT-161).
		"capacity_returned": r.CapacityReturned,
	})
}

// driveReversal discharges a refund's two downstream obligations, IN ORDER.
//
// The order is a safety property, not a preference (ADR-038 §1): freeing the seat while
// the original ticket still admits is the one sequence that can OVERSELL. Voiding first
// can only under-sell. So the capacity return is attempted only once voiding has actually
// happened — which is why this is a guard and not a comment, and why an access outage
// leaves BOTH obligations outstanding rather than letting the second run without the
// first.
//
// Neither leg fails the request. The money has moved and the refund row is durable, so a
// downstream failure answers a successful refund with the obligation still outstanding —
// visible, and retryable by replaying the same idempotency key.
func (s *Server) driveReversal(r *http.Request, refund commercestore.Refund) commercestore.Refund {
	refund = s.voidRefundedTickets(r, refund)
	if !refund.TicketsVoided {
		return refund
	}
	return s.returnRefundedCapacity(r, refund)
}

// returnRefundedCapacity gives the seat back. A partial return of a SEATED claim is
// refused by inventory and stays outstanding forever: nothing associates an issued ticket
// with a seat identity, so no subset of seats can be derived (TKT-164). That is a
// deliberate under-sell — the buyer keeps their refund and the tickets stay void — rather
// than refusing the refund itself to protect a resale.
func (s *Server) returnRefundedCapacity(r *http.Request, refund commercestore.Refund) commercestore.Refund {
	if refund.CapacityReturned || s.inventoryURL == "" || refund.HoldID == uuid.Nil {
		return refund
	}
	code, _, err := s.call(r.Context(), http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/refund-capacity", s.inventoryURL, refund.HoldID), "",
		map[string]any{"organizer_id": refund.OrganizerID, "refund_id": refund.ID, "quantity": refund.Quantity}, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(r.Context(), "refund capacity not returned; left outstanding",
			"refund_id", refund.ID, "hold_id", refund.HoldID, "status", code, "err", err)
		return refund
	}
	if err := commercestore.MarkRefundCapacityReturned(r.Context(), s.db, refund.OrganizerID, refund.ID); err != nil {
		// Inventory returned it; only our record of it failed. A replay re-drives
		// inventory, which answers as a replay, and re-marks.
		slog.Default().ErrorContext(r.Context(), "record refund capacity return", "refund_id", refund.ID, "err", err)
		return refund
	}
	refund.CapacityReturned = true
	return refund
}

// voidRefundedTickets discharges the ticket-voiding half of a reversal, and never fails
// the request. The money has already moved and the refund row is durable, so the honest
// answer to a failure here is a successful refund reporting `tickets_voided:false` —
// visible, and retryable by replaying the same idempotency key.
//
// Access answers 503 when issuance has not caught up (its outbox/JetStream path is
// asynchronous, so a prompt refund genuinely can outrun it). That is a "not yet", not a
// "nothing to void", and it must leave the obligation outstanding.
func (s *Server) voidRefundedTickets(r *http.Request, refund commercestore.Refund) commercestore.Refund {
	if refund.TicketsVoided || s.accessURL == "" {
		return refund
	}
	code, _, err := s.call(r.Context(), http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/refunds", s.accessURL, refund.OrderID), "",
		map[string]any{"organizer_id": refund.OrganizerID, "refund_id": refund.ID, "quantity": refund.Quantity}, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(r.Context(), "refund tickets not voided; left outstanding",
			"refund_id", refund.ID, "order_id", refund.OrderID, "status", code, "err", err)
		return refund
	}
	if err := commercestore.MarkRefundTicketsVoided(r.Context(), s.db, refund.OrganizerID, refund.ID); err != nil {
		// Access voided them; only our record of it failed. A replay re-drives access,
		// which answers as a replay, and re-marks — so this is recoverable, not lost.
		slog.Default().ErrorContext(r.Context(), "record refund ticket voiding", "refund_id", refund.ID, "err", err)
		return refund
	}
	refund.TicketsVoided = true
	return refund
}

// refundPayment moves the money through payments' partial-refund leg. The source key is
// the ORDER's checkout idempotency key, which is the key payments bound the charge
// operation under; the leg key is the refund's own identity, so a retry converges on one
// provider refund.
func (s *Server) refundPayment(r *http.Request, refund commercestore.Refund) (uuid.UUID, int, error) {
	body := map[string]any{
		"organizer_id":    refund.OrganizerID,
		"idempotency_key": refund.PaymentSourceKey,
		"refund_key":      refund.ID.String(),
		"amount":          refund.Amount,
		"currency":        refund.Currency,
	}
	code, out, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/psp/partial-refund", "", body, true)
	if err != nil {
		return uuid.Nil, http.StatusBadGateway, err
	}
	if code == http.StatusConflict {
		return uuid.Nil, http.StatusConflict, errors.New("payments refused the refund")
	}
	if code != http.StatusOK {
		return uuid.Nil, http.StatusBadGateway, errors.New("payments refund unresolved")
	}
	var decoded struct {
		FactID uuid.UUID `json:"fact_id"`
	}
	if json.Unmarshal(out, &decoded) != nil || decoded.FactID == uuid.Nil {
		return uuid.Nil, http.StatusBadGateway, errors.New("invalid payments response")
	}
	return decoded.FactID, http.StatusOK, nil
}

// refundFact appends commerce's own compensating fact. It does NOT reuse s.fact: that
// helper derives its id from (order, type) and always journals the reservation's full
// total, so a second partial refund of the same order would collide on the id and carry
// the wrong amount. occurred_at is the refund row's stable created_at, never the clock —
// the journal compares the whole canonical fact on replay.
func (s *Server) refundFact(r *http.Request, refund commercestore.Refund) error {
	factID := commercestore.RefundFactID(refund.ID)
	occurred := refund.CreatedAt.UTC()
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
		VALUES($1,$2,$3,$4,'order.refunded',$5,$6,$7) ON CONFLICT DO NOTHING`,
		factID, refund.OrderID, refund.OrganizerID, refund.BuyerID, refund.Amount, refund.Currency, occurred); err != nil {
		return err
	}
	code, _, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/facts", "", map[string]any{
		"fact_id": factID, "organizer_id": refund.OrganizerID, "fact_type": "order.refunded",
		"buyer_id": refund.BuyerID, "amount": refund.Amount, "currency": refund.Currency,
		"occurred_at": occurred.Format(time.RFC3339Nano),
		// order_id only — the journal allows no other payload key (a deliberate PII
		// guard), and RefundFactID already ties this fact to its refund.
		"payload": map[string]string{"order_id": refund.OrderID.String()},
	}, true)
	if err != nil || code != 200 {
		return errors.New("journal unavailable")
	}
	return nil
}
