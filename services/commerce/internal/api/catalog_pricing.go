package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

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
// maxContractAmount is the OpenAPI Money bound: every consumer, the storefront
// included, must represent it exactly.
const maxContractAmount int64 = 9007199254740991

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
	ScopeID    uuid.UUID `json:"scope_id"`
	ActionKind string    `json:"action_kind"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
}

type priceResolution struct {
	ResolverVersion int32         `json:"resolver_version"`
	EvaluatedAt     time.Time     `json:"evaluated_at"`
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
	// The snapshot has to be STORABLE, and that has to be known BEFORE a hold
	// exists -- otherwise the insert fails after inventory has committed and the
	// buyer gets a 500 with an orphan hold.
	canonical, err := storableSnapshot(body)
	if err != nil {
		return priceResolution{}, err
	}
	p.raw = canonical
	if err := p.validate(organizerID, quantity); err != nil {
		return priceResolution{}, err
	}
	return p, nil
}

// validate enforces every invariant the sale depends on. Each check exists
// because violating it would let the wrong number reach a buyer.
func (p priceResolution) validate(organizerID uuid.UUID, quantity int32) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", errResolveUnusable, why) }

	// Any version >= 1 is accepted, and the reason is worth writing down because
	// the opposite was tried first and was worse.
	//
	// Capping this at the newest version commerce knew looked like ADR-017's
	// "judge the version before trusting the payload". It is not the same
	// situation. ADR-017 protects a consumer that DECODES a payload whose
	// meaning changed. Commerce decodes almost nothing here: it consumes
	// `resolved_price`, whose contract is "the unit price for this ticket type",
	// and that contract does not change when the COMPARATOR's derivation does.
	// A cap therefore bought no price safety and cost a real outage -- deploy
	// catalog before commerce and every new reservation stops.
	//
	// If catalog ever changes what `resolved_price` MEANS, that is a breaking
	// contract change needing a new field or operation, not a version bump, and
	// the validations below are what would catch a shape that no longer fits.
	// The version is recorded in the snapshot so a stored provenance document
	// stays interpretable -- a read-side concern, not a reason to refuse a sale.
	if p.ResolverVersion < 1 {
		return bad("resolver_version below 1")
	}
	// base_price absent decodes to a zero Money and would never be looked at on
	// the winner path. Require it, and require it to be REAL money: presence
	// alone let a negative amount, a lowercase code, or a currency different
	// from the resolved one sail through on the winner path.
	if err := checkMoney("base_price", p.BasePrice); err != nil {
		return err
	}
	if p.BasePrice.Currency != p.ResolvedPrice.Currency {
		return bad("base_price and resolved_price are in different currencies")
	}
	if p.EvaluatedAt.IsZero() {
		return bad("missing evaluated_at")
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
		if p.Winner.RuleID == uuid.Nil || p.Winner.ScopeLevel == "" || p.Winner.ScopeID == uuid.Nil {
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

// checkMoney enforces the contract's Money bounds on any amount that reaches
// this sale: non-negative, within the range every consumer can represent
// exactly, and an ISO-4217 code in the shape the contract declares.
func checkMoney(field string, m resolvedMoney) error {
	switch {
	case m.Amount < 0:
		return fmt.Errorf("%w: negative %s", errResolveUnusable, field)
	case m.Amount > maxContractAmount:
		return fmt.Errorf("%w: %s exceeds the contract's Money range", errResolveUnusable, field)
	case !isISO4217(m.Currency):
		return fmt.Errorf("%w: %s carries a malformed currency", errResolveUnusable, field)
	}
	return nil
}

func isISO4217(c string) bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// storableSnapshot returns the document in a form PostgreSQL jsonb will accept,
// or refuses it.
//
// Scanning the raw bytes for one spelling of the NUL escape was not a
// storability test -- three inputs got past it and would have failed the INSERT
// *after* the hold existed, leaving an orphan hold and a 500 for the buyer:
// an unpaired surrogate (Go decodes it, PostgreSQL refuses it), a number outside
// PostgreSQL's numeric range, and any of these nested inside a field this build
// does not know. It also false-positived on a legitimate string whose TEXT
// happens to be the six characters of the escape.
//
// Decoding and re-encoding settles all of it: the decoder rejects out-of-range
// numbers and replaces invalid surrogates, and the walk below looks for a real
// NUL rune rather than a spelling of one. The re-encoded bytes are what gets
// stored -- no loss, since jsonb canonicalises anyway, and unknown fields
// survive because the document is decoded as a generic map.
func storableSnapshot(body []byte) ([]byte, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	// UseNumber keeps every number as its ORIGINAL TEXT. Decoding into the
	// default float64 was itself a defect: it silently rounded any integer above
	// 2^53 in a field this build does not know -- corrupting the very provenance
	// document this ticket exists to keep true -- and it REJECTED 1e400, which
	// PostgreSQL stores happily (numeric allows ~131k digits). Money is capped at
	// 2^53-1 by contract so the money fields were never at risk, but "the
	// snapshot is what catalog said" has to hold for the whole document.
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: body is not storable as jsonb: %v", errResolveUnusable, err)
	}
	if err := checkStorable(doc); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: body cannot be re-encoded: %v", errResolveUnusable, err)
	}
	return canonical, nil
}

// pgNumericMaxDigits is PostgreSQL's limit on digits before the decimal point in
// a numeric value. A number past it is valid JSON and unstorable, so it has to
// be caught here rather than by the INSERT -- after the hold exists.
const pgNumericMaxDigits = 131072

// checkStorable walks decoded JSON for the two things PostgreSQL jsonb refuses
// but Go accepts: a NUL rune in any key or string, and a number too large for
// numeric.
func checkStorable(v any) error {
	switch t := v.(type) {
	case string:
		if strings.ContainsRune(t, 0) {
			return fmt.Errorf("%w: body carries a NUL, which jsonb refuses", errResolveUnusable)
		}
	case json.Number:
		if !numericFits(t.String()) {
			return fmt.Errorf("%w: body carries a number jsonb cannot store", errResolveUnusable)
		}
	case map[string]any:
		for k, vv := range t {
			if strings.ContainsRune(k, 0) {
				return fmt.Errorf("%w: body carries a NUL in a key", errResolveUnusable)
			}
			if err := checkStorable(vv); err != nil {
				return err
			}
		}
	case []any:
		for _, vv := range t {
			if err := checkStorable(vv); err != nil {
				return err
			}
		}
	}
	return nil
}

// numericFits reports whether a JSON number's magnitude is within PostgreSQL's
// numeric range, judged from its TEXT so no precision is lost deciding.
func numericFits(n string) bool {
	mantissa, exp := n, 0
	if i := strings.IndexAny(n, "eE"); i >= 0 {
		var err error
		if exp, err = strconv.Atoi(n[i+1:]); err != nil {
			return false
		}
		mantissa = n[:i]
	}
	digits := 0
	for _, r := range strings.TrimLeft(strings.TrimLeft(mantissa, "+-"), "0") {
		if r == '.' {
			break
		}
		digits++
	}
	return digits+exp <= pgNumericMaxDigits && exp >= -pgNumericMaxDigits
}

// total is the composed line amount. Commerce's job, not catalog's.
func (p priceResolution) total(quantity int32) int64 {
	return p.ResolvedPrice.Amount * int64(quantity)
}
