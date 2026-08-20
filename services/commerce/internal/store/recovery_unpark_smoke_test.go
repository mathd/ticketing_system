//go:build smoke

package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TKT-146. The operator path out of a parked recovery order.
//
// Every one of these lives at the smoke tier against real PostgreSQL, and that is not a
// convenience. The claimable set is decided entirely in SQL (ClaimStuckOrders' CTE), so an
// assertion that an unparked order is claimable again proves nothing anywhere else: an
// in-memory fake would be scoping in Go exactly the thing the shipped SQL has to scope.

// seedParked produces an order parked the way ReleaseStuckOrder parks one — attempts
// exhausted, marker set, status untouched — and back-dated past the two-minute in-flight
// grace period so claimability is decided by parking and nothing else.
func seedParked(t *testing.T, status string, attempts int, lastErr string) (*sql.DB, StuckOrder) {
	t.Helper()
	s := seedStuck(t, status)
	db, ctx := outboxDB(t)
	if _, err := db.ExecContext(ctx, `
		UPDATE orders SET recovery_attempts=$2, recovery_parked_at=now(), recovery_last_error=$3,
		    recovery_claim_id=NULL, recovery_lease_until=NULL,
		    updated_at=now()-interval '10 minutes'
		WHERE id=$1`, s.OrderID, attempts, lastErr); err != nil {
		t.Fatal(err)
	}
	return db, s
}

// parkedMarker reads the park marker back. A helper because almost every test here has to
// prove the fixture is genuinely in the state it claims before it can prove anything else.
func parkedMarker(t *testing.T, db *sql.DB, orderID uuid.UUID) (sql.NullTime, int) {
	t.Helper()
	var at sql.NullTime
	var attempts int
	_, ctx := outboxDB(t)
	if err := db.QueryRowContext(ctx,
		`SELECT recovery_parked_at, recovery_attempts FROM orders WHERE id=$1`, orderID).
		Scan(&at, &attempts); err != nil {
		t.Fatal(err)
	}
	return at, attempts
}

// COS 1. The listing returns every parked order with the columns an operator needs, and
// excludes an unparked one.
//
// The unparked control is not decoration: delete it and a ListParkedOrders that dropped
// its WHERE clause entirely would still pass, because every other row in the fixture IS
// parked. It is the only seed that can observe that mutation.
func TestListParkedOrdersReturnsTheParkedPopulation(t *testing.T) {
	db, first := seedParked(t, "release_pending", 10, "psp unreachable")
	_, second := seedParked(t, "reconciliation_required", 4, "captured, claim gone")
	// The control: stuck and claimable, but NOT parked.
	unparked := seedStuck(t, "created")
	_, ctx := outboxDB(t)
	if at, _ := parkedMarker(t, db, unparked.OrderID); at.Valid {
		t.Fatal("fixture: the control order is parked, so its absence from the listing would prove nothing")
	}

	rows, err := ListParkedOrders(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]ParkedOrder{}
	for _, r := range rows {
		byID[r.OrderID] = r
	}
	if _, ok := byID[unparked.OrderID]; ok {
		t.Fatal("an order with no park marker was listed as parked")
	}
	got, ok := byID[first.OrderID]
	if !ok {
		t.Fatal("a parked order was missing from the listing")
	}
	// Values derived from the requirement (COS 1 names these six columns) and from what
	// the fixture wrote — never from a first run's output.
	if got.Status != "release_pending" {
		t.Fatalf("status = %q, want release_pending", got.Status)
	}
	if got.Attempts != 10 {
		t.Fatalf("attempts = %d, want 10", got.Attempts)
	}
	if got.ParkedAt.IsZero() {
		t.Fatal("parked_at was not returned")
	}
	if got.LastError.String != "psp unreachable" {
		t.Fatalf("last error = %q, want %q", got.LastError.String, "psp unreachable")
	}
	if _, ok := byID[second.OrderID]; !ok {
		t.Fatal("the second parked order was missing from the listing")
	}
}

// COS 4, predicate 1 of 3: the order does not exist.
func TestUnparkRefusesAnUnknownOrderDistinguishably(t *testing.T) {
	db, ctx := outboxDB(t)
	err := UnparkOrder(ctx, db, uuid.New(), "operator investigated")
	if !errors.Is(err, ErrRecoveryOrderNotFound) {
		t.Fatalf("err = %v, want ErrRecoveryOrderNotFound", err)
	}
}

// COS 4, predicate 2 of 3: the order exists but is not parked.
//
// The fixture SATISFIES predicate 1 (the order really exists — asserted, not assumed) so
// this case cannot pass by short-circuiting on the earlier refusal. Delete the guard's
// parked check and this goes red; without the existence assertion it would go green for
// the wrong reason.
func TestUnparkRefusesAnUnparkedOrderDistinguishably(t *testing.T) {
	s := seedStuck(t, "created")
	db, ctx := outboxDB(t)
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT true FROM orders WHERE id=$1`, s.OrderID).Scan(&exists); err != nil {
		t.Fatalf("fixture: the order must exist, or predicate 1 answers this test: %v", err)
	}
	if at, _ := parkedMarker(t, db, s.OrderID); at.Valid {
		t.Fatal("fixture: the order is parked, so this test would be about predicate 3")
	}

	err := UnparkOrder(ctx, db, s.OrderID, "operator investigated")
	if !errors.Is(err, ErrRecoveryOrderNotParked) {
		t.Fatalf("err = %v, want ErrRecoveryOrderNotParked", err)
	}
	assertNoUnparkEvidence(t, db, s.OrderID)
}

// COS 4, predicate 3 of 3: parked, but wearing a status the runner cannot claim.
//
// Reachable only by a direct database write — ReleaseStuckOrder parks without touching
// status and ParkForReconciliation sets a status that IS claimable, so no code path in
// this service produces this row. That is precisely why the predicate is here: it is a
// fail-closed guard against a state the service cannot construct, and the fixture has to
// construct it by hand.
//
// The fixture satisfies predicates 1 and 2 (exists, and is genuinely parked — asserted),
// so deleting the status check is the only mutation that turns this green-to-red.
func TestUnparkRefusesANonClaimableStatusDistinguishably(t *testing.T) {
	db, s := seedParked(t, "release_pending", 10, "psp unreachable")
	_, ctx := outboxDB(t)
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='completed' WHERE id=$1`, s.OrderID); err != nil {
		t.Fatal(err)
	}
	if at, _ := parkedMarker(t, db, s.OrderID); !at.Valid {
		t.Fatal("fixture: the order is not parked, so predicate 2 would answer this test")
	}

	err := UnparkOrder(ctx, db, s.OrderID, "operator investigated")
	if !errors.Is(err, ErrRecoveryOrderStatusNotClaimable) {
		t.Fatalf("err = %v, want ErrRecoveryOrderStatusNotClaimable", err)
	}
	// A refused unpark leaves the row exactly as it was — including still parked.
	if at, _ := parkedMarker(t, db, s.OrderID); !at.Valid {
		t.Fatal("a refused unpark cleared the park marker")
	}
	assertNoUnparkEvidence(t, db, s.OrderID)
}

// COS 3. The whole point: after an unpark the runner picks the order up again.
//
// Decided in SQL, so asserted against real PostgreSQL. Nothing above this tier can
// observe it.
func TestUnparkedOrderIsClaimedAgainByTheRunner(t *testing.T) {
	db, s := seedParked(t, "release_pending", 10, "psp unreachable")
	_, ctx := outboxDB(t)

	before, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if containsOrder(before, s.OrderID) {
		t.Fatal("fixture: the parked order was already claimable, so unparking it proves nothing")
	}

	if err := UnparkOrder(ctx, db, s.OrderID, "psp restored; re-driving"); err != nil {
		t.Fatal(err)
	}
	after, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !containsOrder(after, s.OrderID) {
		t.Fatal("an unparked order was not returned to the claimable set")
	}
}

// A3 of the plan review. `updated_at` is the trap this ticket walks straight past.
//
// ClaimStuckOrders requires `updated_at < now() - interval '2 minutes'`. If UnparkOrder
// refreshed it, the order would be excluded for two minutes for a reason that has nothing
// to do with parking — the same trap RecordTerminalOutcome already sprang, which is why
// three tests in recovery_smoke_test.go back-date updated_at by hand.
//
// The two assertions belong in ONE test on purpose. Separately, each is satisfiable by
// editing the fixture; together they say the value the code left behind is the value the
// claim predicate reads.
func TestUnparkDoesNotRefreshTheInFlightGracePeriod(t *testing.T) {
	db, s := seedParked(t, "release_pending", 10, "psp unreachable")
	_, ctx := outboxDB(t)
	var before time.Time
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM orders WHERE id=$1`, s.OrderID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if err := UnparkOrder(ctx, db, s.OrderID, "psp restored"); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM orders WHERE id=$1`, s.OrderID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("unpark moved updated_at %s -> %s; the order is now inside the in-flight grace period", before, after)
	}
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !containsOrder(claimed, s.OrderID) {
		t.Fatal("the unparked order was not claimable, which is what refreshing updated_at would cause")
	}
}

// The no-op trap. ReleaseStuckOrder re-parks via `CASE WHEN recovery_attempts>=10`, so an
// unpark that cleared the marker WITHOUT resetting attempts buys exactly one re-drive and
// then re-parks — the command would look like it worked and change nothing.
//
// The invariant, stated without naming the mechanism: an unpark restores a full retry
// budget, not a single attempt.
func TestUnparkRestoresAFullRetryBudgetNotOneAttempt(t *testing.T) {
	db, s := seedParked(t, "release_pending", MaxRecoveryAttempts, "psp unreachable")
	_, ctx := outboxDB(t)

	if err := UnparkOrder(ctx, db, s.OrderID, "psp restored"); err != nil {
		t.Fatal(err)
	}
	if _, attempts := parkedMarker(t, db, s.OrderID); attempts != 0 {
		t.Fatalf("attempts after unpark = %d, want 0", attempts)
	}

	// One failed re-drive, exactly as the runner would do it.
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var claim uuid.UUID
	for _, c := range claimed {
		if c.OrderID == s.OrderID {
			claim = c.ClaimID
		}
	}
	if claim == uuid.Nil {
		t.Fatal("the unparked order was not claimed")
	}
	if err := ReleaseStuckOrder(ctx, db, s.OrderID, claim, errors.New("psp still flaky")); err != nil {
		t.Fatal(err)
	}
	if at, _ := parkedMarker(t, db, s.OrderID); at.Valid {
		t.Fatal("one failed attempt re-parked the order: the unpark restored a single attempt, not a budget")
	}
}

// COS 5. The evidence an unpark leaves behind.
//
// Every asserted value is one the unpark DESTROYS on the order row, which is the reason
// the row has to exist at all.
func TestUnparkRecordsWhatItResolved(t *testing.T) {
	db, s := seedParked(t, "reconciliation_required", 7, "captured payment whose claim is gone")
	_, ctx := outboxDB(t)
	var parkedAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT recovery_parked_at FROM orders WHERE id=$1`, s.OrderID).Scan(&parkedAt); err != nil {
		t.Fatal(err)
	}

	const reason = "refund issued out of band; ticket OPS-4412"
	if err := UnparkOrder(ctx, db, s.OrderID, reason); err != nil {
		t.Fatal(err)
	}

	var (
		gotReason   string
		gotAttempts int
		gotParked   time.Time
		gotLastErr  sql.NullString
		count       int
	)
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_recovery_unparks WHERE order_id=$1`, s.OrderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unpark evidence rows = %d, want exactly 1", count)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT reason, pre_recovery_attempts, pre_recovery_parked_at, pre_recovery_last_error
		FROM order_recovery_unparks WHERE order_id=$1`, s.OrderID).
		Scan(&gotReason, &gotAttempts, &gotParked, &gotLastErr); err != nil {
		t.Fatal(err)
	}
	if gotReason != reason {
		t.Fatalf("reason = %q, want %q", gotReason, reason)
	}
	if gotAttempts != 7 {
		t.Fatalf("pre_recovery_attempts = %d, want 7 (the value the unpark reset to 0)", gotAttempts)
	}
	if !gotParked.Equal(parkedAt) {
		t.Fatalf("pre_recovery_parked_at = %s, want %s (the marker the unpark cleared)", gotParked, parkedAt)
	}
	if gotLastErr.String != "captured payment whose claim is gone" {
		t.Fatalf("pre_recovery_last_error = %q", gotLastErr.String)
	}

	// COS 2: recovery_last_error is RETAINED on the order as operator context.
	var live sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT recovery_last_error FROM orders WHERE id=$1`, s.OrderID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live.String != "captured payment whose claim is gone" {
		t.Fatalf("recovery_last_error on the order = %q; the unpark must retain it", live.String)
	}
}

// COS 6. An unpark returns a row to the claimable set and decides nothing else — in
// particular nothing about money, which matters because a parked
// `reconciliation_required` order can hold captured funds (ADR-016 §Consequences).
func TestUnparkChangesNothingButTheRecoveryFields(t *testing.T) {
	db, s := seedParked(t, "reconciliation_required", 9, "captured payment whose claim is gone")
	_, ctx := outboxDB(t)

	const snapshot = `SELECT o.status, coalesce(o.terminal_outcome,''), r.total_amount, r.currency
	                  FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`
	var beforeStatus, beforeOutcome, beforeCurrency string
	var beforeAmount int64
	if err := db.QueryRowContext(ctx, snapshot, s.OrderID).
		Scan(&beforeStatus, &beforeOutcome, &beforeAmount, &beforeCurrency); err != nil {
		t.Fatal(err)
	}

	if err := UnparkOrder(ctx, db, s.OrderID, "refund issued out of band"); err != nil {
		t.Fatal(err)
	}

	var afterStatus, afterOutcome, afterCurrency string
	var afterAmount int64
	if err := db.QueryRowContext(ctx, snapshot, s.OrderID).
		Scan(&afterStatus, &afterOutcome, &afterAmount, &afterCurrency); err != nil {
		t.Fatal(err)
	}
	if afterStatus != beforeStatus {
		t.Fatalf("unpark changed status %q -> %q", beforeStatus, afterStatus)
	}
	if afterOutcome != beforeOutcome {
		t.Fatalf("unpark changed terminal_outcome %q -> %q", beforeOutcome, afterOutcome)
	}
	if afterAmount != beforeAmount || afterCurrency != beforeCurrency {
		t.Fatalf("unpark touched money: %d %s -> %d %s", beforeAmount, beforeCurrency, afterAmount, afterCurrency)
	}
}

// A blank reason is refused BEFORE the transaction opens. The reason is the only thing an
// unpark records that a machine could not have derived, so an empty one makes the evidence
// row worthless while still looking like evidence.
//
// "Before the transaction" is the whole assertion, and getting there took a surviving
// mutant. Deleting the Go guard left this test GREEN, because the migration's
// `btrim(reason) <> ”` CHECK refuses the INSERT and the transaction rolls back — so the
// order is still parked and there is still no evidence row, and every observation the
// original test made was satisfied by a completely different mechanism. It was a test about
// the database, wearing the name of a test about this function.
//
// What distinguishes them is WHICH error comes back. The Go guard returns a plain refusal;
// the CHECK surfaces as a wrapped constraint violation naming the table. Asserting the
// message is not string-matching for its own sake — it is the only observable that differs
// between "this function refused" and "PostgreSQL refused three statements later".
func TestUnparkRefusesABlankReasonBeforeItOpensATransaction(t *testing.T) {
	db, s := seedParked(t, "release_pending", 10, "psp unreachable")
	_, ctx := outboxDB(t)

	for _, blank := range []string{"", "   ", "\t\n"} {
		err := UnparkOrder(ctx, db, s.OrderID, blank)
		if err == nil {
			t.Fatalf("a blank reason (%q) was accepted", blank)
		}
		if !strings.Contains(err.Error(), "a reason is required") {
			t.Fatalf("reason %q was refused by something other than the guard in UnparkOrder: %v\n"+
				"a constraint violation here means the function opened a transaction and let the "+
				"database do the refusing, which is a different contract", blank, err)
		}
		if at, _ := parkedMarker(t, db, s.OrderID); !at.Valid {
			t.Fatal("a refused unpark cleared the park marker")
		}
		assertNoUnparkEvidence(t, db, s.OrderID)
	}
}

// Two operators racing. The second sees the marker already cleared and is refused with
// the same distinguishable error as any other unparked order — it does not double-write
// evidence for one intervention.
func TestSecondUnparkOfTheSameOrderIsRefused(t *testing.T) {
	db, s := seedParked(t, "release_pending", 10, "psp unreachable")
	_, ctx := outboxDB(t)

	if err := UnparkOrder(ctx, db, s.OrderID, "first operator"); err != nil {
		t.Fatal(err)
	}
	if err := UnparkOrder(ctx, db, s.OrderID, "second operator"); !errors.Is(err, ErrRecoveryOrderNotParked) {
		t.Fatalf("err = %v, want ErrRecoveryOrderNotParked", err)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_recovery_unparks WHERE order_id=$1`, s.OrderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("evidence rows = %d, want 1: one intervention, one row", count)
	}
}

func assertNoUnparkEvidence(t *testing.T, db *sql.DB, orderID uuid.UUID) {
	t.Helper()
	_, ctx := outboxDB(t)
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_recovery_unparks WHERE order_id=$1`, orderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a refused unpark wrote %d evidence row(s)", count)
	}
}

func containsOrder(orders []StuckOrder, id uuid.UUID) bool {
	for _, o := range orders {
		if o.OrderID == id {
			return true
		}
	}
	return false
}
