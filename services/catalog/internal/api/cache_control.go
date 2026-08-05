package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// The ADR-004 incident kill-switch for catalog's public-read cache (TKT-210).
//
// Both handlers go through the SAME collaborator the four public reads use
// (`s.public`), deliberately. A separate flag could drift from the read path,
// and the failure would be the quiet one: the switch reports disabled, an
// operator believes the cache is off, and reads keep coming from memory.
//
// HAND-MOUNTED AND UNDECLARED, matching every other internal route in this
// service — `getTicketType`, `getPublishedPerformance`, `getPoolOfferState`,
// `pinSeats`. Catalog's internal surface is deliberately not part of its public
// OpenAPI contract (see Router's comment: "service-to-service, not part of the
// public OpenAPI contract; the response validator skips undeclared paths").
// Declaring this one route would make it the only declared internal route in
// catalog — a new inconsistency, not a fix. Inventory declares its internal
// routes because that is ITS convention, and this ticket follows each service's
// own. The behaviour is identical either way; what differs is which convention
// each service keeps.
//
// Guarded by the shared INTERNAL_SERVICE_TOKEN with a 401 refusal, matching
// catalog's other internal routes. No new credential: its holder can already
// mutate seat pins here, so disabling a cache is strictly less powerful.

func (s *Server) cacheControlStatus(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthorized(w, r) {
		return
	}
	s.writeCacheState(w)
}

func (s *Server) cacheControlSet(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthorized(w, r) {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	// A pointer so a missing field is distinguishable from an explicit false.
	// Inferring "disable" from an absent field is how a malformed script takes a
	// cache down.
	// Exactly one JSON value, no trailing anything. A plain Decode reads the
	// first value and ignores the rest, so `{"enabled":false}{"enabled":true}`
	// would be accepted and silently take the cache down on an ambiguous body.
	// This route is hand-mounted and therefore outside request validation, so the
	// check has to be here — inventory gets it from httpx.DecodeJSON.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "enabled (boolean) required"})
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, Error{Error: "body must contain exactly one JSON object"})
		return
	}
	s.public.SetEnabled(*in.Enabled)
	s.writeCacheState(w)
}

func (s *Server) internalAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if s.internalCredential == "" || r.Header.Get("X-Internal-Token") != s.internalCredential {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return false
	}
	return true
}

func (s *Server) writeCacheState(w http.ResponseWriter) {
	st := s.public.Status()
	// no-store: a cached answer about whether a cache is on is exactly the wrong
	// thing to hand an operator mid-incident.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"enabled": st.Enabled, "entries": st.Entries})
}
