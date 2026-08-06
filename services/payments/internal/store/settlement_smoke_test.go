//go:build smoke

package store

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/splits"
)

// DB-backed tests for TKT-217. The entry arithmetic is proved without a database
// by settlement_test.go; what needs one is the two deferred triggers, the
// append-only guard, and the claim that a capture and its ledger commit together.

func capturedFact(org, order uuid.UUID) Fact {
	return Fact{
		ID: uuid.New(), OrganizerID: org, Type: "payment.captured",
		OccurredAt: time.Now().UTC(), BuyerID: uuid.New(), Amount: 5600, Currency: "EUR",
		Payload: map[string]string{"order_id": order.String()},
	}
}

func balancedEntries(amountFace, passedOn, absorbed int64) []SettlementEntry {
	payee := PayeeRef{ID: uuid.New(), Kind: "venue", DisplayName: "The venue"}
	out := []SettlementEntry{
		{Kind: EntryFaceValue, Amount: amountFace - absorbed, Currency: "EUR"},
	}
	if passedOn > 0 {
		out = append(out, SettlementEntry{Kind: EntryFee, Payee: &payee, FeeCode: "booking",
			Incidence: "passed_on", Amount: passedOn, Currency: "EUR"})
	}
	if absorbed > 0 {
		out = append(out, SettlementEntry{Kind: EntryFee, Payee: &payee, FeeCode: "service",
			Incidence: "absorbed", Amount: absorbed, Currency: "EUR"})
	}
	return out
}

// A capture and its ledger commit TOGETHER, and the ledger balances against the
// money that moved.
func TestCaptureAndSettlementCommitTogether(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	org, order := uuid.New(), uuid.New()

	f := capturedFact(org, order)
	if _, _, err := j.AppendWithSettlement(ctx, f, balancedEntries(5000, 600, 400)); err != nil {
		t.Fatalf("a balanced capture must commit: %v", err)
	}
	var total int64
	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(amount),0), count(*) FROM settlement_entries WHERE capture_fact_id=$1`,
		f.ID).Scan(&total, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Errorf("got %d entries, want 3 (organizer + passed-on + absorbed)", rows)
	}
	if total != 5600 {
		t.Errorf("entries sum to %d, want the captured 5600", total)
	}
}

// The invariant in the other direction, and the one that makes it a property of
// the DATABASE: a captured fact with no settlement cannot commit.
//
// FIXTURE NOTE: the fact must be otherwise entirely valid — right type, right
// amount, real payload — so the only thing that can refuse it is the deferred
// trigger. A malformed fact would be rejected by validate() and prove nothing.
func TestCapturedFactWithoutSettlementCannotCommit(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	org, order := uuid.New(), uuid.New()

	f := capturedFact(org, order)
	if _, _, err := j.Append(ctx, f); err == nil {
		t.Fatal("a payment.captured fact with no settlement entries must not commit — " +
			"otherwise the invariant holds only for captures that happened to write them")
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM journal_entries WHERE fact_id=$1`, f.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the refused fact was committed anyway (%d rows)", n)
	}
}

// A fact that is NOT a capture is unaffected — the trigger must not turn every
// journal append into a settlement requirement.
func TestNonCaptureFactsNeedNoSettlement(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	f := Fact{ID: uuid.New(), OrganizerID: uuid.New(), Type: "order.created",
		OccurredAt: time.Now().UTC(), BuyerID: uuid.New(), Amount: 100, Currency: "EUR",
		Payload: map[string]string{"order_id": uuid.NewString()}}
	if _, _, err := j.Append(ctx, f); err != nil {
		t.Fatalf("an ordinary fact must still append: %v", err)
	}
	_ = db
}

// An entry set that does not sum to the captured amount is refused at COMMIT.
//
// FIXTURE NOTE: every row is individually valid — right shape, right currency,
// non-negative fee, real payee — and the amounts are the ONLY thing wrong. A
// fixture that also broke the shape CHECK would fail for the wrong reason and
// pass against a build with no balance trigger at all.
func TestUnbalancedSettlementSetIsRefusedAtCommit(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	org, order := uuid.New(), uuid.New()

	f := capturedFact(org, order) // captured 5600
	short := balancedEntries(5000, 600, 400)
	short[0].Amount = 4599 // one cent short, everything else valid
	if _, _, err := j.AppendWithSettlement(ctx, f, short); err == nil {
		t.Fatal("a settlement set one cent short of the capture must not commit")
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM settlement_entries WHERE capture_fact_id=$1`, f.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the refused entries survived (%d rows)", n)
	}
}

// Replay writes nothing new. The fact id is deterministic, so a second append of
// the same capture takes the existing-fact branch before reaching either write.
func TestReplayedCaptureWritesNoSecondLedger(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	org, order := uuid.New(), uuid.New()

	f := capturedFact(org, order)
	entries := balancedEntries(5000, 600, 400)
	if _, _, err := j.AppendWithSettlement(ctx, f, entries); err != nil {
		t.Fatal(err)
	}
	// The same fact id, exactly as a retried charge would produce.
	_, replay, err := j.AppendWithSettlement(ctx, f, entries)
	if err != nil {
		t.Fatalf("a replay must succeed, not error: %v", err)
	}
	if !replay {
		t.Error("the second append must report replay")
	}
	var total int64
	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(amount),0), count(*) FROM settlement_entries WHERE capture_fact_id=$1`,
		f.ID).Scan(&total, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || total != 5600 {
		t.Errorf("after replay: %d rows summing to %d, want 3 and 5600 — the ledger was "+
			"double-written", rows, total)
	}
}

// Append-only, tested the way journal_entries tests it: each mutation separately,
// including TRUNCATE, which fires no row-level trigger.
func TestSettlementEntriesAreAppendOnly(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)
	if _, _, err := j.AppendWithSettlement(ctx, f, balancedEntries(5000, 600, 400)); err != nil {
		t.Fatal(err)
	}

	for name, stmt := range map[string]string{
		"update":   `UPDATE settlement_entries SET amount = amount + 1 WHERE capture_fact_id=$1`,
		"delete":   `DELETE FROM settlement_entries WHERE capture_fact_id=$1`,
		"truncate": `TRUNCATE settlement_entries`,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if name == "truncate" {
				_, err = db.ExecContext(ctx, stmt)
			} else {
				_, err = db.ExecContext(ctx, stmt, f.ID)
			}
			if err == nil {
				t.Errorf("%s must be refused — settlement entries are append-only", name)
			}
		})
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM settlement_entries WHERE capture_fact_id=$1`, f.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("the refused mutations still changed the ledger: %d rows left, want 3", n)
	}
}

// End to end from a plan: build the entries and settle them, so the pure
// arithmetic and the database agree about the same sale.
func TestSettleFromAPlanBalances(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, mustRing(t, "k1", "0123456789abcdef0123456789abcdef", ""))
	org, order := uuid.New(), uuid.New()

	payee := uuid.New()
	plan := SettlementPlan{
		FaceValue: 5000, PassedOn: 600, Absorbed: 0, TotalAmount: 5600, Currency: "EUR",
		Fees: []FeeLine{{
			FeeCode: "booking", Incidence: "passed_on", Amount: 600, Currency: "EUR",
			Shares: []splits.Share{{PayeeID: payee, ShareBps: 10000}},
			Payees: map[uuid.UUID]PayeeRef{payee: {ID: payee, Kind: "venue", DisplayName: "V"}},
		}},
	}
	entries, err := BuildSettlementEntries(plan, 5600)
	if err != nil {
		t.Fatal(err)
	}
	f := capturedFact(org, order)
	if _, _, err := j.AppendWithSettlement(ctx, f, entries); err != nil {
		t.Fatalf("entries built from a coherent plan must settle: %v", err)
	}
	var total int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(amount),0) FROM settlement_entries WHERE capture_fact_id=$1`,
		f.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 5600 {
		t.Errorf("ledger sums to %d, want 5600", total)
	}
}
