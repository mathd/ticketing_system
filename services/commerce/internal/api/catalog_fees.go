package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Commerce's consumer of catalog's fee resolution (TKT-215 / ADR-046).
//
// The boundary, restated because it is the opposite of the price one in a way
// that matters: catalog is the single authority for WHICH fee rules apply, and
// commerce is the single authority for WHAT THEY COST. Catalog never multiplies
// (ADR-046 §5) — it reports a basis and a value, and the arithmetic below is the
// only place a fee becomes money.
//
// This file sits beside catalog_pricing.go and REUSES its helpers
// (storableSnapshot, checkMoney, isISO4217, the two error classes) rather than
// restating them. It is a second consumer of the same shape, not a second copy
// of the same logic.

// errFeeTotalOverflow is separated from errResolveUnusable because the two get
// different HTTP answers: an unusable document is our data being wrong (500),
// while a total that cannot be represented is a request we refuse (400). Both
// abort before the hold.
var errFeeTotalOverflow = errors.New("composed total is out of range")

// resolvedFeeRule is one winning rule as catalog reports it. Amount and RateBps
// are pointers because the tagged union makes exactly one of them present, and a
// flattened zero would be indistinguishable from "a percentage rule of 0 bps".
type resolvedFeeRule struct {
	RuleID    uuid.UUID `json:"rule_id"`
	FeeCode   string    `json:"fee_code"`
	Basis     string    `json:"basis"`
	Amount    *int64    `json:"amount"`
	RateBps   *int32    `json:"rate_bps"`
	Currency  string    `json:"currency"`
	Incidence string    `json:"incidence"`
}

// resolvedFeeCode is one code's outcome. Winner is NIL when the code was
// considered and nothing currently applies (ADR-046 §9) — every rule for it fell
// outside its window. That is NOT a fee of zero: it contributes nothing and
// appears in no breakdown item, while a zero-AMOUNT winner does appear.
type resolvedFeeCode struct {
	FeeCode string           `json:"fee_code"`
	Winner  *resolvedFeeRule `json:"winner"`
}

type feeResolution struct {
	ResolverVersion int32             `json:"resolver_version"`
	EvaluatedAt     time.Time         `json:"evaluated_at"`
	OrganizerID     uuid.UUID         `json:"organizer_id"`
	PerformanceID   uuid.UUID         `json:"performance_id"`
	Currency        string            `json:"currency"`
	ChannelCode     *string           `json:"channel_code"`
	Fees            []resolvedFeeCode `json:"fees"`

	// raw is the exact bytes catalog sent, persisted verbatim inside the
	// snapshot envelope. Storing the decoded struct would silently drop the
	// losing candidates, which are the answer to "why was I charged this fee".
	raw json.RawMessage
}

// feeBreakdownItem is one charged fee, after arithmetic. This is the shape the
// reservation response carries and TKT-217 settles from.
type feeBreakdownItem struct {
	FeeCode   string `json:"fee_code"`
	Basis     string `json:"basis"`
	Incidence string `json:"incidence"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
}

// feeComposition is the whole arithmetic result: what the buyer additionally
// pays, what the organizer absorbs, and the itemisation of both.
type feeComposition struct {
	Items         []feeBreakdownItem `json:"items"`
	PassedOnTotal int64              `json:"passed_on_total"`
	AbsorbedTotal int64              `json:"absorbed_total"`
}

const (
	basisPerTicketFixed = "per_ticket_fixed"
	basisPerOrderFixed  = "per_order_fixed"
	basisPercentageBps  = "percentage_bps"

	incidencePassedOn = "passed_on"
	incidenceAbsorbed = "absorbed"
)

// computeFeeBreakdown turns resolved rules into money (ADR-046 §2).
//
// PER-TICKET rounding, floored, and the unit is the whole point: it is the only
// one whose answer does not depend on how a cart groups lines. ADR-046's worked
// example — 2×150¢ and 1×100¢ at 333 bps — gives 11¢ per ticket, 12¢ per line
// and 13¢ per order. A future multi-line cart must not silently reinterpret
// reservations priced under this rule.
//
// Every multiplication is checked BEFORE it happens, not tested for wraparound
// after: a wrapped int64 on a money path is indistinguishable from a legitimate
// small number.
func computeFeeBreakdown(fees []resolvedFeeCode, unitFace int64, quantity int32, currency string) (feeComposition, error) {
	out := feeComposition{Items: []feeBreakdownItem{}}
	if quantity < 1 {
		return feeComposition{}, fmt.Errorf("%w: quantity below 1", errFeeTotalOverflow)
	}
	q := int64(quantity)

	for _, f := range fees {
		// A considered code with no live rule contributes nothing and is not an
		// item. Distinct from a zero-amount winner, which IS an item.
		if f.Winner == nil {
			continue
		}
		w := *f.Winner
		if w.Currency != currency {
			return feeComposition{}, fmt.Errorf("%w: fee %s is %s, the sale is %s",
				errResolveUnusable, w.FeeCode, w.Currency, currency)
		}
		var amount int64
		switch w.Basis {
		case basisPerTicketFixed:
			if w.Amount == nil {
				return feeComposition{}, fmt.Errorf("%w: %s carries no amount", errResolveUnusable, w.Basis)
			}
			var err error
			if amount, err = checkedMul(*w.Amount, q); err != nil {
				return feeComposition{}, err
			}
		case basisPerOrderFixed:
			if w.Amount == nil {
				return feeComposition{}, fmt.Errorf("%w: %s carries no amount", errResolveUnusable, w.Basis)
			}
			// Once per reservation. A reservation is one ticket type today, so
			// per-order and per-line are indistinguishable here; the BASIS is
			// persisted alongside the amount so the future cart ticket can decide
			// the multi-line rule without reinterpreting history.
			amount = *w.Amount
		case basisPercentageBps:
			if w.RateBps == nil {
				return feeComposition{}, fmt.Errorf("%w: %s carries no rate", errResolveUnusable, w.Basis)
			}
			perTicket, err := feeFromRate(unitFace, *w.RateBps)
			if err != nil {
				return feeComposition{}, err
			}
			if amount, err = checkedMul(perTicket, q); err != nil {
				return feeComposition{}, err
			}
		default:
			// A future basis means catalog computes fees a way this build does
			// not understand. Refusing is the only safe reading — the same
			// discipline ADR-017 applies to event schemas, and the same one
			// catalog_pricing.go applies to an unknown action_kind.
			return feeComposition{}, fmt.Errorf("%w: unsupported fee basis %s", errResolveUnusable, w.Basis)
		}
		if amount < 0 {
			return feeComposition{}, fmt.Errorf("%w: negative fee %s", errResolveUnusable, w.FeeCode)
		}

		// A zero-amount fee is still a fee (ADR-046 §2): floor(1 × 333/10000) is
		// 0, and a code that vanishes as a function of price leaves TKT-217 with
		// a payee that is sometimes owed nothing and sometimes absent.
		out.Items = append(out.Items, feeBreakdownItem{
			FeeCode: w.FeeCode, Basis: w.Basis, Incidence: w.Incidence,
			Amount: amount, Currency: w.Currency,
		})

		var err error
		switch w.Incidence {
		case incidencePassedOn:
			if out.PassedOnTotal, err = checkedAdd(out.PassedOnTotal, amount); err != nil {
				return feeComposition{}, err
			}
		case incidenceAbsorbed:
			if out.AbsorbedTotal, err = checkedAdd(out.AbsorbedTotal, amount); err != nil {
				return feeComposition{}, err
			}
		default:
			return feeComposition{}, fmt.Errorf("%w: unknown incidence %s", errResolveUnusable, w.Incidence)
		}
	}
	return out, nil
}

// feeFromRate is floor(unitFace × rateBps / 10000) computed WITHOUT ever forming
// the full product (TKT-215 ai-review, [high]).
//
// The first version multiplied first and checked the product against the
// contract's Money cap. That rejected legitimate fees: at unitFace = 10^12 and
// rateBps = 10000 the correct fee is 10^12, but the intermediate 10^16 exceeds
// the cap and the sale was refused. Worse, a test asserted that refusal as
// REQUIRED behaviour, so the bug was pinned rather than caught.
//
// Quotient/remainder decomposition is exact and cannot overflow for any input
// the contract admits:
//
//	floor(a×b/10000) = (a/10000)×b + floor((a mod 10000)×b/10000)
//
// with a ≤ 2^53-1 and b ≤ 10000, the left term is at most a and the right at
// most 10^8. Note the result is bounded ABOVE by unitFace whenever b ≤ 10000, so
// a per-ticket percentage fee can never itself exceed the Money cap — only the
// later multiplication by quantity can, and that is checked where it happens.
func feeFromRate(unitFace int64, rateBps int32) (int64, error) {
	if unitFace < 0 || rateBps < 0 {
		return 0, fmt.Errorf("%w: negative operand", errFeeTotalOverflow)
	}
	r := int64(rateBps)
	hi, err := checkedMul(unitFace/10000, r)
	if err != nil {
		return 0, err
	}
	// (unitFace mod 10000) < 10000 and r <= 10000, so this product is at most
	// 10^8 — it cannot overflow and needs no check.
	lo := (unitFace % 10000) * r / 10000
	return checkedAdd(hi, lo)
}

// checkedMul and checkedAdd bound at int64, NOT at the contract's Money cap.
//
// The distinction is load-bearing and an existing test caught me getting it
// wrong. Catalog's Money schema caps a RULE's amount at 2^53-1, and
// checkFeeValue enforces that on every rule commerce accepts — that is about the
// rule, which crosses the contract. A composed ORDER TOTAL is commerce's own
// number: its contract declares a bare int64 with no maximum, and
// TestReserveOverflowGuardAppliesToResolvedAmount pins the existing rule
// explicitly — "the guard is about overflow, not about large prices", and a
// quantity of 2 at the maximum unit price must still sell.
//
// Bounding these at maxContractAmount silently narrowed what the system accepts,
// refusing sales it had always allowed. Overflow is the real failure; a large
// number is not.
func checkedMul(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, fmt.Errorf("%w: negative operand", errFeeTotalOverflow)
	}
	if a != 0 && b > math.MaxInt64/a {
		return 0, fmt.Errorf("%w: %d × %d", errFeeTotalOverflow, a, b)
	}
	return a * b, nil
}

func checkedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, fmt.Errorf("%w: negative operand", errFeeTotalOverflow)
	}
	if a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: %d + %d", errFeeTotalOverflow, a, b)
	}
	return a + b, nil
}

// resolveTicketTypeFees performs the single catalog read.
//
// The internal credential IS sent here, unlike the price read: ADR-046 §6 makes
// this an /internal/ operation because its response carries absorbed fees, which
// are the organizer's cost structure. Commerce already holds that credential and
// already uses it for /internal/ticket-types/{id}, so this adds no blast radius
// (ADR-043).
func (s *Server) resolveTicketTypeFees(ctx context.Context, ticketTypeID, organizerID, performanceID uuid.UUID, channel *string) (feeResolution, error) {
	endpoint := s.catalogURL + "/internal/ticket-types/" + ticketTypeID.String() + "/fee-resolution"
	if channel != nil {
		// Omitting the parameter is the default/public context, NOT a wildcard
		// (ADR-046 §4). A nil channel must therefore send NO parameter rather
		// than an empty one, which the contract's minLength would reject anyway.
		endpoint += "?channel_code=" + url.QueryEscape(*channel)
	}
	code, body, err := s.call(ctx, http.MethodGet, endpoint, "", nil, true)
	if err != nil {
		return feeResolution{}, fmt.Errorf("%w: %v", errResolveUnavailable, err)
	}
	if code != http.StatusOK {
		return feeResolution{}, fmt.Errorf("%w: catalog returned %d", errResolveUnavailable, code)
	}

	var f feeResolution
	// Strict about semantics, tolerant of unknown fields — the same reasoning
	// catalog_pricing.go documents at length: DisallowUnknownFields would make
	// every additive change to catalog's contract a commerce outage.
	if json.Unmarshal(body, &f) != nil {
		return feeResolution{}, fmt.Errorf("%w: body is not a FeeResolution", errResolveUnusable)
	}
	// Storability is proven BEFORE a hold exists, for the reason TKT-153 moved
	// the same check: an INSERT that fails after inventory has committed leaves
	// an orphan hold and hands the buyer a 500.
	canonical, err := storableSnapshot(body)
	if err != nil {
		return feeResolution{}, err
	}
	f.raw = canonical
	if err := f.validate(organizerID, performanceID, channel); err != nil {
		return feeResolution{}, err
	}
	return f, nil
}

// validate refuses any document the sale cannot be built on.
func (f feeResolution) validate(organizerID, performanceID uuid.UUID, channel *string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", errResolveUnusable, why) }

	if f.ResolverVersion < 1 {
		return bad("fee resolver_version below 1")
	}
	if f.EvaluatedAt.IsZero() {
		return bad("missing evaluated_at")
	}
	// Answering for a different tenant is a tenancy breach, not a pricing error.
	if f.OrganizerID != organizerID {
		return bad("fee resolution is for a different organizer")
	}
	// And it must describe the SAME slot the price resolution did. Without this
	// a schema-valid answer about another performance would be applied to this
	// sale — charging one show's fee schedule against another's hold. Checking
	// organizer alone does not catch it: both performances can belong to the same
	// organizer, which is the common case rather than the exotic one.
	if f.PerformanceID == uuid.Nil {
		return bad("fee resolution names no performance")
	}
	if f.PerformanceID != performanceID {
		return bad("fee resolution is for a different performance")
	}
	if !isISO4217(f.Currency) {
		return bad("fee resolution carries a malformed currency")
	}
	// The echoed channel must be the one asked about. A mismatch means the
	// answer describes a different question, and charging from it would apply
	// another channel's fees.
	switch {
	case channel == nil && f.ChannelCode != nil:
		return bad("resolution echoes a channel the sale did not request")
	case channel != nil && (f.ChannelCode == nil || *f.ChannelCode != *channel):
		return bad("resolution echoes a different channel")
	}
	seen := map[string]struct{}{}
	for _, c := range f.Fees {
		if c.FeeCode == "" {
			return bad("a fee code is empty")
		}
		if _, dup := seen[c.FeeCode]; dup {
			// Two entries for one code would double-charge it, and which one
			// wins would depend on iteration order.
			return bad("duplicate fee code " + c.FeeCode)
		}
		seen[c.FeeCode] = struct{}{}
		if c.Winner == nil {
			continue
		}
		w := *c.Winner
		if w.FeeCode != c.FeeCode {
			return bad("winner's fee code disagrees with its resolution entry")
		}
		if w.RuleID == uuid.Nil {
			return bad("winner is missing its identity")
		}
		if err := checkFeeValue(w); err != nil {
			return err
		}
		if w.Incidence != incidencePassedOn && w.Incidence != incidenceAbsorbed {
			return bad("unknown incidence " + w.Incidence)
		}
		if !isISO4217(w.Currency) {
			return bad("fee rule carries a malformed currency")
		}
	}
	return nil
}

// checkFeeValue enforces the tagged union on the wire. The database CHECK makes
// the other combinations unrepresentable in catalog, but commerce is a separate
// service reading a document over HTTP, and "the other side validates it" is not
// a guarantee this side can rely on.
func checkFeeValue(w resolvedFeeRule) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", errResolveUnusable, why) }
	switch w.Basis {
	case basisPerTicketFixed, basisPerOrderFixed:
		if w.Amount == nil {
			return bad(w.Basis + " carries no amount")
		}
		if w.RateBps != nil {
			return bad(w.Basis + " carries a rate as well as an amount")
		}
		if *w.Amount < 0 || *w.Amount > maxContractAmount {
			return bad("fee amount is outside the contract's Money range")
		}
	case basisPercentageBps:
		if w.RateBps == nil {
			return bad("percentage_bps carries no rate")
		}
		if w.Amount != nil {
			return bad("percentage_bps carries an amount as well as a rate")
		}
		if *w.RateBps < 0 || *w.RateBps > 10000 {
			return bad("rate_bps outside 0..10000")
		}
	default:
		return bad("unsupported fee basis " + w.Basis)
	}
	return nil
}

// composedTotal is what the buyer is charged: face value plus passed-on fees.
// Absorbed fees are deliberately absent — they are borne by the organizer out of
// the face value, and adding them here would charge the buyer for the
// organizer's cost.
func composedTotal(faceValue, passedOn int64) (int64, error) {
	// checkedAdd already bounds the result at the contract's Money cap, which is
	// well below MaxInt64 — so there is deliberately no second int64 check here.
	// One existed and staticcheck correctly called it unreachable: a check that
	// cannot fire is not defence in depth, it is a comment that compiles.
	return checkedAdd(faceValue, passedOn)
}

// feeSnapshotEnvelope assembles what gets persisted: catalog's document
// VERBATIM under `resolution`, plus the arithmetic commerce performed on it.
//
// Both halves are required and neither is derivable from the other. Storing only
// the breakdown discards the losing candidates, which are the answer to "why was
// I charged this fee". Storing only the resolution makes every later reader redo
// the arithmetic — and TKT-217 settles real money from it, so a reader that
// disagreed with the amount actually captured would be a ledger that does not
// balance.
//
// The database CHECK cross-references face_value and total_amount against the
// columns, so an envelope contradicting the row it explains is unrepresentable.
func feeSnapshotEnvelope(f feeResolution, c feeComposition, faceValue, total int64) ([]byte, error) {
	return json.Marshal(map[string]any{
		"resolution":     json.RawMessage(f.raw),
		"breakdown":      c.Items,
		"face_value":     faceValue,
		"passed_on_fees": c.PassedOnTotal,
		"absorbed_fees":  c.AbsorbedTotal,
		"total_amount":   total,
	})
}

// addFeeFields adds the buyer-facing split to a fresh reservation response.
//
// Every field is OPTIONAL in the contract, and that is not laziness: the response
// validator fails closed (ADR-028), so a newly required field would turn every
// pre-existing valid reservation response into a 500. `seats` is the precedent —
// it could not be required either, for exactly this reason.
func addFeeFields(out map[string]any, faceValue int64, c feeComposition) {
	out["face_value"] = faceValue
	out["passed_on_fees"] = c.PassedOnTotal
	// Present even when empty: an absent array and an empty one are different
	// documents, and a consumer branching on presence would read "no fee
	// information" instead of "no fees".
	items := c.Items
	if items == nil {
		items = []feeBreakdownItem{}
	}
	out["fee_breakdown"] = items
}

// addStoredFeeFields replays the breakdown from the persisted envelope.
//
// A NULL snapshot means a reservation created before this ticket, or by a staff
// path that is deliberately fee-free. Those answer with no fee fields at all
// rather than with zeros: "this sale had no fee concept" and "its fees totalled
// nothing" are different facts, and only the first is true of a pre-TKT-215 row.
func addStoredFeeFields(out map[string]any, faceValue int64, snapshot []byte) error {
	if len(snapshot) == 0 {
		return nil
	}
	var env struct {
		Breakdown    []feeBreakdownItem `json:"breakdown"`
		PassedOnFees int64              `json:"passed_on_fees"`
	}
	if err := json.Unmarshal(snapshot, &env); err != nil {
		return err
	}
	if env.Breakdown == nil {
		env.Breakdown = []feeBreakdownItem{}
	}
	out["face_value"] = faceValue
	out["passed_on_fees"] = env.PassedOnFees
	out["fee_breakdown"] = env.Breakdown
	return nil
}

// checkCompositionFits proves the whole composition is representable at a given
// quantity, before anything is held.
//
// It exists as a separate function rather than as a call to computeFeeBreakdown
// with a discarded result so that the pre-hold check and the real computation
// cannot drift apart: this one IS that computation, run for its error.
func checkCompositionFits(fees []resolvedFeeCode, p price, faceValue int64, quantity int32) error {
	composition, err := computeFeeBreakdown(fees, p.Amount, quantity, p.Currency)
	if err != nil {
		return err
	}
	_, err = composedTotal(faceValue, composition.PassedOnTotal)
	return err
}

// sameChannel compares two optional channel codes, treating nil (the
// default/public context) as distinct from any named channel — which it is:
// omitting the channel is not a wildcard (ADR-046 §4).
func sameChannel(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// sameTerms is the ONE definition of "this idempotency key names the same
// request", used by both the replay path and the lost-race path.
//
// It exists as a function rather than as two inline comparisons because those
// two paths answer the same question and previously answered it differently:
// the replay path compared the channel and the race path did not, so one request
// could be accepted as a replay once and refused as a conflict afterwards.
//
// The seat SET is compared, not its size: [1,2] and [2,3] are both two seats,
// and matching on count would replay someone else's seats back to a caller who
// asked for different ones.
func sameTerms(in reserveRequest, qty int32, ticketType uuid.UUID, channel *string, seats []string) bool {
	switch {
	case qty != in.units() || ticketType != in.TicketTypeID:
		return false
	case !sameChannel(channel, in.ChannelCode):
		return false
	case (len(seats) > 0) != in.seated():
		return false
	case in.seated() && !sameSeats(seats, in.canonicalSeatSet()):
		return false
	}
	return true
}
