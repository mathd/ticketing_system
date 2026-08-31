package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The split comparator's truth table. Same axes as the fee comparator, so the
// rows that matter are the ones where a split could plausibly differ: the
// unsplit STATES, which fees do not have.

func sched(n int, level ScopeLevel, code string) SplitSchedule {
	var scopeID uuid.UUID
	switch level {
	case ScopeTicketType:
		scopeID = feeScopes.TicketTypeID
	case ScopeSlot:
		scopeID = feeScopes.SlotID
	case ScopeSeries:
		scopeID = *feeScopes.SeriesID
	case ScopeEvent:
		scopeID = feeScopes.EventID
	case ScopeVenue:
		scopeID = feeScopes.VenueID
	}
	return SplitSchedule{ID: ruleID(n), ScopeLevel: level, ScopeID: scopeID, FeeCode: code,
		Parts: []SplitPart{{Payee: Payee{ID: uuid.New()}, ShareBps: 10000}}}
}

func TestSelectSplitScheduleTruthTable(t *testing.T) {
	past := feeAt.Add(-time.Hour)
	future := feeAt.Add(time.Hour)
	reseller := "reseller"

	for name, tc := range map[string]struct {
		channel    *string
		schedules  []SplitSchedule
		wantMode   SplitMode
		wantReason string
		wantWinner uuid.UUID
		wantLoser  uuid.UUID
		loserWhy   string
	}{
		"no schedule at all": {
			wantMode: SplitModeUnsplit, wantReason: SplitReasonNoSchedule,
		},
		"a schedule for another fee code is not this code's": {
			schedules: []SplitSchedule{sched(1, ScopeVenue, "facility")},
			wantMode:  SplitModeUnsplit, wantReason: SplitReasonNoSchedule,
		},
		"narrower scope wins": {
			schedules:  []SplitSchedule{sched(1, ScopeVenue, "service"), sched(2, ScopeTicketType, "service")},
			wantMode:   SplitModeSplit,
			wantWinner: ruleID(2), wantLoser: ruleID(1), loserWhy: ReasonLessSpecific,
		},
		"a forced broader schedule binds": {
			schedules:  []SplitSchedule{forcedSched(sched(1, ScopeVenue, "service")), sched(2, ScopeTicketType, "service")},
			wantMode:   SplitModeSplit,
			wantWinner: ruleID(1), wantLoser: ruleID(2), loserWhy: ReasonForcedBroaderScope,
		},
		// The row the forced PARTITION needs to be distinguishable at all: a
		// forced NARROWER schedule against an unforced broader one. With the
		// partition, the forced schedule wins because the others are excluded;
		// without it, the inverted scope order hands the win to the broader
		// unforced schedule instead. The reverse arrangement (forced broader vs
		// unforced narrower) gives the SAME winner either way, so a fixture
		// carrying only that one cannot tell a filter from a tie-break — found
		// by mutating the filter away and watching the suite stay green.
		"a forced narrower schedule excludes an unforced broader one": {
			schedules:  []SplitSchedule{forcedSched(sched(1, ScopeTicketType, "service")), sched(2, ScopeVenue, "service")},
			wantMode:   SplitModeSplit,
			wantWinner: ruleID(1), wantLoser: ruleID(2), loserWhy: ReasonExcludedByForcedRule,
		},
		"an exact channel beats channel-agnostic at one level": {
			channel:    &reseller,
			schedules:  []SplitSchedule{sched(1, ScopeEvent, "service"), channelSched(sched(2, ScopeEvent, "service"), "reseller")},
			wantMode:   SplitModeSplit,
			wantWinner: ruleID(2), wantLoser: ruleID(1), loserWhy: ReasonLessChannelSpecific,
		},
		"another channel's schedule never competes and is never reported": {
			channel:   &reseller,
			schedules: []SplitSchedule{channelSched(sched(1, ScopeTicketType, "service"), "presale"), sched(2, ScopeVenue, "service")},
			wantMode:  SplitModeSplit, wantWinner: ruleID(2),
		},
		// The state fees do not have: considered, and nothing applies.
		"every schedule outside its window is UNSPLIT with the losers kept": {
			schedules: []SplitSchedule{windowSched(sched(1, ScopeVenue, "service"), nil, &past)},
			wantMode:  SplitModeUnsplit, wantReason: SplitReasonOutsideWindow,
			wantLoser: ruleID(1), loserWhy: ReasonOutsideWindowPast,
		},
		"a future-only schedule is also unsplit": {
			schedules: []SplitSchedule{windowSched(sched(1, ScopeVenue, "service"), &future, nil)},
			wantMode:  SplitModeUnsplit, wantReason: SplitReasonOutsideWindow,
			wantLoser: ruleID(1), loserWhy: ReasonOutsideWindowFuture,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := SelectSplitSchedule(feeAt, "service", tc.channel, feeScopes, tc.schedules)
			if got.Mode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantWinner != uuid.Nil {
				if got.Winner == nil || got.Winner.ID != tc.wantWinner {
					t.Fatalf("winner = %+v, want %s", got.Winner, tc.wantWinner)
				}
			}
			if tc.wantMode == SplitModeUnsplit && got.Winner != nil {
				t.Errorf("an unsplit code must have no winner, got %s", got.Winner.ID)
			}
			if tc.wantLoser != uuid.Nil {
				found := ""
				for _, c := range got.Candidates {
					if c.Schedule.ID == tc.wantLoser {
						found = c.Reason
					}
				}
				if found != tc.loserWhy {
					t.Errorf("loser %s reason = %q, want %q", tc.wantLoser, found, tc.loserWhy)
				}
			}
			for _, c := range got.Candidates {
				if c.Schedule.ChannelCode != nil && (tc.channel == nil || *c.Schedule.ChannelCode != *tc.channel) {
					t.Errorf("another channel's schedule leaked into provenance: %s", c.Schedule.ID)
				}
			}
		})
	}
}

// "No schedule" and "considered, nothing applies" must not collapse: only the
// second is evidence that somebody authored something and it stopped applying.
func TestSplitUnsplitReasonsAreDistinct(t *testing.T) {
	past := feeAt.Add(-time.Hour)
	none := SelectSplitSchedule(feeAt, "service", nil, feeScopes, nil)
	expired := SelectSplitSchedule(feeAt, "service", nil, feeScopes,
		[]SplitSchedule{windowSched(sched(1, ScopeVenue, "service"), nil, &past)})
	if none.Reason == expired.Reason {
		t.Fatalf("both unsplit states report %q — an operator cannot tell a missing schedule "+
			"from an expired one, which are different problems", none.Reason)
	}
	if len(none.Candidates) != 0 {
		t.Error("a code nobody authored has no candidates to report")
	}
	if len(expired.Candidates) != 1 {
		t.Error("an expired schedule IS the answer to 'why is this fee not split'; it must ship")
	}
}

func forcedSched(s SplitSchedule) SplitSchedule { s.ForceAncestorOverride = true; return s }
func channelSched(s SplitSchedule, c string) SplitSchedule {
	s.ChannelCode = &c
	return s
}
func windowSched(s SplitSchedule, from, until *time.Time) SplitSchedule {
	s.EffectiveFrom, s.EffectiveUntil = from, until
	return s
}

// TKT-306: the split comparator does NOT refuse duplicate ids, and the consequence is
// pinned rather than described.
//
// Its two siblings do refuse them — ErrDuplicatePriceRuleID, ErrDuplicateFeeRuleID —
// because the last tie-break is the id, so two rules sharing one are inseparable and the
// winner depends on input order. This resolver returns a bare SplitSelection with no
// error, so it carries no such guard.
//
// ADR-046 §7's TKT-306 amendment records that as DEFERRED, not impossible: the caller
// could validate the loaded set before its fee loop, SplitSelection could carry an
// invalid state, or the load path could reject duplicates. Calling it structural would
// suppress the revisit trigger on a money-allocation path, which is exactly backwards.
//
// This test asserts the gap the way ADR-021's rollback-gap test does. **If it goes RED,
// a guard has been added — update it to assert the refusal, do not delete it.** A deleted
// gap test is indistinguishable from a gap nobody noticed.
func TestSplitScheduleDuplicateIDsAreOrderDependent_Gap(t *testing.T) {
	// Tied on EVERY ranking axis — same id, scope level, scope, priority, window — so
	// nothing but input order separates them. Different payees make the winner
	// observable; a fixture varying any ranked field would be decided by that field
	// and would prove nothing about order.
	withPayee := func(payee uuid.UUID) SplitSchedule {
		s := sched(1, ScopeEvent, "service")
		s.Parts = []SplitPart{{Payee: Payee{ID: payee}, ShareBps: 10000}}
		return s
	}
	first := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	second := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	a, b := withPayee(first), withPayee(second)

	ab := SelectSplitSchedule(feeAt, "service", nil, feeScopes, []SplitSchedule{a, b})
	ba := SelectSplitSchedule(feeAt, "service", nil, feeScopes, []SplitSchedule{b, a})

	if ab.Winner == nil || ba.Winner == nil {
		t.Fatalf("no winner (ab=%v ba=%v) — the fixture stopped reaching the comparator", ab.Winner, ba.Winner)
	}
	gotAB, gotBA := ab.Winner.Parts[0].Payee.ID, ba.Winner.Parts[0].Payee.ID
	if gotAB == gotBA {
		t.Fatalf("both orderings paid %v: the comparator has become order-INDEPENDENT for "+
			"duplicate ids. If a guard was added, assert the refusal here and close the "+
			"ADR-046 §7 item; if the ranking changed, this gap may have closed by accident "+
			"and that is worth understanding before deleting the test", gotAB)
	}
	if gotAB != first || gotBA != second {
		t.Fatalf("orderings paid %v / %v, want %v / %v — the winner is still the first "+
			"submitted, which is what makes this order-dependent", gotAB, gotBA, first, second)
	}

	// And the ambiguity is invisible in provenance: neither duplicate is reported as a
	// loser, so a caller reading Candidates cannot tell this happened.
	if len(ab.Candidates) != 0 {
		t.Errorf("candidates = %d, want 0 — this test's claim is that the duplicate is "+
			"NOT surfaced; if it now is, the gap is narrower than recorded", len(ab.Candidates))
	}
}
