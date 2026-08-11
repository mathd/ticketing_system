package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"

	"ticketing/shared/httpx"
)

// Event-cancellation bulk refunds (TKT-159, ADR-040). Internal, like every other staff
// operation in commerce: a missing or wrong token reads as 404 rather than 401.
//
// Both handlers are thin. Creating a run is one insert-or-load under an idempotency key;
// the work is done by the background runner, and the report is a read. Nothing here
// touches money — the runner refunds through the shared internal/refunds unit.

const defaultReportLimit = 100

type cancellationRunRequest struct {
	OrganizerID uuid.UUID `json:"organizer_id"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
}

func (s *Server) createCancellationRefundRun(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	slot, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid slot"})
		return
	}
	var in cancellationRunRequest
	if !decode(w, r, &in) {
		return
	}
	// The length bounds are the DATABASE's (actor 1..200, reason 1..500). Validating only
	// non-blankness here let an over-long field through to a CHECK violation, which surfaces
	// as a misleading 500 for what is a bad request.
	if in.OrganizerID == uuid.Nil ||
		strings.TrimSpace(in.Actor) == "" || len(in.Actor) > 200 ||
		strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 500 {
		write(w, 400, map[string]string{"error": "invalid cancellation refund run"})
		return
	}

	run, err := commercestore.BindCancellationRun(r.Context(), s.db, commercestore.CancellationRunRequest{
		OrganizerID: in.OrganizerID, SlotID: slot, IdempotencyKey: key,
		Actor: in.Actor, Reason: in.Reason,
	})
	if err != nil {
		if errors.Is(err, commercestore.ErrCancellationRunConflict) {
			write(w, http.StatusConflict, map[string]string{"error": "run conflicts with an existing request"})
			return
		}
		slog.Default().ErrorContext(r.Context(), "bind cancellation run", "err", err)
		write(w, 500, map[string]string{"error": "persist cancellation run"})
		return
	}
	status := http.StatusCreated
	if run.Replay {
		status = http.StatusOK
	}
	write(w, status, cancellationRunBody(run))
}

func (s *Server) getCancellationRefundReport(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid run"})
		return
	}
	org, err := uuid.Parse(r.URL.Query().Get("organizer_id"))
	if err != nil || org == uuid.Nil {
		write(w, 400, map[string]string{"error": "organizer_id required"})
		return
	}
	limit := defaultReportLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			write(w, 400, map[string]string{"error": "invalid limit"})
			return
		}
		limit = parsed
	}
	after := uuid.Nil
	if raw := r.URL.Query().Get("after_order_id"); raw != "" {
		after, err = uuid.Parse(raw)
		if err != nil {
			write(w, 400, map[string]string{"error": "invalid after_order_id"})
			return
		}
	}

	// Organizer-scoped: a run id under the wrong organizer is a 404, not an empty report.
	// An empty report would read as "this cancellation refunded nobody" (ADR-002).
	page, err := commercestore.CancellationReport(r.Context(), s.db, org, runID, limit, after)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			write(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		slog.Default().ErrorContext(r.Context(), "read cancellation report", "err", err)
		write(w, 500, map[string]string{"error": "read cancellation report"})
		return
	}

	orders := make([]map[string]any, 0, len(page.Orders))
	for _, o := range page.Orders {
		row := map[string]any{
			"order_id": o.OrderID, "outcome": o.Outcome,
			"refunded_quantity": o.RefundedQuantity, "refunded_amount": o.RefundedAmount,
			"currency": o.Currency, "money_refunded": o.MoneyRefunded,
			"tickets_voided": o.TicketsVoided, "capacity_returned": o.CapacityReturned,
		}
		if o.RefundID.Valid {
			row["refund_id"] = o.RefundID.UUID
		}
		if o.FailureCode != "" {
			row["failure"] = map[string]string{"code": o.FailureCode, "reason": o.FailureReason}
		}
		orders = append(orders, row)
	}
	body := map[string]any{
		"run": cancellationRunBody(page.Run),
		"counts": map[string]int{
			"total": page.Counts.Total, "refunded": page.Counts.Refunded,
			"already_refunded": page.Counts.AlreadyRefunded, "failed": page.Counts.Failed,
			"pending": page.Counts.Pending,
		},
		"incomplete_at_enumeration": page.IncompleteAtEnumeration,
		"orders":                    orders,
	}
	if page.NextAfterOrderID.Valid {
		body["next_after_order_id"] = page.NextAfterOrderID.UUID
	}
	// 202 while the run is still going: the per-order rows are withheld so a paginating
	// reader cannot have the membership grow underneath it, but the counts ARE served, so
	// an operator watching a long cancellation can see progress.
	status := http.StatusOK
	if page.Run.Status != "completed" {
		status = http.StatusAccepted
	}
	write(w, status, body)
}

func cancellationRunBody(run commercestore.CancellationRun) map[string]any {
	body := map[string]any{
		"run_id": run.ID, "organizer_id": run.OrganizerID, "slot_id": run.SlotID,
		"status": run.Status, "cutoff_at": run.CutoffAt, "created_at": run.CreatedAt,
		"replay": run.Replay,
	}
	if run.CompletedAt.Valid {
		body["completed_at"] = run.CompletedAt.Time
	}
	return body
}
