package api

// Bulk performance display names (TKT-222 / US-A3).
//
// Commerce's wallet read holds slot ids and needs the name a buyer would
// recognise. One call resolves a whole page, rather than one per order — a wallet
// with twenty rows must not become twenty cross-service reads (ADR-004's
// one-call-per-page rule).

import (
	"net/http"

	"github.com/google/uuid"
)

// ResolvePerformanceDisplayNames is generated-router mounted (the operation is
// declared in the contract), and its /internal/ prefix means guardInternalSurface
// authenticates it before this runs (ADR-043).
func (s *Server) ResolvePerformanceDisplayNames(w http.ResponseWriter, r *http.Request, params ResolvePerformanceDisplayNamesParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: "unsupported locale"})
		return
	}
	// The contract bounds this at 1..20 and the validator enforces it; the check
	// here covers a caller reaching the handler directly.
	if len(params.Ids) == 0 || len(params.Ids) > 20 {
		writeJSON(w, http.StatusBadRequest, Error{Error: "between 1 and 20 performance ids are required"})
		return
	}
	// openapi_types.UUID IS uuid.UUID, so this is a copy rather than a
	// conversion — kept explicit so the store port keeps its own type.
	ids := append(make([]uuid.UUID, 0, len(params.Ids)), params.Ids...)

	found, err := s.displayName.PerformanceDisplayNames(r.Context(), ids)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	// One entry per id that RESOLVED; an unknown id is simply absent. The caller
	// holds a set, and one unknown member must not fail the other nineteen — a
	// wallet with one unnameable row beats a wallet that will not load.
	out := PerformanceDisplayNames{Performances: make([]PerformanceDisplayName, 0, len(found))}
	for _, p := range found {
		out.Performances = append(out.Performances, PerformanceDisplayName{
			PerformanceId: p.PerformanceID,
			EventName:     resolve(p.EventName, params.Locale),
			StartsAt:      p.StartsAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
