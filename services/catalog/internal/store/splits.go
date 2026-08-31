package store

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// Split-schedule resolution (TKT-216 / ADR-047). Which payees a fee code is
// owed to, resolved through the SAME hierarchy the fee itself resolves through.
//
// This is a third resolver in one package and the duplication is deliberate for
// the second time (ADR-046 §7 set the rule: revisit at a third RULE KIND, not a
// third caller). What is shared here is the shape, not the code: a split
// schedule ranks on the same axes as a fee rule, but it wins a set of PARTS
// rather than a value, and it has no currency, no basis and no incidence.

// SplitMode says whether a fee code has an applicable split at all.
type SplitMode string

const (
	// SplitModeSplit: a schedule won, and Winner names its parts.
	SplitModeSplit SplitMode = "split"
	// SplitModeUnsplit: the code was CONSIDERED and nothing applies. Never an
	// empty part list — "split to nobody" and "no schedule" are different
	// facts, and only one of them is a configuration error.
	SplitModeUnsplit SplitMode = "unsplit"
)

// Why a code resolved unsplit. Closed, like every other reason enum here.
const (
	// SplitReasonNoSchedule: no schedule names this code at any level.
	SplitReasonNoSchedule = "no_schedule"
	// SplitReasonOutsideWindow: schedules exist but none is effective now. The
	// losers are still reported — "why is nobody being paid this fee?" has an
	// answer, and it is those rows.
	SplitReasonOutsideWindow = "outside_window"
)

// SplitResolverVersion is a commitment: TKT-215's snapshot stores this document,
// and TKT-217 settles from the stored copy. Changing what the comparator MEANS
// means bumping this or stored provenance stops being interpretable.
const SplitResolverVersion int32 = 1

// Payee is who money is owed to. Kind is descriptive metadata for reporting —
// it must never be read as a routing rule (ADR-047 §2).
type Payee struct {
	ID                uuid.UUID
	OrganizerID       uuid.UUID
	Kind              string
	DisplayName       string
	ExternalReference *string
}

// SplitPart is one payee's share of a schedule, in basis points.
type SplitPart struct {
	Payee    Payee
	ShareBps int32
}

// SplitSchedule is one authored schedule and its parts. The parts are
// guaranteed to sum to 10000 by a deferred constraint trigger — honest-writer
// consistency, not tamper-evidence (ADR-021).
type SplitSchedule struct {
	ID                    uuid.UUID
	OrganizerID           uuid.UUID
	ScopeLevel            ScopeLevel
	ScopeID               uuid.UUID
	FeeCode               string
	ChannelCode           *string
	Priority              int32
	ForceAncestorOverride bool
	EffectiveFrom         *time.Time
	EffectiveUntil        *time.Time
	Parts                 []SplitPart
}

// LosingSplitSchedule is a schedule that did not win its code, and why. Reasons
// are the fee resolver's, reused verbatim: the ranking axes are the same, so
// inventing a parallel vocabulary would mean two words for one fact.
type LosingSplitSchedule struct {
	Schedule SplitSchedule
	Reason   string
}

// SplitSelection is one fee code's split outcome.
type SplitSelection struct {
	ResolverVersion int32
	Mode            SplitMode
	Reason          string
	Winner          *SplitSchedule
	Candidates      []LosingSplitSchedule
}

// SelectSplitSchedule resolves one fee code's schedule. Pure: no database, no
// clock.
//
// The filter/rank order is the fee resolver's, and it is the same order for the
// same reason — the forced partition is a FILTER applied before ranking, not a
// tie-break after it:
//
//	0. scope-pair match      3. forced partition (filter)
//	1. effective window      4. scope level  (inverted if forced)
//	2. channel eligibility   5. channel specificity (inverted if forced)
//	                         6. priority   7. stable id
func SelectSplitSchedule(at time.Time, code string, channel *string, scopes PricingScopes,
	schedules []SplitSchedule) SplitSelection {

	out := SplitSelection{ResolverVersion: SplitResolverVersion, Mode: SplitModeUnsplit,
		Reason: SplitReasonNoSchedule, Candidates: []LosingSplitSchedule{}}

	scoped := make([]SplitSchedule, 0, len(schedules))
	for _, s := range schedules {
		if s.FeeCode != code {
			continue
		}
		if !splitMatchesScope(s, scopes) {
			continue
		}
		if !splitChannelEligible(s, channel) {
			// Dropped entirely, never reported: returning another channel's
			// schedules would publish the whole channel payout matrix to every
			// caller, and split shares are the most sensitive thing in this
			// epic.
			continue
		}
		scoped = append(scoped, s)
	}
	if len(scoped) == 0 {
		return out
	}

	sort.Slice(scoped, func(i, j int) bool { return scoped[i].ID.String() < scoped[j].ID.String() })

	// NO DUPLICATE-ID GUARD HERE, and that is structural rather than an omission
	// (TKT-306; ADR-046 §7's TKT-306 amendment has the table).
	//
	// The price and fee resolvers refuse two rules sharing an id, because the last
	// tie-break is the id and the winner would otherwise depend on input order. The
	// same is true of this comparator — but this function returns a bare
	// SplitSelection with no error, and its one production caller runs it INSIDE a
	// loop over fees on a money path (fees_postgres.go). Adding the guard means
	// changing the signature and making fee resolution newly failable mid-loop, which
	// is a behaviour change rather than an alignment.
	//
	// What holds the invariant instead: id is the primary key, so duplicates are
	// unreachable through Postgres — the same fact that makes the other two guards
	// pure seams. The sort above is what makes the ANSWER order-independent given
	// distinct ids, and that part is present.
	//
	// If this ever needs the guard, the question is the signature, and ADR-046 §7's
	// revisit trigger (a third rule kind — this one) is already owed a decision.

	eligible := make([]SplitSchedule, 0, len(scoped))
	var windowLosers []LosingSplitSchedule
	for _, s := range scoped {
		switch {
		case splitIsPast(at, s):
			windowLosers = append(windowLosers, LosingSplitSchedule{Schedule: s, Reason: ReasonOutsideWindowPast})
		case s.EffectiveFrom != nil && at.Before(*s.EffectiveFrom):
			windowLosers = append(windowLosers, LosingSplitSchedule{Schedule: s, Reason: ReasonOutsideWindowFuture})
		default:
			eligible = append(eligible, s)
		}
	}
	if len(eligible) == 0 {
		// Considered, nothing applies — and the expired rows ARE the answer to
		// "why is this fee not being split?", so they ship.
		out.Reason = SplitReasonOutsideWindow
		out.Candidates = sortedSplitLosers(windowLosers)
		return out
	}

	forcedPresent := false
	for _, s := range eligible {
		if s.ForceAncestorOverride {
			forcedPresent = true
			break
		}
	}
	competition := eligible
	if forcedPresent {
		competition = make([]SplitSchedule, 0, len(eligible))
		for _, s := range eligible {
			if s.ForceAncestorOverride {
				competition = append(competition, s)
			}
		}
	}

	winner := competition[0]
	for _, s := range competition[1:] {
		if splitBeats(s, winner, forcedPresent) {
			winner = s
		}
	}

	losers := append([]LosingSplitSchedule{}, windowLosers...)
	for _, s := range eligible {
		if s.ID == winner.ID {
			continue
		}
		losers = append(losers, LosingSplitSchedule{Schedule: s, Reason: splitLossReason(s, winner)})
	}
	out.Mode, out.Reason, out.Winner = SplitModeSplit, "", &winner
	out.Candidates = sortedSplitLosers(losers)
	return out
}

func splitIsPast(at time.Time, s SplitSchedule) bool {
	return s.EffectiveUntil != nil && !at.Before(*s.EffectiveUntil)
}

func splitChannelEligible(s SplitSchedule, requested *string) bool {
	if s.ChannelCode == nil {
		return true
	}
	return requested != nil && *s.ChannelCode == *requested
}

func splitChannelSpecificity(s SplitSchedule) int {
	if s.ChannelCode != nil {
		return 0
	}
	return 1
}

func splitMatchesScope(s SplitSchedule, sc PricingScopes) bool {
	switch s.ScopeLevel {
	case ScopeTicketType:
		return s.ScopeID == sc.TicketTypeID
	case ScopeSlot:
		return s.ScopeID == sc.SlotID
	case ScopeSeries:
		return sc.SeriesID != nil && s.ScopeID == *sc.SeriesID
	case ScopeEvent:
		return s.ScopeID == sc.EventID
	case ScopeVenue:
		return s.ScopeID == sc.VenueID
	default:
		return false
	}
}

func splitBeats(challenger, best SplitSchedule, inverted bool) bool {
	cr, br := scopeRank[challenger.ScopeLevel], scopeRank[best.ScopeLevel]
	if cr != br {
		if inverted {
			return cr > br
		}
		return cr < br
	}
	cc, bc := splitChannelSpecificity(challenger), splitChannelSpecificity(best)
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

func splitLossReason(loser, winner SplitSchedule) string {
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
	if splitChannelSpecificity(loser) != splitChannelSpecificity(winner) {
		return ReasonLessChannelSpecific
	}
	if loser.Priority != winner.Priority {
		return ReasonLowerPriority
	}
	return ReasonStableIDTiebreak
}

func sortedSplitLosers(losers []LosingSplitSchedule) []LosingSplitSchedule {
	sort.Slice(losers, func(i, j int) bool {
		return losers[i].Schedule.ID.String() < losers[j].Schedule.ID.String()
	})
	return losers
}
