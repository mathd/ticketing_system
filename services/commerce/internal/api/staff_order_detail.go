package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// staffOrderDetail answers what one order contains, for a credentialed staff caller
// (TKT-201).
//
// Shaped after getCancellationRefundReport, which is the closest existing operation: an
// organizer-scoped internal GET behind the same inline guard. Credential first, then the
// path id, then the scope, then the read — and a miss is a 404 with the same body as a
// refusal.
//
// WHY NOT WIDEN GET /orders/{id}: that read carries no credential and answers for any
// order id, and order ids are DERIVED (uuid.NewSHA1 over organizer + idempotency key), so
// anyone who watched a buyer's checkout can recompute one. The security review that
// removed customer_id from OrderState made exactly that argument; putting money there
// would be the same mistake with worse consequences.
func (s *Server) staffOrderDetail(w http.ResponseWriter, r *http.Request) {
	// Either the shared internal token or the back office's commerce credential. The
	// THIRD operation to accept the second (refund, void, and now this read) — and
	// staff_credential_test.go enumerates every internal route so that the set stays
	// deliberate rather than accumulating.
	//
	// Same 404 as everywhere else in this service, not a 401: it does not confirm the
	// route exists (ADR-043).
	//
	// FIRST, and note what that does NOT buy on this route: organizer_id is a required
	// query parameter in the contract, so the request validator answers 400 for a request
	// that omits it before this handler runs at all. A refusal test that leaves it out is
	// therefore testing the validator, not the credential — it must send a well-formed
	// request and be refused anyway.
	if !s.staffOrInternal(r) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	order, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid order"})
		return
	}
	// The SCOPE, not the authority. The credential proves the caller is the back office;
	// this narrows what it may see. The back office supplies it from its authenticated
	// server-side session, never from browser input.
	//
	// Commerce cannot verify it: the signed organizer assertion (ADR-058) is minted with a
	// catalog-only key, so a holder of COMMERCE_STAFF_WRITE_TOKEN could name another
	// organizer. That residual predates this read — the refund takes organizer_id from a
	// request body the same way — and is recorded on ADR-042 rather than pretended away.
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil || org == uuid.Nil {
		write(w, 400, map[string]string{"error": "organizer_id required"})
		return
	}
	detail, err := commercestore.ReadStaffOrderDetail(r.Context(), s.db, org, order)
	if err != nil {
		// An order under the wrong organizer is a 404, not an empty detail — the same
		// call the cancellation report makes, for the same reason: an empty answer would
		// read as "this order contains nothing", and a DISTINCT answer would confirm the
		// order exists in another tenant.
		if errors.Is(err, sql.ErrNoRows) {
			write(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		slog.Default().ErrorContext(r.Context(), "read staff order detail", "err", err)
		write(w, 500, map[string]string{"error": "read staff order detail"})
		return
	}

	refunds := make([]map[string]any, 0, len(detail.Refunds))
	for _, f := range detail.Refunds {
		row := map[string]any{
			"refund_id": f.RefundID, "status": f.Status, "quantity": f.Quantity,
			"amount": f.Amount, "currency": f.Currency,
			"idempotency_key": f.IdempotencyKey, "actor": f.Actor,
			"created_at": f.CreatedAt,
		}
		// Omitted rather than null while pending: the schema makes it optional, and the
		// table's CHECK ties its presence to the status, so an absent value here and a
		// pending status can never disagree.
		if f.CompletedAt != nil {
			row["completed_at"] = *f.CompletedAt
		}
		refunds = append(refunds, row)
	}

	// Every field named explicitly. No buyer_pii is joined by the query, so there is no
	// buyer field to omit here — but the response is assembled by hand rather than
	// marshalled from a struct so that adding one is a visible edit rather than a
	// consequence of adding a column somewhere else (ADR-003).
	write(w, 200, map[string]any{
		"order_id":     detail.OrderID,
		"organizer_id": detail.OrganizerID,
		"status":       detail.Status,
		"line_items": []map[string]any{{
			"ticket_type_id":    detail.Line.TicketTypeID,
			"quantity":          detail.Line.Quantity,
			"unit_amount":       detail.Line.UnitAmount,
			"face_value_amount": detail.Line.FaceValue,
			"total_amount":      detail.Line.TotalAmount,
			"currency":          detail.Line.Currency,
		}},
		"totals": map[string]any{
			"total_amount":      detail.Totals.TotalAmount,
			"face_value_amount": detail.Totals.FaceValue,
			"passed_on_fees":    detail.Totals.PassedOnFees,
			"refunded_amount":   detail.Totals.RefundedAmount,
			"refunded_quantity": detail.Totals.RefundedQuantity,
			"refund_status":     detail.Totals.RefundStatus,
			"currency":          detail.Totals.Currency,
		},
		"refunds": refunds,
	})
}
