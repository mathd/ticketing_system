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
	s.cacheSwitchMu.Lock()
	defer s.cacheSwitchMu.Unlock()
	stAvail := s.avail.Status()
	stOcc := s.occupancy.Status()
	write(w, http.StatusOK, map[string]any{
		"enabled": stAvail.Enabled && stOcc.Enabled,
		"entries": stAvail.Entries + stOcc.Entries,
	})
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
	// One lock spanning BOTH writes and the status snapshot that follows them.
	// Without it two overlapping requests interleave — a disable sets
	// availability, an enable then sets both, and the disable finally sets
	// occupancy — leaving availability serving from memory while this route,
	// which ANDs the two flags, answers enabled:false. An operator reading "the
	// cache is off" mid-incident while a cache still answers from memory is the
	// precise failure this surface exists to prevent, so the switch has to move
	// both caches as one step, not two.
	s.cacheSwitchMu.Lock()
	defer s.cacheSwitchMu.Unlock()
	s.avail.SetEnabled(*in.Enabled)
	s.occupancy.SetEnabled(*in.Enabled)
	stAvail := s.avail.Status()
	stOcc := s.occupancy.Status()
	write(w, http.StatusOK, map[string]any{
		"enabled": stAvail.Enabled && stOcc.Enabled,
		"entries": stAvail.Entries + stOcc.Entries,
	})
}
