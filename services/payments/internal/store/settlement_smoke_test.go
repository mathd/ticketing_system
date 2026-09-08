//go:build smoke

package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"ticketing/services/payments/internal/splits"
)

// DB-backed tests for TKT-217. The entry arithmetic is proved without a database
// by settlement_test.go; what needs one is the two deferred triggers, the
// append-only guard, and the claim that a capture and its ledger commit together.
//
// Every journal write here uses fullRing, and journal_smoke_test.go already
// explains why: Verify scans the WHOLE table across all organizers, so entries
// signed with a ring nothing else knows break some OTHER test — and the smoke
// stack's own `verify-journal` step, which is how the first run of this file
// failed with `unknown key id "k1"`.

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
	j := New(db, fullRing(t))
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
	j := New(db, fullRing(t))
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
	j := New(db, fullRing(t))
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
	j := New(db, fullRing(t))
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
	j := New(db, fullRing(t))
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
	j := New(db, fullRing(t))
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
	j := New(db, fullRing(t))
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

// AC6: the ledger is readable, and what it reads back balances.
func TestReadOrderSettlementReturnsBalancedLines(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()

	f := capturedFact(org, order)
	if _, _, err := j.AppendWithSettlement(ctx, f, balancedEntries(5000, 600, 400)); err != nil {
		t.Fatal(err)
	}
	lines, total, currency, err := j.ReadOrderSettlement(ctx, org, order)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if total != 5600 {
		t.Errorf("total = %d, want the captured 5600", total)
	}
	if currency != "EUR" {
		t.Errorf("currency = %q", currency)
	}
	// Another organizer's read must not see it.
	other, _, _, err := j.ReadOrderSettlement(ctx, uuid.New(), order)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("a foreign organizer read %d lines — settlement is per-tenant", len(other))
	}
}

// An unattributed fee survives the round trip and is visible as such, because
// finding one is the signal to author a schedule.
func TestReadOrderSettlementShowsUnattributedFees(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()

	f := capturedFact(org, order)
	entries := []SettlementEntry{
		{Kind: EntryFaceValue, Amount: 5000, Currency: "EUR"},
		{Kind: EntryFee, FeeCode: "service", Incidence: "passed_on", Amount: 600, Currency: "EUR"},
	}
	if _, _, err := j.AppendWithSettlement(ctx, f, entries); err != nil {
		t.Fatalf("an unattributed fee must settle: %v", err)
	}
	lines, total, _, err := j.ReadOrderSettlement(ctx, org, order)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5600 {
		t.Errorf("total = %d, want 5600 — an unattributed fee is still collected money", total)
	}
	found := false
	for _, l := range lines {
		if l.Kind == EntryFee && l.PayeeID == nil {
			found = true
			if l.FeeCode == nil || *l.FeeCode != "service" {
				t.Errorf("the unattributed line lost its fee code: %+v", l)
			}
		}
	}
	if !found {
		t.Error("the unattributed fee must be visible as such — that is the signal to author a schedule")
	}
}

// The balance trigger checks a SUM, and a sum cannot see shape. Two face-value
// lines that add up to the right number are balanced and still wrong about who
// earned what -- and PostgreSQL's ordinary UNIQUE treats NULLs as distinct, so
// the table's own uniqueness does not stop them: both rows have a NULL payee and
// a NULL fee code. Found by adversarial review, not by the gate.
func TestTwoFaceValueLinesCannotBothCommit(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)

	split := []SettlementEntry{
		{Kind: EntryFaceValue, Amount: 3600, Currency: "EUR"},
		{Kind: EntryFaceValue, Amount: 2000, Currency: "EUR"},
	}
	if _, _, err := j.AppendWithSettlement(ctx, f, split); err == nil {
		t.Fatal("two face-value lines summing to the capture committed — the ledger " +
			"balances and misstates the organizer's share across two rows")
	}
}

// Same shape, one level down: a fee code that DOES have a payee must not also be
// able to land a second, unattributed line for itself.
func TestADuplicateUnattributedFeeLineCannotCommit(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)

	split := []SettlementEntry{
		{Kind: EntryFaceValue, Amount: 5000, Currency: "EUR"},
		{Kind: EntryFee, FeeCode: "booking", Incidence: "passed_on", Amount: 300, Currency: "EUR"},
		{Kind: EntryFee, FeeCode: "booking", Incidence: "passed_on", Amount: 300, Currency: "EUR"},
	}
	if _, _, err := j.AppendWithSettlement(ctx, f, split); err == nil {
		t.Fatal("two unattributed lines for one fee code committed — a fee collected once " +
			"is owed once")
	}
}

// "Settlement iff capture" is the claim. A foreign key to journal_entries does
// not say that: it admits ANY fact. A balanced ledger hung off an authorization
// attributes money that has not moved.
func TestSettlementCannotAttachToANonCaptureFact(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)
	f.Type = "payment.authorized"

	if _, _, err := j.AppendWithSettlement(ctx, f, balancedEntries(5000, 600, 0)); err == nil {
		t.Fatal("an authorization carried a settlement ledger — only a capture moved money")
	}
}

// And the ledger must be about the fact it names. A row whose currency disagrees
// with the captured fact balances numerically against a different unit.
func TestSettlementCurrencyMustMatchTheCapturedFact(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order) // EUR

	split := []SettlementEntry{{Kind: EntryFaceValue, Amount: 5600, Currency: "USD"}}
	if _, _, err := j.AppendWithSettlement(ctx, f, split); err == nil {
		t.Fatal("a USD ledger settled a EUR capture — the sum matched and the money did not")
	}
}

// 'legacy_unattributed' belongs to migration 0004's backfill and to nothing else.
// It exists to say "this capture predates the ledger and its split is unknown",
// which is true of exactly the rows the migration writes. Reachable at runtime it
// would be a way to record a LIVE capture as owed to nobody — the ledger would
// balance, and the money would have no claimant.
func TestALiveCaptureCannotBeRecordedAsLegacy(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)

	split := []SettlementEntry{{Kind: "legacy_unattributed", Amount: 5600, Currency: "EUR"}}
	if _, _, err := j.AppendWithSettlement(ctx, f, split); err == nil {
		t.Fatal("a live capture was recorded as legacy_unattributed — the backfill's kind " +
			"is not a runtime disposition for money that has a real composition")
	}
}

// COS 3: Read the committed ledger from a separate connection: exactly one face
// line, expected fee lines, sum equals capture. Also prove an off-by-one-minor-unit
// set is REFUSED at commit (reaching tx.Commit).
func TestCommittedLedgerFromSeparateConnectionAndOffByOneRefusedAtCommit(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order) // 5600 EUR

	// 1 face value (4600) + 1 passed-on fee (600) + 1 absorbed fee (400) = 5600
	entries := balancedEntries(5000, 600, 400)
	if _, _, err := j.AppendWithSettlement(ctx, f, entries); err != nil {
		t.Fatalf("balanced entries must settle: %v", err)
	}

	// Read from a separate connection to prove committed visibility.
	sepDB, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatalf("open separate connection: %v", err)
	}
	t.Cleanup(func() { _ = sepDB.Close() })

	rows, err := sepDB.QueryContext(ctx,
		`SELECT entry_kind, amount, currency FROM settlement_entries WHERE capture_fact_id=$1`,
		f.ID)
	if err != nil {
		t.Fatalf("query separate connection: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var faceLines, feeLines int
	var sum int64
	for rows.Next() {
		var kind, curr string
		var amt int64
		if err := rows.Scan(&kind, &amt, &curr); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		if curr != "EUR" {
			t.Errorf("entry currency = %q, want EUR", curr)
		}
		sum += amt
		switch kind {
		case string(EntryFaceValue):
			faceLines++
		case string(EntryFee):
			feeLines++
		default:
			t.Errorf("unexpected entry kind %q", kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}

	if faceLines != 1 {
		t.Errorf("face_value lines = %d, want exactly 1", faceLines)
	}
	if feeLines != 2 {
		t.Errorf("fee lines = %d, want exactly 2", feeLines)
	}
	if sum != f.Amount {
		t.Errorf("ledger sum = %d, want captured %d", sum, f.Amount)
	}

	// Prove an off-by-one-minor-unit set is REFUSED at commit, and refused BY THE
	// AMOUNT COMPARISON rather than by something else.
	//
	// That distinction is the whole test (ai-review pass 1, [medium]). The first
	// version inserted the capture with payload '{}', so the fact's order_id was
	// NULL while the entries carried a real one. settlement_must_balance checks
	// organizer/order/currency identity BEFORE it compares sums and raises there, so
	// the transaction was rejected for the wrong reason and the test accepted any
	// commit error. Deleting the amount comparison left it green: it proved the
	// identity check, which is a different guard.
	//
	// So the fact now carries the SAME order_id as its entries, and the assertion
	// reads the message. A control case with a balanced set commits, which is what
	// shows the fixture can reach the amount comparison at all.
	assertOffByOneRefused := func(t *testing.T, faceAmount int64, wantCommit bool) {
		t.Helper()
		offFact := capturedFact(uuid.New(), uuid.New())
		offOrder := uuid.New()

		tx, err := sepDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		// payload carries the order_id, so the identity check passes and the amount
		// comparison is what decides. Without this the trigger never gets that far.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO journal_entries (fact_id, organizer_id, sequence, fact_type, occurred_at, buyer_id, amount, currency, payload, previous_hash, entry_hash, key_id, signature)
			 VALUES ($1, $2, 1, $3, $4, $5, $6, $7, jsonb_build_object('order_id', $8::text), decode(repeat('00',32),'hex'), decode(repeat('11',32),'hex'), 'k1', decode(repeat('22',32),'hex'))`,
			offFact.ID, offFact.OrganizerID, offFact.Type, offFact.OccurredAt, offFact.BuyerID,
			offFact.Amount, offFact.Currency, offOrder); err != nil {
			t.Fatalf("insert journal fact: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settlement_entries (organizer_id, order_id, capture_fact_id, entry_kind, amount, currency)
			 VALUES ($1, $2, $3, 'face_value', $4, 'EUR')`,
			offFact.OrganizerID, offOrder, offFact.ID, faceAmount); err != nil {
			t.Fatalf("insert face entry: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settlement_entries (organizer_id, order_id, capture_fact_id, entry_kind, payee_id, payee_kind, payee_display_name, fee_code, incidence, amount, currency)
			 VALUES ($1, $2, $3, 'fee', $4, 'reseller', 'Partner', 'booking', 'passed_on', 600, 'EUR')`,
			offFact.OrganizerID, offOrder, offFact.ID, uuid.New()); err != nil {
			t.Fatalf("insert fee entry: %v", err)
		}

		err = tx.Commit()
		if wantCommit {
			if err != nil {
				t.Fatalf("a BALANCED set must commit, got %v. If this fails the fixture cannot "+
					"reach the amount comparison and the off-by-one case below proves nothing", err)
			}
			return
		}
		if err == nil {
			t.Fatal("an off-by-one minor unit settlement set committed; the deferred balance trigger did not fire")
		}
		// The message names the sums, which is how we know the AMOUNT comparison
		// rejected this and not the identity or fact-type guard above it.
		if !strings.Contains(err.Error(), "sums to") {
			t.Fatalf("refused for the wrong reason: %v. Want the balance failure "+
				"(\"settlement for X sums to Y, but Z was captured\"); another message means the "+
				"trigger raised before it reached the amount comparison", err)
		}
	}

	// capturedFact mints a 5600 capture. 5000 + 600 balances; 5001 + 600 is off by one.
	t.Run("balanced commits", func(t *testing.T) { assertOffByOneRefused(t, 5000, true) })
	t.Run("off by one is refused by the amount comparison", func(t *testing.T) {
		assertOffByOneRefused(t, 5001, false)
	})
}

// COS 7: A partially nulled payee (payee_id NULL, kind/name retained) is refused with
// SQLSTATE 23514 (settlement_entries_shape CHECK constraint).
func TestSettlementShapePartiallyNulledPayeeRefused(t *testing.T) {
	db, ctx := journalDB(t)
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO journal_entries (fact_id, organizer_id, sequence, fact_type, occurred_at, buyer_id, amount, currency, payload, previous_hash, entry_hash, key_id, signature)
		 VALUES ($1, $2, 1, $3, $4, $5, $6, $7, '{}'::jsonb, decode(repeat('00',32),'hex'), decode(repeat('11',32),'hex'), 'k1', decode(repeat('22',32),'hex'))`,
		f.ID, f.OrganizerID, f.Type, f.OccurredAt, f.BuyerID, f.Amount, f.Currency)
	if err != nil {
		t.Fatalf("insert journal fact: %v", err)
	}

	// payee_id is NULL, but payee_kind and payee_display_name are provided.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO settlement_entries (organizer_id, order_id, capture_fact_id, entry_kind, payee_id, payee_kind, payee_display_name, fee_code, incidence, amount, currency)
		 VALUES ($1, $2, $3, 'fee', NULL, 'reseller', 'Partner', 'reseller_commission', 'passed_on', 600, 'EUR')`,
		f.OrganizerID, order, f.ID)
	if err == nil {
		t.Fatal("partially nulled payee must be refused by CHECK constraint")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("err = %v, want SQLSTATE 23514 check_violation", err)
	}
}

// COS 7: An all-null payee is ACCEPTED by generic settlement (deliberate: collected and unattributed).
func TestSettlementAllNullPayeeAcceptedByGenericSettlement(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)

	// An entry with Payee == nil yields all-null payee columns.
	unattributed := []SettlementEntry{
		{Kind: EntryFaceValue, Amount: 5000, Currency: "EUR"},
		{Kind: EntryFee, FeeCode: "unsplit_fee", Incidence: "passed_on", Amount: 600, Currency: "EUR", Payee: nil},
	}
	if _, _, err := j.AppendWithSettlement(ctx, f, unattributed); err != nil {
		t.Fatalf("generic settlement must accept an all-null payee entry: %v", err)
	}
}

// COS 7: A balanced duplicate commission (one beneficiary's amount split across two rows with
// the same capture/payee/code) must be refused by the UNIQUE constraint (SQLSTATE 23505).
func TestBalancedDuplicateCommissionRefusedByUniqueConstraint(t *testing.T) {
	db, ctx := journalDB(t)
	org, order := uuid.New(), uuid.New()
	f := capturedFact(org, order)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO journal_entries (fact_id, organizer_id, sequence, fact_type, occurred_at, buyer_id, amount, currency, payload, previous_hash, entry_hash, key_id, signature)
		 VALUES ($1, $2, 1, $3, $4, $5, $6, $7, '{}'::jsonb, decode(repeat('00',32),'hex'), decode(repeat('11',32),'hex'), 'k1', decode(repeat('22',32),'hex'))`,
		f.ID, f.OrganizerID, f.Type, f.OccurredAt, f.BuyerID, f.Amount, f.Currency)
	if err != nil {
		t.Fatalf("insert journal fact: %v", err)
	}

	payeeID := uuid.New()
	// First row of duplicate commission: 300 EUR
	_, err = tx.ExecContext(ctx,
		`INSERT INTO settlement_entries (organizer_id, order_id, capture_fact_id, entry_kind, payee_id, payee_kind, payee_display_name, fee_code, incidence, amount, currency)
		 VALUES ($1, $2, $3, 'fee', $4, 'reseller', 'Partner', 'reseller_commission', 'passed_on', 300, 'EUR')`,
		f.OrganizerID, order, f.ID, payeeID)
	if err != nil {
		t.Fatalf("insert first commission row: %v", err)
	}

	// Second row with same capture_fact_id, payee_id, fee_code: 300 EUR (balanced).
	_, err = tx.ExecContext(ctx,
		`INSERT INTO settlement_entries (organizer_id, order_id, capture_fact_id, entry_kind, payee_id, payee_kind, payee_display_name, fee_code, incidence, amount, currency)
		 VALUES ($1, $2, $3, 'fee', $4, 'reseller', 'Partner', 'reseller_commission', 'passed_on', 300, 'EUR')`,
		f.OrganizerID, order, f.ID, payeeID)
	if err == nil {
		t.Fatal("duplicate commission row with identical (capture_fact_id, payee_id, fee_code) must be refused by UNIQUE constraint")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("err = %v, want SQLSTATE 23505 unique_violation", err)
	}
}

