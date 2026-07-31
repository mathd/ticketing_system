package store

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Price-rule resolution (TKT-151). The specification is
// docs/adr/ADR-036-pricing-rules-representation.md; §numbers below refer to it.
//
// SelectPricingRule is deliberately pure: no database, no clock. Choosing the
// rule shape was choosing this seam (ADR-036 §4), and its table-driven test is
// the truth table itself. Everything that needs a database — deriving the five
// scopes, loading the candidates — lives in postgres.go.

// ScopeLevel is one of the five levels a rule can attach to (ADR-036 §1).
type ScopeLevel string

const (
	ScopeTicketType ScopeLevel = "ticket_type"
	ScopeSlot       ScopeLevel = "slot"
	ScopeSeries     ScopeLevel = "series"
	ScopeEvent      ScopeLevel = "event"
	ScopeVenue      ScopeLevel = "venue"
)

// ActionAbsolute is the only action in v1: the rule states a final unit price.
// The field is tagged anyway so widening the union later is additive rather
// than breaking (ADR-036 §2).
const ActionAbsolute = "absolute"

// scopeRank orders the levels narrowest-first:
//
//	ticket_type > slot > series > event > venue
//
// Part containment, part policy, and ADR-036 §1 is explicit about which is
// which. Two things a reader is likely to want to "fix" and must not:
//
//   - series is NARROWER than event. series.event_id is NOT NULL REFERENCES
//     events and AttachPerformanceToSeries refuses a cross-event performance,
//     so a series is a strict subset of one event's slots. docs/product/prd-v1.md:36
//     orders it the other way; that phrasing predates the schema and ADR-036
//     supersedes it.
//   - event above venue is a POLICY choice, not a derivation: performances
//     carries independent FKs to event_id and venue_id, so the two are
//     incomparable in the schema. A venue rule is a house default; an event rule
//     is a deliberate statement about one show, so the event wins — and an
//     explicit override exists precisely to invert that when the house default
//     is meant to bind.
var scopeRank = map[ScopeLevel]int{
	ScopeTicketType: 0,
	ScopeSlot:       1,
	ScopeSeries:     2,
	ScopeEvent:      3,
	ScopeVenue:      4,
}

// Why each losing candidate lost (ADR-036 §5). The enum is closed and total
// over the comparator below: every way a rule can lose has a value, or an
// implementer invents one.
const (
	ReasonLessSpecific         = "less_specific"
	ReasonForcedAncestor       = "forced_ancestor"
	ReasonExcludedByForcedRule = "excluded_by_forced_rule"
	ReasonLowerForcedScope     = "lower_forced_scope"
	ReasonLowerPriority        = "lower_priority"
	ReasonStableIDTiebreak     = "stable_id_tiebreak"
	// Emitted only once TKT-152 adds effective windows; declared here so the
	// provenance shape TKT-153 persists does not change between the two.
	ReasonOutsideWindowPast   = "outside_window_past"
	ReasonOutsideWindowFuture = "outside_window_future"
)

// FallbackNoEligibleRule is set when no rule applied and the ticket type's own
// price is the answer.
const FallbackNoEligibleRule = "no_eligible_rule"

// PricingResolverVersion is a commitment, not a decoration: TKT-153 persists
// provenance snapshots, so changing the comparator's semantics means bumping
// this, or stored snapshots stop being interpretable.
const PricingResolverVersion int32 = 1

// ErrPriceRuleCurrencyMismatch is invalid configuration, not a rule that does
// not apply (ADR-036 §2). Silently skipping it would sell at the wrong price
// and look like nothing happened, so resolution fails instead.
var ErrPriceRuleCurrencyMismatch = errors.New("price rule currency differs from the ticket type's")

// Money is integer minor units + ISO-4217 (ADR-001). Floats are banned here.
type Money struct {
	Amount   int64
	Currency string
}

// PricingScopes are the five scope identities derived from the requested ticket
// type (ADR-036 §1). SeriesID is nil when the slot belongs to no series —
// membership is optional, which makes the series edge partial.
type PricingScopes struct {
	TicketTypeID uuid.UUID
	SlotID       uuid.UUID
	SeriesID     *uuid.UUID
	EventID      uuid.UUID
	VenueID      uuid.UUID
}

// PriceRule is one append-mostly row. Pricing fields never change after insert;
// TKT-152 adds the one mutable field (effective_until, closing only).
type PriceRule struct {
	ID                    uuid.UUID
	OrganizerID           uuid.UUID
	ScopeLevel            ScopeLevel
	ScopeID               uuid.UUID
	ActionKind            string
	Amount                int64
	Currency              string
	Priority              int32
	ForceAncestorOverride bool
	CreatedAt             time.Time
}

// PriceRuleInput creates a rule. The store validates that ScopeID names a real
// row of ScopeLevel's kind owned by OrganizerID — the write-path half of
// ADR-036 §3's deliberate trade of referential integrity for a provable plan.
type PriceRuleInput struct {
	OrganizerID           uuid.UUID
	ScopeLevel            ScopeLevel
	ScopeID               uuid.UUID
	Amount                int64
	Currency              string
	Priority              int32
	ForceAncestorOverride bool
}

// PricingCandidates is everything the pure comparator needs.
type PricingCandidates struct {
	BasePrice Money
	Scopes    PricingScopes
	Rules     []PriceRule
}

// LosingPriceRule is a candidate that did not win, and why.
type LosingPriceRule struct {
	Rule   PriceRule
	Reason string
}

// RuleSelection is the provenance object (ADR-036 §5). Candidates holds every
// considered rule EXCEPT the winner — stated explicitly because "candidates"
// and "the losers" pull in opposite directions.
type RuleSelection struct {
	ResolverVersion int32
	EvaluatedAt     time.Time
	BasePrice       Money
	ResolvedPrice   Money
	Winner          *PriceRule
	Candidates      []LosingPriceRule
	FallbackReason  *string
}

// SelectPricingRule resolves one unit price and reports why (ADR-036 §4).
//
// It returns (RuleSelection, error) rather than folding the failure into the
// struct: the struct is serialization-adjacent, and an error field on it is one
// careless json tag away from leaking an internal message onto a public money
// endpoint.
func SelectPricingRule(at time.Time, in PricingCandidates) (RuleSelection, error) {
	out := RuleSelection{
		ResolverVersion: PricingResolverVersion,
		EvaluatedAt:     at,
		BasePrice:       in.BasePrice,
		ResolvedPrice:   in.BasePrice,
	}

	// Step 0 — keep only rules whose (level, id) is one of the derived pairs.
	// The SQL already filters on the pair (ADR-036 §3); doing it again here is
	// deliberate defence in depth, because UUID uniqueness is per table and a
	// scope_id colliding across tables would otherwise become a candidate.
	scoped := make([]PriceRule, 0, len(in.Rules))
	for _, r := range in.Rules {
		if matchesScope(r, in.Scopes) {
			scoped = append(scoped, r)
		}
	}

	// Step 1 — currency, on every scoped rule, BEFORE the window filter.
	//
	// The ordering is the interesting part and ADR-036 got it wrong twice in
	// opposite directions. Checking only window-surviving rules lets a
	// misconfigured future rule sit silently until its window opens and then
	// reprice; checking rules whose window has already CLOSED would make a
	// long-dead row fail every resolution forever, unrecoverably, since
	// currency is immutable and effective_until only shortens. With no windows
	// in TKT-151 every rule is unbounded and so every scoped rule is checked.
	// TKT-152 narrows this to rules that are not already past.
	for _, r := range scoped {
		if r.Currency != in.BasePrice.Currency {
			return RuleSelection{}, fmt.Errorf("%w: rule %s is %s, ticket type is %s",
				ErrPriceRuleCurrencyMismatch, r.ID, r.Currency, in.BasePrice.Currency)
		}
	}

	// Step 2 — window filter. A no-op until TKT-152 adds the columns; written
	// as an explicit assignment so that story is additive rather than a
	// reshaping of this function.
	eligible := scoped

	if len(eligible) == 0 {
		reason := FallbackNoEligibleRule
		out.FallbackReason = &reason
		return out, nil
	}

	// Step 3 — the override partition. If any eligible rule is forced, the
	// competition is restricted to forced rules and the scope order inverts:
	// among house rules that refuse to be undercut, the broadest wins.
	competition := eligible
	forcedPresent := false
	for _, r := range eligible {
		if r.ForceAncestorOverride {
			forcedPresent = true
			break
		}
	}
	if forcedPresent {
		competition = make([]PriceRule, 0, len(eligible))
		for _, r := range eligible {
			if r.ForceAncestorOverride {
				competition = append(competition, r)
			}
		}
	}

	// Steps 4-6 — rank, then priority, then the stable id tie-break. The last
	// one is deliberately semantically uninteresting: operators express intent
	// through priority, and its only job is to stop row order, a timestamp or a
	// query plan from deciding a price. (created_at would be the wrong
	// tie-break — two rules inserted in one transaction share now().)
	winner := competition[0]
	for _, r := range competition[1:] {
		if beats(r, winner, forcedPresent) {
			winner = r
		}
	}

	out.Winner = &winner
	out.ResolvedPrice = Money{Amount: winner.Amount, Currency: winner.Currency}
	for _, r := range eligible {
		if r.ID == winner.ID {
			continue
		}
		out.Candidates = append(out.Candidates, LosingPriceRule{Rule: r, Reason: lossReason(r, winner)})
	}
	// Stable output ordering. Representation only — the comparator above does
	// not depend on it, and must not.
	sort.Slice(out.Candidates, func(i, j int) bool {
		return out.Candidates[i].Rule.ID.String() < out.Candidates[j].Rule.ID.String()
	})
	return out, nil
}

// matchesScope reports whether a rule attaches to one of the derived identities
// at the level it claims. Both halves matter: a rule at the right level with a
// foreign id is not a candidate, and neither is a rule carrying a derived id at
// the wrong level.
func matchesScope(r PriceRule, s PricingScopes) bool {
	switch r.ScopeLevel {
	case ScopeTicketType:
		return r.ScopeID == s.TicketTypeID
	case ScopeSlot:
		return r.ScopeID == s.SlotID
	case ScopeSeries:
		// Nil means the slot is in no series, so no series rule can apply —
		// including one whose scope_id is the zero UUID.
		return s.SeriesID != nil && r.ScopeID == *s.SeriesID
	case ScopeEvent:
		return r.ScopeID == s.EventID
	case ScopeVenue:
		return r.ScopeID == s.VenueID
	default:
		return false
	}
}

// beats reports whether challenger should displace best. Under a forced
// partition the scope order is inverted (broader wins); otherwise the narrower
// rule wins. Priority then id break the remaining ties.
func beats(challenger, best PriceRule, inverted bool) bool {
	cr, br := scopeRank[challenger.ScopeLevel], scopeRank[best.ScopeLevel]
	if cr != br {
		if inverted {
			return cr > br
		}
		return cr < br
	}
	if challenger.Priority != best.Priority {
		return challenger.Priority > best.Priority
	}
	return challenger.ID.String() < best.ID.String()
}

// lossReason maps a loser to its closed-enum reason (ADR-036 §5).
//
// The three forced-related reasons partition on (was the loser forced?) then
// (is the winner BROADER in the §1 order?). "Broader" means position in that
// order, NOT graph ancestry — event and venue are incomparable in the schema,
// so an ancestry test would leave the commonest case, a forced venue rule
// beating an ordinary event rule, with no reason at all.
func lossReason(loser, winner PriceRule) string {
	if winner.ForceAncestorOverride && !loser.ForceAncestorOverride {
		if scopeRank[winner.ScopeLevel] > scopeRank[loser.ScopeLevel] {
			return ReasonForcedAncestor
		}
		return ReasonExcludedByForcedRule
	}
	if scopeRank[loser.ScopeLevel] != scopeRank[winner.ScopeLevel] {
		if winner.ForceAncestorOverride {
			return ReasonLowerForcedScope
		}
		return ReasonLessSpecific
	}
	if loser.Priority != winner.Priority {
		return ReasonLowerPriority
	}
	return ReasonStableIDTiebreak
}
