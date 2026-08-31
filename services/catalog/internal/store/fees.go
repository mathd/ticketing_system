package store

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Fee-rule resolution (TKT-214). The specification is
// docs/adr/ADR-046-fee-rules-representation.md; §numbers below refer to it.
//
// SelectFeeRules is deliberately pure: no database, no clock. Everything that
// needs a database — deriving the five scopes, loading the candidates — lives in
// fees_postgres.go, exactly as pricing.go / pricing_postgres.go split.
//
// THE ONE STRUCTURAL DIFFERENCE FROM PRICING, and the reason this is a separate
// resolver rather than a widened one: a price resolution has exactly ONE winner,
// a fee resolution has one winner PER FEE CODE. A sale can carry a service fee
// and a facility fee at the same time. So the hierarchy is applied inside each
// code's partition and the results are additive across codes — which lets the
// comparator keep ADR-036's shape instead of inventing a stacking order.
//
// The comparator below DUPLICATES pricing.go's rather than sharing a helper
// with it, and that is deliberate (ADR-046 § Deliberate duplication). Extracting
// a common ranker would mean editing a shipped money path for a ticket that adds
// no pricing behaviour, and the two are not the same function anyway: this one
// adds a channel step, partitions by code, and returns a set. Revisit at a THIRD
// rule kind, not a second.

// FeeBasis is how a rule's number becomes money. The arithmetic itself is
// commerce's (ADR-046 § Ownership) — catalog resolves WHICH rule applies and
// carries its basis, it never multiplies.
type FeeBasis string

const (
	// BasisPerTicketFixed: Amount minor units, once per ticket.
	BasisPerTicketFixed FeeBasis = "per_ticket_fixed"
	// BasisPerOrderFixed: Amount minor units, once per order.
	BasisPerOrderFixed FeeBasis = "per_order_fixed"
	// BasisPercentageBps: RateBps basis points of the resolved unit price.
	BasisPercentageBps FeeBasis = "percentage_bps"
)

// FeeIncidence is who bears the fee. It changes nothing about eligibility,
// precedence, currency validation or provenance — only what commerce does with
// the number later (ADR-046 § Incidence).
type FeeIncidence string

const (
	// IncidencePassedOn is added to what the buyer is charged.
	IncidencePassedOn FeeIncidence = "passed_on"
	// IncidenceAbsorbed is borne by the organizer out of the face value. The
	// buyer never sees it, which is exactly why this resolution is an
	// /internal/ read: an absorbed fee is the organizer's cost structure.
	IncidenceAbsorbed FeeIncidence = "absorbed"
)

// ReasonLessChannelSpecific is this resolver's one new loser reason: a
// channel-agnostic rule lost to an exact-channel rule at the same scope level
// (or, under a forced partition, the other way round — see feeBeats).
const ReasonLessChannelSpecific = "less_channel_specific"

// FeeResolverVersion is a commitment, not a decoration: TKT-215 persists
// resolution snapshots, so changing what this comparator MEANS means bumping
// this, or stored snapshots stop being interpretable.
const FeeResolverVersion int32 = 1

// ErrFeeRuleCurrencyMismatch is invalid configuration, not a rule that does not
// apply. Silently skipping it would charge a fee in the wrong currency and look
// like nothing happened, so resolution fails instead — the same disposition
// ErrPriceRuleCurrencyMismatch has, for the same reason.
var ErrFeeRuleCurrencyMismatch = errors.New("fee rule currency differs from the ticket type's")

// ErrDuplicateFeeRuleID guards the comparator's determinism claim: the last
// tie-break is the id, so two rules sharing one are inseparable and the winner
// would depend on input order. Unreachable through Postgres (id is the primary
// key) — this is the pure seam refusing to pretend otherwise.
var ErrDuplicateFeeRuleID = errors.New("two fee rules share an id")

// FeeRule is one append-mostly row. Pricing-equivalent fields never change after
// insert; effective_until is the only mutable one, and only to close a window.
//
// Amount and RateBps are mutually exclusive by Basis — the database CHECK makes
// the other combinations unrepresentable, and both are pointers here so a
// missing value is distinguishable from a zero one.
type FeeRule struct {
	ID          uuid.UUID
	OrganizerID uuid.UUID
	ScopeLevel  ScopeLevel
	ScopeID     uuid.UUID
	// FeeCode names the additive fee stream. One winner per code; codes do not
	// compete with each other. Opaque and case-sensitive (ADR-046 § Fee codes) —
	// no registry, no normalization; that is TKT-17's story.
	FeeCode   string
	Basis     FeeBasis
	Amount    *int64
	RateBps   *int32
	Currency  string
	Incidence FeeIncidence
	// ChannelCode nil means channel-agnostic: eligible in EVERY channel,
	// including the default/public one. A non-nil code is eligible only for an
	// exact string match (ADR-024 keeps channel codes opaque).
	ChannelCode           *string
	Priority              int32
	ForceAncestorOverride bool
	// Half-open effective window [EffectiveFrom, EffectiveUntil), identical to
	// price rules (ADR-036 §4 step 2). A reversed window is unrepresentable.
	EffectiveFrom  *time.Time
	EffectiveUntil *time.Time
	CreatedAt      time.Time
}

// FeeRuleInput creates a rule. The store validates that ScopeID names a real row
// of ScopeLevel's kind owned by OrganizerID — the write-path half of ADR-036
// §3's deliberate trade of referential integrity for a provable query plan.
type FeeRuleInput struct {
	OrganizerID           uuid.UUID
	ScopeLevel            ScopeLevel
	ScopeID               uuid.UUID
	FeeCode               string
	Basis                 FeeBasis
	Amount                *int64
	RateBps               *int32
	Currency              string
	Incidence             FeeIncidence
	ChannelCode           *string
	Priority              int32
	ForceAncestorOverride bool
	EffectiveFrom         *time.Time
	EffectiveUntil        *time.Time
}

// LosingFeeRule is a candidate that did not win its code, and why.
type LosingFeeRule struct {
	Rule   FeeRule
	Reason string
}

// FeeCodeSelection is one code's outcome: at most one winner, plus every other
// rule that competed for that code and why it lost.
//
// Winner is NIL when the code was considered and nothing currently applies —
// every rule for it fell outside its effective window. That case emits a
// selection rather than nothing, for the reason TKT-152 had to relearn on the
// price side: dropping it would satisfy "an expired fee is not charged" while
// destroying the only answer to the question anyone actually asks, which is
// "why is the booking fee not showing up?". A code with no rules at all emits
// no selection — CONSIDERED and NOT PRESENT are different states.
type FeeCodeSelection struct {
	FeeCode    string
	Winner     *FeeRule
	Candidates []LosingFeeRule
	// Split is who this fee is owed to (TKT-216). It rides the fee resolution
	// rather than a second endpoint deliberately: TKT-215 persists this whole
	// document verbatim as the reservation's snapshot, so carrying the split
	// here is what captures it AT SALE TIME. A schedule edited afterwards then
	// cannot change who gets paid for a sale that already happened — the same
	// snapshot-not-reference discipline migrations 0006 and 0014 argue for.
	//
	// Zero value means "not resolved", which only happens on the pure seam's
	// own tests; the store always fills it.
	Split SplitSelection
}

// FeeSelection is the provenance object. Fees is ordered by fee code so the
// document is stable; the order carries no precedence, because codes do not
// compete.
type FeeSelection struct {
	ResolverVersion int32
	EvaluatedAt     time.Time
	OrganizerID     uuid.UUID
	PerformanceID   uuid.UUID
	// Currency of the ticket type every rule here was validated against.
	Currency string
	// Channel the resolution was requested for; nil is the default/public
	// context, where only channel-agnostic rules are eligible.
	Channel *string
	Fees    []FeeCodeSelection
}

// FeeCandidates is everything the pure comparator needs.
type FeeCandidates struct {
	Currency string
	Scopes   PricingScopes
	Channel  *string
	Rules    []FeeRule
}

// SelectFeeRules resolves one winner per fee code and reports why.
//
// Step order matters and is NOT the same as a comparator chain — the first four
// steps are FILTERS over the candidate set, applied before anything is ranked:
//
//	0. scope-pair match          4. forced partition (a filter, per code)
//	1. currency (non-past rules) 5. scope level  (inverted if forced)
//	2. effective window          6. channel specificity (inverted if forced)
//	3. channel eligibility       7. priority     8. stable id
//
// Getting 4 wrong — treating the forced partition as a tie-break after scope
// comparison — would let a narrower unforced rule beat a forced one, which is
// the exact inversion force_ancestor_override exists to prevent.
func SelectFeeRules(at time.Time, in FeeCandidates) (FeeSelection, error) {
	out := FeeSelection{
		ResolverVersion: FeeResolverVersion,
		EvaluatedAt:     at,
		PerformanceID:   in.Scopes.SlotID,
		Currency:        in.Currency,
		Channel:         in.Channel,
		// Never nil: "no fees apply" must serialize as an empty set, not null.
		Fees: []FeeCodeSelection{},
	}

	// Step 0 — keep only rules whose (level, id) is one of the derived pairs.
	// The SQL already filters on the pair; doing it again here is deliberate
	// defence in depth, because UUID uniqueness is per table and a scope_id
	// colliding across tables would otherwise become a candidate (ADR-036 §3).
	scoped := make([]FeeRule, 0, len(in.Rules))
	for _, r := range in.Rules {
		if feeMatchesScope(r, in.Scopes) {
			scoped = append(scoped, r)
		}
	}

	// Sort by id up front so EVERY output is order-independent, not just the
	// winner: without it the currency check below reports whichever bad rule
	// came first, and that id is what the handler logs.
	sort.Slice(scoped, func(i, j int) bool { return scoped[i].ID.String() < scoped[j].ID.String() })

	// Step 1 — currency, before the window filter and before channel
	// eligibility, on every scoped rule that is not already inert.
	//
	// The ordering is inherited from ADR-036 §4 step 1 and it is subtle in both
	// directions. Checking only surviving rules lets a misconfigured future rule
	// sit silently until its window opens and then charge in the wrong currency.
	// Checking rules whose window has already CLOSED would make a long-dead row
	// fail every resolution forever, unrecoverably, since currency is immutable
	// and effective_until only shortens.
	//
	// Channel is deliberately NOT part of this guard's condition: a rule
	// misconfigured for another channel is still misconfigured, and it will be
	// charged the moment a sale arrives on that channel. Failing now is what
	// makes it findable then.
	for _, r := range scoped {
		if feeIsPast(at, r) {
			continue
		}
		if r.Currency != in.Currency {
			return FeeSelection{}, fmt.Errorf("%w: rule %s is %s, ticket type is %s",
				ErrFeeRuleCurrencyMismatch, r.ID, r.Currency, in.Currency)
		}
	}

	// Steps 2 and 3 — window, then channel eligibility.
	//
	// A rule ineligible by WINDOW is reported as a loser: "why is this fee not
	// applying" is answered by seeing its window. A rule ineligible by CHANNEL
	// is dropped entirely and never appears, because it was never competing for
	// this request (ADR-046 § Provenance): returning every other channel's rules
	// would publish the whole channel fee matrix to every caller.
	eligible := make([]FeeRule, 0, len(scoped))
	windowLosers := map[string][]LosingFeeRule{}
	for _, r := range scoped {
		if !feeChannelEligible(r, in.Channel) {
			continue
		}
		switch {
		case feeIsPast(at, r):
			windowLosers[r.FeeCode] = append(windowLosers[r.FeeCode],
				LosingFeeRule{Rule: r, Reason: ReasonOutsideWindowPast})
		case r.EffectiveFrom != nil && at.Before(*r.EffectiveFrom):
			windowLosers[r.FeeCode] = append(windowLosers[r.FeeCode],
				LosingFeeRule{Rule: r, Reason: ReasonOutsideWindowFuture})
		default:
			eligible = append(eligible, r)
		}
	}

	// Determinism, AFTER the channel filter (TKT-306, aligning this resolver with
	// the price resolver's TKT-237 narrowing).
	//
	// The guard protects the DETERMINISM OF THE ANSWER: the last tie-break is the
	// id, so two rules sharing one are inseparable and the winner would depend on
	// input order. A rule ineligible for the requested channel is dropped above and
	// never ranks, so it cannot affect the answer — erroring on it would refuse a
	// resolution that has exactly one correct result. Eligible duplicates still
	// error, which is the case the guard was written for.
	//
	// This ran BEFORE the channel filter until TKT-306, which is the same defect
	// TKT-237 had already fixed in pricing.go. The two resolvers are duplicated on
	// purpose (ADR-046 §7) and the duplication is only honest while the copies say
	// the same thing.
	//
	// It covers window losers as well as winners: a windowed-out rule is still
	// REPORTED in provenance, and two rules under one id there is the same
	// order-dependence wearing different clothes.
	//
	// Walked over `scoped` in its SORTED order rather than over `eligible` plus
	// `windowLosers`, and that is not a style choice: windowLosers is a map keyed by
	// fee code, so ranging it would make WHICH duplicate gets reported depend on Go's
	// map iteration order — reintroducing, inside the guard, the exact
	// nondeterminism the guard exists to refuse. `scoped` was sorted by id above,
	// so this reports the same id every time.
	//
	// NOT moved past the currency check above, which is deliberately cross-channel
	// for a reason of its own ("a rule misconfigured for another channel is still
	// misconfigured"). Different guard, different question — the two orderings are
	// independent and both are load-bearing.
	seen := make(map[uuid.UUID]struct{}, len(scoped))
	for _, r := range scoped {
		if !feeChannelEligible(r, in.Channel) {
			continue
		}
		if _, isDup := seen[r.ID]; isDup {
			return FeeSelection{}, fmt.Errorf("%w: %s", ErrDuplicateFeeRuleID, r.ID)
		}
		seen[r.ID] = struct{}{}
	}

	// Partition by code. A code is CONSIDERED if any channel-eligible rule
	// carries it, whether or not that rule survived its window — see
	// FeeCodeSelection.Winner for why the window-only case still reports.
	byCode := map[string][]FeeRule{}
	codes := make([]string, 0, len(eligible))
	seenCode := map[string]struct{}{}
	noteCode := func(code string) {
		if _, ok := seenCode[code]; !ok {
			seenCode[code] = struct{}{}
			codes = append(codes, code)
		}
	}
	for _, r := range eligible {
		noteCode(r.FeeCode)
		byCode[r.FeeCode] = append(byCode[r.FeeCode], r)
	}
	for code := range windowLosers {
		noteCode(code)
	}
	sort.Strings(codes)

	for _, code := range codes {
		out.Fees = append(out.Fees, selectOneFeeCode(code, byCode[code], windowLosers[code]))
	}
	return out, nil
}

// selectOneFeeCode runs steps 4-8 inside one code's partition.
func selectOneFeeCode(code string, competitors []FeeRule, windowLosers []LosingFeeRule) FeeCodeSelection {
	if len(competitors) == 0 {
		// Considered, nothing applies. The window losers ARE the answer.
		losers := append([]LosingFeeRule{}, windowLosers...)
		sort.Slice(losers, func(i, j int) bool { return losers[i].Rule.ID.String() < losers[j].Rule.ID.String() })
		return FeeCodeSelection{FeeCode: code, Candidates: losers}
	}
	// Step 4 — the override partition, per code. If any eligible rule for this
	// code is forced, the competition is restricted to forced rules and the
	// order inverts: among house rules that refuse to be undercut, the broadest
	// wins. This is a FILTER, not a tie-break.
	forcedPresent := false
	for _, r := range competitors {
		if r.ForceAncestorOverride {
			forcedPresent = true
			break
		}
	}
	competition := competitors
	if forcedPresent {
		competition = make([]FeeRule, 0, len(competitors))
		for _, r := range competitors {
			if r.ForceAncestorOverride {
				competition = append(competition, r)
			}
		}
	}

	winner := competition[0]
	for _, r := range competition[1:] {
		if feeBeats(r, winner, forcedPresent) {
			winner = r
		}
	}

	losers := append([]LosingFeeRule{}, windowLosers...)
	for _, r := range competitors {
		if r.ID == winner.ID {
			continue
		}
		losers = append(losers, LosingFeeRule{Rule: r, Reason: feeLossReason(r, winner)})
	}
	sort.Slice(losers, func(i, j int) bool { return losers[i].Rule.ID.String() < losers[j].Rule.ID.String() })
	return FeeCodeSelection{FeeCode: code, Winner: &winner, Candidates: losers}
}

// feeIsPast reports that a rule's window has closed at `at`. Half-open: the
// instant equal to EffectiveUntil is already outside.
func feeIsPast(at time.Time, r FeeRule) bool {
	return r.EffectiveUntil != nil && !at.Before(*r.EffectiveUntil)
}

// feeChannelEligible reports whether a rule can compete for the requested
// channel. A channel-agnostic rule (nil) competes everywhere, including in the
// default/public context; a channel-specific rule competes only on an exact
// string match. `requested == nil` is the default/public context, where no
// channel-specific rule is eligible — omitting the channel is not a wildcard.
func feeChannelEligible(r FeeRule, requested *string) bool {
	if r.ChannelCode == nil {
		return true
	}
	return requested != nil && *r.ChannelCode == *requested
}

// channelSpecificity ranks a rule's channel selectivity narrowest-first, in the
// same direction as scopeRank: 0 is the narrower (exact channel) statement.
func channelSpecificity(r FeeRule) int {
	if r.ChannelCode != nil {
		return 0
	}
	return 1
}

// feeMatchesScope reports whether a rule attaches to one of the derived
// identities at the level it claims. Both halves matter: a rule at the right
// level with a foreign id is not a candidate, and neither is a rule carrying a
// derived id at the wrong level.
//
// Duplicated from matchesScope rather than shared — see the file header.
func feeMatchesScope(r FeeRule, s PricingScopes) bool {
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

// feeBeats reports whether challenger should displace best within one code.
//
// Under a forced partition BOTH specificity axes invert — scope level and
// channel. A forced rule is a house floor that refuses to be undercut, so the
// BROADEST such statement is the one that binds, and a channel-agnostic rule is
// the broader statement. Left undefined this case would fall through to priority
// and be decided by accident, on a money path (TKT-214 plan-review A4).
func feeBeats(challenger, best FeeRule, inverted bool) bool {
	cr, br := scopeRank[challenger.ScopeLevel], scopeRank[best.ScopeLevel]
	if cr != br {
		if inverted {
			return cr > br
		}
		return cr < br
	}
	cc, bc := channelSpecificity(challenger), channelSpecificity(best)
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

// feeLossReason maps a loser to its closed-enum reason. The forced-related
// reasons partition exactly as pricing's do; the channel reason slots in between
// scope and priority, mirroring feeBeats.
func feeLossReason(loser, winner FeeRule) string {
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
	if channelSpecificity(loser) != channelSpecificity(winner) {
		return ReasonLessChannelSpecific
	}
	if loser.Priority != winner.Priority {
		return ReasonLowerPriority
	}
	return ReasonStableIDTiebreak
}
