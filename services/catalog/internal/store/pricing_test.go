package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The TKT-151-owned rows of ADR-036 §4's truth table, as one table-driven test
// over the pure seam. The window rows (§4 step 2) belong to TKT-152 and are
// absent here by design — the ADR assigns every row an owner precisely so the
// two stories cannot both claim window semantics.
//
// Every winner case asserts the FULL loser list with each loser's exact reason,
// not just the winner. ADR-036 §5: a resolver hard-coded to return the
// ticket-type rule satisfies any winner-only assertion, so the losers are what
// makes the comparator's claim falsifiable.
//
// Fixed UUIDs, fixed instant: the ids are load-bearing (the last tie-break is
// lowest id ascending), so they are chosen explicitly rather than generated.

var (
	ttID     = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	slotID   = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	seriesID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	eventID  = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	venueID  = uuid.MustParse("44444444-4444-4444-4444-444444444444")

	// ruleA < ruleB by uuid ascending — the stable_id_tiebreak fixture.
	ruleA = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000000")
	ruleB = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000000")

	evalAt = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	basePrice = Money{Amount: 4550, Currency: "EUR"}
)

func testScopes(withSeries bool) PricingScopes {
	s := PricingScopes{TicketTypeID: ttID, SlotID: slotID, EventID: eventID, VenueID: venueID}
	if withSeries {
		id := seriesID
		s.SeriesID = &id
	}
	return s
}

// rule builds a candidate. priority 0 and forced false unless overridden.
func rule(id uuid.UUID, level ScopeLevel, scopeID uuid.UUID, amount int64) PriceRule {
	return PriceRule{
		ID: id, ScopeLevel: level, ScopeID: scopeID,
		ActionKind: ActionAbsolute, Amount: amount, Currency: "EUR",
	}
}

func forced(r PriceRule) PriceRule { r.ForceAncestorOverride = true; return r }

func withPriority(r PriceRule, p int32) PriceRule { r.Priority = p; return r }

// window sets a half-open [from, until) on a rule. Both bounds are offsets from
// evalAt, so every fixture is relative to the instant under test rather than to
// a calendar literal — a literal is green at merge and rots once the clock
// crosses it (docs/learnings/2026-07-18-time-window-fixtures-must-be-relative.md).
func window(r PriceRule, fromOffset, untilOffset *time.Duration) PriceRule {
	if fromOffset != nil {
		t := evalAt.Add(*fromOffset)
		r.EffectiveFrom = &t
	}
	if untilOffset != nil {
		t := evalAt.Add(*untilOffset)
		r.EffectiveUntil = &t
	}
	return r
}

func dur(d time.Duration) *time.Duration { return &d }

type loserWant struct {
	id     uuid.UUID
	reason string
}

func TestSelectPricingRule(t *testing.T) {
	tests := []struct {
		name       string
		withSeries bool
		rules      []PriceRule
		wantWinner *uuid.UUID
		wantAmount int64
		wantLosers []loserWant
		wantErr    bool
	}{
		{
			// The regression everyone actually cares about: existing data, no
			// rules, prices exactly as it does today.
			name:       "no_rules_falls_back_to_base_price",
			withSeries: true,
			rules:      nil,
			wantAmount: 4550,
		},
		{
			name:       "ticket_type_rule_wins",
			withSeries: true,
			rules:      []PriceRule{rule(ruleA, ScopeTicketType, ttID, 3000)},
			wantWinner: &ruleA, wantAmount: 3000,
		},
		{
			name:       "slot_rule_wins",
			withSeries: true,
			rules:      []PriceRule{rule(ruleA, ScopeSlot, slotID, 3100)},
			wantWinner: &ruleA, wantAmount: 3100,
		},
		{
			name:       "series_rule_wins",
			withSeries: true,
			rules:      []PriceRule{rule(ruleA, ScopeSeries, seriesID, 3200)},
			wantWinner: &ruleA, wantAmount: 3200,
		},
		{
			name:       "event_rule_wins",
			withSeries: true,
			rules:      []PriceRule{rule(ruleA, ScopeEvent, eventID, 3300)},
			wantWinner: &ruleA, wantAmount: 3300,
		},
		{
			name:       "venue_rule_wins",
			withSeries: true,
			rules:      []PriceRule{rule(ruleA, ScopeVenue, venueID, 3400)},
			wantWinner: &ruleA, wantAmount: 3400,
		},
		{
			name:       "narrowest_of_several_levels_wins",
			withSeries: true,
			rules: []PriceRule{
				rule(ruleA, ScopeVenue, venueID, 3400),
				rule(ruleB, ScopeSlot, slotID, 3100),
			},
			wantWinner: &ruleB, wantAmount: 3100,
			wantLosers: []loserWant{{ruleA, ReasonLessSpecific}},
		},
		{
			// A slot with no series membership contributes no series candidate;
			// a series rule that exists for some other run must not apply.
			name:       "slot_without_series_has_no_series_candidate",
			withSeries: false,
			rules:      []PriceRule{rule(ruleA, ScopeSeries, seriesID, 3200)},
			wantAmount: 4550,
		},
		{
			// ADR-036 §1: series is NARROWER than event. prd-v1.md:36 says the
			// opposite; the ADR supersedes it. This row is the guard.
			name:       "series_beats_event",
			withSeries: true,
			rules: []PriceRule{
				rule(ruleA, ScopeEvent, eventID, 3300),
				rule(ruleB, ScopeSeries, seriesID, 3200),
			},
			wantWinner: &ruleB, wantAmount: 3200,
			wantLosers: []loserWant{{ruleA, ReasonLessSpecific}},
		},
		{
			// A rule carrying a scope_id that collides with a derived id but at
			// a level that is NOT the one that id belongs to must never be a
			// candidate. The SQL defends this too (ADR-036 §3); the pure seam
			// defends it again, because UUID uniqueness is per table.
			name:       "scope_id_collision_is_not_a_candidate",
			withSeries: true,
			// scope_level 'event' carrying the TICKET TYPE's id
			rules:      []PriceRule{rule(ruleA, ScopeEvent, ttID, 9900)},
			wantAmount: 4550,
		},
		{
			name:       "forced_venue_beats_ordinary_event",
			withSeries: true,
			rules: []PriceRule{
				forced(rule(ruleA, ScopeVenue, venueID, 3400)),
				rule(ruleB, ScopeEvent, eventID, 3300),
			},
			wantWinner: &ruleA, wantAmount: 3400,
			wantLosers: []loserWant{{ruleB, ReasonForcedBroaderScope}},
		},
		{
			// The winner is forced but NARROWER than the loser, so
			// forced_broader_scope does not fit — this is the case that had no
			// reason at all until ADR-036 gained excluded_by_forced_rule.
			name:       "forced_event_beats_ordinary_venue",
			withSeries: true,
			rules: []PriceRule{
				forced(rule(ruleA, ScopeEvent, eventID, 3300)),
				rule(ruleB, ScopeVenue, venueID, 3400),
			},
			wantWinner: &ruleA, wantAmount: 3300,
			wantLosers: []loserWant{{ruleB, ReasonExcludedByForcedRule}},
		},
		{
			name:       "broader_forced_rule_beats_narrower_forced_rule",
			withSeries: true,
			rules: []PriceRule{
				forced(rule(ruleA, ScopeVenue, venueID, 3400)),
				forced(rule(ruleB, ScopeSlot, slotID, 3100)),
			},
			wantWinner: &ruleA, wantAmount: 3400,
			wantLosers: []loserWant{{ruleB, ReasonLowerForcedScope}},
		},
		{
			name:       "higher_priority_wins_at_same_level",
			withSeries: true,
			rules: []PriceRule{
				withPriority(rule(ruleA, ScopeEvent, eventID, 3300), 1),
				withPriority(rule(ruleB, ScopeEvent, eventID, 3350), 5),
			},
			wantWinner: &ruleB, wantAmount: 3350,
			wantLosers: []loserWant{{ruleA, ReasonLowerPriority}},
		},
		{
			// Deliberately semantically uninteresting (ADR-036 §4): its only
			// job is to stop row order or a query plan deciding a price.
			name:       "lowest_uuid_wins_at_same_level_and_priority",
			withSeries: true,
			rules: []PriceRule{
				rule(ruleB, ScopeEvent, eventID, 3350),
				rule(ruleA, ScopeEvent, eventID, 3300),
			},
			wantWinner: &ruleA, wantAmount: 3300,
			wantLosers: []loserWant{{ruleB, ReasonStableIDTiebreak}},
		},
		// ---- TKT-152 rows (ADR-036 §4 step 2) ----
		{
			// The half-open interval's closed end: at == effective_from is IN.
			name:       "effective_from_is_inclusive",
			withSeries: true,
			rules:      []PriceRule{window(rule(ruleA, ScopeEvent, eventID, 3300), dur(0), nil)},
			wantWinner: &ruleA, wantAmount: 3300,
		},
		{
			// The open end: at == effective_until is OUT. This is the boundary
			// an inclusive/exclusive ambiguity turns into a money bug.
			name:       "effective_until_is_exclusive",
			withSeries: true,
			rules:      []PriceRule{window(rule(ruleA, ScopeEvent, eventID, 3300), nil, dur(0))},
			wantAmount: 4550,
			wantLosers: []loserWant{{ruleA, ReasonOutsideWindowPast}},
		},
		{
			name:       "closed_rule_loses_as_outside_window_past",
			withSeries: true,
			rules: []PriceRule{
				window(rule(ruleA, ScopeEvent, eventID, 3300), dur(-48*time.Hour), dur(-24*time.Hour)),
				rule(ruleB, ScopeVenue, venueID, 3400),
			},
			wantWinner: &ruleB, wantAmount: 3400,
			wantLosers: []loserWant{{ruleA, ReasonOutsideWindowPast}},
		},
		{
			name:       "future_rule_loses_as_outside_window_future",
			withSeries: true,
			rules: []PriceRule{
				window(rule(ruleA, ScopeEvent, eventID, 3300), dur(24*time.Hour), nil),
				rule(ruleB, ScopeVenue, venueID, 3400),
			},
			wantWinner: &ruleB, wantAmount: 3400,
			wantLosers: []loserWant{{ruleA, ReasonOutsideWindowFuture}},
		},
		{
			// The case the empty-eligible early return would silently break:
			// the price is right and the provenance is gone. Asserting the
			// losers is what makes this row worth having.
			name:       "all_tiers_expired_falls_back_to_base_price",
			withSeries: true,
			rules: []PriceRule{
				window(rule(ruleA, ScopeEvent, eventID, 3300), dur(-48*time.Hour), dur(-24*time.Hour)),
				window(rule(ruleB, ScopeVenue, venueID, 3400), dur(-72*time.Hour), dur(-1*time.Hour)),
			},
			wantAmount: 4550,
			wantLosers: []loserWant{
				{ruleA, ReasonOutsideWindowPast},
				{ruleB, ReasonOutsideWindowPast},
			},
		},
		{
			// A wrong-currency rule whose window has CLOSED is inert: it can
			// never price anything again, so failing every resolution forever
			// on its account would be an unrecoverable outage (currency is
			// immutable, effective_until only shortens). ADR-036 §4 step 1.
			name:       "closed_currency_mismatch_is_inert",
			withSeries: true,
			rules: []PriceRule{
				window(PriceRule{ID: ruleA, ScopeLevel: ScopeEvent, ScopeID: eventID,
					ActionKind: ActionAbsolute, Amount: 3300, Currency: "USD"},
					dur(-48*time.Hour), dur(-24*time.Hour)),
			},
			wantAmount: 4550,
			wantLosers: []loserWant{{ruleA, ReasonOutsideWindowPast}},
		},
		{
			// ...but one that has not opened yet still HAS a future, so it must
			// fail now rather than reprice mysteriously the moment it opens.
			name:       "future_currency_mismatch_fails_resolution",
			withSeries: true,
			rules: []PriceRule{
				window(PriceRule{ID: ruleA, ScopeLevel: ScopeEvent, ScopeID: eventID,
					ActionKind: ActionAbsolute, Amount: 3300, Currency: "USD"},
					dur(24*time.Hour), nil),
			},
			wantErr: true,
		},
		{
			// Overlap: windows decide ELIGIBILITY, never precedence. Both are
			// live, so the existing comparator settles it on priority.
			name:       "overlapping_windows_use_priority",
			withSeries: true,
			rules: []PriceRule{
				withPriority(window(rule(ruleA, ScopeEvent, eventID, 3300), dur(-24*time.Hour), dur(24*time.Hour)), 1),
				withPriority(window(rule(ruleB, ScopeEvent, eventID, 3350), dur(-1*time.Hour), dur(1*time.Hour)), 5),
			},
			wantWinner: &ruleB, wantAmount: 3350,
			wantLosers: []loserWant{{ruleA, ReasonLowerPriority}},
		},
		{
			// The narrower window does NOT win for being narrower — equal
			// priority falls through to the id tie-break, unchanged.
			name:       "equal_priority_overlapping_windows_use_lowest_uuid",
			withSeries: true,
			rules: []PriceRule{
				window(rule(ruleB, ScopeEvent, eventID, 3350), dur(-1*time.Hour), dur(1*time.Hour)),
				window(rule(ruleA, ScopeEvent, eventID, 3300), dur(-24*time.Hour), dur(24*time.Hour)),
			},
			wantWinner: &ruleA, wantAmount: 3300,
			wantLosers: []loserWant{{ruleB, ReasonStableIDTiebreak}},
		},
		{
			// ADR-036 §2/§4 step 1: a mismatch is invalid configuration and
			// fails loudly. It is never "this rule does not apply".
			name:       "currency_mismatch_fails_resolution",
			withSeries: true,
			rules: []PriceRule{
				{ID: ruleA, ScopeLevel: ScopeEvent, ScopeID: eventID,
					ActionKind: ActionAbsolute, Amount: 3300, Currency: "USD"},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectPricingRule(evalAt, PricingCandidates{
				BasePrice: basePrice,
				Scopes:    testScopes(tc.withSeries),
				Rules:     tc.rules,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got none — a misconfigured rule must not be silently skipped")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.EvaluatedAt != evalAt {
				t.Errorf("EvaluatedAt = %v, want %v", got.EvaluatedAt, evalAt)
			}
			if got.BasePrice != basePrice {
				t.Errorf("BasePrice = %+v, want %+v", got.BasePrice, basePrice)
			}
			if got.ResolvedPrice.Amount != tc.wantAmount {
				t.Errorf("ResolvedPrice.Amount = %d, want %d", got.ResolvedPrice.Amount, tc.wantAmount)
			}
			if got.ResolvedPrice.Currency != "EUR" {
				t.Errorf("ResolvedPrice.Currency = %q, want EUR", got.ResolvedPrice.Currency)
			}
			switch {
			case tc.wantWinner == nil && got.Winner != nil:
				t.Fatalf("Winner = %v, want none", got.Winner.ID)
			case tc.wantWinner == nil:
				if got.FallbackReason == nil || *got.FallbackReason != FallbackNoEligibleRule {
					t.Errorf("FallbackReason = %v, want %q", got.FallbackReason, FallbackNoEligibleRule)
				}
			case got.Winner == nil:
				t.Fatalf("Winner = none, want %v", *tc.wantWinner)
			case got.Winner.ID != *tc.wantWinner:
				t.Fatalf("Winner = %v, want %v", got.Winner.ID, *tc.wantWinner)
			}
			if tc.wantWinner != nil && got.FallbackReason != nil {
				t.Errorf("FallbackReason = %v, want none when a rule won", *got.FallbackReason)
			}
			assertLosers(t, got, tc.wantLosers)
		})
	}
}

// assertLosers checks the loser set exactly: every expected loser present with
// its exact reason, no extras, and the winner never among them (ADR-036 §5
// states candidates excludes the winner, because "candidates" and "the losers"
// pull in opposite directions and two implementations would otherwise differ).
func assertLosers(t *testing.T, got RuleSelection, want []loserWant) {
	t.Helper()
	if len(got.Candidates) != len(want) {
		t.Fatalf("got %d losers, want %d: %+v", len(got.Candidates), len(want), got.Candidates)
	}
	byID := map[uuid.UUID]string{}
	for _, c := range got.Candidates {
		if got.Winner != nil && c.Rule.ID == got.Winner.ID {
			t.Errorf("winner %v also appears in Candidates", c.Rule.ID)
		}
		byID[c.Rule.ID] = c.Reason
	}
	for _, w := range want {
		reason, ok := byID[w.id]
		if !ok {
			t.Errorf("loser %v missing from Candidates", w.id)
			continue
		}
		if reason != w.reason {
			t.Errorf("loser %v reason = %q, want %q", w.id, reason, w.reason)
		}
	}
}

// A slot with no series must not match a rule whose scope_id happens to be the
// zero UUID. The store passes a typed nil for a missing series so the SQL pair
// never matches; this pins the same property in the pure seam, where a
// uuid.Nil sentinel would silently do the wrong thing.
func TestSelectPricingRuleIgnoresZeroUUIDSeriesRule(t *testing.T) {
	got, err := SelectPricingRule(evalAt, PricingCandidates{
		BasePrice: basePrice,
		Scopes:    testScopes(false),
		Rules:     []PriceRule{rule(ruleA, ScopeSeries, uuid.Nil, 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Winner != nil {
		t.Fatalf("Winner = %v, want none — a zero-UUID series rule is not a candidate", got.Winner.ID)
	}
	if got.ResolvedPrice.Amount != basePrice.Amount {
		t.Fatalf("ResolvedPrice = %d, want base %d", got.ResolvedPrice.Amount, basePrice.Amount)
	}
}

// Two rules sharing an id have no separable order — the last tie-break IS the
// id — so the winner would depend on input order and the loser loop would
// suppress both. Postgres cannot produce this (id is the primary key), but the
// pure seam is what TKT-152 and TKT-153 build on, so it refuses the input
// rather than quietly returning one of two different prices.
func TestSelectPricingRuleRejectsDuplicateIDs(t *testing.T) {
	dup := []PriceRule{
		rule(ruleA, ScopeEvent, eventID, 3300),
		rule(ruleA, ScopeEvent, eventID, 9900),
	}
	forward, errF := SelectPricingRule(evalAt, PricingCandidates{
		BasePrice: basePrice, Scopes: testScopes(true), Rules: dup})
	reversed, errR := SelectPricingRule(evalAt, PricingCandidates{
		BasePrice: basePrice, Scopes: testScopes(true), Rules: []PriceRule{dup[1], dup[0]}})
	if !errors.Is(errF, ErrDuplicatePriceRuleID) || !errors.Is(errR, ErrDuplicatePriceRuleID) {
		t.Fatalf("want ErrDuplicatePriceRuleID both ways, got %v / %v (results %d / %d) — "+
			"without the guard these two orderings resolve to different prices",
			errF, errR, forward.ResolvedPrice.Amount, reversed.ResolvedPrice.Amount)
	}
}

// Order independence, stated as a property rather than hoped for: every
// permutation of a candidate set must produce the same winner, the same price
// and the same loser reasons.
func TestSelectPricingRuleIsOrderIndependent(t *testing.T) {
	rules := []PriceRule{
		forced(rule(ruleA, ScopeVenue, venueID, 3400)),
		rule(ruleB, ScopeEvent, eventID, 3300),
		withPriority(rule(uuid.MustParse("cccccccc-0000-0000-0000-000000000000"), ScopeSlot, slotID, 3100), 7),
	}
	want, err := SelectPricingRule(evalAt, PricingCandidates{
		BasePrice: basePrice, Scopes: testScopes(true), Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range [][]int{{0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		shuffled := []PriceRule{rules[perm[0]], rules[perm[1]], rules[perm[2]]}
		got, err := SelectPricingRule(evalAt, PricingCandidates{
			BasePrice: basePrice, Scopes: testScopes(true), Rules: shuffled})
		if err != nil {
			t.Fatalf("permutation %v: %v", perm, err)
		}
		if got.Winner.ID != want.Winner.ID || got.ResolvedPrice != want.ResolvedPrice {
			t.Errorf("permutation %v: winner/price = %v/%+v, want %v/%+v",
				perm, got.Winner.ID, got.ResolvedPrice, want.Winner.ID, want.ResolvedPrice)
		}
		if len(got.Candidates) != len(want.Candidates) {
			t.Fatalf("permutation %v: %d losers, want %d", perm, len(got.Candidates), len(want.Candidates))
		}
		for i := range got.Candidates {
			if got.Candidates[i] != want.Candidates[i] {
				t.Errorf("permutation %v: loser %d = %+v, want %+v",
					perm, i, got.Candidates[i], want.Candidates[i])
			}
		}
	}
}

// The currency failure must name the SAME rule regardless of input order. It is
// not cosmetic: the handler logs that id as the offending rule, so the same
// broken data producing different diagnostics run to run is how an operator
// chases the wrong row.
func TestSelectPricingRuleCurrencyErrorIsOrderIndependent(t *testing.T) {
	bad := []PriceRule{
		{ID: ruleB, ScopeLevel: ScopeVenue, ScopeID: venueID, ActionKind: ActionAbsolute, Amount: 1, Currency: "GBP"},
		{ID: ruleA, ScopeLevel: ScopeEvent, ScopeID: eventID, ActionKind: ActionAbsolute, Amount: 1, Currency: "USD"},
	}
	var messages []string
	for _, rules := range [][]PriceRule{bad, {bad[1], bad[0]}} {
		_, err := SelectPricingRule(evalAt, PricingCandidates{
			BasePrice: basePrice, Scopes: testScopes(true), Rules: rules})
		if !errors.Is(err, ErrPriceRuleCurrencyMismatch) {
			t.Fatalf("want a currency mismatch, got %v", err)
		}
		messages = append(messages, err.Error())
	}
	if messages[0] != messages[1] {
		t.Errorf("the reported rule depends on input order:\n  %s\n  %s", messages[0], messages[1])
	}
	if !strings.Contains(messages[0], ruleA.String()) {
		t.Errorf("want the lowest-id offender %v reported, got %q", ruleA, messages[0])
	}
}

// assertLosers compares by id, so it cannot see ORDER — and both code paths
// promise an id-ascending loser list. Removing sortedCandidates leaves every
// other test green, so without this the promise is unenforced.
//
// Mutation-checked, and the result is worth writing down because it is not
// symmetric:
//
//   - the WINNER path genuinely needs the sort, and its subtest reddens without
//     it. That path concatenates window losers with comparator losers, so the
//     two groups interleave and input order survives into the output.
//   - the FALLBACK path does NOT redden, even with a reversed fixture, because
//     `scoped` is already id-sorted upstream (TKT-151 sorts it so the currency
//     error is order-independent) and window losers are appended in that order.
//     Its subtest pins the PROPERTY — id-ascending output — not the line that
//     currently provides it. That is the right thing to pin: a consumer depends
//     on the order, not on which statement produces it.
func TestSelectPricingRuleOrdersLosersByID(t *testing.T) {
	ruleC := uuid.MustParse("cccccccc-0000-0000-0000-000000000000")

	t.Run("all expired, input reversed", func(t *testing.T) {
		got, err := SelectPricingRule(evalAt, PricingCandidates{
			BasePrice: basePrice, Scopes: testScopes(true),
			Rules: []PriceRule{
				window(rule(ruleB, ScopeVenue, venueID, 3400), dur(-72*time.Hour), dur(-1*time.Hour)),
				window(rule(ruleA, ScopeEvent, eventID, 3300), dur(-48*time.Hour), dur(-24*time.Hour)),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertLoserOrder(t, got, ruleA, ruleB)
	})

	t.Run("winner present, window loser sorts after a comparator loser", func(t *testing.T) {
		// ruleC is the WINDOW loser and ruleB the COMPARATOR loser, so the two
		// groups concatenate as [C, B] — descending — and only the sort turns
		// that into [B, C]. Mutation-checked: removing sortedCandidates reddens
		// exactly this subtest. ruleA (slot, narrowest) wins.
		got, err := SelectPricingRule(evalAt, PricingCandidates{
			BasePrice: basePrice, Scopes: testScopes(true),
			Rules: []PriceRule{
				window(rule(ruleC, ScopeVenue, venueID, 3400), dur(-72*time.Hour), dur(-1*time.Hour)),
				rule(ruleB, ScopeEvent, eventID, 3300),
				rule(ruleA, ScopeSlot, slotID, 3100),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Winner == nil || got.Winner.ID != ruleA {
			t.Fatalf("Winner = %+v, want the slot rule", got.Winner)
		}
		assertLoserOrder(t, got, ruleB, ruleC)
	})
}

func assertLoserOrder(t *testing.T, got RuleSelection, want ...uuid.UUID) {
	t.Helper()
	if len(got.Candidates) != len(want) {
		t.Fatalf("got %d losers, want %d: %+v", len(got.Candidates), len(want), got.Candidates)
	}
	// Enforce the PROPERTY independently of what the caller asked for. Without
	// this the helper only checks equality with `want`, so a caller passing a
	// descending list would get a pass while the failure message still claimed
	// "id-ascending" — the helper would be asserting the caller's opinion
	// rather than the contract.
	for i := 1; i < len(got.Candidates); i++ {
		if got.Candidates[i-1].Rule.ID.String() >= got.Candidates[i].Rule.ID.String() {
			t.Fatalf("losers are not id-ascending at index %d: %v then %v",
				i, got.Candidates[i-1].Rule.ID, got.Candidates[i].Rule.ID)
		}
	}
	for i, id := range want {
		if got.Candidates[i].Rule.ID != id {
			var ids []string
			for _, c := range got.Candidates {
				ids = append(ids, c.Rule.ID.String())
			}
			t.Fatalf("loser order = %v, want id-ascending %v", ids, want)
		}
	}
}
