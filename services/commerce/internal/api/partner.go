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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ResellerCommissionFeeCode is the fee code for reseller commission.
const ResellerCommissionFeeCode = "reseller_commission"

// partnerIdempotencyKey derives an isolated idempotency key for partner confirms.
// It scopes the key by organizer and reseller so different partners using the same
// raw key do not collide. Rotated credentials for the same partner replay correctly
// because credential ID is not included.
func partnerIdempotencyKey(organizerID, resellerID uuid.UUID, rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return fmt.Sprintf("partner:%s:%s:%x", organizerID, resellerID, sum)
}

// The two ways a reseller commission can be unusable, and they are NOT the same
// question. ADR-024 makes the channel registry a lookup, so a channel nobody has
// configured a commission for must still sell -- that is errCommissionAbsent, and
// the sale completes with the money recorded as collected-and-unattributed.
//
// A commission that EXISTS and names the wrong party is the opposite case. The
// snapshot travels verbatim into settlementPlanFromSnapshot, which forwards whatever
// payee it names, and payments accepts any set that balances: a sum cannot see who
// it credits. So letting this through settles a real obligation to a partner who did
// not make the sale, and the ledger is append-only. "Absent is tolerated" does not
// extend to "wrong is tolerated", and conflating the two was this handler's first
// version of the bug in the other direction (ai-review pass 1, [high]).
var (
	errCommissionAbsent    = errors.New("no reseller commission is configured")
	errCommissionMisplaced = errors.New("the configured reseller commission names another party")
)

// validatePartnerCommission validates that the persisted fee resolution snapshot
// carries an explicit, valid reseller commission split for the authenticated reseller.
//
// Returns errCommissionAbsent when nothing is configured and errCommissionMisplaced
// when something is configured and does not belong to this reseller. Callers must
// treat the two differently; see the sentinels above.
func validatePartnerCommission(snapshot []byte, channelCode string, resellerID uuid.UUID) error {
	if len(snapshot) == 0 {
		return fmt.Errorf("%w: missing fee resolution snapshot", errCommissionAbsent)
	}

	var env struct {
		Breakdown []struct {
			FeeCode   string `json:"fee_code"`
			Incidence string `json:"incidence"`
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
		} `json:"breakdown"`
		Resolution struct {
			Fees []struct {
				FeeCode string `json:"fee_code"`
				Split   struct {
					Mode   string `json:"mode"`
					Winner *struct {
						ChannelCode *string `json:"channel_code"`
						Parts       []struct {
							Payee struct {
								PayeeID           string  `json:"payee_id"`
								Kind              string  `json:"kind"`
								DisplayName       string  `json:"display_name"`
								ExternalReference *string `json:"external_reference"`
							} `json:"payee"`
							ShareBps int32 `json:"share_bps"`
						} `json:"parts"`
					} `json:"winner"`
				} `json:"split"`
			} `json:"fees"`
		} `json:"resolution"`
	}

	if err := json.Unmarshal(snapshot, &env); err != nil {
		return fmt.Errorf("%w: unreadable fee snapshot: %v", errCommissionMisplaced, err)
	}

	hasBreakdown := false
	for _, b := range env.Breakdown {
		if b.FeeCode == ResellerCommissionFeeCode {
			hasBreakdown = true
			break
		}
	}
	if !hasBreakdown {
		return fmt.Errorf("%w: %s is not in the fee breakdown", errCommissionAbsent, ResellerCommissionFeeCode)
	}

	var matchedFee *struct {
		FeeCode string `json:"fee_code"`
		Split   struct {
			Mode   string `json:"mode"`
			Winner *struct {
				ChannelCode *string `json:"channel_code"`
				Parts       []struct {
					Payee struct {
						PayeeID           string  `json:"payee_id"`
						Kind              string  `json:"kind"`
						DisplayName       string  `json:"display_name"`
						ExternalReference *string `json:"external_reference"`
					} `json:"payee"`
					ShareBps int32 `json:"share_bps"`
				} `json:"parts"`
			} `json:"winner"`
		} `json:"split"`
	}

	for i := range env.Resolution.Fees {
		if env.Resolution.Fees[i].FeeCode == ResellerCommissionFeeCode {
			matchedFee = &env.Resolution.Fees[i]
			break
		}
	}
	if matchedFee == nil {
		return fmt.Errorf("%w: %s is not in the fee resolution", errCommissionAbsent, ResellerCommissionFeeCode)
	}

	if matchedFee.Split.Mode != "split" || matchedFee.Split.Winner == nil {
		return fmt.Errorf("%w: the commission fee resolved unsplit", errCommissionAbsent)
	}

	winner := matchedFee.Split.Winner
	if winner.ChannelCode == nil || *winner.ChannelCode != channelCode {
		return fmt.Errorf("%w: the winning split is scoped to channel %v, not %q", errCommissionMisplaced, winner.ChannelCode, channelCode)
	}

	expectedRef := fmt.Sprintf("reseller:%s", resellerID)
	var matches int
	for _, part := range winner.Parts {
		if part.Payee.ExternalReference != nil && *part.Payee.ExternalReference == expectedRef {
			matches++
		}
	}

	if matches == 0 {
		return fmt.Errorf("%w: no beneficiary carries external_reference %q", errCommissionMisplaced, expectedRef)
	}
	if matches > 1 {
		return fmt.Errorf("%w: %d beneficiaries carry external_reference %q", errCommissionMisplaced, matches, expectedRef)
	}

	return nil
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

// partnerConfirm completes a sale on merchant-of-record terms for an authenticated partner.
func (s *Server) partnerConfirm(w http.ResponseWriter, r *http.Request) {
	scope, ok := requirePartnerScope(w, r)
	if !ok {
		return
	}
	if !s.limitPartner(w, scope) {
		return
	}
	s.confirmWithScope(w, r, scope)
}

func (s *Server) confirmWithScope(w http.ResponseWriter, r *http.Request, scope partnerScope) {
	// The same bound every other idempotent write applies (checkout, reserve, the
	// exchange). A partner-only spelling of this guard would drift from its three
	// siblings, and the header is attacker-controlled on a write path.
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	// decode(), not a bare json.Decoder, and the difference is load-bearing twice
	// over. It bounds the body at 1MB, and it sets DisallowUnknownFields -- which is
	// the GO-tier half of "a partner cannot name its own reseller or channel". The
	// contract refuses those fields too, and this must not depend on the contract:
	// reserveWithScope makes the same argument for overwriting rather than trusting
	// the body's channel, as the second line of defence for the day the contract is
	// edited.
	var in checkoutRequest
	if !decode(w, r, &in) {
		return
	}
	if in.ReservationID == uuid.Nil || strings.TrimSpace(in.Name) == "" ||
		!strings.Contains(in.Email, "@") || strings.TrimSpace(in.PaymentToken) == "" {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid checkout"})
		return
	}
	x, err := s.loadPartnerReservation(r.Context(), in.ReservationID, scope.OrganizerID, scope.ResellerID, scope.ChannelCode)
	if err != nil {
		code, message := persistenceReadProblem(err)
		if code != http.StatusNotFound {
			slog.Default().ErrorContext(r.Context(), "load partner reservation", "err", err)
			write(w, code, map[string]string{"error": message})
			return
		}
		write(w, code, map[string]string{"error": "reservation " + message})
		return
	}
	if x.IsSeated {
		write(w, http.StatusConflict, map[string]string{
			"error": "this slot is seated and cannot be sold by quantity. A seated claim carries " +
				"no channel and does not consume a channel allocation -- TKT-176 owns that seam.",
			"code": string(SeatedPoolUnsupported),
		})
		return
	}
	// A commission the snapshot cannot support is OBSERVED AND LOGGED, never refused.
	//
	// The first version of this handler refused it, and the gate said what that meant:
	// two TKT-241 tests went red, both of which exist to prove that catalog's channel
	// registry is a LOOKUP AND NOT A CONSTRAINT -- an unregistered channel sells
	// exactly as a registered one does (ADR-024). A partner sale that cannot complete
	// because nobody authored a split schedule for its channel turns that lookup back
	// into a constraint, one layer up, and does it after the buyer has paid.
	//
	// It is also the trade this epic has now declined three times. BuildSettlementEntries
	// says it in its own comment: a fee with no split is unattributed, not invalid,
	// because refusing would fail sales at CHECKOUT after the buyer committed, and "a
	// payout misconfiguration must not refuse a purchase". The settlement path already
	// records the money as collected-and-unattributed and leaves the gap QUERYABLE,
	// which is what an operator needs. Refusing here would have been a worse answer to
	// a problem the ledger already answers well.
	//
	// So the ticket's requirement is read exactly as written: a CONFIGURED commission
	// must settle to the reseller payee and be derived from the schedule's share_bps.
	// Nothing in the COS asks for a sale to be refused when it is absent.
	//
	// The reason is logged and never returned. It names schedules, channels and payees,
	// which is the payout matrix -- the same thing SelectSplitSchedule drops ineligible
	// schedules to avoid publishing (splits.go:120-127).
	if err := validatePartnerCommission(x.FeeSnapshot, x.ChannelCode, scope.ResellerID); err != nil {
		if errors.Is(err, errCommissionMisplaced) {
			// Refuse BEFORE the charge, which is the only place refusing is free.
			// The snapshot names a beneficiary that is not this reseller, and it
			// travels verbatim into settlementPlanFromSnapshot -- so completing
			// here writes an append-only obligation to the wrong partner, and the
			// balance trigger cannot see it because a sum cannot see who it
			// credits. Refusing an unsold hold costs the partner a retry; settling
			// to the wrong payee costs somebody real money and cannot be reversed
			// by this system (ADR-048 leaves reversal undecided).
			slog.Default().ErrorContext(r.Context(), "partner commission names another party",
				"reseller_id", scope.ResellerID, "channel", x.ChannelCode, "reason", err)
			write(w, http.StatusConflict, map[string]string{
				"error": "the commission configured for this sale does not belong to your channel",
			})
			return
		}
		// errCommissionAbsent: nothing is configured, and that must not stop a sale.
		// The ledger records the fee as collected-and-unattributed and the gap stays
		// queryable, which is what an operator can act on.
		slog.Default().WarnContext(r.Context(), "partner sale has no reseller commission configured",
			"reseller_id", scope.ResellerID, "channel", x.ChannelCode, "reason", err)
	}
	// The buyer attribution, resolved exactly as public checkout resolves it. A
	// partner sale can still carry a customer assertion: the partner is the SELLER,
	// and the buyer is whoever the assertion names.
	//
	// Passing uuid.NullUUID{} unconditionally instead would silently downgrade a
	// forged or expired assertion to a guest order -- the exact failure checkout
	// answers 401 for, so that a buyer is never told a purchase succeeded under an
	// attribution that did not. The operation declares 401 and this is what returns it.
	customer, err := customerFromRequest(s.assertionKey, r.Header.Get(assertionHeader), time.Now())
	if err != nil {
		write(w, http.StatusUnauthorized, map[string]string{"error": "invalid customer assertion"})
		return
	}
	internalKey := partnerIdempotencyKey(scope.OrganizerID, scope.ResellerID, key)
	s.executeCheckout(w, r, x.reservation, internalKey, in, customer)
}
