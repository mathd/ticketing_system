//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// DB-backed tests for TKT-151. The comparator is proved without a database by
// pricing_test.go; what needs one is the write gate, the migration's
// constraints, and ADR-019's two claims about the candidate read.

const pricingEvalAt = "2026-07-30T12:00:00Z"

func pricingAt(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, pricingEvalAt)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// seedPricingChain builds one organizer with a venue, event, series, published
// slot and ticket type, and returns the ticket type plus its scope identities.
func seedPricingChain(ctx context.Context, t *testing.T, db *sql.DB) (ttID uuid.UUID, orgID, venueID, eventID, slotID, seriesID uuid.UUID) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizers(name) VALUES('pricing org') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO venues(organizer_id,name,ga_capacity) VALUES($1,'hall',500) RETURNING id`,
		orgID).Scan(&venueID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO events(organizer_id,name) VALUES($1,'{"en":"show"}') RETURNING id`,
		orgID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO performances(organizer_id,event_id,venue_id,kind,status,starts_at,timezone)
		 VALUES($1,$2,$3,'performance','published',TIMESTAMPTZ '2026-10-01 20:00:00-04','America/Toronto')
		 RETURNING id`, orgID, eventID, venueID).Scan(&slotID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO series(organizer_id,event_id,name) VALUES($1,$2,'{"en":"run"}') RETURNING id`,
		orgID, eventID).Scan(&seriesID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO series_performances(series_id,performance_id,position) VALUES($1,$2,1)`,
		seriesID, slotID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
		 VALUES($1,$2,'{"en":"GA"}',4550,'EUR') RETURNING id`,
		orgID, slotID).Scan(&ttID); err != nil {
		t.Fatal(err)
	}
	return ttID, orgID, venueID, eventID, slotID, seriesID
}

// The migration's CHECKs are the last line of defence on a money column: the
// upper bound matches the OpenAPI Money.amount cap, so a row above it could be
// written and then 500 the contract-declared read (ADR-028).
func TestPriceRulesMigrationConstraints(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)

	insert := func(amount int64, scopeLevel, actionKind string) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO price_rules(organizer_id,scope_level,scope_id,action_kind,amount,currency)
			 VALUES($1,$2,$3,$4,$5,'EUR')`, orgID, scopeLevel, venueID, actionKind, amount)
		return err
	}
	for _, ok := range []int64{0, 9007199254740991} {
		if err := insert(ok, "venue", "absolute"); err != nil {
			t.Errorf("amount %d must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []int64{-1, 9007199254740992} {
		if err := insert(bad, "venue", "absolute"); err == nil {
			t.Errorf("amount %d must be rejected — it is outside the contract's Money range", bad)
		}
	}
	if err := insert(100, "festival", "absolute"); err == nil {
		t.Error("an unknown scope_level must be rejected")
	}
	if err := insert(100, "venue", "multiplier"); err == nil {
		t.Error("an unknown action_kind must be rejected — the union has one member today")
	}
	// A lowercase code is legal in char(3) but violates the contract's
	// ^[A-Z]{3}$, so it would resolve fine and then 500 the declared read
	// (ADR-028). The column enforces the contract instead.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO price_rules(organizer_id,scope_level,scope_id,action_kind,amount,currency)
		 VALUES($1,'venue',$2,'absolute',100,'eur')`, orgID, venueID); err == nil {
		t.Error("a lowercase currency must be rejected — the contract requires ^[A-Z]{3}$")
	}
}

// scope_id carries no FK because the target table depends on scope_level
// (ADR-036 §3). The store pays that back at the write path, and this is the
// test that the payment is real.
func TestCreatePriceRuleValidatesScopeKind(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, eventID, _, _ := seedPricingChain(ctx, t, db)

	if _, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: eventID, Amount: 100, Currency: "EUR",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an EVENT id inserted as a VENUE rule must be refused, got %v", err)
	}
	if _, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: uuid.New(), ScopeLevel: ScopeVenue, ScopeID: venueID, Amount: 100, Currency: "EUR",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a foreign organizer must be refused, got %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM price_rules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("refused inserts left %d row(s) behind", n)
	}
	if _, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, Amount: 100, Currency: "EUR",
	}); err != nil {
		t.Errorf("a well-formed rule must be accepted: %v", err)
	}
}

// The regression the whole epic hangs on: existing data with no rules resolves
// to exactly today's price.
func TestResolveTicketTypePriceFallsBackToBasePrice(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, _, _, _, _, _ := seedPricingChain(ctx, t, db)

	sel, err := st.ResolveTicketTypePrice(ctx, ttID, nil, pricingAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if sel.ResolvedPrice.Amount != 4550 || sel.ResolvedPrice.Currency != "EUR" {
		t.Errorf("ResolvedPrice = %+v, want the unchanged base 4550 EUR", sel.ResolvedPrice)
	}
	if sel.Winner != nil {
		t.Errorf("Winner = %+v, want none", sel.Winner)
	}
	if sel.FallbackReason == nil || *sel.FallbackReason != FallbackNoEligibleRule {
		t.Errorf("FallbackReason = %v, want %q", sel.FallbackReason, FallbackNoEligibleRule)
	}
}

// The full chain through real SQL: series beats event (ADR-036 §1 — the order
// that contradicts prd-v1.md:36), and the event rule is reported as a loser.
func TestResolveTicketTypePriceUsesTheHierarchy(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, _, eventID, _, seriesID := seedPricingChain(ctx, t, db)

	eventRule, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeEvent, ScopeID: eventID, Amount: 5000, Currency: "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	seriesRule, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeSeries, ScopeID: seriesID, Amount: 4000, Currency: "EUR"})
	if err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypePrice(ctx, ttID, nil, pricingAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Winner == nil || sel.Winner.ID != seriesRule.ID {
		t.Fatalf("Winner = %+v, want the series rule %v — a series is NARROWER than its event", sel.Winner, seriesRule.ID)
	}
	if sel.ResolvedPrice.Amount != 4000 {
		t.Errorf("ResolvedPrice = %d, want 4000", sel.ResolvedPrice.Amount)
	}
	if len(sel.Candidates) != 1 || sel.Candidates[0].Rule.ID != eventRule.ID {
		t.Fatalf("losers = %+v, want the event rule reported", sel.Candidates)
	}
	if sel.Candidates[0].Reason != ReasonLessSpecific {
		t.Errorf("loser reason = %q, want %q", sel.Candidates[0].Reason, ReasonLessSpecific)
	}
}

// ADR-019 evidence 1 of 2 — RESULT SCOPE, via a poison row.
//
// UUID uniqueness is per table, so an unrelated event can share the requested
// ticket type's id. A predicate matching scope_id alone would load that event's
// rule as a candidate and price the sale from it. The rule is same-organizer,
// same-currency and plausible, so a regression does not merely over-return —
// it returns a visibly wrong price.
func TestResolveTicketTypePriceDoesNotLoadScopeIDCollision(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, _, _, _, _ := seedPricingChain(ctx, t, db)

	// A second event whose id IS the requested ticket type's id.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events(id,organizer_id,name) VALUES($1,$2,'{"en":"collider"}')`,
		ttID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeEvent, ScopeID: ttID, Amount: 9900, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypePrice(ctx, ttID, nil, pricingAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Winner != nil {
		t.Fatalf("Winner = %+v, want none — the colliding event's rule is not a candidate", sel.Winner)
	}
	if len(sel.Candidates) != 0 {
		t.Errorf("Candidates = %+v, want empty — the foreign rule must not even be loaded", sel.Candidates)
	}
	if sel.ResolvedPrice.Amount != 4550 {
		t.Errorf("ResolvedPrice = %d, want the base 4550", sel.ResolvedPrice.Amount)
	}
}

// ADR-019 evidence 2 of 2 — PHYSICAL SCAN COST.
//
// A poison row cannot make this claim: a correct result is still producible by
// reading every rule and discarding them. So assert the plan of the exact
// production statement, under force_generic_plan, through the shared
// explainGenericPlan helper (generalized to six parameters in place — forking
// it is how one copy quietly stops asserting anything).
//
// The NULL-series case needs no second assertion: a generic plan is built with
// no parameter values, so a slot with no series yields the SAME cached plan.
func TestResolveTicketTypePriceIsIndexScoped(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, eventID, slotID, seriesID := seedPricingChain(ctx, t, db)
	if _, err := st.ResolveTicketTypePrice(ctx, ttID, nil, pricingAt(t)); err != nil {
		t.Fatal(err)
	}

	// A plan assertion is only meaningful once a sequential scan is the WRONG,
	// expensive choice: on a handful of rows Postgres rightly ignores any index
	// and the assertion would fail for a reason unrelated to this change. Seed
	// enough irrelevant rules that scanning them is the expensive option — which
	// is also the only condition under which an unscoped read actually bites.
	//
	// Same organizer on purpose: a different one would let the leading index
	// column look selective while the (scope_level, scope_id) filter went
	// unexercised.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_rules(organizer_id,scope_level,scope_id,action_kind,amount,currency)
		SELECT $1, 'event', gen_random_uuid(), 'absolute', 1000, 'EUR'
		FROM generate_series(1,10000)`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE price_rules`); err != nil {
		t.Fatal(err)
	}

	plan := explainGenericPlan(ctx, t, db, priceRuleCandidatesQuery,
		orgID, ttID, slotID, seriesID, eventID, venueID)
	assertReachesVia(t, plan, "price_rules", "price_rules_scope")

	// assertReachesVia alone is not enough here, and the gap is worth naming:
	// it checks that the index appears and no sequential scan does. A read that
	// walked the WHOLE index for this organizer — every rule they own, at every
	// scope level — would satisfy it while doing exactly the unscoped work
	// ADR-019 exists to forbid. So assert the scope predicate is IN the index
	// condition, not merely a filter applied after the rows are fetched.
	// Check the Index Cond LINE itself, and require BOTH paired columns in it.
	// Looking for the two strings anywhere in the plan is not enough: a read
	// widened to
	//     WHERE organizer_id = $1 AND scope_level IN ('ticket_type',...,'venue')
	// uses the same index, prints both strings, leaves no scope_id Filter, and
	// scans every rule the organizer owns — the exact unscoped work ADR-019
	// forbids, wearing an index name. Only scope_id inside the access condition
	// distinguishes the two.
	var indexCond string
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "Index Cond:") {
			indexCond = line
			break
		}
	}
	if indexCond == "" {
		t.Fatalf("no Index Cond in the plan — rows are not being reached BY the index.\nplan:\n%s", plan)
	}
	if !strings.Contains(indexCond, "scope_level") || !strings.Contains(indexCond, "scope_id") {
		t.Fatalf("the index condition does not use the (scope_level, scope_id) pair, so the read "+
			"may be walking the organizer's whole rule set: %s\nplan:\n%s", indexCond, plan)
	}
	// A leftover post-index Filter on the scope columns is the same defect
	// wearing a different name: rows fetched, then discarded.
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "Filter:") && strings.Contains(line, "scope_id") {
			t.Errorf("scope_id is filtered AFTER the index fetch, not by it: %s\nplan:\n%s", line, plan)
		}
	}
}

// The reversed-window CHECK. Without it an instant could be simultaneously
// after `until` and before `from`, so both provenance reasons would apply to
// one rule with no stated precedence — and the resolver's two window branches
// would stop being mutually exclusive.
func TestPriceRulesEffectiveWindowConstraint(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)

	insert := func(from, until any) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO price_rules(organizer_id,scope_level,scope_id,action_kind,amount,currency,
			                         effective_from,effective_until)
			 VALUES($1,'venue',$2,'absolute',100,'EUR',$3,$4)`, orgID, venueID, from, until)
		return err
	}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct{ from, until any }{
		"both unbounded": {nil, nil},
		"open-ended":     {base, nil},
		"only an end":    {nil, base},
		"ordered window": {base, base.Add(time.Hour)},
	} {
		if err := insert(tc.from, tc.until); err != nil {
			t.Errorf("%s must be accepted: %v", name, err)
		}
	}
	if err := insert(base.Add(time.Hour), base); err == nil {
		t.Error("a reversed window must be rejected")
	}
	if err := insert(base, base); err == nil {
		t.Error("an empty window (from == until) must be rejected — it can never be eligible")
	}
}

// COS-2, proven: an early-bird tier switches over BY THE CLOCK ALONE.
//
// Both tiers are written once, before either resolution, and NOTHING is written
// between the two calls. The only thing that differs is the instant passed in.
// If this passes, no cron, job or scheduled write is involved in a tier taking
// effect — which is the whole claim of "without manual intervention".
//
// The instants come from ONE base read off the DATABASE clock and truncated to
// microsecond precision before use: a calendar literal is green at merge and
// rots once the clock crosses it, and an untruncated base would be compared
// against a value that did not survive the timestamptz round trip.
//
// The early-bird rule is given the HIGHER priority deliberately. If window
// eligibility did not precede the ordinary comparator, it would keep winning
// past its boundary — so the successor winning at `cutover` proves the filter
// runs first, not merely that some filter exists.
func TestResolveTicketTypePriceSwitchesTierWithoutAnyWrite(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, _, eventID, _, _ := seedPricingChain(ctx, t, db)

	var base time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	base = base.UTC().Truncate(time.Microsecond)
	cutover := base.Add(time.Hour)
	earlyFrom := base.Add(-time.Hour)

	earlyBird, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeEvent, ScopeID: eventID,
		Amount: 3000, Currency: "EUR", Priority: 100,
		EffectiveFrom: &earlyFrom, EffectiveUntil: &cutover})
	if err != nil {
		t.Fatal(err)
	}
	successor, err := st.CreatePriceRule(ctx, PriceRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeEvent, ScopeID: eventID,
		Amount: 4000, Currency: "EUR", Priority: 0,
		EffectiveFrom: &cutover})
	if err != nil {
		t.Fatal(err)
	}

	// ---- no writes from here on ----

	before, err := st.ResolveTicketTypePrice(ctx, ttID, nil, cutover.Add(-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if before.Winner == nil || before.Winner.ID != earlyBird.ID {
		t.Fatalf("at cutover-1ns winner = %+v, want the early bird %v", before.Winner, earlyBird.ID)
	}
	if before.ResolvedPrice.Amount != 3000 {
		t.Errorf("at cutover-1ns price = %d, want 3000", before.ResolvedPrice.Amount)
	}
	if len(before.Candidates) != 1 || before.Candidates[0].Reason != ReasonOutsideWindowFuture {
		t.Errorf("at cutover-1ns losers = %+v, want the successor as outside_window_future", before.Candidates)
	}

	after, err := st.ResolveTicketTypePrice(ctx, ttID, nil, cutover)
	if err != nil {
		t.Fatal(err)
	}
	if after.Winner == nil || after.Winner.ID != successor.ID {
		t.Fatalf("at cutover winner = %+v, want the successor %v — the early bird has priority 100 "+
			"and must still lose, or window eligibility is not running before the comparator",
			after.Winner, successor.ID)
	}
	if after.ResolvedPrice.Amount != 4000 {
		t.Errorf("at cutover price = %d, want 4000", after.ResolvedPrice.Amount)
	}
	if len(after.Candidates) != 1 || after.Candidates[0].Reason != ReasonOutsideWindowPast {
		t.Errorf("at cutover losers = %+v, want the early bird as outside_window_past", after.Candidates)
	}
}
