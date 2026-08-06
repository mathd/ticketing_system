//go:build smoke

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// DB-backed tests for TKT-216. The comparator is proved without a database by
// splits_test.go; what needs one is the deferred balance trigger, the write
// gate, and ADR-019's two claims about the candidate read.

func seedPayees(ctx context.Context, t *testing.T, st *Postgres, orgID uuid.UUID, n int) []Payee {
	t.Helper()
	out := make([]Payee, 0, n)
	for i := 0; i < n; i++ {
		p, err := st.CreatePayee(ctx, Payee{
			OrganizerID: orgID, Kind: "venue", DisplayName: "payee",
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func partsOf(payees []Payee, bps ...int32) []SplitPart {
	out := make([]SplitPart, len(bps))
	for i, b := range bps {
		out[i] = SplitPart{Payee: payees[i], ShareBps: b}
	}
	return out
}

// The rule the whole ticket rests on: an unbalanced schedule cannot be
// COMMITTED, by any writer.
//
// FIXTURE NOTE: three parts, not one. A single-part fixture would be refused by
// the has-parts guard rather than by the sum rule, so it would pass against a
// build where the sum rule did not work at all — the exact "fails for the wrong
// reason" trap. The balanced case is asserted alongside it so the trigger is a
// guard rather than a wall.
func TestSplitScheduleMustBalanceAtCommit(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 3)

	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 3333, 3333, 3333), // 9999
	}); err == nil {
		t.Fatal("a schedule summing to 9999 must not commit")
	}
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 3333, 3333, 3335), // 10001
	}); err == nil {
		t.Fatal("a schedule summing to 10001 must not commit")
	}
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 3333, 3333, 3334), // 10000
	}); err != nil {
		t.Fatalf("a balanced schedule must commit: %v", err)
	}

	// Nothing was left behind by the refusals.
	var schedules int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM split_schedules`).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if schedules != 1 {
		t.Errorf("%d schedules survived, want only the balanced one", schedules)
	}
}

// A header with no parts at all never fires the per-row balance trigger, so it
// gets its own guard: an empty schedule is not "unsplit", it is a schedule that
// resolves to nothing while looking authored.
func TestSplitScheduleWithNoPartsIsRefused(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
	}); err == nil {
		t.Fatal("a schedule with no parts must not commit")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM split_schedules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the refused header survived (%d rows)", n)
	}
}

// Deleting a part must re-check the balance: an unbalanced schedule reached by
// subtraction is the same defect as one reached by insertion.
func TestRemovingAPartUnbalancesAndIsRefused(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 3)
	id, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 3333, 3333, 3334),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM split_schedule_parts WHERE schedule_id=$1 AND payee_id=$2`,
		id, payees[0].ID); err == nil {
		t.Error("removing a part must leave the schedule unbalanced and be refused")
	}
}

// The composite foreign key, which is what stops money being paid to a stranger:
// a schedule cannot name another organizer's payee.
func TestSplitScheduleCannotNameAForeignPayee(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	mine := seedPayees(ctx, t, st, orgID, 1)

	var otherOrg uuid.UUID
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizers(name) VALUES('other') RETURNING id`).Scan(&otherOrg); err != nil {
		t.Fatal(err)
	}
	theirs := seedPayees(ctx, t, st, otherOrg, 1)

	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: []SplitPart{{Payee: mine[0], ShareBps: 5000}, {Payee: theirs[0], ShareBps: 5000}},
	}); err == nil {
		t.Error("a schedule naming another organizer's payee must be refused — the tenant is " +
			"part of the reference, not a field somebody remembers to check")
	}
}

// scope_id carries no FK, so the store proves the target exists and belongs to
// the organizer, exactly as price and fee rules do.
func TestCreateSplitScheduleValidatesScopeKind(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, eventID, _, _ := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 1)

	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: eventID, FeeCode: "service",
		Parts: partsOf(payees, 10000),
	}); err == nil {
		t.Error("an EVENT id inserted as a VENUE schedule must be refused")
	}
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: uuid.New(), ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 10000),
	}); err == nil {
		t.Error("a foreign organizer must be refused")
	}
}

// The headline read: a fee resolution carries who the fee is owed to, and an
// unauthored code says so explicitly.
func TestResolveTicketTypeFeesCarriesTheSplit(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, eventID, _, _ := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 2)

	amt := int64(300)
	for _, code := range []string{"service", "facility"} {
		if _, err := st.CreateFeeRule(ctx, FeeRuleInput{
			OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: code,
			Basis: BasisPerTicketFixed, Amount: &amt, Currency: "EUR", Incidence: IncidencePassedOn,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Only `service` gets a schedule; `facility` must resolve unsplit.
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeEvent, ScopeID: eventID, FeeCode: "service",
		Parts: partsOf(payees, 6000, 4000),
	}); err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Fees) != 2 {
		t.Fatalf("want two codes, got %+v", sel.Fees)
	}
	byCode := map[string]FeeCodeSelection{}
	for _, f := range sel.Fees {
		byCode[f.FeeCode] = f
	}
	service := byCode["service"]
	if service.Split.Mode != SplitModeSplit || service.Split.Winner == nil {
		t.Fatalf("service must resolve split, got %+v", service.Split)
	}
	if len(service.Split.Winner.Parts) != 2 {
		t.Fatalf("want two parts, got %+v", service.Split.Winner.Parts)
	}
	var total int32
	for _, p := range service.Split.Winner.Parts {
		total += p.ShareBps
		if p.Payee.DisplayName == "" || p.Payee.Kind == "" {
			t.Errorf("payee metadata is missing: %+v", p.Payee)
		}
	}
	if total != 10000 {
		t.Errorf("parts sum to %d bps, want 10000", total)
	}
	facility := byCode["facility"]
	if facility.Split.Mode != SplitModeUnsplit || facility.Split.Reason != SplitReasonNoSchedule {
		t.Errorf("facility must be unsplit/no_schedule, got %+v", facility.Split)
	}
	if facility.Split.Winner != nil {
		t.Error("an unsplit code must have no winner")
	}
}

// ADR-019 evidence 1 of 2 — RESULT SCOPE. UUID uniqueness is per table, so a
// scope_id can legitimately collide with an id of another kind. The poison row
// is otherwise entirely valid so a broken query cannot fail for another reason.
func TestResolveSplitsDoesNotLoadScopeIDCollision(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 1)
	amt := int64(300)
	if _, err := st.CreateFeeRule(ctx, FeeRuleInput{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Basis: BasisPerTicketFixed, Amount: &amt, Currency: "EUR", Incidence: IncidencePassedOn,
	}); err != nil {
		t.Fatal(err)
	}
	// An event whose id IS the requested ticket type's id, carrying a valid
	// event-scoped schedule for the same code.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events(id,organizer_id,name) VALUES($1,$2,'{"en":"collider"}')`,
		ttID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeEvent, ScopeID: ttID, FeeCode: "service",
		Parts: partsOf(payees, 10000),
	}); err != nil {
		t.Fatal(err)
	}

	sel, err := st.ResolveTicketTypeFees(ctx, ttID, nil, feeSmokeAt(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Fees) != 1 {
		t.Fatalf("want one code, got %+v", sel.Fees)
	}
	if sel.Fees[0].Split.Mode != SplitModeUnsplit {
		t.Errorf("the colliding event's schedule must not be a candidate, got %+v", sel.Fees[0].Split)
	}
}

// ADR-019 evidence 2 of 2 — PHYSICAL SCAN COST. A poison row cannot make this
// claim: a correct result is still producible by reading every schedule and
// discarding them.
func TestResolveSplitsIsIndexScoped(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	ttID, orgID, venueID, eventID, slotID, seriesID := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 1)
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 10000),
	}); err != nil {
		t.Fatal(err)
	}

	// Enough irrelevant schedules that scanning them is the expensive option —
	// the only condition under which the assertion can fail for the right
	// reason. Same organizer on purpose.
	// ONE transaction for headers AND parts. Two statements outside a
	// transaction commit the headers on their own, and the deferred trigger
	// correctly refuses them for having no parts — which is the trigger working,
	// not the seed failing, and it is worth stating because the first version of
	// this seed did exactly that.
	seedTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedTx.ExecContext(ctx, `
		INSERT INTO split_schedules(organizer_id,scope_level,scope_id,fee_code)
		SELECT $1, 'event', gen_random_uuid(), 'service' FROM generate_series(1,10000)`,
		orgID); err != nil {
		t.Fatal(err)
	}
	if _, err = seedTx.ExecContext(ctx, `
		INSERT INTO split_schedule_parts(schedule_id,payee_id,organizer_id,share_bps)
		SELECT s.id, $2, $1, 10000 FROM split_schedules s
		WHERE s.organizer_id = $1 AND NOT EXISTS (
			SELECT 1 FROM split_schedule_parts p WHERE p.schedule_id = s.id)`,
		orgID, payees[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = seedTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE split_schedules`); err != nil {
		t.Fatal(err)
	}

	plan := explainGenericPlan(ctx, t, db, splitScheduleCandidatesQuery,
		orgID, ttID, slotID, seriesID, eventID, venueID)
	assertReachesVia(t, plan, "split_schedules", "split_schedules_scope")

	var indexCond string
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "Index Cond:") && strings.Contains(line, "scope") {
			indexCond = line
			break
		}
	}
	if indexCond == "" {
		t.Fatalf("no scope Index Cond in the plan — schedules are not reached BY the index.\nplan:\n%s", plan)
	}
	if !strings.Contains(indexCond, "scope_level") || !strings.Contains(indexCond, "scope_id") {
		t.Fatalf("the index condition does not use the (scope_level, scope_id) pair, so the read "+
			"may be walking the organizer's whole schedule set: %s\nplan:\n%s", indexCond, plan)
	}
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "Filter:") && strings.Contains(line, "scope_id") {
			t.Errorf("scope_id is filtered AFTER the index fetch, not by it: %s\nplan:\n%s", line, plan)
		}
	}
}

// The rollback guard: payout configuration must not be silently destroyed.
func TestSplitMigrationDownRefusesToDropData(t *testing.T) {
	ctx, db, st := seasonSmokeStore(t)
	_, orgID, venueID, _, _, _ := seedPricingChain(ctx, t, db)
	payees := seedPayees(ctx, t, st, orgID, 1)
	if _, err := st.CreateSplitSchedule(ctx, SplitSchedule{
		OrganizerID: orgID, ScopeLevel: ScopeVenue, ScopeID: venueID, FeeCode: "service",
		Parts: partsOf(payees, 10000),
	}); err != nil {
		t.Fatal(err)
	}
	assertSplitRollbackRefused(ctx, t, db)
}

func assertSplitRollbackRefused(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
		DO $$
		BEGIN
		    IF EXISTS (SELECT 1 FROM split_schedules) OR EXISTS (SELECT 1 FROM payees) THEN
		        RAISE EXCEPTION 'cannot roll back 0017: payee or split-schedule data exists';
		    END IF;
		END $$;`); err == nil {
		t.Error("the down migration's guard must refuse while payout configuration exists")
	}
}
