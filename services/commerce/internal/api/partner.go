package api

// The partner (reseller) surface (TKT-240 / ADR-056).
//
// ONE operation in this slice: read availability for your own channel. The
// credential decides which organizer and which channel "your own" means, and the
// handler never takes either from the request body.
//
// There is deliberately no partner WRITE here. A hold has to consume the
// credential's channel allocation to mean anything, and inventory does not yet
// enforce that -- the channel stops at catalog's fee resolution and never reaches
// the claim path. A write shipped now would be a hold that silently consumes
// PUBLIC stock while the contract, the ADR and this comment all claimed the
// partner was confined to its channel. It was written, reviewed, and removed for
// exactly that reason; TKT-246 adds it back together with the enforcement.

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// requirePartnerScope resolves the authenticated scope, refusing when there is
// none.
//
// Reaching a partner handler without a scope should be impossible — the contract
// declares the security and the validator enforces it before routing — so this is
// defence against a future wiring change that registers a partner route without
// the declaration, not against normal operation. It fails closed because the
// alternative is a handler running with organizer uuid.Nil and channel "", which
// compares equal to nothing and would quietly serve no one while looking healthy.
func requirePartnerScope(w http.ResponseWriter, r *http.Request) (partnerScope, bool) {
	scope, ok := partnerScopeFrom(r.Context())
	if !ok {
		write(w, http.StatusUnauthorized, map[string]string{"error": "partner credential is not recognised"})
		return partnerScope{}, false
	}
	return scope, true
}

// limitPartner spends one unit of the reseller's budget (ADR-051, ADR-055).
//
// Keyed on the RESELLER, not the credential and not the address: a partner that
// rotates its credential after a leak is the same partner and should not get a
// fresh budget by re-enrolling, and a partner is a server whose address says
// nothing useful about how much it should be allowed to send.
//
// It is called from each handler rather than installed as route middleware
// because the key does not exist until the validator has authenticated -- and
// middleware on the chi router runs INSIDE the validator, but the scope slot is
// only filled during authentication, so a middleware ordering mistake here would
// silently key every partner on uuid.Nil and give them one shared budget.
func (s *Server) limitPartner(w http.ResponseWriter, scope partnerScope) bool {
	if !s.lim().partner.Allow(scope.ResellerID.String()) {
		writeTooManyRequests(w)
		return false
	}
	return true
}

// partnerAvailability answers what the credential's own channel has left.
//
// The organizer and channel are the credential's, so this operation takes no
// parameters that could name anybody else's. Inventory's availability read already
// accepts a channel and answers per-allocation.
func (s *Server) partnerAvailability(w http.ResponseWriter, r *http.Request) {
	scope, ok := requirePartnerScope(w, r)
	if !ok {
		return
	}
	if !s.limitPartner(w, scope) {
		return
	}
	slot, err := uuid.Parse(r.URL.Query().Get("slot_id"))
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid slot id"})
		return
	}
	code, body, err := s.call(r.Context(), http.MethodGet,
		s.inventoryURL+"/slots/"+slot.String()+"/availability?organizer_id="+scope.OrganizerID.String()+
			"&channel="+url.QueryEscape(scope.ChannelCode), "", nil, false)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "availability unavailable"})
		return
	}
	if code == http.StatusNotFound {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if code != http.StatusOK {
		write(w, http.StatusBadGateway, map[string]string{"error": "availability unavailable"})
		return
	}
	var upstream struct {
		Available *int `json:"available"`
		Remaining *int `json:"remaining"`
	}
	_ = json.Unmarshal(body, &upstream)
	available := 0
	switch {
	case upstream.Available != nil:
		available = *upstream.Available
	case upstream.Remaining != nil:
		available = *upstream.Remaining
	}
	if available < 0 {
		// The contract declares a minimum of 0 and ADR-028's response validation is
		// fail-closed, so a negative would become a 500. Clamping is honest here:
		// "less than nothing is available" and "nothing is available" are the same
		// fact to a seller.
		available = 0
	}
	write(w, http.StatusOK, PartnerAvailability{
		SlotId:      slot,
		ChannelCode: scope.ChannelCode,
		Available:   available,
	})
}
