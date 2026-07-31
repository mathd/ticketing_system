//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
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

	sel, err := st.ResolveTicketTypePrice(ctx, ttID, pricingAt(t))
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

	sel, err := st.ResolveTicketTypePrice(ctx, ttID, pricingAt(t))
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

	sel, err := st.ResolveTicketTypePrice(ctx, ttID, pricingAt(t))
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
	if _, err := st.ResolveTicketTypePrice(ctx, ttID, pricingAt(t)); err != nil {
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
}
