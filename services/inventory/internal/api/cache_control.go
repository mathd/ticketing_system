package api

import (
	"net/http"

	"ticketing/shared/httpx"
)

// The ADR-004 incident kill-switch (TKT-210).
//
// ADR-004's Consequences named this the moment in-memory hot-event structures
// existed: "needs staleness tests and a kill-switch to bypass caches during
// incidents". TKT-205 built the cache; this is the switch.
//
// Both handlers go through the SAME collaborator the public read uses
// (`s.avail`), deliberately. A separate flag or controller object could drift
// from the read path, and the failure would be the quiet one: the switch reports
// disabled, an operator believes the cache is off, and reads keep being served
// from memory.
//
// Guarded by internalOnly — the shared INTERNAL_SERVICE_TOKEN, no new
// credential. Its holder can already place operational holds, adjust capacity
// and return refunded capacity on this service; disabling a cache is strictly
// less powerful than what they hold. A narrower credential would cost
// distribution, rotation and startup validation while reducing nobody's
// privilege. 401 on refusal, matching every other internal route here; the
// gateway keeps answering 404 at the edge for /api/inventory/internal/*.

func (s *Server) cacheControlStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.avail.Status()
	write(w, http.StatusOK, map[string]any{"enabled": st.Enabled, "entries": st.Entries})
}

func (s *Server) cacheControlSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	// A pointer so a missing field is distinguishable from an explicit false —
	// silently reading an absent `enabled` as "disable" is the wrong default for
	// an operator surface.
	if err := httpx.DecodeJSON(w, r, &in, 1<<16); err != nil || in.Enabled == nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "enabled (boolean) required"})
		return
	}
	s.avail.SetEnabled(*in.Enabled)
	st := s.avail.Status()
	write(w, http.StatusOK, map[string]any{"enabled": st.Enabled, "entries": st.Entries})
}
