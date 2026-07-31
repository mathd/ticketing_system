package api

import (
	"errors"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"ticketing/services/catalog/internal/store"
)

// CacheControlPriceResolution is a correctness tier, not a performance one
// (ADR-004's "never" bucket, fixed by ADR-036 §6): a resolved price feeds a
// money decision, and once TKT-152 adds effective windows the answer's
// correctness expires at a known instant, so caching it past that instant would
// serve a stale price to a buyer.
const CacheControlPriceResolution = "no-store"

// ResolveTicketTypePrice answers "what does this ticket type cost right now,
// and why" (TKT-151 / ADR-036 §6 — catalog's question since that ADR amended
// ADR-002's pricing row).
//
// The evaluation instant is captured HERE and is deliberately not a request
// parameter: a caller-supplied instant on a sale-time price endpoint would let
// anyone ask for early-bird pricing after the window closed. The store and the
// pure comparator still take an instant, so the clock stays testable below HTTP
// — which is also where TKT-152's boundary proof drives it.
func (s *Server) ResolveTicketTypePrice(w http.ResponseWriter, r *http.Request, ticketTypeID openapi_types.UUID) {
	sel, err := s.store.ResolveTicketTypePrice(r.Context(), ticketTypeID, time.Now().UTC())
	if err != nil {
		// A rule whose currency differs from the ticket type's is invalid
		// configuration in OUR data, not something the caller can fix by
		// changing the request — so it is a 5xx, not a 409. ADR-028 sets the
		// precedent: server-side data the service cannot honour fails closed.
		// The offending rule id is logged, never returned: this operation is
		// reachable through the gateway and rule ids are not public.
		if errors.Is(err, store.ErrPriceRuleCurrencyMismatch) {
			s.log.ErrorContext(r.Context(), "price rule currency mismatch",
				"ticket_type_id", ticketTypeID, "err", err)
			writeJSON(w, http.StatusInternalServerError, Error{Error: "price rule misconfigured"})
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", CacheControlPriceResolution)
	writeJSON(w, http.StatusOK, priceResolutionToAPI(sel))
}

func priceResolutionToAPI(sel store.RuleSelection) PriceResolution {
	out := PriceResolution{
		ResolverVersion: sel.ResolverVersion,
		EvaluatedAt:     sel.EvaluatedAt,
		BasePrice:       Money{Amount: sel.BasePrice.Amount, Currency: sel.BasePrice.Currency},
		ResolvedPrice:   Money{Amount: sel.ResolvedPrice.Amount, Currency: sel.ResolvedPrice.Currency},
		// Never nil: the contract requires the array, and "no losers" must
		// serialize as [] rather than null.
		Candidates: []LosingPriceRule{},
	}
	if sel.Winner != nil {
		winner := priceRuleToAPI(*sel.Winner)
		out.Winner = &winner
	}
	if sel.FallbackReason != nil {
		reason := PriceResolutionFallbackReason(*sel.FallbackReason)
		out.FallbackReason = &reason
	}
	for _, c := range sel.Candidates {
		out.Candidates = append(out.Candidates, LosingPriceRule{
			Rule:   priceRuleToAPI(c.Rule),
			Reason: LosingPriceRuleReason(c.Reason),
		})
	}
	return out
}

// priceRuleToAPI maps one rule into its provenance shape. effective_from and
// effective_until stay nil until TKT-152 — declared now so the shape TKT-153
// persists as a snapshot does not change between the two stories.
func priceRuleToAPI(r store.PriceRule) PriceRuleProvenance {
	return PriceRuleProvenance{
		RuleId:     r.ID,
		ScopeLevel: PriceRuleProvenanceScopeLevel(r.ScopeLevel),
		ScopeId:    r.ScopeID,
		ActionKind: PriceRuleProvenanceActionKind(r.ActionKind),
		Amount:     r.Amount,
		Currency:   r.Currency,
		Priority:   r.Priority,
		Forced:     r.ForceAncestorOverride,
	}
}
