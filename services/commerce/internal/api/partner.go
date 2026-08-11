package api

// The partner (reseller) surface (TKT-240 / ADR-056).
//
// Three operations: read availability for your own channel, hold GA stock against
// your own channel's allocation, confirm that hold into a sale. The credential
// decides which organizer and which channel "your own" means, and these handlers
// never take either from the request body.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"

)

// partnerRefusal writes a refusal carrying a machine-readable code. The codes are
// a closed enum in the contract, so a new one is a contract change and not a
// string a handler can invent.
func partnerRefusal(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]string{"error": message, "code": code})
}

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

// partnerOrganizerMatches compares a request-supplied organizer against the
// credential's.
//
// The request's value is NEVER the authority — the credential's is. This exists
// so that a partner integration naming the wrong organizer is TOLD (403) instead
// of being silently served its own data, which would leave a real integration bug
// undiagnosed for as long as the two happened to agree.
//
// The comparison direction matters: this must not become "use whichever the
// request supplied". ADR-053 records what that costs — catalog's staff credential
// can enumerate and mutate across tenants precisely because the organizer arrives
// in the request and nothing compares it to anything.
func partnerOrganizerMatches(w http.ResponseWriter, scope partnerScope, requested uuid.UUID) bool {
	if requested != scope.OrganizerID {
		partnerRefusal(w, http.StatusForbidden, "partner_scope_mismatch",
			"the credential is not issued for the organizer named in this request")
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

// partnerReserve holds GA stock against the credential's channel allocation.
//
// It reuses the ordinary reserve path deliberately: pricing, fee composition,
// idempotency and the inventory hold are the same operations a customer sale
// performs, and a second implementation of them would be a second place for the
// no-oversell guarantee to be wrong. What differs is only WHERE the channel comes
// from — the credential, not the body.
func (s *Server) partnerReserve(w http.ResponseWriter, r *http.Request) {
	scope, ok := requirePartnerScope(w, r)
	if !ok {
		return
	}
	if !s.limitPartner(w, scope) {
		return
	}
	var in PartnerReservationCreate
	if !decode(w, r, &in) {
		return
	}
	if !partnerOrganizerMatches(w, scope, in.OrganizerId) {
		return
	}
	// A seated pool is refused with `seated_pool_unsupported` citing TKT-176. That
	// translation lives in the reserve path itself (server.go, at the inventory
	// refusal), not here: the slot is only resolved from the ticket type INSIDE
	// reserve, and inventory is the thing that actually knows the pool kind --
	// it refuses under the pool row lock. A pre-check here would be a second,
	// weaker copy of a guard that already exists at the right tier.
	// Delegate to the ordinary reserve by REWRITING the body with the credential's
	// channel, rather than re-implementing the path or threading a parameter
	// through it. Two reasons, both about keeping one implementation of the
	// no-oversell guarantee: `reserve` decodes, prices, composes fees, derives the
	// idempotent reservation id and calls inventory, and a partner copy of that
	// would be a second place for any of it to be wrong; and the channel arrives
	// here the same way a customer's does, so the seam closure committed earlier
	// carries it to inventory with no partner-specific branch.
	//
	// The rewrite is the SCOPE being applied, not the request being trusted: the
	// body is rebuilt from fields already validated against the contract plus the
	// channel from the credential, and a `channel_code` the caller sent could not
	// have survived, because PartnerReservationCreate is additionalProperties:false
	// and declares no such field.
	body, err := json.Marshal(map[string]any{
		"organizer_id":   in.OrganizerId,
		"ticket_type_id": in.TicketTypeId,
		"quantity":       in.Quantity,
		"channel_code":   scope.ChannelCode,
	})
	if err != nil {
		write(w, http.StatusInternalServerError, map[string]string{"error": "could not build reservation"})
		return
	}
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	s.reserve(w, inner)
}
