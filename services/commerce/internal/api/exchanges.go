package api

import (
	"context"
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

	"ticketing/shared/httpx"
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
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
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
		// The replay re-drives settlement, which is idempotent and — on a row that already
		// owes its event — a no-op that costs one locked read (ai-review pass 4).
		//
		// It is here to make a premise unnecessary rather than to fix a reachable bug.
		// Settlement and the outbox row share one transaction, so this branch cannot
		// currently observe `settled_at` set with no event owed; the only way to produce
		// one is data written by the pre-TKT-166 code, and TKT-158 is merged but never
		// released (no tags, no deploy workflow, and it reached `main` the same day). But
		// "unreachable given how it was rolled out" is a claim that has to be re-argued
		// every time this path is touched, and the replay is the natural place to repair a
		// settled exchange that owes nothing — so it repairs one.
		if err := commercestore.CompleteExchangeSettlement(r.Context(), s.db, existing.OrganizerID, existing.ID, existing.ReplacementOrderID); err != nil {
			slog.Default().ErrorContext(r.Context(), "repair settled exchange on replay", "exchange_id", existing.ID, "err", err)
			write(w, 500, map[string]string{"error": "persist exchange"})
			return
		}
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
	resolution, err := s.resolveTicketTypePrice(r.Context(), in.TargetTicketTypeID, in.OrganizerID, src.Quantity, src.ChannelCode)
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
	hold, err := s.holdExchangeTarget(r, exchangeID, in.OrganizerID, in.TargetTicketTypeID, src.Quantity, resolution, src)
	if err != nil {
		write(w, http.StatusConflict, map[string]string{"error": "exchange target is unavailable"})
		return
	}

	ex, err := commercestore.BindOrderExchange(r.Context(), s.db, request)
	if err != nil {
		// Release the hold THIS request took — but only when it is certainly ours
		// (ai-review pass 4). The hold is keyed on the exchange IDENTITY, and two
		// concurrent requests sharing an idempotency key and target share that identity,
		// so they share the hold. On ErrExchangeConflict the other request is the owner
		// and is about to finalize this very claim; releasing it would make the WINNER
		// fail, leaving a durable exchange bound to a released claim. Every other bind
		// error means no exchange under this identity bound at all, so the hold is ours.
		if shouldReleaseHoldOnBindError(err) {
			s.releaseExchangeHold(r, in.OrganizerID, hold)
		}
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

	// Prove the whole exchange is REPRESENTABLE before ANYTHING becomes hard to
	// undo (ai-review passes 3 and 4). The carried fee is added to the target to
	// produce the replacement's gross, and an unrepresentable sum has no good
	// resting place later: after settlement the delta is charged and the fact
	// cannot be journalled, and after FINALIZE the claim is out of the expiry
	// predicate — so a permanently-refusing exchange would withhold that capacity
	// forever while every retry hit the same arithmetic. Refusing here costs a
	// 409 and nothing else.
	if _, err := replacementGross(ex, ex.TargetTotal); err != nil {
		write(w, http.StatusConflict, map[string]string{"error": "exchange total out of range"})
		return
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

// shouldReleaseHoldOnBindError decides whether the hold this request took belongs to it.
//
// ErrExchangeConflict is the one case where it does not: the same exchange identity is
// already bound by another request, which therefore shares this hold and is about to
// finalize it. Releasing then breaks the winner rather than tidying after the loser.
func shouldReleaseHoldOnBindError(err error) bool {
	return !errors.Is(err, commercestore.ErrExchangeConflict)
}

// releaseExchangeHold returns a hold this request created and will not use. Best-effort:
// an unreleased hold expires on its own, so a failure here costs a TTL of capacity rather
// than correctness — which is why it must not turn a refusal into a 500.
//
// It runs on a DETACHED context (ai-review pass 4). The commonest reason a bind fails is
// the client giving up, and r.Context() is already cancelled by then — so cleanup keyed to
// it cannot run at exactly the moment it is most needed, during the contention that made
// the client give up. Bounded, because best-effort work must not outlive its usefulness.
func (s *Server) releaseExchangeHold(r *http.Request, org, hold uuid.UUID) {
	if hold == uuid.Nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	code, _, err := s.call(ctx, http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/release?organizer_id=%s", s.inventoryURL, hold, org), "", nil, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(ctx, "exchange target hold not released; it will expire",
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
	// Three states, not two (ai-review pass 3). `completed` used to mean only that the
	// tickets had switched, which reported an exchange as done while the old capacity was
	// still withheld — hiding the very substate migration 0011 exists to expose.
	status := "switch_pending"
	switch {
	case ex.TicketsExchanged && ex.CapacityReturned:
		status = "completed"
	case ex.TicketsExchanged:
		status = "capacity_pending"
	}
	write(w, 200, map[string]any{
		"exchange_id": ex.ID, "source_order_id": ex.SourceOrderID,
		"replacement_order_id": ex.ReplacementOrderID, "quantity": ex.Quantity,
		"source_total": ex.SourceTotal, "target_total": ex.TargetTotal,
		"delta_amount": ex.DeltaAmount, "currency": ex.Currency,
		"status": status, "tickets_exchanged": ex.TicketsExchanged,
		"capacity_returned": ex.CapacityReturned, "replay": replay,
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

func (s *Server) holdExchangeTarget(r *http.Request, exchangeID, org, ticketType uuid.UUID, quantity int32, res priceResolution, src commercestore.ExchangeSource) (uuid.UUID, error) {
	body := map[string]any{
		"organizer_id": org, "slot_id": res.PerformanceID,
		"ticket_type_id": ticketType, "quantity": quantity,
		"unit_amount": res.ResolvedPrice.Amount, "currency": res.ResolvedPrice.Currency,
	}
	// The target hold consumes the SOURCE's channel and presents its reseller
	// (TKT-246). The second of the two sibling paths TKT-240 missed.
	//
	// Before this, the exchange target sent no channel at all while the target was
	// REPRICED on the source's channel (TKT-237) -- so a channelled exchange took
	// that channel's prices out of PUBLIC stock, and the channel's own allocation
	// never moved. The two facts have to travel together or the money and the
	// inventory describe different sales.
	//
	// Both come from the PERSISTED source reservation, not from a request or a
	// session: an exchange is a staff or buyer action on an existing order, and the
	// reseller that sold it is a historical fact of that row. That also makes the
	// hold's authorization reproducible on a retry, which a caller-supplied identity
	// would not be.
	// BOTH or NEITHER, gated on the RESELLER (ai-review [high] F2).
	//
	// Forwarding the channel whenever the source had one re-opens the bypass this
	// ticket exists to close, one step removed in time. A PUBLIC reserve still
	// persists whatever channel_code its unauthenticated body named -- it is used for
	// fee resolution and reporting, and only the inventory forward was withheld. So a
	// public buyer could name a reseller's channel, and a later exchange of that order
	// would present the channel to inventory with no reseller identity, consuming an
	// unbound allocation that nobody authorized them to touch. Every allocation is
	// unbound today, so it would have been reachable on day one.
	//
	// The reseller is the authority, not the channel: a source with no reseller was
	// never an authorized channelled sale, whatever its channel_code says. Gating on
	// src.ResellerID means the exchange inherits authorization only where
	// authorization actually existed, and a public source's target stays public --
	// which is exactly what its ORIGINAL hold did.
	if src.ResellerID != nil && src.ChannelCode != nil {
		body["channel"] = *src.ChannelCode
		body["reseller_id"] = *src.ResellerID
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
		// The delta charge settles FEE-FREE, and that follows from a decision this
		// epic already took rather than from convenience: TKT-215 made the fee
		// travel WITH the order on an exchange, so no fee money moves here — the
		// delta is a pure price difference and the organizer is owed all of it.
		//
		// A settlement plan is required because payments refuses a captured fact
		// with no attribution (ADR-048); "no fees" is an attribution, not an
		// absence of one.
		code, _, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/charges",
			"exchange-charge:"+ex.ID.String(), map[string]any{
				"order_id": ex.SourceOrderID, "organizer_id": ex.OrganizerID, "buyer_id": ex.BuyerID,
				"amount": delta, "currency": ex.Currency, "payment_token": "fake-ok",
				"settlement": map[string]any{
					"face_value": delta, "passed_on": 0, "absorbed": 0,
					"total_amount": delta, "currency": ex.Currency, "fees": []any{},
				},
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
// exchangeFactLeg is one money fact an exchange publishes.
type exchangeFactLeg struct {
	id     uuid.UUID
	typ    string
	amount int64
}

// exchangeFactLegs decides WHICH amount each leg carries, and is a named
// function so that decision is testable without a database (ai-review pass 2).
//
// The reversal leg carries the GROSS — what was actually captured — while the
// exchange DELTA is computed from face values. Repointing the delta at the face
// value was correct and silently made this fact wrong: a fee-carrying order
// reversed the face value against a gross capture, and the payments journal
// stopped agreeing with the original charge. The delta is a price comparison;
// the reversal is a money movement. They need different numbers.
// replacementGross is what the buyer has paid for the order they end up holding:
// the new face value plus the fee carried over from the old one.
//
// It returns an error rather than wrapping, and that is not defensive
// decoration — the first version added the two unchecked, and with a source
// gross near int64's ceiling the sum wrapped NEGATIVE. Worse, the balance test
// accepted it, because `sold - reversed` wraps back to the right answer: two
// wrapped numbers whose difference is correct. Go's arithmetic is consistent
// even when it is nonsense.
//
// The check must run BEFORE the provider moves money. After settlement there is
// no good answer left: the delta has been charged, and commerce would be trying
// to journal a fact it cannot represent.
func replacementGross(ex commercestore.Exchange, targetTotal int64) (int64, error) {
	retained, err := checkedSub(ex.SourceGrossTotal, ex.SourceTotal)
	if err != nil {
		return 0, err
	}
	return checkedAdd(targetTotal, retained)
}

func exchangeFactLegs(ex commercestore.Exchange, targetTotal int64) ([]exchangeFactLeg, error) {
	// The retained fee travels with the order. targetTotal is a rule-resolved
	// price carrying no fee, so selling it bare against a GROSS reversal records
	// a refund the provider never made:
	//
	//	face 9100, gross 9400, target 9100  ->  provider delta 0
	//	reversed 9400, sold 9100            ->  journal net -300
	//
	// Carrying the fee is also the only reading that needs no money to move, and
	// this ticket deliberately does not decide fee-on-exchange policy — that is
	// the product question carved out of TKT-6. Carrying preserves the status quo:
	// the buyer paid 9400, still holds a ticket, and nothing is refunded or
	// recharged.
	sold, err := replacementGross(ex, targetTotal)
	if err != nil {
		return nil, err
	}
	return []exchangeFactLeg{
		{commercestore.ExchangeReversedFactID(ex.ID), "order.exchange.reversed", ex.SourceGrossTotal},
		{commercestore.ExchangeSoldFactID(ex.ID), "order.exchange.sold", sold},
	}, nil
}

func (s *Server) exchangeFacts(r *http.Request, ex commercestore.Exchange, targetTotal int64) error {
	occurred := ex.CreatedAt.UTC()
	legs, err := exchangeFactLegs(ex, targetTotal)
	if err != nil {
		return err
	}
	for _, leg := range legs {
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
	// The replacement must record what the buyer actually paid for it, or
	// commerce and the payments journal disagree about the same sale
	// (ai-review pass 3). Storing the bare target as both total and face made the
	// carried fee vanish from the order the buyer now holds — and a SECOND
	// exchange of that order would then reverse a gross that had silently lost
	// the fee.
	gross, err := replacementGross(ex, ex.TargetTotal)
	if err != nil {
		return uuid.Nil, err
	}
	reservation := ex.ReplacementReservationID
	replacement := uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-order:"+ex.ID.String()))
	// The replacement inherits the source's SALES ATTRIBUTION -- channel_code and
	// reseller_id -- copied in the INSERT from the source reservation (TKT-246).
	//
	// Without this, exchanging a reseller-attributed order produced a public,
	// unattributed one. Irreversible in the strict sense: nothing else records who
	// sold a ticket, so once the replacement is written the fact is gone, and
	// settlement (TKT-23) would pay the wrong party or nobody. The source row is the
	// authority rather than any request field, so a retry reproduces it exactly.
	//
	// The same reasoning the customer_id copy below already applies, extended to the
	// two columns it did not cover. That comment says an exchange is "the same
	// purchase in a different seat" -- who sold that purchase does not change either.
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status,price_resolution_snapshot,channel_code,reseller_id)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'completed',$12,src.channel_code,src.reseller_id
		FROM reservations src WHERE src.id=$13
		ON CONFLICT(id) DO NOTHING`,
		reservation, ex.OrganizerID, ex.TargetHoldID, ex.TargetSlotID, ex.TargetTicketTypeID, ex.BuyerID,
		ex.Quantity, ex.TargetUnitAmount, gross, ex.TargetTotal, ex.Currency, ex.TargetPriceSnapshot,
		ex.SourceReservation); err != nil {
		return uuid.Nil, err
	}
	// The replacement INHERITS the source order's attribution (TKT-221). An
	// exchange is the same purchase in a different seat, so a signed-in buyer's
	// exchanged order has to stay theirs — otherwise it silently disappears from
	// the account, and the buyer has no way to tell that from a lost order.
	//
	// Copied inside the INSERT rather than read first: there is no window where
	// the source could change (an exchange holds its own order), and a second
	// round trip to fetch one column is work for nothing. A guest source selects
	// NULL, which is exactly right.
	// The order inherits customer_id from the SOURCE ORDER and its sales attribution
	// from the REPLACEMENT RESERVATION written just above (TKT-246).
	//
	// Two sources, deliberately, because the columns mean different things and live
	// in different places: customer_id is a property of the order (who bought), while
	// channel_code and reseller_id were just copied onto the replacement reservation
	// (who sold). Reading the attribution back from the row this function authored
	// keeps the order and its reservation in agreement by construction -- the normal
	// checkout path does the same thing (server.go: SELECT r.channel_code,
	// r.reseller_id FROM reservations r), so both writers of an order derive
	// attribution the same way rather than two ways that must be kept in step.
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,customer_id,channel_code,reseller_id)
		SELECT $1,$2,'completed',$3,'exchange',src.customer_id,rep.channel_code,rep.reseller_id
		FROM orders src, reservations rep WHERE src.id=$4 AND rep.id=$2
		ON CONFLICT(id) DO NOTHING`,
		replacement, reservation, "exchange:"+ex.ID.String(), ex.SourceOrderID); err != nil {
		return uuid.Nil, err
	}
	return replacement, nil
}

// exchangeTicketsSwitched is access reporting that its switch transaction COMMITTED
// (TKT-166, ADR-038 §1).
//
// The whole endpoint exists to make one ordering checkable across a service boundary.
// Commerce cannot see access's transaction, and access has no authority over the source
// hold — so without an explicit callback, either capacity comes back on a timer that
// cannot know whether the old tickets still admit, or access acquires hold authority it
// should not have. Both are worse than one call.
//
// The marker is recorded BEFORE inventory is asked. That order is deliberate and it is
// the reason `capacity_returned_at` exists (migration 0011): marking after would mean a
// crash could free capacity while the row still claimed the switch never happened, and
// the ordering this endpoint enforces would be unauditable. Marking first leaves the
// opposite substate — switched, capacity outstanding — which under-sells until the retry
// lands, and is visible.
//
// This is an HONEST-CALLER guarantee, not tamper-evidence (ADR-021): anyone holding the
// internal token can call inventory's refund-capacity directly and skip all of this. The
// adversary being defended against here is a crash, not a writer.
func (s *Server) exchangeTicketsSwitched(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid exchange"})
		return
	}
	var body struct {
		OrganizerID uuid.UUID `json:"organizer_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.OrganizerID == uuid.Nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid switch notification"})
		return
	}
	ex, err := commercestore.LoadExchangeSwitch(r.Context(), s.db, body.OrganizerID, id)
	if err != nil {
		if errors.Is(err, commercestore.ErrExchangeNotSettled) {
			write(w, http.StatusNotFound, map[string]string{"error": "no settled exchange"})
			return
		}
		slog.Default().ErrorContext(r.Context(), "load exchange switch", "exchange_id", id, "err", err)
		write(w, 500, map[string]string{"error": "load exchange"})
		return
	}
	if !ex.TicketsExchanged {
		if err := commercestore.MarkExchangeTicketsSwitched(r.Context(), s.db, ex.OrganizerID, ex.ID); err != nil {
			slog.Default().ErrorContext(r.Context(), "record exchange switch", "exchange_id", id, "err", err)
			write(w, 500, map[string]string{"error": "record switch"})
			return
		}
		ex.TicketsExchanged = true
	}
	ex = s.returnExchangedCapacity(r, ex)
	if !ex.CapacityReturned {
		// The switch is committed and recorded either way — that is the half that had to
		// happen first. Answering 502 keeps the caller's message unacknowledged so the
		// return is retried; a replay finds the marker set and drives only the remainder.
		write(w, http.StatusBadGateway, map[string]string{"error": "capacity return unresolved"})
		return
	}
	write(w, http.StatusOK, map[string]any{"exchange_id": ex.ID, "tickets_exchanged": true, "capacity_returned": true})
}

// returnExchangedCapacity gives the OLD line's capacity back, reusing the refund-capacity
// operation and its receipt. The exchange id is the deterministic `refund_id`, so a repeat
// answers as a replay rather than returning capacity twice.
//
// Reusing a refund-named contract for an exchange is a real cost — the receipt in
// `claim_history` says `refund_return` for something nobody refunded — and it buys the
// idempotent, seated-aware return that already exists. The source claim is GA by
// construction (TKT-158 refuses a seated source), and the return is FULL, which is the
// case ADR-038 §9 says is the only one seated claims accept anyway.
func (s *Server) returnExchangedCapacity(r *http.Request, ex commercestore.ExchangeSwitch) commercestore.ExchangeSwitch {
	if ex.CapacityReturned || s.inventoryURL == "" || ex.SourceHoldID == uuid.Nil {
		return ex
	}
	code, _, err := s.call(r.Context(), http.MethodPost,
		fmt.Sprintf("%s/internal/holds/%s/refund-capacity", s.inventoryURL, ex.SourceHoldID), "",
		map[string]any{"organizer_id": ex.OrganizerID, "refund_id": ex.ID, "quantity": ex.Quantity}, true)
	if err != nil || code != http.StatusOK {
		slog.Default().WarnContext(r.Context(), "exchange capacity not returned; left outstanding",
			"exchange_id", ex.ID, "hold_id", ex.SourceHoldID, "status", code, "err", err)
		return ex
	}
	if err := commercestore.MarkExchangeCapacityReturned(r.Context(), s.db, ex.OrganizerID, ex.ID); err != nil {
		slog.Default().ErrorContext(r.Context(), "record exchange capacity return", "exchange_id", ex.ID, "err", err)
		return ex
	}
	ex.CapacityReturned = true
	return ex
}
