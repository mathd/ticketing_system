package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/splits"
)

// The ledger's one obligation: the entries sum to the money that actually moved.
//
// The identity being proved is (face − absorbed) + passed_on + absorbed = captured,
// and the absorbed term is the one that makes it interesting: it appears on both
// sides, reducing the organizer's line and adding a payee's, so a build that
// simply ignores absorbed fees still balances against the CHARGE while being
// wrong about who earned what.

func payeeID(n int) uuid.UUID {
	return uuid.MustParse("00000000-0000-0000-0000-00000000000" + string("0123456789abcdef"[n%16]))
}

func feeLine(code, incidence string, amount int64, shares ...int32) FeeLine {
	f := FeeLine{FeeCode: code, Incidence: incidence, Amount: amount, Currency: "EUR",
		Payees: map[uuid.UUID]PayeeRef{}}
	for i, bps := range shares {
		id := payeeID(i)
		f.Shares = append(f.Shares, splits.Share{PayeeID: id, ShareBps: bps})
		f.Payees[id] = PayeeRef{ID: id, Kind: "venue", DisplayName: "payee"}
	}
	return f
}

func sumOf(t *testing.T, entries []SettlementEntry) int64 {
	t.Helper()
	total, err := sumEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// The worked case from the plan, asserted in full: an absorbed fee must reduce
// the organizer's line AND remain owed to a payee, and the set must still sum to
// what the buyer paid.
func TestBuildSettlementEntriesAbsorbedFeeBalances(t *testing.T) {
	plan := SettlementPlan{
		FaceValue: 5000, PassedOn: 600, Absorbed: 400, TotalAmount: 5600, Currency: "EUR",
		Fees: []FeeLine{
			feeLine("booking", "passed_on", 600, 6000, 4000),
			feeLine("service", "absorbed", 400, 10000),
		},
	}
	entries, err := BuildSettlementEntries(plan, 5600)
	if err != nil {
		t.Fatal(err)
	}
	if got := sumOf(t, entries); got != 5600 {
		t.Errorf("entries sum to %d, want the captured 5600", got)
	}

	var organizer, fees int64
	var organizerLines int
	for _, e := range entries {
		if e.Kind == EntryFaceValue {
			organizer += e.Amount
			organizerLines++
			continue
		}
		fees += e.Amount
	}
	if organizerLines != 1 {
		t.Errorf("want exactly one organizer line, got %d", organizerLines)
	}
	// The heart of it: 5000 face less the 400 they absorbed.
	if organizer != 4600 {
		t.Errorf("organizer line = %d, want 4600 (face 5000 − absorbed 400). 5000 means the "+
			"absorbed fee was recorded as owed to a payee AND left with the organizer", organizer)
	}
	// And both fees are owed, not just the one the buyer saw.
	if fees != 1000 {
		t.Errorf("fee lines total %d, want 1000 (600 passed-on + 400 absorbed). 600 means the "+
			"absorbed fee was never attributed to anyone", fees)
	}
	// The passed-on fee's split, allocated: 60/40 of 600.
	byPayee := map[uuid.UUID]int64{}
	for _, e := range entries {
		if e.Kind == EntryFee && e.FeeCode == "booking" {
			byPayee[e.Payee.ID] += e.Amount
		}
	}
	if byPayee[payeeID(0)] != 360 || byPayee[payeeID(1)] != 240 {
		t.Errorf("booking split = %v, want 360/240", byPayee)
	}
}

// The identity holds for every shape, including the ones nobody designs for.
func TestBuildSettlementEntriesIdentityHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		face, passed, absorbed int64
		fees                   []FeeLine
	}{
		"no fees at all": {face: 5000, passed: 0, absorbed: 0},
		"passed-on only": {face: 5000, passed: 600, absorbed: 0,
			fees: []FeeLine{feeLine("booking", "passed_on", 600, 10000)}},
		"absorbed only": {face: 5000, passed: 0, absorbed: 400,
			fees: []FeeLine{feeLine("service", "absorbed", 400, 10000)}},
		"a fee of zero is still attributed": {face: 5000, passed: 0, absorbed: 0,
			fees: []FeeLine{feeLine("service", "passed_on", 0, 3333, 3333, 3334)}},
		"a cent split three ways": {face: 5000, passed: 1, absorbed: 0,
			fees: []FeeLine{feeLine("booking", "passed_on", 1, 3333, 3333, 3334)}},
		// The organizer absorbed more than they earned. The identity still
		// holds and the ledger says so rather than refusing the sale.
		"absorbed exceeds the face value": {face: 100, passed: 0, absorbed: 400,
			fees: []FeeLine{feeLine("service", "absorbed", 400, 10000)}},
		"a zero-value sale": {face: 0, passed: 0, absorbed: 0},
	} {
		t.Run(name, func(t *testing.T) {
			captured := tc.face + tc.passed
			plan := SettlementPlan{FaceValue: tc.face, PassedOn: tc.passed, Absorbed: tc.absorbed,
				TotalAmount: captured, Currency: "EUR", Fees: tc.fees}
			entries, err := BuildSettlementEntries(plan, captured)
			if err != nil {
				t.Fatalf("a coherent plan must settle: %v", err)
			}
			if got := sumOf(t, entries); got != captured {
				t.Errorf("entries sum to %d, want the captured %d", got, captured)
			}
		})
	}
}

// A negative organizer line is a TRUE statement about a misconfigured sale, and
// refusing it here would let payout configuration refuse a purchase.
func TestBuildSettlementEntriesAllowsANegativeOrganizerLine(t *testing.T) {
	plan := SettlementPlan{FaceValue: 100, PassedOn: 0, Absorbed: 400, TotalAmount: 100,
		Currency: "EUR", Fees: []FeeLine{feeLine("service", "absorbed", 400, 10000)}}
	entries, err := BuildSettlementEntries(plan, 100)
	if err != nil {
		t.Fatalf("an organizer who absorbed more than they earned is a real sale: %v", err)
	}
	var organizer int64
	for _, e := range entries {
		if e.Kind == EntryFaceValue {
			organizer = e.Amount
		}
	}
	if organizer != -300 {
		t.Errorf("organizer line = %d, want −300 — the ledger records what is true, it does not "+
			"police configuration", organizer)
	}
	if got := sumOf(t, entries); got != 100 {
		t.Errorf("entries sum to %d, want the captured 100", got)
	}
}

// Every zero-amount payee keeps its line (ADR-046 §2). "Owed nothing on this
// sale" and "not a payee of this sale" are different facts.
func TestBuildSettlementEntriesKeepsZeroAmountPayees(t *testing.T) {
	plan := SettlementPlan{FaceValue: 5000, TotalAmount: 5000, Currency: "EUR",
		Fees: []FeeLine{feeLine("service", "passed_on", 0, 3333, 3333, 3334)}}
	entries, err := BuildSettlementEntries(plan, 5000)
	if err != nil {
		t.Fatal(err)
	}
	fees := 0
	for _, e := range entries {
		if e.Kind == EntryFee {
			fees++
			if e.Amount != 0 {
				t.Errorf("payee %s got %d, want 0", e.Payee.ID, e.Amount)
			}
		}
	}
	if fees != 3 {
		t.Errorf("got %d fee lines, want 3 — a payee owed zero is still a payee", fees)
	}
}

// A plan this build cannot settle is refused BEFORE the provider is called.
func TestBuildSettlementEntriesRefusesUnusablePlans(t *testing.T) {
	base := func() SettlementPlan {
		return SettlementPlan{FaceValue: 5000, PassedOn: 600, Absorbed: 0, TotalAmount: 5600,
			Currency: "EUR", Fees: []FeeLine{feeLine("booking", "passed_on", 600, 10000)}}
	}
	for name, mutate := range map[string]func(*SettlementPlan){

		"a persisted split that does not balance": func(p *SettlementPlan) {
			p.Fees[0] = feeLine("booking", "passed_on", 600, 3333, 3333, 3333)
		},
		"a fee in another currency": func(p *SettlementPlan) {
			p.Fees[0].Currency = "USD"
		},
		"an unknown incidence": func(p *SettlementPlan) {
			p.Fees[0].Incidence = "shared"
		},
		"fees that disagree with the plan's own totals": func(p *SettlementPlan) {
			p.PassedOn = 999
		},
		"face plus passed-on that is not the total": func(p *SettlementPlan) {
			p.FaceValue = 4000
		},
		"a payee with no snapshotted identity": func(p *SettlementPlan) {
			p.Fees[0].Payees = map[uuid.UUID]PayeeRef{}
		},
		"a negative fee": func(p *SettlementPlan) {
			p.Fees[0].Amount = -1
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := base()
			mutate(&plan)
			if _, err := BuildSettlementEntries(plan, 5600); !errors.Is(err, ErrSettlementPlanUnusable) {
				t.Errorf("want ErrSettlementPlanUnusable, got %v", err)
			}
		})
	}
}

// Commerce and payments must agree about what was charged, or one of them is
// settling a different sale.
func TestBuildSettlementEntriesRefusesAProviderMismatch(t *testing.T) {
	plan := SettlementPlan{FaceValue: 5000, PassedOn: 600, TotalAmount: 5600, Currency: "EUR",
		Fees: []FeeLine{feeLine("booking", "passed_on", 600, 10000)}}
	if _, err := BuildSettlementEntries(plan, 5599); !errors.Is(err, ErrSettlementPlanUnusable) {
		t.Errorf("a captured amount that differs from the plan must be refused, got %v", err)
	}
}

// A fee with no split is COLLECTED AND UNATTRIBUTED, not refused.
//
// The first implementation refused it, and the gate showed what that meant:
// TKT-215 shipped fees before TKT-216 shipped split schedules, so every fee sold
// in that window has no schedule — and refusing failed those sales at CHECKOUT,
// after the buyer had committed. Breaking shipped sales to enforce a
// configuration rule is the wrong trade, and the same one this epic already
// declined twice.
//
// The ledger still balances; the gap is recorded rather than hidden.
func TestBuildSettlementEntriesRecordsUnattributedFees(t *testing.T) {
	unsplit := FeeLine{FeeCode: "service", Incidence: "passed_on", Amount: 600,
		Currency: "EUR", Payees: map[uuid.UUID]PayeeRef{}}
	plan := SettlementPlan{FaceValue: 5000, PassedOn: 600, Absorbed: 0, TotalAmount: 5600,
		Currency: "EUR", Fees: []FeeLine{unsplit}}

	entries, err := BuildSettlementEntries(plan, 5600)
	if err != nil {
		t.Fatalf("a fee with no split must still settle — refusing breaks sales made before "+
			"split schedules existed: %v", err)
	}
	if got := sumOf(t, entries); got != 5600 {
		t.Errorf("entries sum to %d, want the captured 5600", got)
	}
	var unattributed int
	for _, e := range entries {
		if e.Kind == EntryFee && e.Payee == nil {
			unattributed++
			if e.FeeCode != "service" || e.Amount != 600 {
				t.Errorf("unattributed entry = %+v, want the 600 service fee", e)
			}
		}
	}
	if unattributed != 1 {
		t.Errorf("got %d unattributed fee entries, want 1 — the money was collected, so it must "+
			"appear in the ledger even though nobody was configured to receive it", unattributed)
	}
}

// A ZERO-amount unsplit fee is recorded too. It changes no total, so nothing
// else in the builder would notice it going missing.
func TestBuildSettlementEntriesRecordsAZeroUnattributedFee(t *testing.T) {
	plan := SettlementPlan{FaceValue: 5600, PassedOn: 0, TotalAmount: 5600, Currency: "EUR",
		Fees: []FeeLine{{FeeCode: "service", Incidence: "passed_on", Amount: 0,
			Currency: "EUR", Payees: map[uuid.UUID]PayeeRef{}}}}
	entries, err := BuildSettlementEntries(plan, 5600)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == EntryFee && e.Payee == nil && e.FeeCode == "service" {
			found = true
		}
	}
	if !found {
		t.Error("a zero-amount unattributed fee must still be recorded — it affects no total, " +
			"so nothing else would notice it vanishing")
	}
}
