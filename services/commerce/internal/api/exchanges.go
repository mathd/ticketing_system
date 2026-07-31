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

// Exchanges (TKT-158, ADR-039). Staff-facing and internal, 404 on a bad token like every
// other commerce staff operation.
//
// The ordering below is the whole design. Every refusal that can happen must happen
// BEFORE any money moves, because an exchange — unlike a refund — has no safe partial
// state: a settled delta plus a half-done exchange leaves the buyer holding the wrong
// thing. That is why the seated and sold-out checks precede settlement, and it is the
// point on which this ticket deliberately differs from ADR-038 §9, where refusing a
// buyer's refund to protect a resale was the wrong trade.

type exchangeRequest struct {
	OrganizerID        uuid.UUID `json:"organizer_id"`
	TargetTicketTypeID uuid.UUID `json:"target_ticket_type_id"`
	Actor              string    `json:"actor"`
	Reason             string    `json:"reason"`
}

// exchangeProblem maps a store error onto a status the contract declares. Separate from
// the handler so the mapping is table-testable without a database, and so an undeclared
// status cannot slip in (ADR-028).
func exchangeProblem(err error) (int, string) {
	switch {
	case errors.Is(err, commercestore.ErrOrderNotExchangeable):
		return http.StatusConflict, "only a completed, unreversed order can be exchanged"
	case errors.Is(err, commercestore.ErrExchangeConflict):
		return http.StatusConflict, "exchange conflicts with an existing request"
	case errors.Is(err, commercestore.ErrExchangeCurrencyMismatch):
		return http.StatusConflict, "exchange target is priced in a different currency"
	case errors.Is(err, sql.ErrNoRows):
		return http.StatusNotFound, "not found"
	default:
		return http.StatusInternalServerError, "persist exchange"
	}
}

func (s *Server) exchangeOrder(w http.ResponseWriter, r *http.Request) {
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
	var in exchangeRequest
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || in.TargetTicketTypeID == uuid.Nil ||
		strings.TrimSpace(in.Actor) == "" || strings.TrimSpace(in.Reason) == "" {
		write(w, 400, map[string]string{"error": "invalid exchange"})
		return
	}

	ex, err := commercestore.BindOrderExchange(r.Context(), s.db, commercestore.ExchangeRequest{
		SourceOrderID: order, OrganizerID: in.OrganizerID, TargetTicketTypeID: in.TargetTicketTypeID,
		IdempotencyKey: key, Actor: in.Actor, Reason: in.Reason,
	})
	if err != nil {
		code, message := exchangeProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "bind order exchange", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	if ex.Settled {
		writeExchange(w, ex, true)
		return
	}

	// 1. The SOURCE must not be seated. Nothing associates an issued ticket with a seat,
	//    so an exchange of a seated line cannot say which seat leaves (TKT-164). Refused
	//    here, before the target is touched: checking after would mutate target inventory
	//    for a request that was always going to be refused.
	if seated, err := s.claimIsSeated(r, ex.OrganizerID, ex.HoldID); err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "inventory unavailable"})
		return
	} else if seated {
		write(w, http.StatusConflict, map[string]string{"error": "a seated order cannot be exchanged yet (no ticket-to-seat association)"})
		return
	}

	// 2. Price the target through catalog's RULE RESOLUTION, never the raw column and
	//    never the source's snapshot (ADR-036 §5/§6). The new line gets its own
	//    provenance; copying the old one forward is the specific mistake that ADR exists
	//    to prevent.
	resolution, err := s.resolveTicketTypePrice(r.Context(), ex.TargetTicketTypeID, ex.OrganizerID, ex.Quantity)
	if err != nil {
		if errors.Is(err, errResolveUnavailable) {
			write(w, http.StatusBadGateway, map[string]string{"error": "catalog unavailable"})
			return
		}
		write(w, 500, map[string]string{"error": "price resolution unusable"})
		return
	}
	targetTotal := resolution.total(ex.Quantity)
	if err := commercestore.ValidateExchangeTarget(ex, targetTotal, resolution.ResolvedPrice.Currency); err != nil {
		code, message := exchangeProblem(err)
		write(w, code, map[string]string{"error": message})
		return
	}

	// 3. Take the target hold. Sold out, closed, or seated on the target side stops here —
	//    still before any money.
	hold, err := s.holdExchangeTarget(r, ex, resolution)
	if err != nil {
		write(w, http.StatusConflict, map[string]string{"error": "exchange target is unavailable"})
		return
	}

	// 4. Only now does money move, and exactly once: the signed delta. NOT a refund of the
	//    old gross plus a charge of the new — that is two provider movements and the wrong
	//    cash-flow story.
	delta := commercestore.ExchangeDelta(ex.SourceTotal, targetTotal)
	if err := s.settleExchangeDelta(r, ex, delta); err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "exchange settlement unresolved"})
		return
	}

	// 5. Both GROSS legs are journalled whichever way the delta went. The provider moved
	//    the difference; the trail records that a line worth X was reversed and one worth
	//    Y was sold (ADR-003).
	if err := s.exchangeFacts(r, ex, targetTotal); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}

	replacement, err := s.persistExchangeReplacement(r, ex, resolution, hold, targetTotal)
	if err != nil {
		slog.Default().ErrorContext(r.Context(), "persist exchange replacement", "err", err)
		write(w, 500, map[string]string{"error": "persist exchange"})
		return
	}
	if err := commercestore.CompleteExchangeSettlement(r.Context(), s.db, ex.OrganizerID, ex.ID, replacement, targetTotal, delta); err != nil {
		slog.Default().ErrorContext(r.Context(), "complete exchange settlement", "err", err)
		write(w, 500, map[string]string{"error": "persist exchange"})
		return
	}
	ex.ReplacementOrderID, ex.TargetTotal, ex.DeltaAmount, ex.Settled = replacement, targetTotal, delta, true
	writeExchange(w, ex, false)
}

// writeExchange reports the exchange, including what has NOT happened yet. `switch_pending`
// is a real state and the response says so: the delta is settled and the replacement is
// confirmed, and the buyer still holds valid old tickets until TKT-166 switches them.
func writeExchange(w http.ResponseWriter, ex commercestore.Exchange, replay bool) {
	status := "switch_pending"
	if ex.TicketsExchanged {
		status = "completed"
	}
	write(w, 200, map[string]any{
		"exchange_id": ex.ID, "source_order_id": ex.SourceOrderID,
		"replacement_order_id": ex.ReplacementOrderID, "quantity": ex.Quantity,
		"source_total": ex.SourceTotal, "target_total": ex.TargetTotal,
		"delta_amount": ex.DeltaAmount, "currency": ex.Currency,
		"status": status, "tickets_exchanged": ex.TicketsExchanged, "replay": replay,
	})
}

// claimIsSeated asks inventory whether the source claim holds seats. Seatedness is a
// property of the claim, and inventory is the only service that knows it.
func (s *Server) claimIsSeated(r *http.Request, org, hold uuid.UUID) (bool, error) {
	code, body, err := s.call(r.Context(), http.MethodGet,
		fmt.Sprintf("%s/internal/holds/%s/seating?organizer_id=%s", s.inventoryURL, hold, org), "", nil, true)
	if err != nil || code != http.StatusOK {
		return false, fmt.Errorf("inventory seating lookup: status %d: %w", code, err)
	}
	var out struct {
		Seated bool `json:"seated"`
	}
	if json.Unmarshal(body, &out) != nil {
		return false, errors.New("invalid inventory seating response")
	}
	return out.Seated, nil
}

func (s *Server) holdExchangeTarget(r *http.Request, ex commercestore.Exchange, res priceResolution) (uuid.UUID, error) {
	body := map[string]any{
		"organizer_id": ex.OrganizerID, "slot_id": res.PerformanceID,
		"ticket_type_id": ex.TargetTicketTypeID, "quantity": ex.Quantity,
		"unit_amount": res.ResolvedPrice.Amount, "currency": res.ResolvedPrice.Currency,
	}
	code, out, err := s.call(r.Context(), http.MethodPost, s.inventoryURL+"/holds", "exchange:"+ex.ID.String(), body, false)
	if err != nil || (code != 200 && code != 201) {
		return uuid.Nil, fmt.Errorf("target hold: status %d: %w", code, err)
	}
	var hold struct {
		ID uuid.UUID `json:"hold_id"`
	}
	if json.Unmarshal(out, &hold) != nil || hold.ID == uuid.Nil {
		return uuid.Nil, errors.New("invalid inventory hold response")
	}
	return hold.ID, nil
}

// settleExchangeDelta moves exactly the difference, once. Upgrade charges it, downgrade
// refunds it through the partial-refund leg against the ORIGINAL charge, and an equal
// exchange calls nobody.
func (s *Server) settleExchangeDelta(r *http.Request, ex commercestore.Exchange, delta int64) error {
	switch {
	case delta == 0:
		return nil
	case delta > 0:
		code, _, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/charges",
			"exchange-charge:"+ex.ID.String(), map[string]any{
				"order_id": ex.SourceOrderID, "organizer_id": ex.OrganizerID, "buyer_id": ex.BuyerID,
				"amount": delta, "currency": ex.Currency, "payment_token": "fake-ok",
			}, true)
		if err != nil || code != http.StatusOK {
			return fmt.Errorf("exchange charge: status %d: %w", code, err)
		}
		return nil
	default:
		code, _, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/psp/partial-refund", "",
			map[string]any{
				"organizer_id": ex.OrganizerID, "idempotency_key": ex.PaymentSourceKey,
				"refund_key": "exchange-refund:" + ex.ID.String(), "amount": -delta, "currency": ex.Currency,
			}, true)
		if err != nil || code != http.StatusOK {
			return fmt.Errorf("exchange refund leg: status %d: %w", code, err)
		}
		return nil
	}
}

// exchangeFacts journals both GROSS legs. Deterministic ids and the exchange row's stable
// created_at, never the clock — the journal compares the whole canonical fact on replay.
func (s *Server) exchangeFacts(r *http.Request, ex commercestore.Exchange, targetTotal int64) error {
	occurred := ex.CreatedAt.UTC()
	for _, leg := range []struct {
		id     uuid.UUID
		typ    string
		amount int64
	}{
		{commercestore.ExchangeReversedFactID(ex.ID), "order.exchange.reversed", ex.SourceTotal},
		{commercestore.ExchangeSoldFactID(ex.ID), "order.exchange.sold", targetTotal},
	} {
		if _, err := s.db.ExecContext(r.Context(), `
			INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`,
			leg.id, ex.SourceOrderID, ex.OrganizerID, ex.BuyerID, leg.typ, leg.amount, ex.Currency, occurred); err != nil {
			return err
		}
		code, _, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/facts", "", map[string]any{
			"fact_id": leg.id, "organizer_id": ex.OrganizerID, "fact_type": leg.typ,
			"buyer_id": ex.BuyerID, "amount": leg.amount, "currency": ex.Currency,
			"occurred_at": occurred.Format(time.RFC3339Nano),
			"payload":     map[string]string{"order_id": ex.SourceOrderID.String()},
		}, true)
		if err != nil || code != 200 {
			return errors.New("journal unavailable")
		}
	}
	return nil
}

// persistExchangeReplacement writes the replacement reservation and order. It deliberately
// does NOT owe an `order.completed` event: that would make access issue the new tickets
// while the old ones still admit — a both-admit window, which is exactly what the chosen
// failure mode forbids. TKT-166 publishes the exchange event that switches them atomically.
func (s *Server) persistExchangeReplacement(r *http.Request, ex commercestore.Exchange, res priceResolution, hold uuid.UUID, targetTotal int64) (uuid.UUID, error) {
	reservation := uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-reservation:"+ex.ID.String()))
	replacement := uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-order:"+ex.ID.String()))
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status,price_resolution_snapshot)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'completed',$11) ON CONFLICT(id) DO NOTHING`,
		reservation, ex.OrganizerID, hold, res.PerformanceID, ex.TargetTicketTypeID, ex.BuyerID,
		ex.Quantity, res.ResolvedPrice.Amount, targetTotal, res.ResolvedPrice.Currency, []byte(res.raw)); err != nil {
		return uuid.Nil, err
	}
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'exchange') ON CONFLICT(id) DO NOTHING`,
		replacement, reservation, "exchange:"+ex.ID.String()); err != nil {
		return uuid.Nil, err
	}
	return replacement, nil
}
