package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refunds"
	commercestore "ticketing/services/commerce/internal/store"
)

// Post-purchase refunds (TKT-156, ADR-037). Staff-facing and internal: the gateway denies
// /internal/* at the edge, and a missing or wrong token reads as 404 here, matching
// staffSale and deliveryEmail rather than inventing a 401 convention for commerce.
//
// The handler is deliberately thin. Every decision that needs the database — the
// completed-order check, the quantity ceiling, the idempotent replay — lives in
// store.BindOrderRefund under the order row lock, which is also where it is tested. The
// protocol itself (money → fact → completion → reversal) lives in internal/refunds, which
// the event-cancellation bulk runner shares (TKT-159): one money path, two callers.

type refundRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
	Quantity    int32     `json:"quantity"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
}

// refundProblem maps a store or coordinator error onto the status the contract declares.
// Kept separate from the handler so the whole mapping is table-testable without a database.
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
	case errors.Is(err, refunds.ErrPaymentsRefused):
		return http.StatusConflict, "refund unresolved"
	case errors.Is(err, refunds.ErrPaymentsUnresolved):
		return http.StatusBadGateway, "refund unresolved"
	case errors.Is(err, refunds.ErrJournalUnavailable):
		return http.StatusServiceUnavailable, "journal unavailable"
	case errors.Is(err, refunds.ErrPersist):
		return http.StatusInternalServerError, "persist refund"
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
	// The `cancel:` namespace belongs to the event-cancellation runner, which DERIVES its
	// keys (TKT-159). A staff refund that borrowed one would create the same refund id under
	// a human actor/reason, and every cancellation run would then find that row, disagree
	// with its fingerprint, and report the order failed forever — even when the staff refund
	// had fully succeeded. Reserving the prefix is cheaper than reconciling the collision.
	if strings.HasPrefix(key, commercestore.CancellationRefundKeyPrefix) {
		write(w, 400, map[string]string{"error": "Idempotency-Key uses a reserved prefix"})
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

	result, err := s.refunds.Refund(r.Context(), commercestore.RefundRequest{
		OrderID: order, OrganizerID: in.OrganizerID, Quantity: in.Quantity,
		IdempotencyKey: key, Actor: in.Actor, Reason: in.Reason,
	})
	if err != nil {
		code, message := refundProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "refund order", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	refund := result.Refund
	if !result.Replay {
		// Re-read the projection so the response reports the state this refund produced,
		// not the one it was bound against. A read failure here costs the caller nothing
		// that matters — the money moved and the refund is durable — so it answers with
		// the pre-completion projection rather than turning a successful refund into an
		// error.
		if qty, amount, state, err := commercestore.OrderRefundProjection(r.Context(), s.db, order); err == nil {
			refund.RefundedQty, refund.RefundedAmount, refund.OrderRefundState = qty, amount, state
		}
	}
	writeRefund(w, refund, result.Replay)
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
