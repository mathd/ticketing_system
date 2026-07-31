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

	// A SETTLED replay answers first, before any external call (ai-review pass 2).
	// Resolving the price on every request meant a settled exchange could not be replayed
	// while catalog was unreachable — an operation that already happened failing because
	// of a dependency it no longer needs.
	exchangeID := commercestore.ExchangeID(in.OrganizerID, key)
	request := commercestore.ExchangeRequest{
		SourceOrderID: order, OrganizerID: in.OrganizerID, TargetTicketTypeID: in.TargetTicketTypeID,
		IdempotencyKey: key, Actor: in.Actor, Reason: in.Reason,
	}
	existing, found, err := commercestore.LookupExchangeFor(r.Context(), s.db, request)
	if err != nil {
		code, message := exchangeProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "look up exchange", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	if found && existing.Settled {
		writeExchange(w, existing, true)
		return
	}

	// ELIGIBILITY, and nothing durable yet (ai-review F2). Binding before these checks left
	// a row behind on every refusal — a typo or a sold-out target then made the order
	// permanently unreversible, because the one-per-order index blocked a corrected attempt
	// and the refund path treats any exchange row as a live exchange. No money had moved;
	// the order was simply stuck.
	src, err := commercestore.LoadExchangeSource(r.Context(), s.db, in.OrganizerID, order)
	if err != nil {
		code, message := exchangeProblem(err)
		write(w, code, map[string]string{"error": message})
		return
	}

	// The SOURCE must not be seated: nothing associates an issued ticket with a seat, so
	// an exchange of a seated line cannot say which seat leaves (TKT-164).
	if seated, err := s.claimIsSeated(r, in.OrganizerID, src.HoldID); err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "inventory unavailable"})
		return
	} else if seated {
		write(w, http.StatusConflict, map[string]string{"error": "a seated order cannot be exchanged yet (no ticket-to-seat association)"})
		return
	}

	// Price the target through catalog's RULE RESOLUTION — never the raw column, never the
	// source's snapshot (ADR-036 §5/§6).
	resolution, err := s.resolveTicketTypePrice(r.Context(), in.TargetTicketTypeID, in.OrganizerID, src.Quantity)
	if err != nil {
		if errors.Is(err, errResolveUnavailable) {
			write(w, http.StatusBadGateway, map[string]string{"error": "catalog unavailable"})
			return
		}
		write(w, 500, map[string]string{"error": "price resolution unusable"})
		return
	}
	targetTotal := resolution.total(src.Quantity)
	if resolution.ResolvedPrice.Currency != src.Currency {
		write(w, http.StatusConflict, map[string]string{"error": "exchange target is priced in a different currency"})
		return
	}

	// THE TARGET HOLD, still before anything durable (ai-review pass 2, P2-1). Taking it
	// after the bind meant a sold-out target left an exchange row behind and locked the
	// order exactly as a typo did. A hold that is never used simply expires; a durable row
	// does not.
	hold, err := s.holdExchangeTarget(r, exchangeID, in.OrganizerID, in.TargetTicketTypeID, src.Quantity, resolution)
	if err != nil {
		write(w, http.StatusConflict, map[string]string{"error": "exchange target is unavailable"})
		return
	}

	ex, err := commercestore.BindOrderExchange(r.Context(), s.db, request)
	if err != nil {
		// Release the hold THIS request just took. A losing racer that keeps its hold
		// consumes target capacity for the full TTL, so repeated attempts against an
		// already-exchanged order drain an on-sale pool (ai-review pass 3). Best-effort and
		// safe either way: the hold is keyed on this exchange identity, so releasing it
		// cannot touch another request's claim.
		s.releaseExchangeHold(r, in.OrganizerID, hold)
		code, message := exchangeProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "bind order exchange", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	ex.BuyerID, ex.HoldID, ex.PaymentSourceKey = src.BuyerID, src.HoldID, src.PaymentSourceKey

	// THE BASIS, before any money (ai-review F3), and complete: hold, reservation, total,
	// signed delta, AND the unit price, slot and provenance snapshot. The replacement is
	// written from these, so a price change between the basis and the replacement cannot
	// produce a reservation whose total disagrees with quantity × unit, or whose
	// provenance describes a different basis from the money that moved (pass 2, P2-3).
	if !ex.BasisRecorded {
		basis := commercestore.ExchangeBasis{
			TargetHoldID:             hold,
			ReplacementReservationID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-reservation:"+ex.ID.String())),
			TargetSlotID:             resolution.PerformanceID,
			TargetTotal:              targetTotal,
			DeltaAmount:              commercestore.ExchangeDelta(ex.SourceTotal, targetTotal),
			TargetUnitAmount:         resolution.ResolvedPrice.Amount,
			PriceSnapshot:            []byte(resolution.raw),
		}
		recorded, err := commercestore.RecordExchangeBasis(r.Context(), s.db, ex.OrganizerID, ex.ID, basis)
		if err != nil {
			slog.Default().ErrorContext(r.Context(), "record exchange basis", "err", err)
			write(w, 500, map[string]string{"error": "persist exchange"})
			return
		}
		if !recorded {
			// Another writer persisted a basis first, and the money's basis is theirs, not
			// the one in this request's hand. Continuing on an unpersisted basis is how a
			// reservation ends up storing a snapshot that disagrees with the exchange row
			// (ai-review pass 3). Resuming from the authoritative row is TKT-167; refusing
			// to guess is this ticket's job.
			write(w, http.StatusConflict, map[string]string{"error": "exchange is already in flight; retry with the same key"})
			return
		}
		ex.TargetHoldID, ex.ReplacementReservationID = basis.TargetHoldID, basis.ReplacementReservationID
		ex.TargetSlotID, ex.TargetUnitAmount, ex.TargetPriceSnapshot = basis.TargetSlotID, basis.TargetUnitAmount, basis.PriceSnapshot
		ex.TargetTotal, ex.DeltaAmount, ex.BasisRecorded = basis.TargetTotal, basis.DeltaAmount, true
	}

	// FINALIZE before the money and CONFIRM after — the sequence checkout uses, and the
	// steps the plan listed that the first implementation dropped (ai-review F1). Finalize
	// also takes the claim out of the expiry predicate, which only fires on `held`, so the
	// target cannot lapse between settlement and confirmation.
	if err := s.transitionExchangeHold(r, ex, "finalize"); err != nil {
		write(w, http.StatusConflict, map[string]string{"error": "exchange target is unavailable"})
		return
	}
	if err := s.settleExchangeDelta(r, ex, ex.DeltaAmount); err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "exchange settlement unresolved"})
		return
	}
	if err := s.transitionExchangeHold(r, ex, "confirm"); err != nil {
		// Money moved, capacity did not confirm. The basis is durable and the claim is
		// finalizing (so it cannot expire), which is what makes the retry able to finish
		// against the same numbers rather than re-deriving them.
		write(w, http.StatusAccepted, map[string]any{"exchange_id": ex.ID, "status": "confirmation_pending"})
		return
	}

	// Both GROSS legs, whichever way the delta went (ADR-003, ADR-039 §1).
	if err := s.exchangeFacts(r, ex, ex.TargetTotal); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	replacement, err := s.persistExchangeReplacement(r, ex)
	if err != nil {
		slog.Default().ErrorContext(r.Context(), "persist exchange replacement", "err", err)
		write(w, 500, map[string]string{"error": "persist exchange"})
		return
	}
	if err := commercestore.CompleteExchangeSettlement(r.Context(), s.db, ex.OrganizerID, ex.ID, replacement); err != nil {
		slog.Default().ErrorContext(r.Context(), "complete exchange settlement", "err", err)
		write(w, 500, map[string]string{"error": "persist exchange"})
		return
	}
	ex.ReplacementOrderID, ex.Settled = replacement, true
	writeExchange(w, ex, false)
}

// releaseExchangeHold returns a hold this request created and will not use. Best-effort:
// an unreleased hold expires on its own, so a failure here costs a TTL of capacity rather
// than correctness — which is why it must not turn a refusal into a 500.
func (s *Server) releaseExchangeHold(r *http.Request, org, hold uuid.UUID) {
	if hold == uuid.Nil {
		return
	}
	code, _, err := s.call(r.Context(), http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/release?organizer_id=%s", s.inventoryURL, hold, org), "", nil, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(r.Context(), "exchange target hold not released; it will expire",
			"hold_id", hold, "status", code, "err", err)
	}
}

// transitionExchangeHold drives the target claim through the same finalize/confirm steps
// checkout uses. Idempotent on inventory's side: a replayed transition to a state the
// claim already holds is satisfied, not a conflict.
func (s *Server) transitionExchangeHold(r *http.Request, ex commercestore.Exchange, step string) error {
	code, _, err := s.call(r.Context(), http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/%s?organizer_id=%s", s.inventoryURL, ex.TargetHoldID, step, ex.OrganizerID), "", nil, true)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("target hold %s: status %d: %w", step, code, err)
	}
	return nil
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

func (s *Server) holdExchangeTarget(r *http.Request, exchangeID, org, ticketType uuid.UUID, quantity int32, res priceResolution) (uuid.UUID, error) {
	body := map[string]any{
		"organizer_id": org, "slot_id": res.PerformanceID,
		"ticket_type_id": ticketType, "quantity": quantity,
		"unit_amount": res.ResolvedPrice.Amount, "currency": res.ResolvedPrice.Currency,
	}
	// Keyed on the EXCHANGE identity, which is derivable from (organizer, idempotency key)
	// before the exchange row exists — that is what lets the hold precede the bind (P2-1)
	// without two different exchanges sharing a hold. Keying it on organizer+ticket type
	// did exactly that: a second exchange onto the same type replayed the first one's hold
	// and then collided persisting its replacement.
	code, out, err := s.call(r.Context(), http.MethodPost, s.inventoryURL+"/holds",
		"exchange-target:"+exchangeID.String(), body, false)
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
// persistExchangeReplacement writes the replacement from the PERSISTED basis only — never
// from a freshly resolved price (ai-review pass 2, P2-3). Mixing a stored total with a
// fresh unit amount is how a reservation ends up claiming a total that is not its quantity
// times its unit price, with provenance describing a basis the money never used.
//
// It deliberately does NOT owe an `order.completed` event: that would make access issue the
// new tickets while the old ones still admit — the both-admit window ADR-039 §3 rejects.
// TKT-166 publishes the exchange event that switches them atomically.
func (s *Server) persistExchangeReplacement(r *http.Request, ex commercestore.Exchange) (uuid.UUID, error) {
	reservation := ex.ReplacementReservationID
	replacement := uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-order:"+ex.ID.String()))
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status,price_resolution_snapshot)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'completed',$11) ON CONFLICT(id) DO NOTHING`,
		reservation, ex.OrganizerID, ex.TargetHoldID, ex.TargetSlotID, ex.TargetTicketTypeID, ex.BuyerID,
		ex.Quantity, ex.TargetUnitAmount, ex.TargetTotal, ex.Currency, ex.TargetPriceSnapshot); err != nil {
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
