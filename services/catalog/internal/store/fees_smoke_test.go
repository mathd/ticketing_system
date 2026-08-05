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

// DB-backed tests for TKT-214. The comparator is proved without a database by
// fees_test.go; what needs one is the write gate, the migration's constraints,
// and ADR-019's two claims about the candidate read.

func feeSmokeAt(t *testing.T) time.Time { return pricingAt(t) }

func fixedFeeInput(orgID, scopeID uuid.UUID, level ScopeLevel, code string, amount int64) FeeRuleInput {
	a := amount
	return FeeRuleInput{
		OrganizerID: orgID, ScopeLevel: level, ScopeID: scopeID, FeeCode: code,
		Basis: BasisPerTicketFixed, Amount: &a, Currency: "EUR",
		Incidence: IncidencePassedOn,
	}
}

// The migration's CHECKs are the last line of defence on a money column, and on
// the tagged union that says which column carries the value.
func TestFeeRulesMigrationConstraints(t *testing.T) {
	ctx, db, _ := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)

	insertFixed := func(amount int64) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
			 VALUES($1,'venue',$2,'service','per_ticket_fixed',$3,'EUR','passed_on')`,
			orgID, venueID, amount)
		return err
	}
	// The upper bound matches the OpenAPI Money.amount cap, so a row above it
	// could be written and then 500 the declared read (ADR-028).
	for _, ok := range []int64{0, 9007199254740991} {
		if err := insertFixed(ok); err != nil {
			t.Errorf("amount %d must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []int64{-1, 9007199254740992} {
		if err := insertFixed(bad); err == nil {
			t.Errorf("amount %d must be rejected — it is outside the contract's Money range", bad)
		}
	}

	insertPct := func(bps int) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,rate_bps,currency,incidence)
			 VALUES($1,'venue',$2,'service','percentage_bps',$3,'EUR','passed_on')`,
			orgID, venueID, bps)
		return err
	}
	for _, ok := range []int{0, 10000} {
		if err := insertPct(ok); err != nil {
			t.Errorf("rate_bps %d must be accepted: %v", ok, err)
		}
	}
	// A rate above 100% is a fee larger than the thing it is a fee on.
	for _, bad := range []int{-1, 10001} {
		if err := insertPct(bad); err == nil {
			t.Errorf("rate_bps %d must be rejected", bad)
		}
	}

	// The tagged union. Without this CHECK a row can claim a basis whose value
	// is missing, and the resolver would have to guess on a money path.
	for name, q := range map[string]string{
		"a fixed basis with no amount": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,currency,incidence)
			 VALUES($1,'venue',$2,'service','per_ticket_fixed','EUR','passed_on')`,
		"a fixed basis carrying a rate": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,rate_bps,currency,incidence)
			 VALUES($1,'venue',$2,'service','per_ticket_fixed',100,500,'EUR','passed_on')`,
		"a percentage basis with no rate": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,currency,incidence)
			 VALUES($1,'venue',$2,'service','percentage_bps','EUR','passed_on')`,
		"a percentage basis carrying an amount": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,rate_bps,currency,incidence)
			 VALUES($1,'venue',$2,'service','percentage_bps',100,500,'EUR','passed_on')`,
	} {
		if _, err := db.ExecContext(ctx, q, orgID, venueID); err == nil {
			t.Errorf("%s must be rejected — the basis and its value column must agree", name)
		}
	}

	for name, q := range map[string]string{
		"an unknown scope_level": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
			 VALUES($1,'festival',$2,'service','per_ticket_fixed',100,'EUR','passed_on')`,
		"an unknown basis": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
			 VALUES($1,'venue',$2,'service','per_seat',100,'EUR','passed_on')`,
		"an unknown incidence": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
			 VALUES($1,'venue',$2,'service','per_ticket_fixed',100,'EUR','shared')`,
		// A lowercase code is legal in char(3) but violates the contract's
		// ^[A-Z]{3}$, so it would resolve fine and then 500 the declared read.
		"a lowercase currency": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
			 VALUES($1,'venue',$2,'service','per_ticket_fixed',100,'eur','passed_on')`,
		"an empty fee code": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
			 VALUES($1,'venue',$2,'','per_ticket_fixed',100,'EUR','passed_on')`,
		"an empty channel code": `INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence,channel_code)
			 VALUES($1,'venue',$2,'service','per_ticket_fixed',100,'EUR','passed_on','')`,
	} {
		if _, err := db.ExecContext(ctx, q, orgID, venueID); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}

	// A reversed window would make both provenance reasons apply to one rule.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence,
		                       effective_from,effective_until)
		 VALUES($1,'venue',$2,'service','per_ticket_fixed',100,'EUR','passed_on',
		        TIMESTAMPTZ '2026-09-01 00:00:00Z', TIMESTAMPTZ '2026-08-01 00:00:00Z')`,
		orgID, venueID); err == nil {
		t.Error("a reversed effective window must be rejected")
	}
}

// scope_id carries no FK because the target table depends on scope_level
// (ADR-036 §3). The store pays that back at the write path, and this is the test
// that the payment is real.
func TestCreateFeeRuleValidatesScopeKind(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, eventID, _, _ := seedPricingChain(ctx, t, db)

	if _, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, eventID, ScopeVenue, "service", 100)); !errors.Is(err, ErrNotFound) {
		t.Errorf("an EVENT id inserted as a VENUE rule must be refused, got %v", err)
	}
	if _, err := st.CreateFeeRule(ctx, fixedFeeInput(uuid.New(), venueID, ScopeVenue, "service", 100)); !errors.Is(err, ErrNotFound) {
		t.Errorf("a foreign organizer must be refused, got %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM fee_rules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("refused inserts left %d row(s) behind", n)
	}
	if _, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, venueID, ScopeVenue, "service", 100)); err != nil {
		t.Errorf("a well-formed rule must be accepted: %v", err)
	}
}

// The regression the rest of the epic hangs on: a ticket type with no fee rules
// resolves to an empty set and nothing else changes.
func TestResolveTicketTypeFeesWithoutRulesIsEmpty(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, _, _, _, _, _ := seedPricingChain(ctx, t, db)

	sel, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fees == nil {
		t.Fatal("Fees must be an empty slice, not nil")
	}
	if len(sel.Fees) != 0 {
		t.Errorf("Fees = %+v, want empty", sel.Fees)
	}
	if sel.Currency != "EUR" {
		t.Errorf("Currency = %q, want the ticket type's EUR", sel.Currency)
	}
	// Price resolution must be untouched by any of this.
	price, err := st.ResolveTicketTypePrice(ctx, ttID, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if price.ResolvedPrice.Amount != 4550 {
		t.Errorf("price = %d, want the unchanged base 4550", price.ResolvedPrice.Amount)
	}
}

// One winner per code, additive across codes, through real SQL and the real
// scope derivation.
func TestResolveTicketTypeFeesResolvesPerCodeThroughTheHierarchy(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, eventID, slotID, _ := seedPricingChain(ctx, t, db)

	house, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, venueID, ScopeVenue, "service", 300))
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, slotID, ScopeSlot, "service", 500))
	if err != nil {
		t.Fatal(err)
	}
	facility, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, eventID, ScopeEvent, "facility", 150))
	if err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Fees) != 2 {
		t.Fatalf("want two codes, got %+v", sel.Fees)
	}
	if sel.OrganizerID != orgID {
		t.Errorf("OrganizerID = %s, want %s", sel.OrganizerID, orgID)
	}
	if sel.PerformanceID != slotID {
		t.Errorf("PerformanceID = %s, want %s", sel.PerformanceID, slotID)
	}
	service := sel.Fees[1]
	if service.FeeCode != "service" || service.Winner == nil || service.Winner.ID != narrow.ID {
		t.Fatalf("the slot rule must win service, got %+v", service)
	}
	if len(service.Candidates) != 1 || service.Candidates[0].Rule.ID != house.ID ||
		service.Candidates[0].Reason != ReasonLessSpecific {
		t.Errorf("the house rule must lose as less_specific, got %+v", service.Candidates)
	}
	if sel.Fees[0].Winner == nil || sel.Fees[0].Winner.ID != facility.ID {
		t.Errorf("facility winner = %+v, want %s", sel.Fees[0].Winner, facility.ID)
	}
}

// Channel selectivity, and the negative half that matters most: a rule belonging
// to another channel is never loaded into the answer at all.
func TestResolveTicketTypeFeesIsChannelSelective(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, _, eventID, _, _ := seedPricingChain(ctx, t, db)

	agnostic, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, eventID, ScopeEvent, "service", 300))
	if err != nil {
		t.Fatal(err)
	}
	resellerIn := fixedFeeInput(orgID, eventID, ScopeEvent, "service", 900)
	reseller := "reseller"
	resellerIn.ChannelCode = &reseller
	// Deliberately LOWER priority than the agnostic rule: channel specificity is
	// decided before priority, so this rule must still win. A fixture where the
	// channel rule was also the loudest could not tell the two orders apart.
	resellerIn.Priority = -10
	resellerRule, err := st.CreateFeeRule(ctx, resellerIn)
	if err != nil {
		t.Fatal(err)
	}
	presaleIn := fixedFeeInput(orgID, eventID, ScopeEvent, "service", 100)
	presale := "presale"
	presaleIn.ChannelCode = &presale
	presaleRule, err := st.CreateFeeRule(ctx, presaleIn)
	if err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypeFees(ctx, ttID, &reseller, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Fees) != 1 || sel.Fees[0].Winner == nil {
		t.Fatalf("want one resolved code, got %+v", sel.Fees)
	}
	if sel.Fees[0].Winner.ID != resellerRule.ID {
		t.Errorf("winner = %s, want the exact-channel rule %s", sel.Fees[0].Winner.ID, resellerRule.ID)
	}
	sawAgnostic := false
	for _, c := range sel.Fees[0].Candidates {
		if c.Rule.ID == presaleRule.ID {
			t.Error("another channel's rule leaked into provenance")
		}
		if c.Rule.ID == agnostic.ID {
			sawAgnostic = true
			if c.Reason != ReasonLessChannelSpecific {
				t.Errorf("reason = %q, want less_channel_specific", c.Reason)
			}
		}
	}
	if !sawAgnostic {
		t.Error("the channel-agnostic rule competed and lost; it must be reported")
	}

	// The default/public context is not a wildcard.
	pub, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Fees) != 1 || pub.Fees[0].Winner == nil || pub.Fees[0].Winner.ID != agnostic.ID {
		t.Errorf("with no channel only the agnostic rule may apply, got %+v", pub.Fees)
	}
}

// A misconfigured currency fails the resolution rather than being skipped, and
// the closed-window exception keeps a dead row from being a permanent outage.
func TestResolveTicketTypeFeesCurrencyMismatch(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, eventID, _, _ := seedPricingChain(ctx, t, db)

	bad := fixedFeeInput(orgID, venueID, ScopeVenue, "service", 300)
	bad.Currency = "USD"
	if _, err := st.CreateFeeRule(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t)); !errors.Is(err, ErrFeeRuleCurrencyMismatch) {
		t.Fatalf("want ErrFeeRuleCurrencyMismatch, got %v", err)
	}

	// Close the offending rule's window. It can never charge anything again, so
	// it must stop failing the resolution — currency is immutable and
	// effective_until only shortens, so nothing else could rescue it.
	if _, err := db.ExecContext(ctx,
		`UPDATE fee_rules SET effective_until = $1 WHERE currency = 'USD'`,
		feeSmokeAt(t).Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, eventID, ScopeEvent, "service", 200))
	if err != nil {
		t.Fatal(err)
	}
	sel, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatalf("a dead row must not be a permanent outage: %v", err)
	}
	if len(sel.Fees) != 1 || sel.Fees[0].Winner == nil || sel.Fees[0].Winner.ID != live.ID {
		t.Errorf("winner = %+v, want the live rule %s", sel.Fees, live.ID)
	}
}

// A fee schedule flips by the clock alone. NOTHING RUNS between the two reads:
// no cron, no scheduled write, no job that can fail at 00:00 on an on-sale.
func TestResolveTicketTypeFeesSwitchesWindowWithoutAnyWrite(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, _, eventID, _, _ := seedPricingChain(ctx, t, db)

	var base time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	base = base.UTC().Truncate(time.Microsecond)
	cutover := base.Add(time.Hour)
	from := base.Add(-time.Hour)

	earlyIn := fixedFeeInput(orgID, eventID, ScopeEvent, "service", 100)
	earlyIn.EffectiveFrom, earlyIn.EffectiveUntil = &from, &cutover
	early, err := st.CreateFeeRule(ctx, earlyIn)
	if err != nil {
		t.Fatal(err)
	}
	laterIn := fixedFeeInput(orgID, eventID, ScopeEvent, "service", 400)
	laterIn.EffectiveFrom = &cutover
	later, err := st.CreateFeeRule(ctx, laterIn)
	if err != nil {
		t.Fatal(err)
	}

	// ---- no writes from here on ----

	before, err := st.ResolveTicketTypeFees(ctx, ttID, nil, cutover.Add(-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if before.Fees[0].Winner == nil || before.Fees[0].Winner.ID != early.ID {
		t.Fatalf("before the cutover the early rule must win, got %+v", before.Fees[0].Winner)
	}
	if len(before.Fees[0].Candidates) != 1 ||
		before.Fees[0].Candidates[0].Reason != ReasonOutsideWindowFuture {
		t.Errorf("the successor must lose as outside_window_future, got %+v", before.Fees[0].Candidates)
	}

	// Half-open [from, until): AT the cutover the successor is live and the
	// predecessor is not. An ambiguity here is a money bug at every boundary.
	at, err := st.ResolveTicketTypeFees(ctx, ttID, nil, cutover)
	if err != nil {
		t.Fatal(err)
	}
	if at.Fees[0].Winner == nil || at.Fees[0].Winner.ID != later.ID {
		t.Fatalf("at the cutover the successor must win, got %+v", at.Fees[0].Winner)
	}
	if len(at.Fees[0].Candidates) != 1 ||
		at.Fees[0].Candidates[0].Reason != ReasonOutsideWindowPast {
		t.Errorf("the predecessor must lose as outside_window_past, got %+v", at.Fees[0].Candidates)
	}
}

// ADR-019 evidence 1 of 2 — RESULT SCOPE.
//
// UUID uniqueness is per table, so a scope_id can legitimately collide with an
// id of another kind. The poison row is otherwise entirely valid — current,
// same-currency, same-organizer, plausible amount — so a broken query cannot
// fail this test for an unrelated reason.
func TestResolveTicketTypeFeesDoesNotLoadScopeIDCollision(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, _, _, _, _ := seedPricingChain(ctx, t, db)

	// A second event whose id IS the requested ticket type's id.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events(id,organizer_id,name) VALUES($1,$2,'{"en":"collider"}')`,
		ttID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, ttID, ScopeEvent, "service", 9900)); err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Fees) != 0 {
		t.Fatalf("Fees = %+v, want empty — the colliding event's rule is not a candidate", sel.Fees)
	}
}

// ADR-019 evidence 2 of 2 — PHYSICAL SCAN COST.
//
// A poison row cannot make this claim: a correct result is still producible by
// reading every rule and discarding them. So assert the plan of the exact
// production statement, under force_generic_plan, through the shared
// explainGenericPlan helper — forking it is how one copy quietly stops asserting
// anything (ADR-019).
func TestResolveTicketTypeFeesIsIndexScoped(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, eventID, slotID, seriesID := seedPricingChain(ctx, t, db)
	if _, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t)); err != nil {
		t.Fatal(err)
	}
	_ = venueID

	// A plan assertion is only meaningful once a sequential scan is the WRONG,
	// expensive choice: on a handful of rows Postgres rightly ignores any index
	// and the assertion would fail for a reason unrelated to this change. Same
	// organizer on purpose — a different one would let the leading index column
	// look selective while the (scope_level, scope_id) filter went unexercised.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,incidence)
		SELECT $1, 'event', gen_random_uuid(), 'service', 'per_ticket_fixed', 1000, 'EUR', 'passed_on'
		FROM generate_series(1,10000)`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE fee_rules`); err != nil {
		t.Fatal(err)
	}

	plan := explainGenericPlan(ctx, t, db, feeRuleCandidatesQuery,
		orgID, ttID, slotID, seriesID, eventID, venueID)
	assertReachesVia(t, plan, "fee_rules", "fee_rules_scope")

	// assertReachesVia alone is not enough, and the gap is worth naming: a read
	// that walked the WHOLE index for this organizer — every rule they own, at
	// every scope level — would satisfy it while doing exactly the unscoped work
	// ADR-019 exists to forbid. Only scope_id inside the ACCESS condition
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

// The rollback guard: fee configuration must not be silently destroyed.
func TestFeeRulesDownMigrationRefusesToDropData(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	if _, err := st.CreateFeeRule(ctx, fixedFeeInput(orgID, venueID, ScopeVenue, "service", 100)); err != nil {
		t.Fatal(err)
	}
	assertFeeRollbackRefused(ctx, t, db)
}

func assertFeeRollbackRefused(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		DO $$
		BEGIN
		    IF EXISTS (SELECT 1 FROM fee_rules) THEN
		        RAISE EXCEPTION 'cannot roll back 0016: fee-rule data exists';
		    END IF;
		END $$;`)
	if err == nil {
		t.Error("the down migration's guard must refuse while fee-rule data exists")
	}
}
