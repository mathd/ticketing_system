package store

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The truth table for TKT-214's pure fee resolver. No database, no clock — the
// seam ADR-046 picked, exercised directly.
//
// FIXTURE FLOOR, stated because a fixture too small to distinguish the states it
// claims to test is this repo's recurring failure mode: every precedence case
// carries at least TWO competing rules with the same fee_code and
// distinguishable identities, and the multi-code cases carry at least two
// distinct codes. A single rule per code cannot tell additive multi-code
// resolution apart from accidental single-rule behaviour.

var feeScopes = PricingScopes{
	TicketTypeID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	SlotID:       uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	SeriesID:     ptr(uuid.MustParse("33333333-3333-3333-3333-333333333333")),
	EventID:      uuid.MustParse("44444444-4444-4444-4444-444444444444"),
	VenueID:      uuid.MustParse("55555555-5555-5555-5555-555555555555"),
}

func ptr[T any](v T) *T { return &v }

// feeAt is the evaluation instant every window case is written against.
var feeAt = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

// ruleID builds ids whose STRING ORDER is the digit order, so the stable-id
// tie-break is legible in the table rather than accidental.
func ruleID(n int) uuid.UUID {
	return uuid.MustParse("aaaaaaaa-0000-0000-0000-00000000000" + string(rune('0'+n)))
}

// fee builds a per-ticket fixed rule at a scope. Everything a case does not care
// about is held constant so the varying field is the only explanation for the
// outcome.
func fee(n int, level ScopeLevel, code string) FeeRule {
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
	return FeeRule{
		ID: ruleID(n), ScopeLevel: level, ScopeID: scopeID, FeeCode: code,
		Basis: BasisPerTicketFixed, Amount: ptr(int64(100 * n)), Currency: "EUR",
		Incidence: IncidencePassedOn,
	}
}

func feeWithChannel(r FeeRule, c string) FeeRule  { r.ChannelCode = ptr(c); return r }
func feeWithPriority(r FeeRule, p int32) FeeRule  { r.Priority = p; return r }
func feeForced(r FeeRule) FeeRule                 { r.ForceAncestorOverride = true; return r }
func feeWithCurrency(r FeeRule, c string) FeeRule { r.Currency = c; return r }
func feeWithWindow(r FeeRule, from, until *time.Time) FeeRule {
	r.EffectiveFrom, r.EffectiveUntil = from, until
	return r
}

func resolve(t *testing.T, channel *string, rules ...FeeRule) FeeSelection {
	t.Helper()
	sel, err := SelectFeeRules(feeAt, FeeCandidates{
		Currency: "EUR", Scopes: feeScopes, Channel: channel, Rules: rules,
	})
	if err != nil {
		t.Fatalf("unexpected resolution error: %v", err)
	}
	return sel
}

// winnerOf returns the winning rule id for a code, or uuid.Nil when the code
// resolved with no winner, and reports whether the code appears at all.
func winnerOf(sel FeeSelection, code string) (uuid.UUID, bool) {
	for _, f := range sel.Fees {
		if f.FeeCode != code {
			continue
		}
		if f.Winner == nil {
			return uuid.Nil, true
		}
		return f.Winner.ID, true
	}
	return uuid.Nil, false
}

func reasonFor(sel FeeSelection, code string, id uuid.UUID) string {
	for _, f := range sel.Fees {
		if f.FeeCode != code {
			continue
		}
		for _, c := range f.Candidates {
			if c.Rule.ID == id {
				return c.Reason
			}
		}
	}
	return ""
}

// TestSelectFeeRulesTruthTable is ADR-046's comparator, case by case. Each row
// names the winner AND the loser's reason, because a right winner reached by the
// wrong path is a bug that only shows up on the next rule someone authors.
func TestSelectFeeRulesTruthTable(t *testing.T) {
	past := feeAt.Add(-2 * time.Hour)
	future := feeAt.Add(2 * time.Hour)

	for name, tc := range map[string]struct {
		channel    *string
		rules      []FeeRule
		wantWinner uuid.UUID
		wantLoser  uuid.UUID
		wantReason string
	}{
		"narrower scope beats broader": {
			rules:      []FeeRule{fee(1, ScopeVenue, "service"), fee(2, ScopeTicketType, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLessSpecific,
		},
		"slot beats series": {
			rules:      []FeeRule{fee(1, ScopeSeries, "service"), fee(2, ScopeSlot, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLessSpecific,
		},
		"event beats venue (policy, not containment)": {
			rules:      []FeeRule{fee(1, ScopeVenue, "service"), fee(2, ScopeEvent, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLessSpecific,
		},
		"a forced broader rule beats a narrower unforced one": {
			rules:      []FeeRule{feeForced(fee(1, ScopeVenue, "service")), fee(2, ScopeTicketType, "service")},
			wantWinner: ruleID(1), wantLoser: ruleID(2), wantReason: ReasonForcedBroaderScope,
		},
		"among forced rules the broadest binds": {
			rules:      []FeeRule{feeForced(fee(1, ScopeVenue, "service")), feeForced(fee(2, ScopeTicketType, "service"))},
			wantWinner: ruleID(1), wantLoser: ruleID(2), wantReason: ReasonLowerForcedScope,
		},
		"priority breaks a same-level tie": {
			rules:      []FeeRule{fee(1, ScopeEvent, "service"), feeWithPriority(fee(2, ScopeEvent, "service"), 5)},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLowerPriority,
		},
		"the lowest id breaks a full tie": {
			rules:      []FeeRule{fee(2, ScopeEvent, "service"), fee(1, ScopeEvent, "service")},
			wantWinner: ruleID(1), wantLoser: ruleID(2), wantReason: ReasonStableIDTiebreak,
		},
		"an exact channel beats channel-agnostic at the same level": {
			channel:    ptr("reseller"),
			rules:      []FeeRule{fee(1, ScopeEvent, "service"), feeWithChannel(fee(2, ScopeEvent, "service"), "reseller")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLessChannelSpecific,
		},
		// The ordering claim: channel specificity is decided BEFORE priority, so
		// a channel rule wins even when the agnostic one is louder. Swap the two
		// steps and this row flips.
		"an exact channel beats channel-agnostic even at lower priority": {
			channel: ptr("reseller"),
			rules: []FeeRule{feeWithPriority(fee(1, ScopeEvent, "service"), 99),
				feeWithChannel(fee(2, ScopeEvent, "service"), "reseller")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLessChannelSpecific,
		},
		// Scope still outranks channel: a broader exact-channel rule must NOT
		// beat a narrower agnostic one.
		"a narrower channel-agnostic rule beats a broader exact-channel one": {
			channel:    ptr("reseller"),
			rules:      []FeeRule{feeWithChannel(fee(1, ScopeVenue, "service"), "reseller"), fee(2, ScopeTicketType, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonLessSpecific,
		},
		// A4: under the forced partition BOTH specificity axes invert, so the
		// broader (channel-agnostic) house floor binds. Left undefined this
		// would fall through to priority and be decided by accident.
		"under a forced partition the channel-agnostic house rule binds": {
			channel: ptr("reseller"),
			rules: []FeeRule{feeForced(fee(1, ScopeVenue, "service")),
				feeForced(feeWithChannel(fee(2, ScopeVenue, "service"), "reseller"))},
			wantWinner: ruleID(1), wantLoser: ruleID(2), wantReason: ReasonLessChannelSpecific,
		},
		"a rule for another channel never competes": {
			channel:    ptr("reseller"),
			rules:      []FeeRule{feeWithChannel(fee(1, ScopeTicketType, "service"), "presale"), fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(2),
		},
		"no requested channel admits only channel-agnostic rules": {
			rules:      []FeeRule{feeWithChannel(fee(1, ScopeTicketType, "service"), "reseller"), fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(2),
		},
		"a rule whose window has closed loses to a live one": {
			rules: []FeeRule{feeWithWindow(fee(1, ScopeTicketType, "service"), nil, &past),
				fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonOutsideWindowPast,
		},
		"a rule whose window has not opened loses to a live one": {
			rules: []FeeRule{feeWithWindow(fee(1, ScopeTicketType, "service"), &future, nil),
				fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonOutsideWindowFuture,
		},
		// Half-open [from, until): eligible AT from, not eligible AT until.
		"a rule is eligible at the instant its window opens": {
			rules:      []FeeRule{feeWithWindow(fee(1, ScopeTicketType, "service"), &feeAt, nil), fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(1), wantLoser: ruleID(2), wantReason: ReasonLessSpecific,
		},
		"a rule is not eligible at the instant its window closes": {
			rules:      []FeeRule{feeWithWindow(fee(1, ScopeTicketType, "service"), nil, &feeAt), fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(2), wantLoser: ruleID(1), wantReason: ReasonOutsideWindowPast,
		},
		// Scope-pair defence in depth: a rule at the wrong LEVEL carrying a
		// derived id is not a candidate, even though its scope_id matches.
		"a derived id at the wrong level is not a candidate": {
			rules: []FeeRule{
				FeeRule{ID: ruleID(1), ScopeLevel: ScopeVenue, ScopeID: feeScopes.EventID, FeeCode: "service",
					Basis: BasisPerTicketFixed, Amount: ptr(int64(100)), Currency: "EUR", Incidence: IncidencePassedOn},
				fee(2, ScopeVenue, "service")},
			wantWinner: ruleID(2),
		},
	} {
		t.Run(name, func(t *testing.T) {
			sel := resolve(t, tc.channel, tc.rules...)
			got, ok := winnerOf(sel, "service")
			if !ok {
				t.Fatalf("no resolution for fee code \"service\": %+v", sel.Fees)
			}
			if got != tc.wantWinner {
				t.Errorf("winner = %s, want %s", got, tc.wantWinner)
			}
			if tc.wantLoser != uuid.Nil {
				if r := reasonFor(sel, "service", tc.wantLoser); r != tc.wantReason {
					t.Errorf("loser %s reason = %q, want %q", tc.wantLoser, r, tc.wantReason)
				}
			}
		})
	}
}

// Fees are ADDITIVE across codes and never stack within one. This is the
// property that makes fee resolution structurally different from price
// resolution, so it is asserted on a fixture that can actually distinguish the
// two: two rules share a code, a third carries another.
func TestSelectFeeRulesResolvesOneWinnerPerCode(t *testing.T) {
	sel := resolve(t, nil,
		fee(1, ScopeVenue, "service"),
		fee(2, ScopeTicketType, "service"),
		fee(3, ScopeEvent, "facility"),
	)
	if len(sel.Fees) != 2 {
		t.Fatalf("want 2 fee codes, got %d: %+v", len(sel.Fees), sel.Fees)
	}
	if sel.Fees[0].FeeCode != "facility" || sel.Fees[1].FeeCode != "service" {
		t.Errorf("codes must be ordered for a stable document, got %s then %s",
			sel.Fees[0].FeeCode, sel.Fees[1].FeeCode)
	}
	if w, _ := winnerOf(sel, "service"); w != ruleID(2) {
		t.Errorf("service winner = %s, want the ticket-type rule %s", w, ruleID(2))
	}
	if w, _ := winnerOf(sel, "facility"); w != ruleID(3) {
		t.Errorf("facility winner = %s, want %s", w, ruleID(3))
	}
	// The losing service rule must be attributed to ITS code, not pooled.
	if n := len(sel.Fees[0].Candidates); n != 0 {
		t.Errorf("facility has no competitors, got %d candidate(s)", n)
	}
	if n := len(sel.Fees[1].Candidates); n != 1 {
		t.Errorf("service has exactly one loser, got %d", n)
	}
}

// A code every rule of which is outside its window is CONSIDERED and resolves to
// no winner — with the losers attached, because "why is the booking fee not
// showing up" is the only question that gets asked about it.
func TestSelectFeeRulesReportsAConsideredCodeWithNoLiveRule(t *testing.T) {
	past := feeAt.Add(-time.Hour)
	sel := resolve(t, nil,
		feeWithWindow(fee(1, ScopeVenue, "booking"), nil, &past),
		fee(2, ScopeEvent, "service"),
	)
	if len(sel.Fees) != 2 {
		t.Fatalf("an expired code must still be reported, got %+v", sel.Fees)
	}
	booking := sel.Fees[0]
	if booking.FeeCode != "booking" {
		t.Fatalf("want the booking code first, got %s", booking.FeeCode)
	}
	if booking.Winner != nil {
		t.Errorf("an expired-only code must have no winner, got %s", booking.Winner.ID)
	}
	if len(booking.Candidates) != 1 || booking.Candidates[0].Reason != ReasonOutsideWindowPast {
		t.Errorf("the expired rule must be reported as the reason, got %+v", booking.Candidates)
	}
}

// A code no rule carries emits NOTHING. Considered-with-no-winner and
// not-present are different states and must not collapse into one.
func TestSelectFeeRulesOmitsCodesNobodyAuthored(t *testing.T) {
	sel := resolve(t, nil, fee(1, ScopeEvent, "service"))
	if len(sel.Fees) != 1 {
		t.Fatalf("want only the authored code, got %+v", sel.Fees)
	}
	if _, ok := winnerOf(sel, "facility"); ok {
		t.Error("an unauthored code must not appear in the resolution")
	}
}

// No rules at all is an empty SET, never nil: the contract declares an array and
// "no fees" must serialize as [].
func TestSelectFeeRulesWithNoRulesIsAnEmptySet(t *testing.T) {
	sel := resolve(t, nil)
	if sel.Fees == nil {
		t.Fatal("Fees must be an empty slice, not nil — null is a different document")
	}
	if len(sel.Fees) != 0 {
		t.Errorf("want no fees, got %+v", sel.Fees)
	}
}

// Every output is order-independent, not just the winner. Shuffling the input
// must not move a winner or a reason.
func TestSelectFeeRulesIsInputOrderIndependent(t *testing.T) {
	rules := []FeeRule{
		fee(1, ScopeVenue, "service"),
		feeWithPriority(fee(2, ScopeEvent, "service"), 3),
		fee(3, ScopeEvent, "service"),
		fee(4, ScopeSlot, "facility"),
	}
	want := resolve(t, nil, rules...)
	for i := range rules {
		shuffled := append([]FeeRule{}, rules[i:]...)
		shuffled = append(shuffled, rules[:i]...)
		got := resolve(t, nil, shuffled...)
		if len(got.Fees) != len(want.Fees) {
			t.Fatalf("rotation %d changed the code count", i)
		}
		for j := range got.Fees {
			if got.Fees[j].FeeCode != want.Fees[j].FeeCode {
				t.Errorf("rotation %d reordered codes", i)
			}
			if got.Fees[j].Winner.ID != want.Fees[j].Winner.ID {
				t.Errorf("rotation %d changed the winner of %s", i, got.Fees[j].FeeCode)
			}
			for k := range got.Fees[j].Candidates {
				if got.Fees[j].Candidates[k] != want.Fees[j].Candidates[k] {
					t.Errorf("rotation %d changed candidate %d of %s", i, k, got.Fees[j].FeeCode)
				}
			}
		}
	}
}

// Currency is invalid CONFIGURATION, not a rule that does not apply — the
// distinction ADR-036 §2 insists on, inherited here. The three rows are the
// three dispositions, and the middle one is the subtle one.
func TestSelectFeeRulesCurrencyMismatch(t *testing.T) {
	past := feeAt.Add(-time.Hour)
	future := feeAt.Add(time.Hour)

	t.Run("a live mismatched rule fails the resolution", func(t *testing.T) {
		_, err := SelectFeeRules(feeAt, FeeCandidates{Currency: "EUR", Scopes: feeScopes,
			Rules: []FeeRule{feeWithCurrency(fee(1, ScopeVenue, "service"), "USD")}})
		if !errors.Is(err, ErrFeeRuleCurrencyMismatch) {
			t.Errorf("want ErrFeeRuleCurrencyMismatch, got %v", err)
		}
	})
	// A future rule is checked NOW so the misconfiguration is found before its
	// window opens and it starts charging in the wrong currency.
	t.Run("a future mismatched rule fails the resolution now", func(t *testing.T) {
		_, err := SelectFeeRules(feeAt, FeeCandidates{Currency: "EUR", Scopes: feeScopes,
			Rules: []FeeRule{feeWithWindow(feeWithCurrency(fee(1, ScopeVenue, "service"), "USD"), &future, nil)}})
		if !errors.Is(err, ErrFeeRuleCurrencyMismatch) {
			t.Errorf("a misconfigured FUTURE rule must fail now, not when its window opens: %v", err)
		}
	})
	// A closed rule can never charge anything again, so failing on its account
	// would be permanent and unrecoverable — currency is immutable and
	// effective_until only shortens, so no write could rescue it.
	t.Run("a closed mismatched rule is inert", func(t *testing.T) {
		sel, err := SelectFeeRules(feeAt, FeeCandidates{Currency: "EUR", Scopes: feeScopes,
			Rules: []FeeRule{
				feeWithWindow(feeWithCurrency(fee(1, ScopeVenue, "service"), "USD"), nil, &past),
				fee(2, ScopeEvent, "service")}})
		if err != nil {
			t.Fatalf("a dead row must not be an outage: %v", err)
		}
		if w, _ := winnerOf(sel, "service"); w != ruleID(2) {
			t.Errorf("winner = %s, want %s", w, ruleID(2))
		}
	})
	// Channel is NOT part of the guard: a rule misconfigured for another channel
	// is still misconfigured, and it will charge the moment a sale arrives
	// there. Failing now is what makes it findable then.
	t.Run("a mismatched rule for another channel still fails", func(t *testing.T) {
		_, err := SelectFeeRules(feeAt, FeeCandidates{Currency: "EUR", Scopes: feeScopes, Channel: ptr("web"),
			Rules: []FeeRule{feeWithChannel(feeWithCurrency(fee(1, ScopeVenue, "service"), "USD"), "reseller")}})
		if !errors.Is(err, ErrFeeRuleCurrencyMismatch) {
			t.Errorf("a rule misconfigured for another channel must still fail: %v", err)
		}
	})
}

// The determinism claim has one input that breaks it, and the pure seam refuses
// rather than pretending. Unreachable through Postgres — id is the primary key.
func TestSelectFeeRulesRefusesDuplicateIDs(t *testing.T) {
	a := fee(1, ScopeEvent, "service")
	b := fee(1, ScopeVenue, "service")
	if _, err := SelectFeeRules(feeAt, FeeCandidates{Currency: "EUR", Scopes: feeScopes,
		Rules: []FeeRule{a, b}}); !errors.Is(err, ErrDuplicateFeeRuleID) {
		t.Errorf("want ErrDuplicateFeeRuleID, got %v", err)
	}
}

// A rule for another channel is not merely a loser — it is absent. Returning it
// would publish one caller's channel fee matrix to every other caller.
func TestSelectFeeRulesHidesOtherChannelsEntirely(t *testing.T) {
	sel := resolve(t, ptr("web"),
		feeWithChannel(fee(1, ScopeEvent, "service"), "reseller"),
		fee(2, ScopeVenue, "service"))
	for _, f := range sel.Fees {
		for _, c := range f.Candidates {
			if c.Rule.ID == ruleID(1) {
				t.Errorf("another channel's rule leaked into provenance as %q", c.Reason)
			}
		}
	}
	if w, _ := winnerOf(sel, "service"); w != ruleID(2) {
		t.Errorf("winner = %s, want %s", w, ruleID(2))
	}
}

// Every rule the comparator can produce has a reason, and the enum is closed:
// an unmapped path would surface as an empty string on a money document.
func TestFeeLossReasonsAreClosed(t *testing.T) {
	known := map[string]bool{
		ReasonLessSpecific: true, ReasonForcedBroaderScope: true, ReasonExcludedByForcedRule: true,
		ReasonLowerForcedScope: true, ReasonLowerPriority: true, ReasonStableIDTiebreak: true,
		ReasonOutsideWindowPast: true, ReasonOutsideWindowFuture: true, ReasonLessChannelSpecific: true,
	}
	past := feeAt.Add(-time.Hour)
	sel := resolve(t, ptr("reseller"),
		feeForced(fee(1, ScopeVenue, "service")),
		fee(2, ScopeTicketType, "service"),
		feeWithChannel(fee(3, ScopeVenue, "service"), "reseller"),
		feeWithWindow(fee(4, ScopeEvent, "service"), nil, &past),
		feeWithPriority(fee(5, ScopeVenue, "service"), 9),
	)
	var seen []string
	for _, f := range sel.Fees {
		for _, c := range f.Candidates {
			if !known[c.Reason] {
				t.Errorf("rule %s lost with an unmapped reason %q", c.Rule.ID, c.Reason)
			}
			seen = append(seen, c.Reason)
		}
	}
	sort.Strings(seen)
	if len(seen) != 4 {
		t.Errorf("want four losers, got %d (%v)", len(seen), seen)
	}
}
