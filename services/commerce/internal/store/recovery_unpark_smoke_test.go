//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
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
// Table-driven over EVERY status in the claimable set, and that is the entire value of the
// test. The single-status version this replaces seeded only `release_pending`, so removing
// any of the other four from `claimableRecoveryStatuses` OR from ClaimStuckOrders' SQL left
// it green — while its comment claimed it was what kept the two duplicated sets equivalent.
// The comment was false, and the drift it would have missed includes
// `reconciliation_required`, the money-sensitive one (ai-review F3).
//
// Decided in SQL, so asserted against real PostgreSQL. Nothing above this tier can observe it.
func TestUnparkedOrderIsClaimedAgainByTheRunnerForEveryClaimableStatus(t *testing.T) {
	// Derived from the requirement — the runner's claimable set — not from a run.
	for _, status := range []string{
		"created", "payment_unknown", "confirmation_pending", "release_pending", "reconciliation_required",
	} {
		t.Run(status, func(t *testing.T) {
			db, s := seedParked(t, status, 10, "psp unreachable")
			_, ctx := outboxDB(t)

			if _, found := claimUntilFound(t, db, ctx, s.OrderID); found {
				t.Fatal("fixture: the parked order was already claimable, so unparking it proves nothing")
			}

			if err := UnparkOrder(ctx, db, s.OrderID, "psp restored; re-driving"); err != nil {
				t.Fatalf("unpark refused a status the runner can claim: %v", err)
			}
			if _, found := claimUntilFound(t, db, ctx, s.OrderID); !found {
				t.Fatal("an unparked order was not returned to the claimable set")
			}
		})
	}
}

// The negative half of the set equivalence, and the reason it reads the vocabulary out of
// PostgreSQL instead of listing statuses by hand (ai-review F4).
//
// Two independent holes, both of which a hand-written table has by construction. First, a
// negative case that only calls UnparkOrder observes `claimableRecoveryStatuses` and nothing
// else: broaden the SQL to admit 'completed' while leaving the map alone and the refusal still
// arrives, the positive test still passes, and the runner starts claiming terminal orders.
// Each status is therefore checked against BOTH mechanisms separately — the map via
// UnparkOrder, the SQL via a row placed directly in the unparked state and offered to
// ClaimStuckOrders. Second, a hand-written list cannot cover a status that does not exist yet:
// adding one to orders_status_check would leave both tables silently short. So the list comes
// from the CHECK constraint itself, and the expectation for each status comes from the
// requirement — the runner drives non-terminal orders, and the five in claimableRecoveryStatuses
// are the non-terminal ones.
func TestTheTwoStatusSetsAgreeOnEveryStatusTheSchemaPermits(t *testing.T) {
	db, ctx := outboxDB(t)

	// Straight from the live constraint: whatever the schema permits today, including a value
	// added after this test was written.
	//
	// Scoped to the `orders` relation, because constraint names are per-relation and not
	// database-global — an unscoped lookup would silently read some other table's constraint
	// the day one is named the same, and iterate its vocabulary instead (ai-review F7).
	var checkClause string
	if err := db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid
		WHERE c.conname='orders_status_check' AND t.relname='orders'`).
		Scan(&checkClause); err != nil {
		t.Fatal(err)
	}

	// LOSSLESS extraction: every single-quoted literal, whatever it contains. The earlier
	// version matched `[a-z_]+` and would have skipped a future value like '3ds_pending'
	// entirely — creating no subtest for it, so drift on that status escaped both mechanisms
	// while the test stayed green. A parse that silently omits is worse than one that fails,
	// because this test's whole value is the COMPLETENESS of the list it iterates.
	//
	// PostgreSQL renders the clause as CHECK ((status = ANY (ARRAY['a'::text, ...]))) and
	// doubles any embedded quote, so scanning quote-delimited runs is faithful to the
	// rendering rather than to a guess about the character set.
	vocabulary := regexp.MustCompile(`'((?:[^']|'')*)'`).FindAllStringSubmatch(checkClause, -1)
	statuses := make([]string, 0, len(vocabulary))
	for _, m := range vocabulary {
		statuses = append(statuses, strings.ReplaceAll(m[1], "''", "'"))
	}
	// Fail closed on a rendering this parse cannot account for. The literal count must match
	// the clause's own comma count, so a value the regex failed to extract is caught rather
	// than skipped, and the statuses the runner is documented to drive must all be present, so
	// a wholesale change of shape is caught too.
	if got, want := len(statuses), strings.Count(checkClause, ",")+1; got != want {
		t.Fatalf("parsed %d statuses out of orders_status_check but the clause has %d comma-separated "+
			"values (%q); the parse is dropping or inventing entries and this test would be vacuous",
			got, want, checkClause)
	}
	parsed := map[string]bool{}
	for _, status := range statuses {
		parsed[status] = true
	}
	for _, required := range []string{"created", "payment_unknown", "confirmation_pending",
		"release_pending", "reconciliation_required", "completed"} {
		if !parsed[required] {
			t.Fatalf("the status vocabulary parsed out of orders_status_check is missing %q (%q); "+
				"either the parse is broken or the constraint changed shape", required, checkClause)
		}
	}

	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			// The requirement, not the implementation: the recovery runner drives orders that
			// have not reached a terminal state. Deliberately NOT read from
			// claimableRecoveryStatuses — an expectation taken from the mechanism under test
			// agrees with it by construction.
			//
			// Stated as an EXHAUSTIVE classification with no default, so that adding a status
			// to orders_status_check fails this test loudly instead of being silently assumed
			// claimable and then "confirmed" by whatever the code happens to do with it. A
			// `!terminal[status]` default would turn the arrival of a new status into a test
			// that blesses the implementation — the failure mode the whole test exists to
			// close, one level up.
			terminal := map[string]bool{"completed": true, "declined": true, "timeout": true, "refunded": true}
			nonTerminal := map[string]bool{
				"created": true, "payment_unknown": true, "confirmation_pending": true,
				"release_pending": true, "reconciliation_required": true,
			}
			if terminal[status] == nonTerminal[status] {
				t.Fatalf("status %q is in the schema's vocabulary but this test does not classify it "+
					"as terminal or non-terminal. Decide which the recovery runner should drive, add it "+
					"to the right map here, and check claimableRecoveryStatuses and ClaimStuckOrders' "+
					"SQL agree — do not let it default.", status)
			}
			wantClaimable := nonTerminal[status]

			// Mechanism 1: the Go map, observed through UnparkOrder.
			_, parked := seedParked(t, status, 10, "psp unreachable")
			err := UnparkOrder(ctx, db, parked.OrderID, "operator investigated")
			switch {
			case wantClaimable && err != nil:
				t.Fatalf("UnparkOrder refused %q, which the runner can drive: %v", status, err)
			case !wantClaimable && !errors.Is(err, ErrRecoveryOrderStatusNotClaimable):
				t.Fatalf("UnparkOrder answered %v for terminal status %q, want ErrRecoveryOrderStatusNotClaimable", err, status)
			}

			// Mechanism 2: the SQL, observed through ClaimStuckOrders on a row put into the
			// unparked state DIRECTLY — so this half is independent of whether UnparkOrder
			// would have agreed to produce it.
			direct := seedStuck(t, status)
			if _, err := db.ExecContext(ctx, `
				UPDATE orders SET recovery_parked_at=NULL, recovery_attempts=0,
				    recovery_next_attempt_at=now(), recovery_claim_id=NULL, recovery_lease_until=NULL,
				    updated_at=now()-interval '10 minutes'
				WHERE id=$1`, direct.OrderID); err != nil {
				t.Fatal(err)
			}
			_, claimed := claimUntilFound(t, db, ctx, direct.OrderID)
			if claimed != wantClaimable {
				t.Fatalf("ClaimStuckOrders claimed=%v for status %q, want %v — the SQL status list "+
					"and claimableRecoveryStatuses have drifted", claimed, status, wantClaimable)
			}
		})
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
	if _, found := claimUntilFound(t, db, ctx, s.OrderID); !found {
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
	claimed, found := claimUntilFound(t, db, ctx, s.OrderID)
	if !found {
		t.Fatal("the unparked order was not claimed")
	}
	if err := ReleaseStuckOrder(ctx, db, s.OrderID, claimed.ClaimID, errors.New("psp still flaky")); err != nil {
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

// claimUntilFound drains the claimable set in pages until it finds the order, or a page comes
// back empty.
//
// A plain `ClaimStuckOrders(ctx, db, N, ...)` would be an assertion about N, not about the
// order. This suite shares one database with every other smoke test in the package, and the
// claim is `ORDER BY recovery_next_attempt_at LIMIT N` — while an unpark sets
// recovery_next_attempt_at to now(), which sorts LAST among rows whose backoff has already
// elapsed. So the target is exactly the row a truncated page drops, and any fixed limit makes
// these tests fail as the package grows, for a reason that has nothing to do with unparking.
//
// Claiming leases each row for the duration, so a page is never returned twice and the loop
// terminates.
func claimUntilFound(t *testing.T, db *sql.DB, ctx context.Context, id uuid.UUID) (StuckOrder, bool) {
	t.Helper()
	for {
		page, err := ClaimStuckOrders(ctx, db, 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return StuckOrder{}, false
		}
		for _, o := range page {
			if o.OrderID == id {
				return o, true
			}
		}
	}
}
