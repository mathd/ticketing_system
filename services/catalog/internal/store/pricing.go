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
	ReasonForcedBroaderScope   = "forced_broader_scope"
	ReasonExcludedByForcedRule = "excluded_by_forced_rule"
	ReasonLowerForcedScope     = "lower_forced_scope"
	ReasonLowerPriority        = "lower_priority"
	ReasonStableIDTiebreak     = "stable_id_tiebreak"
	// Emitted only once TKT-152 adds effective windows; declared here so the
	// provenance shape TKT-153 persists does not change between the two.
	ReasonOutsideWindowPast   = "outside_window_past"
	ReasonOutsideWindowFuture = "outside_window_future"
)

// TKT-237 emits ReasonLessChannelSpecific, declared by the fee resolver
// (fees.go) since TKT-214. Shared rather than redeclared, and the reason is that
// the two resolvers agree on this ONE string by necessity, not by coincidence:
// it is a value in two closed OpenAPI enums, and a price resolution emitting
// "less_channel_specific " while a fee resolution emits "less_channel_specific"
// would be two contract bugs that look like one typo.
//
// This is the ONLY thing the two comparators share, and it is data rather than
// logic. ADR-046 §7's decision to duplicate the RANKING stands — see the
// amendment TKT-237 made to it.
//
// A channel-agnostic rule lost to an exact-channel rule at the same scope level
// (or, under a forced partition, the other way round — see beats). A rule
// belonging to a DIFFERENT channel is never a loser at all: it is ineligible and
// absent from provenance entirely.

// FallbackNoEligibleRule is set when no rule applied and the ticket type's own
// price is the answer.
const FallbackNoEligibleRule = "no_eligible_rule"

// PricingResolverVersion is a commitment, not a decoration: TKT-153 persists
// provenance snapshots, so changing the comparator's semantics means bumping
// this, or stored snapshots stop being interpretable.
// Bumped to 2 by TKT-152: window eligibility changes what the comparator
// MEANS, even though the response shape is unchanged. Version 1 cannot honestly
// describe both "windows ignored" and "windows filter eligibility", and TKT-153
// persists this number in snapshots that must stay interpretable.
// Bumped to 3 by TKT-237: the comparator gains a channel axis — a new
// eligibility filter and a new ranking step between scope and priority. A
// channel-agnostic request over channel-agnostic rules resolves identically, but
// version 2 cannot honestly describe both "channel is not a concept" and
// "channel filters eligibility and ranks below scope". Version 2 snapshots stay
// valid and are NOT rewritten; commerce accepts any version >= 1.
const PricingResolverVersion int32 = 3

// ErrPriceRuleCurrencyMismatch is invalid configuration, not a rule that does
// not apply (ADR-036 §2). Silently skipping it would sell at the wrong price
// and look like nothing happened, so resolution fails instead.
var ErrPriceRuleCurrencyMismatch = errors.New("price rule currency differs from the ticket type's")

// ErrDuplicatePriceRuleID guards the comparator's determinism claim: the last
// tie-break is the id, so two rules sharing one are inseparable and the winner
// would depend on input order. Unreachable through Postgres (id is the primary
// key) — this is the pure seam refusing to pretend otherwise.
var ErrDuplicatePriceRuleID = errors.New("two price rules share an id")

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
	// Half-open effective window [EffectiveFrom, EffectiveUntil) (TKT-152,
	// ADR-036 §4 step 2). Either bound nil means unbounded on that side; a
	// reversed window is unrepresentable (CHECK in migration 0013).
	//
	// This is what makes a tier flip "without manual intervention": nothing
	// runs. The same rows resolve differently as the evaluation instant
	// crosses a boundary — no cron, no scheduled write, no job to fail.
	EffectiveFrom  *time.Time
	EffectiveUntil *time.Time
	// ChannelCode nil means channel-agnostic: eligible in EVERY channel,
	// including the default/public one. A non-nil code competes only on an
	// exact string match (ADR-024 keeps channel codes opaque — nothing trims
	// or case-folds). TKT-237.
	ChannelCode *string
	CreatedAt   time.Time
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
	EffectiveFrom         *time.Time
	EffectiveUntil        *time.Time
	// nil = channel-agnostic. Exact and opaque; nothing normalizes (ADR-024).
	ChannelCode *string
}

// PricingCandidates is everything the pure comparator needs.
type PricingCandidates struct {
	BasePrice Money
	Scopes    PricingScopes
	Rules     []PriceRule
	// Channel the resolution was requested for; nil is the default/public
	// context, where only channel-agnostic rules are eligible. Omitting it is
	// NOT a wildcard (ADR-046 §4, applied to prices by TKT-237).
	Channel *string
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
	// Identity of what was priced. Carried here so ONE read answers a caller's
	// whole question — commerce needs the organizer to authorize and the slot to
	// place the hold, and a second catalog read to fetch them would have to be
	// reconciled against this one on every sale (TKT-153).
	OrganizerID   uuid.UUID
	PerformanceID uuid.UUID
	BasePrice     Money
	ResolvedPrice Money
	// Channel this resolution answered for; nil is the default/public context.
	// Echoed so a persisted snapshot (TKT-153) records WHICH question was
	// answered — without it a stored provenance object is ambiguous between
	// "no channel rules existed" and "the sale named no channel".
	Channel        *string
	Winner         *PriceRule
	Candidates     []LosingPriceRule
	FallbackReason *string
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
		PerformanceID:   in.Scopes.SlotID,
		BasePrice:       in.BasePrice,
		ResolvedPrice:   in.BasePrice,
		Channel:         in.Channel,
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

	// Sort by id up front so EVERY output of this function is order-independent,
	// not just the winner. Without it the currency check below reports whichever
	// bad rule happened to come first, and that id is what the handler logs — so
	// the same broken data would produce different operator-facing diagnostics
	// run to run.
	sort.Slice(scoped, func(i, j int) bool {
		return scoped[i].ID.String() < scoped[j].ID.String()
	})

	// Channel eligibility, BEFORE the currency check and before the window
	// filter. Both orderings are load-bearing and neither is stylistic (TKT-237).
	//
	// A rule belonging to another channel must be ABSENT from provenance, not
	// reported as a loser: returning it would publish which channels carry
	// bespoke pricing, and at what amounts, on a route the gateway proxies to
	// the internet. (ADR-046 §4 argues this for fees, where the endpoint is
	// /internal/; here the audience is the internet, so the reason is strictly
	// stronger. TKT-155 already tracks this array over-disclosing.)
	//
	// BEFORE THE WINDOW, because filtering after it would classify a foreign
	// channel's expired rule as `outside_window_past` and report it — leaking
	// exactly what this filter hides, through the branch that looks unrelated to
	// channels.
	//
	// BEFORE THE CURRENCY CHECK, and this one was a real defect found at
	// ai-review. The fee resolver deliberately checks currency across channels,
	// on the argument that a rule misconfigured for another channel is still
	// misconfigured and should fail loudly rather than lie in wait. Mirroring
	// that here was wrong, because the two resolvers do not fail the same way: a
	// currency mismatch aborts the WHOLE resolution with an error, so one
	// misconfigured `pos` rule made every `reseller` and every public request
	// return 500. That is a cross-channel outage — one channel's bad
	// configuration taking down every other channel's sales — and, on a public
	// endpoint, an oracle for the existence of a rule the filter exists to hide.
	//
	// The cost of the correct order, stated: a misconfigured rule on a channel
	// nobody is currently buying through stays silent until a sale arrives on
	// it, and then fails closed. That is the same latency the window filter
	// already accepts for a future-dated rule, and it is strictly better than an
	// outage on channels that are configured correctly. An operator-facing
	// validation sweep is the right place to surface it early; TKT-243 carries
	// that.
	channelScoped := make([]PriceRule, 0, len(scoped))
	for _, r := range scoped {
		if priceChannelEligible(r, in.Channel) {
			channelScoped = append(channelScoped, r)
		}
	}
	scoped = channelScoped

	// Two rules sharing an id would make the answer depend on input order: the
	// final tie-break is the id itself, so they form an equivalence class the
	// comparator cannot separate, and the loser loop would suppress both. The
	// primary key makes this unreachable through Postgres — but this function is
	// the seam TKT-152 and TKT-153 build on, and a function that advertises
	// determinism must not quietly have an input that breaks it.
	//
	// NARROWED BY TKT-237, deliberately: this now runs AFTER the channel filter,
	// so duplicate ids among rules that are ineligible for the requested channel
	// no longer error. That is the right scope. The guard protects the
	// DETERMINISM OF THE ANSWER, and a rule that cannot compete cannot affect
	// the answer — erroring on it would refuse a resolution that has exactly one
	// correct result. Eligible duplicates still error, which is the case the
	// guard was written for and the one a test pins.
	seen := make(map[uuid.UUID]struct{}, len(scoped))
	for _, r := range scoped {
		if _, dup := seen[r.ID]; dup {
			return RuleSelection{}, fmt.Errorf("%w: %s", ErrDuplicatePriceRuleID, r.ID)
		}
		seen[r.ID] = struct{}{}
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
		if isPast(at, r) {
			// Inert: its window closed, so it can never price anything again.
			// Failing on its account would be permanent and unrecoverable --
			// currency is immutable and effective_until only shortens, so no
			// write could rescue it. A dead row must not be an outage.
			continue
		}
		if r.Currency != in.BasePrice.Currency {
			return RuleSelection{}, fmt.Errorf("%w: rule %s is %s, ticket type is %s",
				ErrPriceRuleCurrencyMismatch, r.ID, r.Currency, in.BasePrice.Currency)
		}
	}

	// Step 2 — window filter. Half-open [EffectiveFrom, EffectiveUntil): the
	// closed end is inclusive, the open end is not. An ambiguity here is a
	// money bug at every tier boundary, so both ends are asserted by tests.
	//
	// The CHECK in migration 0013 makes a reversed window unrepresentable, so
	// no rule can be simultaneously past and future and the two reasons below
	// are mutually exclusive.
	eligible := make([]PriceRule, 0, len(scoped))
	var windowLosers []LosingPriceRule
	for _, r := range scoped {
		switch {
		case isPast(at, r):
			windowLosers = append(windowLosers, LosingPriceRule{Rule: r, Reason: ReasonOutsideWindowPast})
		case r.EffectiveFrom != nil && at.Before(*r.EffectiveFrom):
			windowLosers = append(windowLosers, LosingPriceRule{Rule: r, Reason: ReasonOutsideWindowFuture})
		default:
			eligible = append(eligible, r)
		}
	}

	if len(eligible) == 0 {
		reason := FallbackNoEligibleRule
		out.FallbackReason = &reason
		// The window losers still ship. Returning the base price with EMPTY
		// provenance would satisfy "all tiers expired falls back to the base
		// price" while destroying the only answer to the question anyone
		// actually asks -- "why is it showing 60 and not the early-bird 45?".
		// TKT-151's version of this early return did exactly that; it was
		// latent because no reason could reach it until this ticket.
		out.Candidates = sortedCandidates(windowLosers)
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
	losers := windowLosers
	for _, r := range eligible {
		if r.ID == winner.ID {
			continue
		}
		losers = append(losers, LosingPriceRule{Rule: r, Reason: lossReason(r, winner)})
	}
	out.Candidates = sortedCandidates(losers)
	return out, nil
}

// isPast reports that a rule's window has closed at `at`. Half-open: the
// instant equal to EffectiveUntil is already outside.
func isPast(at time.Time, r PriceRule) bool {
	return r.EffectiveUntil != nil && !at.Before(*r.EffectiveUntil)
}

// sortedCandidates gives the loser list a stable order. Representation only --
// the comparator does not depend on it, and must not.
func sortedCandidates(losers []LosingPriceRule) []LosingPriceRule {
	sort.Slice(losers, func(i, j int) bool {
		return losers[i].Rule.ID.String() < losers[j].Rule.ID.String()
	})
	return losers
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

// priceChannelEligible reports whether a rule can compete for the requested
// channel (TKT-237, mirroring ADR-046 §4).
//
// A channel-agnostic rule (nil) competes everywhere, including in the
// default/public context; a channel-specific rule competes only on an exact,
// case-sensitive string match (ADR-024 keeps channel codes opaque). A nil
// request is the default/public context, where no channel-specific rule is
// eligible — **omitting the channel is not a wildcard**, and that asymmetry is
// the whole point: a rule authored for one channel must never price a sale that
// named none.
func priceChannelEligible(r PriceRule, requested *string) bool {
	if r.ChannelCode == nil {
		return true
	}
	return requested != nil && *r.ChannelCode == *requested
}

// priceChannelSpecificity ranks a rule's channel selectivity narrowest-first, in
// the same direction as scopeRank: 0 is the narrower (exact channel) statement.
func priceChannelSpecificity(r PriceRule) int {
	if r.ChannelCode != nil {
		return 0
	}
	return 1
}

// beats reports whether challenger should displace best. Under a forced
// partition BOTH specificity orders invert — scope level and channel; otherwise
// the narrower rule wins on each. Priority then id break the remaining ties.
//
// Channel sits BELOW scope and ABOVE priority (ADR-046 §4, applied to prices).
// That placement is deliberate and testable: a channel rule wins even when the
// agnostic rule carries a higher priority, because priority disambiguates rules
// of equal specificity and a channel rule is not of equal specificity. A
// BROADER exact-channel rule still loses to a NARROWER agnostic one, because
// scope is compared first.
//
// Both axes invert under the forced partition for one reason: a forced rule is a
// house floor that refuses to be undercut, so the BROADEST such statement binds
// — and a channel-agnostic rule is the broader statement. Left undefined this
// case would fall through to priority and be decided by accident, on a money
// path.
func beats(challenger, best PriceRule, inverted bool) bool {
	cr, br := scopeRank[challenger.ScopeLevel], scopeRank[best.ScopeLevel]
	if cr != br {
		if inverted {
			return cr > br
		}
		return cr < br
	}
	cc, bc := priceChannelSpecificity(challenger), priceChannelSpecificity(best)
	if cc != bc {
		if inverted {
			return cc > bc
		}
		return cc < bc
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
			return ReasonForcedBroaderScope
		}
		return ReasonExcludedByForcedRule
	}
	if scopeRank[loser.ScopeLevel] != scopeRank[winner.ScopeLevel] {
		if winner.ForceAncestorOverride {
			return ReasonLowerForcedScope
		}
		return ReasonLessSpecific
	}
	// Channel specificity, below scope and above priority.
	//
	// This reason is emitted ONLY for a rule that was channel-ELIGIBLE and lost
	// on specificity — i.e. a channel-agnostic rule beaten by an exact-channel
	// rule at the same scope (or, under a forced partition, the reverse). A rule
	// belonging to a DIFFERENT channel never reaches here: it is filtered out as
	// ineligible and is absent from provenance entirely, so it has no reason.
	//
	// The distinction matters in exactly one direction. Declaring this enum
	// member and never emitting it would be harmless; emitting it without
	// declaring it 500s a public money read under ADR-028's fail-closed response
	// validation, and TestPriceLossReasonEnumMatchesTheContract is what stops
	// that.
	if priceChannelSpecificity(loser) != priceChannelSpecificity(winner) {
		return ReasonLessChannelSpecific
	}
	if loser.Priority != winner.Priority {
		return ReasonLowerPriority
	}
	return ReasonStableIDTiebreak
}
