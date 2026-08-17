//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// DB-backed tests for TKT-243 — the operator sweep that finds price and fee
// rules misconfigured for a currency their ticket type does not use.
//
// These live at the smoke tier and not beside pricing_test.go because the
// mechanism under test IS the SQL: the scope-pair join, the three predicates and
// the two deliberate omissions. An assertion against an in-memory fake would
// prove the fake and the caller agree and nothing else (AGENTS.md).
//
// Every test below states the mutation that must make it red. Read them as a
// set: the guard is a conjunction, so one fixture refused by an earlier
// predicate never reaches a later one, and a single "it found something" test
// would be silent about four of the five things this query has to get right.

// seedValidationChain builds one organizer with a venue, event, series,
// published slot and a EUR ticket type, and returns the ticket type plus its
// scope identities. Same shape as seedPricingChain; kept separate so a change to
// the pricing fixture cannot silently move this suite's baseline currency.
func seedValidationChain(ctx context.Context, t *testing.T, db *sql.DB, orgName string) (ttID, orgID, venueID, eventID, slotID, seriesID uuid.UUID) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizers(name) VALUES($1) RETURNING id`, orgName).Scan(&orgID); err != nil {
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

// insertPriceRule writes a rule directly, bypassing CreatePriceRule's scope
// gate. Direct on purpose: several fixtures below need a row the write path
// would refuse (a scope_id naming the wrong kind of entity), and the sweep's
// job is to report what is IN the table, not what the write path would allow.
func insertPriceRule(ctx context.Context, t *testing.T, db *sql.DB, orgID uuid.UUID, scopeLevel string, scopeID uuid.UUID, currency string, channel *string, from, until *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO price_rules(organizer_id,scope_level,scope_id,action_kind,amount,currency,
		                         channel_code,effective_from,effective_until)
		 VALUES($1,$2,$3,'absolute',5000,$4,$5,$6,$7) RETURNING id`,
		orgID, scopeLevel, scopeID, currency, channel, from, until).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertFeeRule(ctx context.Context, t *testing.T, db *sql.DB, orgID uuid.UUID, scopeLevel string, scopeID uuid.UUID, feeCode, currency string, channel *string, from, until *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO fee_rules(organizer_id,scope_level,scope_id,fee_code,basis,amount,currency,
		                       incidence,channel_code,effective_from,effective_until)
		 VALUES($1,$2,$3,$4,'per_ticket_fixed',250,$5,'passed_on',$6,$7,$8) RETURNING id`,
		orgID, scopeLevel, scopeID, feeCode, currency, channel, from, until).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// findingsFor filters a sweep result to one organizer, so tests that seed their
// own chain are not perturbed by rows another test in the same schema left.
func findingsFor(all []RuleCurrencyMismatch, orgID uuid.UUID) []RuleCurrencyMismatch {
	var out []RuleCurrencyMismatch
	for _, f := range all {
		if f.OrganizerID == orgID {
			out = append(out, f)
		}
	}
	return out
}

func hasRule(findings []RuleCurrencyMismatch, ruleID uuid.UUID) bool {
	for _, f := range findings {
		if f.RuleID == ruleID {
			return true
		}
	}
	return false
}

// S1. A rule at a broad scope covers many ticket types, each with its own
// currency, so one rule produces one finding PER covered ticket type. This is
// the whole reason the ticket's original phrasing ("every rule whose currency
// differs from its ticket type's") does not typecheck: a rule has no ticket
// type. ADR-036 §4 step 1 names the same asymmetry as the reason write-time
// validation cannot do this job.
//
// Mutation: dedupe by rule id, or restrict the sweep to scope_level='ticket_type'.
func TestRuleCurrencySweepReportsOnePairPerCoveredTicketType(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, slotID, _ := seedValidationChain(ctx, t, db, "sweep s1")

	// A second EUR ticket type on the same slot: both are covered by a venue rule.
	var tt2 uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
		 VALUES($1,$2,'{"en":"VIP"}',9900,'EUR') RETURNING id`, orgID, slotID).Scan(&tt2); err != nil {
		t.Fatal(err)
	}

	ruleID := insertPriceRule(ctx, t, db, orgID, "venue", venueID, "USD", nil, nil, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := findingsFor(all, orgID)
	if len(got) != 2 {
		t.Fatalf("one venue rule over two ticket types must report two pairs, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.RuleID != ruleID {
			t.Errorf("unexpected rule in findings: %v", f.RuleID)
		}
		if f.RuleCurrency != "USD" || f.TicketTypeCurrency != "EUR" {
			t.Errorf("currencies must be reported as stored: got rule=%q ticket=%q", f.RuleCurrency, f.TicketTypeCurrency)
		}
	}
}

// S2. The sweep reports MISMATCHES, not rules. Without the inequality it would
// report every rule in the database and be worthless.
//
// Mutation: drop the currency comparison from the WHERE clause.
func TestRuleCurrencySweepIgnoresMatchingCurrency(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedValidationChain(ctx, t, db, "sweep s2")

	insertPriceRule(ctx, t, db, orgID, "venue", venueID, "EUR", nil, nil, nil)
	insertFeeRule(ctx, t, db, orgID, "venue", venueID, "service", "EUR", nil, nil, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsFor(all, orgID); len(got) != 0 {
		t.Fatalf("a rule whose currency matches its ticket type is not a finding, got %+v", got)
	}
}

// S3. COS (b): the sweep must find rules on channels nobody is buying through,
// INCLUDING codes absent from the channels registry. The registry is a lookup,
// not a constraint (0018_channels.sql:10-17) — a code that was never registered
// sells exactly like one that was, so a sweep that consulted the registry would
// systematically miss the rules most likely to be misconfigured.
//
// No channels row is seeded here ON PURPOSE. Seeding one would imply the sweep
// reads that table; it must not, and a decorative seed makes a fixture look like
// it covers a layer it never observes (TKT-241).
//
// Mutation: add any channel predicate, or a join to channels.
func TestRuleCurrencySweepFindsUnregisteredChannelRules(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedValidationChain(ctx, t, db, "sweep s3")

	priceID := insertPriceRule(ctx, t, db, orgID, "venue", venueID, "USD", ptr("never-registered"), nil, nil)
	feeID := insertFeeRule(ctx, t, db, orgID, "venue", venueID, "service", "GBP", ptr("also-unregistered"), nil, nil)

	var registered int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM channels WHERE organizer_id=$1`, orgID).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if registered != 0 {
		t.Fatalf("fixture invariant: this test must run with an empty registry, found %d rows", registered)
	}

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := findingsFor(all, orgID)
	if !hasRule(got, priceID) {
		t.Error("a price rule on an unregistered channel must be reported")
	}
	if !hasRule(got, feeID) {
		t.Error("a fee rule on an unregistered channel must be reported")
	}
}

// S4. A rule whose window has not opened yet is exactly what this sweep exists
// to surface: it will price the moment it opens, and nothing today would notice.
// ADR-036 §4 step 1 is explicit that a not-yet-open wrong-currency rule is a
// misconfiguration "deliberately" caught.
//
// Mutation: add `effective_from <= now()` to the sweep.
func TestRuleCurrencySweepFindsRulesWhoseWindowHasNotOpened(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedValidationChain(ctx, t, db, "sweep s4")

	future := time.Now().Add(720 * time.Hour)
	ruleID := insertPriceRule(ctx, t, db, orgID, "venue", venueID, "USD", nil, &future, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(findingsFor(all, orgID), ruleID) {
		t.Error("a wrong-currency rule whose window has not opened must be reported — it will price when it opens")
	}
}

// S5. The opposite polarity, and the one that is only discoverable by reading
// two ADRs. A rule whose window has CLOSED is inert and must NOT be reported:
// ADR-036 §4 step 1 and ADR-046 §8 both say failing on its account would be
// "permanent and unrecoverable, since currency is immutable and effective_until
// only shortens". An operator handed that finding can do nothing about it —
// they cannot fix the currency, reopen the window, or delete the row — so
// reporting it is noise that trains them to ignore the sweep.
//
// Mutation: drop the `effective_until IS NULL OR effective_until > now()` guard.
func TestRuleCurrencySweepExcludesRulesWhoseWindowHasClosed(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedValidationChain(ctx, t, db, "sweep s5")

	past := time.Now().Add(-720 * time.Hour)
	closedPrice := insertPriceRule(ctx, t, db, orgID, "venue", venueID, "USD", nil, nil, &past)
	closedFee := insertFeeRule(ctx, t, db, orgID, "venue", venueID, "service", "USD", nil, nil, &past)

	// A live rule alongside it, so the test cannot pass by the sweep returning
	// nothing at all — the fixture must be able to produce a finding.
	liveID := insertPriceRule(ctx, t, db, orgID, "venue", venueID, "GBP", nil, nil, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := findingsFor(all, orgID)
	if !hasRule(got, liveID) {
		t.Fatal("fixture invariant: the live wrong-currency rule must be reported, else this test proves nothing")
	}
	if hasRule(got, closedPrice) {
		t.Error("a price rule whose window has closed is inert and unrecoverable — reporting it is noise (ADR-036 §4 step 1)")
	}
	if hasRule(got, closedFee) {
		t.Error("a fee rule whose window has closed is inert and unrecoverable — reporting it is noise (ADR-046 §8)")
	}
}

// S6. A slot belonging to no series must not match a rule whose scope_id is the
// zero UUID. The resolver's candidate query documents the same trap
// (pricing_postgres.go:100-103): the parameter must be a typed NULL, never
// uuid.Nil, or ('series', NULL) becomes ('series', 00000000-...) and matches.
//
// Mutation: coalesce the absent series to uuid.Nil / '00000000-0000-0000-0000-000000000000'.
func TestRuleCurrencySweepDoesNotMatchAbsentSeriesToZeroUUID(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, eventID, _, _ := seedValidationChain(ctx, t, db, "sweep s6")

	// A second slot with a ticket type, deliberately NOT attached to any series.
	var loneSlot, loneTT uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO performances(organizer_id,event_id,venue_id,kind,status,starts_at,timezone)
		 VALUES($1,$2,$3,'performance','published',TIMESTAMPTZ '2026-11-02 20:00:00-04','America/Toronto')
		 RETURNING id`, orgID, eventID, venueID).Scan(&loneSlot); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO ticket_types(organizer_id,performance_id,name,price_amount,currency)
		 VALUES($1,$2,'{"en":"lone"}',3000,'EUR') RETURNING id`, orgID, loneSlot).Scan(&loneTT); err != nil {
		t.Fatal(err)
	}

	zeroScoped := insertPriceRule(ctx, t, db, orgID, "series", uuid.Nil, "USD", nil, nil, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findingsFor(all, orgID) {
		if f.RuleID == zeroScoped && f.TicketTypeID == loneTT {
			t.Error("a ticket type in no series must not match a series rule at the zero UUID")
		}
	}
}

// S7. The join matches (scope_level, scope_id) PAIRS, never scope_id alone.
// UUID uniqueness is per table, so a rule declaring scope_level='event' whose
// scope_id happens to equal a VENUE id must not match through the venue edge.
// ADR-036 §3 calls matching scope_id alone "a correctness bug, not a shortcut".
//
// Mutation: match on scope_id alone (drop scope_level from the comparison).
func TestRuleCurrencySweepMatchesScopePairNotScopeIDAlone(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedValidationChain(ctx, t, db, "sweep s7")

	// scope_level says 'event', scope_id is the VENUE's id. No FK stops this
	// row existing, which is precisely why the pair comparison is load-bearing.
	mismatched := insertPriceRule(ctx, t, db, orgID, "event", venueID, "USD", nil, nil, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasRule(findingsFor(all, orgID), mismatched) {
		t.Error("an 'event' rule whose scope_id is a venue id must not match through the venue edge — UUID uniqueness is per table (ADR-036 §3)")
	}
}

// S8 — COS (d), the case that motivated the ticket.
//
// TKT-237 moved channel eligibility ahead of the currency check, so a
// wrong-currency rule on a channel nobody is buying through is invisible to
// resolution until a sale arrives on that channel. This test pins both halves
// against ONE seeded row: the resolver walks past it cleanly on a different
// channel, and the sweep finds it anyway.
//
// Mutations: move the currency check ahead of channel eligibility in
// SelectPricingRule (the resolver half goes red), or add a channel predicate to
// the sweep (the sweep half goes red).
func TestRuleCurrencySweepFindsWhatChannelScopedResolutionIgnores(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, _, _, _ := seedValidationChain(ctx, t, db, "sweep s8")

	// Open window, wrong currency, on a channel that is neither registered nor
	// the one the buyer is using.
	ruleID := insertPriceRule(ctx, t, db, orgID, "venue", venueID, "USD", ptr("pos"), nil, nil)

	// The resolver, asked for a DIFFERENT channel, must succeed — the rule is
	// filtered out by channel before its currency is ever compared.
	sel, err := st.ResolveTicketTypePrice(ctx, ttID, ptr("web"), time.Now())
	if err != nil {
		t.Fatalf("resolution on another channel must not fail on a foreign channel's misconfigured rule: %v", err)
	}
	if sel.ResolvedPrice.Currency != "EUR" || sel.ResolvedPrice.Amount != 4550 {
		t.Errorf("resolution must fall through to the ticket type's base price, got %d %s",
			sel.ResolvedPrice.Amount, sel.ResolvedPrice.Currency)
	}

	// And the sweep finds it regardless, which is the point of the ticket.
	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(findingsFor(all, orgID), ruleID) {
		t.Error("the sweep must report a rule that channel-scoped resolution deliberately ignores — this is the ticket")
	}
}

// The sweep must not match a rule to another organizer's ticket type. Rules
// carry no FK to their scope target, so the organizer predicate is the only
// thing preventing a cross-tenant pair when two organizers share a scope id.
//
// Mutation: drop `r.organizer_id = t.organizer_id` from the join.
func TestRuleCurrencySweepDoesNotPairAcrossOrganizers(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgA, venueA, _, _, _ := seedValidationChain(ctx, t, db, "sweep tenant A")
	_, orgB, _, _, _, _ := seedValidationChain(ctx, t, db, "sweep tenant B")

	// Organizer B owns a rule scoped to organizer A's venue id.
	crossed := insertPriceRule(ctx, t, db, orgB, "venue", venueA, "USD", nil, nil, nil)

	all, err := st.ListRuleCurrencyMismatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findingsFor(all, orgA) {
		if f.RuleID == crossed {
			t.Error("another organizer's rule must never pair with this organizer's ticket type (ADR-002)")
		}
	}
	for _, f := range findingsFor(all, orgB) {
		if f.RuleID == crossed {
			t.Error("a rule scoped to a venue its organizer does not own must not pair through it")
		}
	}
}
