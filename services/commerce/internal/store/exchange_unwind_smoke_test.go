//go:build smoke

package store

// TKT-255. The durable half of the operator path out of a wedged exchange.
//
// EVERY TEST HERE IS AT THE SMOKE TIER AGAINST REAL POSTGRESQL, and that is the point
// rather than a convenience. The three things this ticket has to prove are all decided in
// SQL or by a constraint, and an in-memory fake would be enforcing in Go exactly what the
// shipped schema has to enforce:
//
//   - "the source order is free for a corrected attempt" is `order_exchanges_one_per_source`;
//   - "the refund path stops treating it as live" is `BindOrderRefund`'s bare `count(*)`;
//   - "the evidence cannot lie about the pre-state" is a set of CHECK constraints in 0024.
//
// A test asserting any of the three against a fake would prove the fake and the handler
// agree, and nothing else.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// seedWedged binds a real exchange through BindOrderExchange and leaves it unsettled —
// which is precisely the wedged shape, because an exchange that never settles is exactly
// what a terminal target claim produces.
//
// It binds through the REAL function rather than INSERTing a row, for the reason
// exchange_resume_smoke_test.go records for its own fixture: a row written by hand is a
// precondition the code under test never created, and the constraints that would have
// rejected an impossible shape go unexercised.
func seedWedged(t *testing.T, db *sql.DB, ctx context.Context, key string) (Completion, Exchange) {
	t.Helper()
	c, _ := seedCompleted(t, db, ctx, "src-"+key, 2, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, key, uuid.New()))
	if err != nil {
		t.Fatalf("bind exchange: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_exchange_unwinds WHERE exchange_id=$1`, ex.ID)
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE id=$1`, ex.ID)
	})
	return c, ex
}

// withBasis records a real basis on a bound exchange, producing the OTHER wedge shape: the
// one where money may have moved. `delta` is signed, and the target total is derived from it
// so that 0010's `order_exchanges_delta_is_the_difference` CHECK is satisfied by
// construction rather than by luck.
func withBasis(t *testing.T, db *sql.DB, ctx context.Context, c Completion, ex Exchange, delta int64) ExchangeBasis {
	t.Helper()
	sourceTotal := int64(2000) // 2 × 1000, from seedCompleted
	targetTotal := sourceTotal + delta
	if targetTotal%2 != 0 {
		t.Fatalf("test bug: target total %d must divide by the quantity of 2", targetTotal)
	}
	basis := ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: targetTotal, DeltaAmount: delta, TargetUnitAmount: targetTotal / 2,
		PriceSnapshot: []byte(`{"resolver_version":1}`),
	}
	if _, _, err := RecordExchangeBasis(ctx, db, c.OrganizerID, ex.ID, basis); err != nil {
		t.Fatalf("record basis: %v", err)
	}
	return basis
}

// exchangeRowCount is the observable that says whether the binding is still there.
func exchangeRowCount(t *testing.T, db *sql.DB, ctx context.Context, sourceOrder uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_exchanges WHERE source_order_id=$1`, sourceOrder).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// unwindRowCount counts the evidence.
func unwindRowCount(t *testing.T, db *sql.DB, ctx context.Context, exchangeID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_exchange_unwinds WHERE exchange_id=$1`, exchangeID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------------------
// COS 1 — the unwind frees the order, in BOTH directions, which are two separate facts.
// ---------------------------------------------------------------------------------------

// The binding is removed and BOTH consequences are observed independently.
//
// Two assertions and not one flag, because "the source order is free" and "the refund path
// stops treating it as live" are enforced by two different mechanisms — a unique index and a
// count in BindOrderRefund — and an implementation could satisfy either without the other.
// Asserting only that the row is gone would prove neither: it would be a statement about the
// DELETE, which is the thing under test, rather than about what the DELETE was for.
func TestUnwindFreesTheSourceOrderForBothAnExchangeAndARefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "unwind-frees")

	// The order is genuinely stuck in both directions BEFORE the unwind. Without this the
	// test could pass against a system where nothing was ever blocked.
	if _, err := BindOrderExchange(ctx, db, exchangeRequest(c, "corrected-first-try", uuid.New())); !errors.Is(err, ErrOrderNotExchangeable) {
		t.Fatalf("a second exchange answered %v, want ErrOrderNotExchangeable — the fixture is not "+
			"wedged and the rest of this test would prove nothing", err)
	}
	if _, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: 1,
		IdempotencyKey: "refund-first-try", Actor: "a", Reason: "r",
	}); !errors.Is(err, ErrOrderNotRefundable) {
		t.Fatalf("a refund answered %v, want ErrOrderNotRefundable", err)
	}

	if err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "target claim released by hand", false); err != nil {
		t.Fatalf("unwind: %v", err)
	}

	if n := exchangeRowCount(t, db, ctx, c.OrderID); n != 0 {
		t.Errorf("order_exchanges rows = %d, want 0", n)
	}
	// CONSEQUENCE ONE: order_exchanges_one_per_source is free.
	corrected, err := BindOrderExchange(ctx, db, exchangeRequest(c, "corrected", uuid.New()))
	if err != nil {
		t.Fatalf("a corrected exchange still refused after the unwind: %v. The unique index on "+
			"source_order_id is what blocks it, so this is the assertion that proves the index is free", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_exchanges WHERE id=$1`, corrected.ID) })

	// CONSEQUENCE TWO: the refund path's count is satisfied. Asserted on a SECOND source
	// order, because the corrected exchange above has now re-blocked this one — which is
	// itself the correct behaviour and is why the two consequences cannot share a fixture.
	c2, ex2 := seedWedged(t, db, ctx, "unwind-frees-refund")
	if err := UnwindWedgedExchange(ctx, db, c2.OrganizerID, ex2.ID, "target claim expired", false); err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if _, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: c2.OrderID, OrganizerID: c2.OrganizerID, Quantity: 1,
		IdempotencyKey: "refund-after", Actor: "a", Reason: "r",
	}); err != nil {
		t.Fatalf("a refund still refused after the unwind: %v. BindOrderRefund counts ANY "+
			"order_exchanges row for the source with no state predicate, so this is the assertion "+
			"that proves the count is satisfied", err)
	}
}

// Both wedge shapes unwind: basis recorded, and bound with no basis.
//
// They are separate cases because they exercise different halves of 0024's
// `order_exchange_unwinds_basis_shape` CHECK, and because the basis-less shape is the one a
// naive implementation forgets — the resume branch never fires for it, so it is invisible
// from the handler's perspective.
func TestBothWedgeShapesUnwind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		basis bool
		delta int64
	}{
		{"bound with no basis", false, 0},
		{"basis recorded, upgrade", true, +1000},
		{"basis recorded, downgrade", true, -1000},
		{"basis recorded, even", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := outboxDB(t)
			key := "shape-" + strings.ReplaceAll(tc.name, " ", "-")
			c, ex := seedWedged(t, db, ctx, key)
			if tc.basis {
				withBasis(t, db, ctx, c, ex, tc.delta)
			}
			if err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "terminal target claim", false); err != nil {
				t.Fatalf("unwind: %v", err)
			}
			if n := exchangeRowCount(t, db, ctx, c.OrderID); n != 0 {
				t.Errorf("order_exchanges rows = %d, want 0", n)
			}
			if n := unwindRowCount(t, db, ctx, ex.ID); n != 1 {
				t.Errorf("evidence rows = %d, want 1", n)
			}
			// The evidence records the basis fact faithfully — it is what a later reader
			// uses to decide whether money COULD have moved.
			var recordedBasis bool
			var delta sql.NullInt64
			if err := db.QueryRowContext(ctx,
				`SELECT pre_basis_recorded, pre_delta_amount FROM order_exchange_unwinds WHERE exchange_id=$1`,
				ex.ID).Scan(&recordedBasis, &delta); err != nil {
				t.Fatal(err)
			}
			if recordedBasis != tc.basis {
				t.Errorf("pre_basis_recorded = %t, want %t", recordedBasis, tc.basis)
			}
			if tc.basis && (!delta.Valid || delta.Int64 != tc.delta) {
				t.Errorf("pre_delta_amount = %v, want %d", delta, tc.delta)
			}
			if !tc.basis && delta.Valid {
				t.Errorf("pre_delta_amount = %v for an exchange with no basis, want NULL", delta)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// The four guard predicates. ONE CASE EACH, each satisfying the earlier ones, so that an
// earlier refusal cannot short-circuit the predicate under test and leave it unproven.
// ---------------------------------------------------------------------------------------

// Predicate 1: a blank reason is refused BEFORE a transaction opens.
//
// THE "BEFORE" IS THE WHOLE ASSERTION, and the first version of this test did not make it.
// That version asserted only the effect — an error came back, the row survived, no evidence
// was written — and it stayed GREEN with the Go guard deleted, because 0024's own
// `btrim(reason) <> ”` CHECK rejects the insert and the transaction rolls back to the same
// observable state. A test that cannot tell which layer refused is not a test of either.
//
// So the assertion is about the DATABASE HANDLE, not the state: the unwind is called with a
// closed pool, which makes any attempt to open a transaction fail with a recognisable
// driver error. A guard that runs first never touches the pool and still reports the reason
// problem; a guard that has been moved or deleted reports the connection instead.
//
// Why the ordering is worth pinning at all: `recovery_operations_test.go` states the house
// contract that an operator command's argument validation must be reachable without a
// database, and the CLI's blank-reason path relies on this function refusing the same way.
func TestUnwindRefusesABlankReasonBeforeItOpensATransaction(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "blank-reason")

	// A pool that cannot serve a transaction. Anything reaching BeginTx fails on it.
	closed, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()

	for _, reason := range []string{"", "   ", "\t\n"} {
		err := UnwindWedgedExchange(ctx, closed, c.OrganizerID, ex.ID, reason, false)
		if err == nil {
			t.Fatalf("a blank reason %q was accepted; it is the only part of the record a later "+
				"reader cannot reconstruct", reason)
		}
		if !strings.Contains(err.Error(), "reason is required") {
			t.Fatalf("a blank reason %q was refused by %v, not by the reason guard. The guard has "+
				"to run BEFORE the transaction opens — otherwise the only thing refusing a blank "+
				"reason is 0024's CHECK, and the store would be relying on a constraint to enforce "+
				"an argument contract its callers depend on", reason, err)
		}
	}

	// And the real pool is untouched, which is the effect half of the contract.
	if n := exchangeRowCount(t, db, ctx, c.OrderID); n != 1 {
		t.Errorf("order_exchanges rows = %d, want 1 — a refused unwind must change nothing", n)
	}
	if n := unwindRowCount(t, db, ctx, ex.ID); n != 0 {
		t.Errorf("evidence rows = %d, want 0", n)
	}
}

// The database refuses a blank reason TOO, independently of the Go guard.
//
// Both layers, tested separately, because they defend against different writers: the Go
// guard is an argument contract for this service's own callers, and the CHECK constrains
// anyone inserting directly. Establishing that they are two mechanisms is what the previous
// test's failure mode showed was necessary.
func TestTheEvidenceTableAlsoRefusesABlankReason(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "blank-reason-check", 1, 1000)
	id := uuid.New()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_exchange_unwinds WHERE id=$1`, id) })

	_, err := db.ExecContext(ctx, `
		INSERT INTO order_exchange_unwinds
		  (id, organizer_id, exchange_id, source_order_id, reason, idempotency_key, actor,
		   pre_source_total, currency, pre_basis_recorded)
		VALUES ($1,$2,$3,$4,'   ','key','actor',2000,'EUR',false)`,
		id, uuid.New(), uuid.New(), c.OrderID)
	if err == nil {
		t.Error("a whitespace-only reason was accepted by a direct insert; the CHECK is what " +
			"constrains a writer that is not UnwindWedgedExchange")
	}
}

// Predicate 2: an unknown exchange refuses distinguishably, with a valid non-blank reason so
// the reason check cannot be what refused.
func TestUnwindRefusesAnUnknownExchangeDistinguishably(t *testing.T) {
	db, ctx := outboxDB(t)
	err := UnwindWedgedExchange(ctx, db, uuid.New(), uuid.New(), "a perfectly good reason", false)
	if !errors.Is(err, ErrExchangeNotFound) {
		t.Fatalf("err = %v, want ErrExchangeNotFound. Reporting anything else — 'money moved', "+
			"say — sends an operator hunting for a charge that was never made", err)
	}
}

// Predicate 3: a SETTLED exchange refuses and survives.
//
// The fixture settles for real through CompleteExchangeSettlement, so the row satisfies
// 0010's settlement shape (basis present, replacement order present). A hand-written
// `UPDATE ... SET settled_at=now()` would be rejected by that CHECK, and a fixture that
// worked around it by also writing a replacement id would be asserting against a shape the
// code never produces.
//
// It passes the earlier predicates deliberately: the reason is good and the exchange exists.
// So deleting the settled check makes this test fail, which is what it is for.
func TestUnwindRefusesASettledExchangeAndItSurvives(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "settled-guard")
	withBasis(t, db, ctx, c, ex, +1000)

	replacement := uuid.New()
	seedReplacementOrder(t, db, ctx, c, replacement)
	if err := CompleteExchangeSettlement(ctx, db, c.OrganizerID, ex.ID, replacement); err != nil {
		t.Fatalf("settle: %v", err)
	}

	err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "a perfectly good reason", false)
	if !errors.Is(err, ErrExchangeSettled) {
		t.Fatalf("err = %v, want ErrExchangeSettled. A settled exchange is not wedged: its money "+
			"moved and its replacement order exists, so unwinding it would delete the record of a "+
			"completed sale", err)
	}
	if n := exchangeRowCount(t, db, ctx, c.OrderID); n != 1 {
		t.Errorf("order_exchanges rows = %d, want 1 — the settled row must survive", n)
	}
	if n := unwindRowCount(t, db, ctx, ex.ID); n != 0 {
		t.Errorf("evidence rows = %d, want 0", n)
	}
}

// Predicate 4: money-moved refuses and the row survives.
//
// Passes all three earlier predicates — good reason, existing exchange, unsettled — so the
// only thing that can refuse it is the money guard. Delete that guard and this test fails.
func TestUnwindRefusesWhenMoneyMovedAndTheRowSurvives(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "money-guard")
	withBasis(t, db, ctx, c, ex, +1000)

	err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "a perfectly good reason", true)
	if !errors.Is(err, ErrExchangeMoneyMoved) {
		t.Fatalf("err = %v, want ErrExchangeMoneyMoved", err)
	}
	if n := exchangeRowCount(t, db, ctx, c.OrderID); n != 1 {
		t.Errorf("order_exchanges rows = %d, want 1 — deleting the binding of a charged buyer "+
			"strands them worse than the wedge does", n)
	}
	if n := unwindRowCount(t, db, ctx, ex.ID); n != 0 {
		t.Errorf("evidence rows = %d, want 0 — a refused unwind records nothing", n)
	}
	// And the order is still blocked in both directions, which is the state the refusal
	// preserves rather than merely declines to change.
	if _, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: 1,
		IdempotencyKey: "refund-blocked", Actor: "a", Reason: "r",
	}); !errors.Is(err, ErrOrderNotRefundable) {
		t.Errorf("the refund path was freed by a REFUSED unwind: %v", err)
	}
}

// ---------------------------------------------------------------------------------------
// The evidence itself.
// ---------------------------------------------------------------------------------------

// The evidence records what was destroyed, and it is captured from the row rather than from
// the caller's arguments — which is the difference between a copy and a reconstruction.
func TestUnwindRecordsWhatItDestroyed(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "evidence")
	basis := withBasis(t, db, ctx, c, ex, -400)

	if err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "  target claim released by an operator  ", false); err != nil {
		t.Fatalf("unwind: %v", err)
	}

	var (
		org, exchangeID, sourceOrder uuid.UUID
		hold                         uuid.NullUUID
		reason, key, actor, currency string
		delta, target                sql.NullInt64
		sourceTotal                  int64
		basisRecorded                bool
	)
	if err := db.QueryRowContext(ctx, `
		SELECT organizer_id, exchange_id, source_order_id, reason, idempotency_key, actor,
		       pre_delta_amount, pre_target_total, pre_source_total, currency,
		       pre_basis_recorded, pre_target_hold_id
		FROM order_exchange_unwinds WHERE exchange_id=$1`, ex.ID).
		Scan(&org, &exchangeID, &sourceOrder, &reason, &key, &actor,
			&delta, &target, &sourceTotal, &currency, &basisRecorded, &hold); err != nil {
		t.Fatalf("read evidence: %v", err)
	}

	if org != c.OrganizerID || exchangeID != ex.ID || sourceOrder != c.OrderID {
		t.Errorf("identity = %s/%s/%s, want %s/%s/%s", org, exchangeID, sourceOrder,
			c.OrganizerID, ex.ID, c.OrderID)
	}
	// Trimmed, as the store trims it — evidence with leading whitespace is a different
	// string from the one the operator meant.
	if reason != "target claim released by an operator" {
		t.Errorf("reason = %q, want the trimmed reason", reason)
	}
	// The idempotency key and actor come from the ROW, not from any argument the caller
	// passed — the caller never supplies them. That is what makes this a copy.
	if key != "evidence" {
		t.Errorf("idempotency_key = %q, want the exchange's own key %q", key, "evidence")
	}
	if actor != "support@example.test" {
		t.Errorf("actor = %q, want the exchange's own actor", actor)
	}
	if !basisRecorded {
		t.Error("pre_basis_recorded = false though a basis was recorded")
	}
	if !delta.Valid || delta.Int64 != -400 {
		t.Errorf("pre_delta_amount = %v, want -400", delta)
	}
	if !target.Valid || target.Int64 != basis.TargetTotal {
		t.Errorf("pre_target_total = %v, want %d", target, basis.TargetTotal)
	}
	if sourceTotal != 2000 {
		t.Errorf("pre_source_total = %d, want 2000", sourceTotal)
	}
	if currency != "EUR" {
		t.Errorf("currency = %q, want EUR", currency)
	}
	if !hold.Valid || hold.UUID != basis.TargetHoldID {
		t.Errorf("pre_target_hold_id = %v, want %s — it is the claim an operator goes and "+
			"inspects in inventory afterwards", hold, basis.TargetHoldID)
	}
}

// The database refuses evidence that contradicts itself, so a future caller cannot record a
// pre-state the source row could never have held.
//
// Every case is a DIRECT insert, deliberately: these constraints exist to constrain writers
// that are not `UnwindWedgedExchange`, and routing them through it would prove only that the
// Go code is careful.
func TestTheEvidenceTableRefusesAnImpossiblePreState(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "evidence-constraints", 1, 1000)

	insert := func(t *testing.T, cols, vals string, args ...any) error {
		t.Helper()
		id := uuid.New()
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_exchange_unwinds WHERE id=$1`, id) })
		full := append([]any{id, uuid.New(), uuid.New(), c.OrderID}, args...)
		_, err := db.ExecContext(ctx, `
			INSERT INTO order_exchange_unwinds
			  (id, organizer_id, exchange_id, source_order_id, `+cols+`)
			VALUES ($1,$2,$3,$4,`+vals+`)`, full...)
		return err
	}

	const okCols = `reason, idempotency_key, actor, pre_source_total, currency, pre_basis_recorded,
	                pre_delta_amount, pre_target_total, pre_target_hold_id`

	// A positive control FIRST. Without it, every case below could be failing for a reason
	// that has nothing to do with the constraint it names — a typo in the column list would
	// look exactly like a constraint doing its job.
	t.Run("a well-formed row is accepted", func(t *testing.T) {
		if err := insert(t, okCols, `$5,$6,$7,$8,$9,$10,$11,$12,$13`,
			"a reason", "key", "actor", int64(2000), "EUR", true, int64(1000), int64(3000), uuid.New()); err != nil {
			t.Fatalf("a valid evidence row was refused: %v", err)
		}
	})

	t.Run("a blank reason is refused", func(t *testing.T) {
		if err := insert(t, okCols, `$5,$6,$7,$8,$9,$10,$11,$12,$13`,
			"   ", "key", "actor", int64(2000), "EUR", true, int64(1000), int64(3000), uuid.New()); err == nil {
			t.Error("a whitespace-only reason was accepted")
		}
	})

	t.Run("a basis-less row carrying basis values is refused", func(t *testing.T) {
		if err := insert(t, okCols, `$5,$6,$7,$8,$9,$10,$11,$12,$13`,
			"a reason", "key", "actor", int64(2000), "EUR", false, int64(1000), int64(3000), uuid.New()); err == nil {
			t.Error("pre_basis_recorded=false with a delta, total and hold was accepted; the " +
				"basis-shape CHECK is what makes the boolean trustworthy")
		}
	})

	t.Run("a basis row missing its values is refused", func(t *testing.T) {
		if err := insert(t, okCols, `$5,$6,$7,$8,$9,$10,NULL,NULL,NULL`,
			"a reason", "key", "actor", int64(2000), "EUR", true); err == nil {
			t.Error("pre_basis_recorded=true with no delta, total or hold was accepted")
		}
	})

	t.Run("a delta that is not the difference is refused", func(t *testing.T) {
		// target 3000 − source 2000 = 1000, so 9999 is arithmetically impossible. This is
		// 0010's money invariant preserved on the copy: a pre-state that does not satisfy
		// the constraint the source row was under is not a faithful record of it.
		if err := insert(t, okCols, `$5,$6,$7,$8,$9,$10,$11,$12,$13`,
			"a reason", "key", "actor", int64(2000), "EUR", true, int64(9999), int64(3000), uuid.New()); err == nil {
			t.Error("a delta that is not target − source was accepted")
		}
	})

	t.Run("a second unwind for one exchange is refused", func(t *testing.T) {
		org, exchangeID := uuid.New(), uuid.New()
		twice := func() error {
			id := uuid.New()
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_exchange_unwinds WHERE id=$1`, id) })
			_, err := db.ExecContext(ctx, `
				INSERT INTO order_exchange_unwinds
				  (id, organizer_id, exchange_id, source_order_id, reason, idempotency_key, actor,
				   pre_source_total, currency, pre_basis_recorded)
				VALUES ($1,$2,$3,$4,'a reason','key','actor',2000,'EUR',false)`,
				id, org, exchangeID, c.OrderID)
			return err
		}
		if err := twice(); err != nil {
			t.Fatalf("the first unwind record was refused: %v", err)
		}
		if err := twice(); err == nil {
			t.Error("a second unwind record for the same exchange was accepted; that is either a " +
				"duplicated record of one intervention or an exchange re-created and re-unwound, " +
				"and an operator needs both refused rather than recorded twice")
		}
	})
}

// ---------------------------------------------------------------------------------------
// The listing (COS 3).
// ---------------------------------------------------------------------------------------

// The listing returns unsettled exchanges of BOTH shapes and excludes settled ones.
//
// The settled exclusion is not decoration: delete `settled_at IS NULL` and this test fails,
// which is what proves the predicate is live. And the columns asserted are the ones the
// operator's decision actually needs — the basis flag and the delta decide which payments
// record must be consulted, and the source key is half of a downgrade's address.
func TestListWedgedExchangesReturnsBothShapesAndExcludesSettled(t *testing.T) {
	db, ctx := outboxDB(t)

	cNoBasis, exNoBasis := seedWedged(t, db, ctx, "list-nobasis")
	cBasis, exBasis := seedWedged(t, db, ctx, "list-basis")
	withBasis(t, db, ctx, cBasis, exBasis, -600)
	cSettled, exSettled := seedWedged(t, db, ctx, "list-settled")
	withBasis(t, db, ctx, cSettled, exSettled, +1000)
	replacement := uuid.New()
	seedReplacementOrder(t, db, ctx, cSettled, replacement)
	if err := CompleteExchangeSettlement(ctx, db, cSettled.OrganizerID, exSettled.ID, replacement); err != nil {
		t.Fatalf("settle: %v", err)
	}

	all, err := ListWedgedExchanges(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[uuid.UUID]WedgedExchange{}
	for _, w := range all {
		byID[w.ID] = w
	}

	if _, ok := byID[exSettled.ID]; ok {
		t.Error("a SETTLED exchange appeared in the wedged listing. It is not wedged — its money " +
			"moved and its replacement exists — and offering it to an operator invites deleting " +
			"the record of a completed sale")
	}

	noBasis, ok := byID[exNoBasis.ID]
	if !ok {
		t.Fatal("the bound-with-no-basis exchange is missing from the listing. It is the shape " +
			"the resume branch never fires for, so it is invisible everywhere else")
	}
	if noBasis.BasisRecorded {
		t.Error("BasisRecorded = true for an exchange with no basis")
	}
	if noBasis.TargetHoldID != uuid.Nil {
		t.Errorf("TargetHoldID = %s for an exchange with no basis, want zero", noBasis.TargetHoldID)
	}
	if noBasis.SourceOrderID != cNoBasis.OrderID {
		t.Errorf("SourceOrderID = %s, want %s", noBasis.SourceOrderID, cNoBasis.OrderID)
	}

	withB, ok := byID[exBasis.ID]
	if !ok {
		t.Fatal("the basis-recorded exchange is missing from the listing")
	}
	if !withB.BasisRecorded {
		t.Error("BasisRecorded = false though a basis was recorded")
	}
	if withB.DeltaAmount != -600 {
		t.Errorf("DeltaAmount = %d, want -600 — the SIGN is what selects which payments record "+
			"the unwind must consult", withB.DeltaAmount)
	}
	// The source order's checkout key, which a downgrade's refund-leg lookup needs and which
	// lives on `orders` rather than on the exchange. A listing that dropped the join would
	// leave every downgrade unaddressable.
	if withB.PaymentSourceKey != "src-list-basis" {
		t.Errorf("PaymentSourceKey = %q, want the SOURCE ORDER's checkout key %q",
			withB.PaymentSourceKey, "src-list-basis")
	}
	if withB.Actor != "support@example.test" || withB.Currency != "EUR" || withB.Quantity != 2 {
		t.Errorf("listing row = %+v, want the exchange's own actor, currency and quantity", withB)
	}
	if withB.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; it is how an operator tells a second-old row from a week-old one")
	}
}

// LoadWedgedExchange refuses a settled exchange and an unknown one, distinguishably.
//
// It refuses the settled one HERE as well as inside the transaction, and both refusals are
// tested, because this read is what the money check is performed against: answering for a
// settled row would send the caller to payments about an exchange it must not touch anyway.
func TestLoadWedgedExchangeRefusesSettledAndUnknown(t *testing.T) {
	db, ctx := outboxDB(t)

	if _, err := LoadWedgedExchange(ctx, db, uuid.New(), uuid.New()); !errors.Is(err, ErrExchangeNotFound) {
		t.Errorf("err = %v, want ErrExchangeNotFound", err)
	}

	c, ex := seedWedged(t, db, ctx, "load-settled")
	withBasis(t, db, ctx, c, ex, +1000)
	replacement := uuid.New()
	seedReplacementOrder(t, db, ctx, c, replacement)
	if err := CompleteExchangeSettlement(ctx, db, c.OrganizerID, ex.ID, replacement); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := LoadWedgedExchange(ctx, db, c.OrganizerID, ex.ID); !errors.Is(err, ErrExchangeSettled) {
		t.Errorf("err = %v, want ErrExchangeSettled", err)
	}
}

// seedReplacementOrder writes the order a settlement points at. CompleteExchangeSettlement
// sets replacement_order_id, which is a real foreign key to `orders`, so a settled fixture
// needs a real replacement row to exist.
func seedReplacementOrder(t *testing.T, db *sql.DB, ctx context.Context, c Completion, orderID uuid.UUID) {
	t.Helper()
	reservation := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,
		                         unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1500,3000,3000,'EUR','completed')`,
		reservation, c.OrganizerID, uuid.New(), c.SlotID, uuid.New(), c.BuyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`,
		orderID, reservation, "replacement-"+orderID.String(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id=$1`, orderID)
		_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, orderID)
		_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, reservation)
	})
}

// ---------------------------------------------------------------------------------------
// The settlement-in-flight guard (ai-review pass 1 [critical]).
// ---------------------------------------------------------------------------------------

// Predicate 5: an exchange whose settlement is IN FLIGHT refuses, and the row survives.
//
// This is the guard that closes the race the source-order lock cannot see. A resume releases
// that lock when its bind transaction commits, and only then finalizes, charges and settles —
// so between those moments an unwind holding the lock would observe an unsettled row and a
// clean payments answer, and delete the binding out from under a charge about to happen.
//
// It passes every earlier predicate — good reason, existing exchange, unsettled, and payments
// says no money moved — so the settling marker is the only thing that can refuse it. Delete
// the `settlingAt.Valid` check and this test fails.
func TestUnwindRefusesAnExchangeWhoseSettlementIsInFlight(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "settling-guard")
	withBasis(t, db, ctx, c, ex, +1000)

	// Marked the way the handler marks it: through the real function, at the moment finalize
	// has succeeded. Not a hand-written UPDATE — the fixture must exercise the writer.
	if err := MarkExchangeSettling(ctx, db, c.OrganizerID, ex.ID); err != nil {
		t.Fatalf("mark settling: %v", err)
	}

	err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "a perfectly good reason", false)
	if !errors.Is(err, ErrExchangeSettling) {
		t.Fatalf("err = %v, want ErrExchangeSettling. This exchange has passed finalize and may "+
			"be at the provider right now; deleting its binding would strand a charge with no "+
			"durable record of what it was for", err)
	}
	if n := exchangeRowCount(t, db, ctx, c.OrderID); n != 1 {
		t.Errorf("order_exchanges rows = %d, want 1", n)
	}
	if n := unwindRowCount(t, db, ctx, ex.ID); n != 0 {
		t.Errorf("evidence rows = %d, want 0", n)
	}
}

// The marker is idempotent and preserves its FIRST instant.
//
// A resume marks on every retry, so a second call must not move the timestamp — an operator
// reading "settling since" needs the moment the settlement actually started, not the moment
// of the most recent retry, or a permanently-retrying exchange would look permanently fresh.
func TestMarkingSettlingTwiceKeepsTheFirstInstant(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "settling-idempotent")
	withBasis(t, db, ctx, c, ex, +1000)

	if err := MarkExchangeSettling(ctx, db, c.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	first := settlingMarker(t, db, ctx, ex.ID)
	if first.IsZero() {
		t.Fatal("the first mark wrote nothing")
	}
	time.Sleep(10 * time.Millisecond)
	if err := MarkExchangeSettling(ctx, db, c.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	if second := settlingMarker(t, db, ctx, ex.ID); !second.Equal(first) {
		t.Errorf("settling_at moved from %s to %s on a repeat. A resume marks on every retry, "+
			"so a moving marker makes an exchange that has been stuck for an hour look seconds "+
			"old to the operator deciding whether to investigate", first, second)
	}
}

// The listing reports the marker, because it changes what an operator may do with the row.
func TestTheListingReportsASettlingExchange(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "settling-listed")
	withBasis(t, db, ctx, c, ex, +1000)
	if err := MarkExchangeSettling(ctx, db, c.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}

	all, err := ListWedgedExchanges(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range all {
		if w.ID != ex.ID {
			continue
		}
		if !w.Settling {
			t.Error("Settling = false for an exchange that is settling; an operator reading the " +
				"listing would try to unwind it")
		}
		if w.SettlingAt.IsZero() {
			t.Error("SettlingAt is zero; its AGE is what distinguishes a settlement in flight " +
				"from one that crashed after finalize")
		}
		return
	}
	t.Fatal("the settling exchange is missing from the listing entirely")
}

// A settlement whose exchange was unwound underneath it FAILS LOUDLY rather than silently.
//
// Before this ticket a missing exchange row was unreachable, so CompleteExchangeSettlement
// treated `sql.ErrNoRows` as "no basis yet" and returned nil. Once an exchange became
// deletable that branch acquired a second meaning, and the silent nil is the dangerous one:
// the caller has already submitted the buyer's charge, so returning success records no
// exchange, no replacement order and no `order.exchanged` event, with nothing left to notice.
//
// The unwind evidence is what tells the two apart, which is the second reason it is durable.
func TestSettlingAnUnwoundExchangeFailsLoudly(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "settle-after-unwind")
	withBasis(t, db, ctx, c, ex, +1000)

	if err := UnwindWedgedExchange(ctx, db, c.OrganizerID, ex.ID, "unwound mid-flight", false); err != nil {
		t.Fatalf("unwind: %v", err)
	}

	replacement := uuid.New()
	seedReplacementOrder(t, db, ctx, c, replacement)
	err := CompleteExchangeSettlement(ctx, db, c.OrganizerID, ex.ID, replacement)
	if err == nil {
		t.Fatal("settling an unwound exchange reported SUCCESS. The buyer's charge has already " +
			"been submitted by the caller, so this records nothing at all and no retry can " +
			"notice — strictly worse than the wedge this ticket exists to fix")
	}
	if !errors.Is(err, ErrExchangeUnwound) {
		t.Fatalf("err = %v, want ErrExchangeUnwound", err)
	}
}

// And an exchange with NO BASIS still settles as a no-op, which is the branch that was there
// before and must not be turned into an error by the fix above.
//
// The two cases share `sql.ErrNoRows` and mean opposite things; this is the control that
// proves the fix distinguishes them rather than failing everything.
func TestSettlingAnExchangeWithNoBasisIsStillANoOp(t *testing.T) {
	db, ctx := outboxDB(t)
	c, ex := seedWedged(t, db, ctx, "settle-no-basis")

	if err := CompleteExchangeSettlement(ctx, db, c.OrganizerID, ex.ID, uuid.New()); err != nil {
		t.Fatalf("settling an exchange with no basis returned %v, want nil. Settlement cannot "+
			"precede the basis and the caller's flow always records one first — turning this "+
			"into an error would break TKT-158's asserted contract", err)
	}
}

// settlingMarker reads the in-flight marker back.
func settlingMarker(t *testing.T, db *sql.DB, ctx context.Context, exchangeID uuid.UUID) time.Time {
	t.Helper()
	var at sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT settling_at FROM order_exchanges WHERE id=$1`, exchangeID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.Time
}
