package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/google/uuid"
)

// Commerce's consumer of catalog's price resolution (TKT-153 / ADR-036 §6).
//
// The boundary, stated once: catalog is the SINGLE authority for the
// rule-resolved unit price. Commerce consumes `resolved_price` and never
// recomputes it from the winning rule's action — two places computing one number
// is a divergence bug waiting for its first mismatch. What commerce owns is
// sale-time COMPOSITION (price + fees + promos + taxes into an order total),
// which is untouched here.
//
// ONE read. The response carries identity, slot, money and provenance together,
// so there is nothing to reconcile against a second call. An earlier design used
// two reads plus a `base_price` coherence check between them — that check would
// have 500'd a request that was never wrong whenever the two reads straddled a
// legitimate price edit.

// The two failure classes ADR-028 distinguishes, and the AC insists on:
//
//   - errResolveUnavailable: we could not get an answer (transport, non-200).
//   - errResolveUnusable:    we got a 200 whose body we cannot trust.
//
// Neither ever degrades to the base price. "No rule matched" is NOT in this
// list: it is a successful resolution that answers with the base price, and
// conflating the two is exactly how a sale silently prices itself wrong.
var (
	errResolveUnavailable = errors.New("price resolution unavailable")
	errResolveUnusable    = errors.New("price resolution unusable")
)

type resolvedMoney struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type resolvedRule struct {
	RuleID     uuid.UUID `json:"rule_id"`
	ScopeLevel string    `json:"scope_level"`
	ActionKind string    `json:"action_kind"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
}

type priceResolution struct {
	ResolverVersion int32         `json:"resolver_version"`
	OrganizerID     uuid.UUID     `json:"organizer_id"`
	PerformanceID   uuid.UUID     `json:"performance_id"`
	BasePrice       resolvedMoney `json:"base_price"`
	ResolvedPrice   resolvedMoney `json:"resolved_price"`
	Winner          *resolvedRule `json:"winner"`
	FallbackReason  *string       `json:"fallback_reason"`

	// raw is the exact bytes catalog sent, persisted verbatim as the
	// reservation's provenance snapshot. Storing the decoded struct instead
	// would silently drop `candidates` — the losing rules and their reasons,
	// which are the whole answer to "why was I charged this?".
	raw json.RawMessage
}

// resolveTicketTypePrice performs the single catalog read and refuses anything
// it cannot fully trust.
//
// The internal credential is deliberately NOT sent: this is a declared,
// publicly routable operation, and putting a service credential on a public
// route would be strictly worse than the exposure TKT-155 records.
func (s *Server) resolveTicketTypePrice(ctx context.Context, ticketTypeID, organizerID uuid.UUID, quantity int32) (priceResolution, error) {
	code, body, err := s.call(ctx, http.MethodGet,
		s.catalogURL+"/ticket-types/"+ticketTypeID.String()+"/price-resolution", "", nil, false)
	if err != nil {
		return priceResolution{}, fmt.Errorf("%w: %v", errResolveUnavailable, err)
	}
	if code != http.StatusOK {
		return priceResolution{}, fmt.Errorf("%w: catalog returned %d", errResolveUnavailable, code)
	}

	var p priceResolution
	// Strict about SEMANTICS, tolerant of unknown FIELDS — and the distinction
	// is load-bearing. DisallowUnknownFields would make every additive change
	// to catalog's contract a commerce outage: adding organizer_id to this very
	// response, an additive change, would have broken the sale path. What must
	// not be tolerated is a field whose meaning we cannot honour, and that is
	// what validate() below checks. ADR-017's discipline, applied to a
	// synchronous read: dispatch on what you understand, refuse what you do not.
	if json.Unmarshal(body, &p) != nil {
		return priceResolution{}, fmt.Errorf("%w: body is not a PriceResolution", errResolveUnusable)
	}
	p.raw = append(json.RawMessage(nil), body...)
	if err := p.validate(organizerID, quantity); err != nil {
		return priceResolution{}, err
	}
	return p, nil
}

// validate enforces every invariant the sale depends on. Each check exists
// because violating it would let the wrong number reach a buyer.
func (p priceResolution) validate(organizerID uuid.UUID, quantity int32) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", errResolveUnusable, why) }

	if p.ResolverVersion < 1 {
		return bad("resolver_version below 1")
	}
	// Whose ticket type this is. Answering for a different tenant is not a
	// pricing error, it is a tenancy breach (ADR-002).
	if p.OrganizerID != organizerID {
		return bad("resolution is for a different organizer")
	}
	if p.PerformanceID == uuid.Nil {
		return bad("no performance id — nothing to place a hold against")
	}
	// Exactly one of winner / fallback_reason. The schema cannot express it
	// (see the PriceResolution description); commerce refuses to guess.
	hasWinner, hasFallback := p.Winner != nil, p.FallbackReason != nil
	if hasWinner == hasFallback {
		return bad("winner and fallback_reason are not mutually exclusive")
	}
	if hasFallback && *p.FallbackReason != "no_eligible_rule" {
		return bad("unknown fallback_reason " + *p.FallbackReason)
	}
	if hasFallback && p.ResolvedPrice != p.BasePrice {
		return bad("fallback did not resolve to the base price")
	}
	if hasWinner {
		if p.Winner.ActionKind != "absolute" {
			// A future action kind means catalog computes prices a way this
			// build does not understand. Refusing is the only safe reading —
			// the same discipline ADR-017 applies to event schemas.
			return bad("unsupported action_kind " + p.Winner.ActionKind)
		}
		// Catalog is the authority, so commerce checks its arithmetic rather
		// than redoing it: if the winner and the resolved price disagree, the
		// response is incoherent and neither number can be trusted.
		if p.Winner.Amount != p.ResolvedPrice.Amount || p.Winner.Currency != p.ResolvedPrice.Currency {
			return bad("resolved_price disagrees with the winning rule")
		}
		if p.Winner.RuleID == uuid.Nil || p.Winner.ScopeLevel == "" {
			return bad("winner is missing its identity")
		}
	}
	// Money invariants (ADR-001). EUR-only is commerce's own pre-existing
	// limitation, not the rule model's — catalog stores arbitrary ISO codes.
	if p.ResolvedPrice.Amount < 0 {
		return bad("negative resolved price")
	}
	if p.ResolvedPrice.Currency != "EUR" {
		return bad("commerce sells in EUR only")
	}
	// The same overflow guard the raw-price path had. Moving where the amount
	// comes from must not move the guard.
	if quantity < 1 || p.ResolvedPrice.Amount > math.MaxInt64/int64(quantity) {
		return bad("resolved price overflows the order total")
	}
	return nil
}

// total is the composed line amount. Commerce's job, not catalog's.
func (p priceResolution) total(quantity int32) int64 {
	return p.ResolvedPrice.Amount * int64(quantity)
}
