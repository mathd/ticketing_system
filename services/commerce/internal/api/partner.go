package api

// The partner (reseller) surface (TKT-240 / ADR-056).
//
// ONE operation in this slice: read availability for your own channel. The
// credential decides which organizer and which channel "your own" means, and the
// handler never takes either from the request body.
//
// TKT-246 added the WRITE, together with the enforcement its absence was waiting
// for. The note that stood here said a partner write could not ship until inventory
// enforced the channel allocation, because a hold that silently consumed PUBLIC
// stock would make the contract, the ADR and this comment all liars. Inventory now
// judges it: an allocation may bind to a seller (sold_by) and the guard runs under
// the pool row lock, before capacity, so a partner hold either consumes its own
// channel's allocation or is refused.
//
// Both partner operations take organizer and channel from the credential and from
// nowhere else. partnerReserve additionally compares the body's organizer_id against
// the scope rather than trusting it -- see reserveWithScope.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

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

// partnerReserve holds stock against the credential's own channel allocation
// (TKT-246).
//
// A thin wrapper: the reserve path is shared with the public route so that pricing,
// fees, idempotency and seat handling have ONE implementation. What the scope adds is
// the authorization -- the channel and reseller inventory decides on, taken from the
// credential and never from the body.
func (s *Server) partnerReserve(w http.ResponseWriter, r *http.Request) {
	scope, ok := requirePartnerScope(w, r)
	if !ok {
		return
	}
	if !s.limitPartner(w, scope) {
		return
	}
	s.reserveWithScope(w, r, &scope)
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
	// An answer commerce cannot read is a BROKEN UPSTREAM, never a sellout (TKT-305).
	//
	// This decode used to discard its error and fall through a nil pointer to
	// `available = 0`, which is the one wrong answer this endpoint can give: a
	// reseller polls it to decide whether to keep selling, reads 0, and backs off.
	// An inventory outage then looks exactly like a sold-out show, and the partner
	// stops selling seats that exist. 502 says "ask again"; `available: 0` says
	// "stop", and only one of those is true when the upstream is broken. Same
	// 502-on-undecodable idiom as server.go's "invalid inventory response".
	//
	// BOTH CHECKS ARE LOAD-BEARING, and the reason is worth writing down because the
	// first attempt at this fix dropped the decode error on an argument that is FALSE.
	//
	// The tempting claim is that encoding/json never leaves a pointer field populated
	// on a body it rejects, making the error check unreachable behind the nil guard.
	// That holds for syntax errors and nothing else. A TYPE error populates the field
	// first and reports afterwards: `{"available":"bad"}` errors and leaves
	// `Available = &0` — a 200 with `available: 0`, exactly the defect this fix exists
	// to remove — and `{"available":7,"available":"bad"}` errors with `&7`, which is
	// worse still, since it invents a number the upstream never asserted.
	//
	// So: refuse an unreadable body, THEN refuse a readable one that omits the field.
	// Neither subsumes the other. (ai-review [high]; the first version was mutation-
	// checked against syntax errors only, which is a harness that could not catch what
	// it was hunting.)
	var upstream struct {
		Available *int `json:"available"`
		// Decoded so the answer can be checked against the QUESTION (ai-review pass 2).
		// `slot_id` is required on inventory's Availability, and inventory reads the
		// slot from a path parameter through a CACHE (availability.Read) — a cache keyed
		// or invalidated wrongly is the realistic way another slot's figure arrives here
		// looking perfectly well-formed. Republishing it under the requested slot's id
		// would hand a reseller a number inventory never asserted about their slot, and
		// no other guard in this handler could tell.
		SlotID *string `json:"slot_id"`
	}
	if json.Unmarshal(body, &upstream) != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "invalid inventory response"})
		return
	}
	// A DECODABLE body that omits the field is the same failure in a valid envelope.
	// `available` is `required` on inventory's Availability schema, so its absence is a
	// contract violation and not a slot with nothing left — and a *int left nil is
	// indistinguishable from an explicit zero once it has been defaulted.
	//
	// The `remaining` fallback that stood here is gone: /slots/{id}/availability has no
	// such field, so it could only ever have masked a malformed answer.
	if upstream.Available == nil {
		write(w, http.StatusBadGateway, map[string]string{"error": "invalid inventory response"})
		return
	}
	// A NEGATIVE is a broken answer, not an empty slot, so it is refused rather than
	// clamped (ai-review pass 2). This used to read `available = 0` on the argument
	// that "less than nothing available" and "nothing available" are the same fact to
	// a seller. They are not the same fact about INVENTORY: a negative count means the
	// upstream's arithmetic is wrong, and turning it into a sellout is the very
	// substitution this ticket exists to stop — the reseller stops selling, and the
	// one signal that something is broken has been rounded away.
	//
	// The clamp existed because commerce's OWN PartnerAvailability schema declares
	// `minimum: 0`, so a negative would fail ADR-028's fail-closed response validation
	// and surface as a 500. That reason survives — 502 is simply the honest status for
	// it, and it is the one every other unusable-upstream branch here already uses.
	// (Inventory's Availability declares no minimum, so nothing upstream prevents one.)
	// The answer must be about the slot that was asked about.
	if upstream.SlotID == nil || !strings.EqualFold(*upstream.SlotID, slot.String()) {
		write(w, http.StatusBadGateway, map[string]string{"error": "invalid inventory response"})
		return
	}
	available := *upstream.Available
	if available < 0 {
		write(w, http.StatusBadGateway, map[string]string{"error": "invalid inventory response"})
		return
	}
	write(w, http.StatusOK, PartnerAvailability{
		SlotId:      slot,
		ChannelCode: scope.ChannelCode,
		Available:   available,
	})
}
