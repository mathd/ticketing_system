package api

import (
	"errors"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"ticketing/services/catalog/internal/store"
)

// ResolveTicketTypeFees answers "which fees apply to this ticket type, in this
// channel, right now, and why" (TKT-214 / ADR-046).
//
// INTERNAL, and the credential check is the first thing it does. This response
// carries `absorbed` fees — the organizer's cost structure, not anything the
// buyer pays — so unlike its price-resolution sibling it is not a public read.
// The gateway already denies /api/<svc>/internal/ at the edge; this guard is
// what stands between it and the container network, and it is inline rather than
// declared because ADR-043 puts a service's internal surface on that side of the
// line.
//
// The evaluation instant is captured HERE and is deliberately not a request
// parameter, for the same reason ResolveTicketTypePrice refuses one: a
// caller-supplied instant on a sale-time endpoint would let anyone ask for a fee
// schedule that has expired or has not opened. The store and the pure comparator
// still take an instant, so the clock stays testable below HTTP.
func (s *Server) ResolveTicketTypeFees(w http.ResponseWriter, r *http.Request, id openapi_types.UUID, params ResolveTicketTypeFeesParams) {
	if s.internalCredential == "" || r.Header.Get("X-Internal-Token") != s.internalCredential {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	// nil channel is the default/public context, in which only channel-agnostic
	// rules are eligible. Omitting the parameter is NOT a wildcard, and the
	// pointer is what carries that distinction all the way to the comparator —
	// a "" sentinel would be indistinguishable from a caller who sent an empty
	// channel, which the contract's minLength refuses anyway.
	sel, err := s.store.ResolveTicketTypeFees(r.Context(), id, params.ChannelCode, time.Now().UTC())
	if err != nil {
		// A rule whose currency differs from the ticket type's is invalid
		// configuration in OUR data, not something the caller can fix by
		// changing the request — so it is a 5xx, not a 409. ADR-028 sets the
		// precedent: server-side data the service cannot honour fails closed.
		// The offending rule id is logged, never returned.
		if errors.Is(err, store.ErrFeeRuleCurrencyMismatch) {
			s.log.ErrorContext(r.Context(), "fee rule currency mismatch",
				"ticket_type_id", id, "err", err)
			writeJSON(w, http.StatusInternalServerError, Error{Error: "fee rule misconfigured"})
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", CacheControlPriceResolution)
	writeJSON(w, http.StatusOK, feeResolutionToAPI(sel))
}

func feeResolutionToAPI(sel store.FeeSelection) FeeResolution {
	out := FeeResolution{
		ResolverVersion: sel.ResolverVersion,
		EvaluatedAt:     sel.EvaluatedAt,
		OrganizerId:     sel.OrganizerID,
		PerformanceId:   sel.PerformanceID,
		Currency:        sel.Currency,
		ChannelCode:     sel.Channel,
		// Never nil: the contract requires the array, and "no fees apply" must
		// serialize as [] rather than null.
		Fees: []FeeCodeResolution{},
	}
	for _, f := range sel.Fees {
		code := FeeCodeResolution{
			FeeCode: f.FeeCode,
			// Same reason as above, one level down.
			Candidates: []LosingFeeRule{},
		}
		if f.Winner != nil {
			winner := feeRuleToAPI(*f.Winner)
			code.Winner = &winner
		}
		for _, c := range f.Candidates {
			code.Candidates = append(code.Candidates, LosingFeeRule{
				Rule:   feeRuleToAPI(c.Rule),
				Reason: LosingFeeRuleReason(c.Reason),
			})
		}
		out.Fees = append(out.Fees, code)
	}
	return out
}

// feeRuleToAPI maps one rule into its provenance shape. Every nullable field is
// carried as a pointer rather than flattened to a zero value: TKT-215 persists
// this document as a snapshot, and `amount: 0` on a percentage rule would be a
// lie that survives into settlement.
func feeRuleToAPI(r store.FeeRule) FeeRuleProvenance {
	return FeeRuleProvenance{
		RuleId:         r.ID,
		ScopeLevel:     FeeRuleProvenanceScopeLevel(r.ScopeLevel),
		ScopeId:        r.ScopeID,
		FeeCode:        r.FeeCode,
		Basis:          FeeRuleProvenanceBasis(r.Basis),
		Amount:         r.Amount,
		RateBps:        r.RateBps,
		Currency:       r.Currency,
		Incidence:      FeeRuleProvenanceIncidence(r.Incidence),
		ChannelCode:    r.ChannelCode,
		EffectiveFrom:  r.EffectiveFrom,
		EffectiveUntil: r.EffectiveUntil,
		Priority:       r.Priority,
		Forced:         r.ForceAncestorOverride,
	}
}
