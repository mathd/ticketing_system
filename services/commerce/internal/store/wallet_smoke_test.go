//go:build smoke

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The wallet read against real Postgres (TKT-222 / US-A3).
//
// ADR-019 asks for TWO proofs and they are not the same proof: the RESULT is
// scoped to one customer, and the SCAN is. A read that returns the right rows
// after reading every order in the table is the defect the ADR exists to prevent,
// and only the second test can see it.

func seedWalletOrder(t *testing.T, db *sql.DB, ctx context.Context, customer uuid.NullUUID, status string, createdAt time.Time) uuid.UUID {
	t.Helper()
	reservation, order := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1500,3000,3000,'EUR','completed')`,
		reservation, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,customer_id,guest_order_ref,created_at)
		VALUES($1,$2,$3,$4,'fingerprint',$5,$6,$7)`,
		order, reservation, status, "wallet-"+uuid.NewString(), customer, uuid.New(), createdAt); err != nil {
		t.Fatal(err)
	}
	return order
}

// Result scope. The poison rows matter as much as the wanted ones: another
// customer's completed order, and this customer's order that is NOT completed. A
// fixture with only the happy rows cannot fail.
func TestCustomerOrdersReturnsOnlyThisCustomersCompletedOrders(t *testing.T) {
	db, ctx := outboxDB(t)
	alice, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-alice"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-bob"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-24 * time.Hour)

	mine := seedWalletOrder(t, db, ctx, uuid.NullUUID{UUID: alice.ID, Valid: true}, "completed", base)
	seedWalletOrder(t, db, ctx, uuid.NullUUID{UUID: bob.ID, Valid: true}, "completed", base)
	seedWalletOrder(t, db, ctx, uuid.NullUUID{UUID: alice.ID, Valid: true}, "created", base)
	seedWalletOrder(t, db, ctx, uuid.NullUUID{}, "completed", base) // a guest order

	page, _, err := CustomerOrders(ctx, db, alice.ID, WalletCursor{}, WalletPageLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].OrderID != mine {
		t.Fatalf("page = %+v, want exactly Alice's one COMPLETED order %s", page, mine)
	}
}

// Newest first, and the cursor resumes exactly where the page stopped — no row
// returned twice, none skipped.
func TestCustomerOrdersPageNewestFirstAndResumeWithoutGaps(t *testing.T) {
	db, ctx := outboxDB(t)
	account, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-page"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	customer := uuid.NullUUID{UUID: account.ID, Valid: true}

	base := time.Now().Add(-48 * time.Hour)
	const total = 5
	want := make([]uuid.UUID, 0, total)
	for i := range total {
		// Newest last in the fixture, so "newest first" is a real reordering
		// rather than the insertion order coming back.
		want = append(want, seedWalletOrder(t, db, ctx, customer, "completed", base.Add(time.Duration(i)*time.Minute)))
	}
	// Reverse: the expected order is newest first.
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}

	var got []uuid.UUID
	cursor := WalletCursor{}
	for range total { // bounded: a cursor that never terminates must fail, not hang
		page, next, err := CustomerOrders(ctx, db, account.ID, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page {
			got = append(got, o.OrderID)
		}
		if next.OrderID == uuid.Nil {
			break
		}
		cursor = next
	}

	if len(got) != total {
		t.Fatalf("paged %d orders, want %d — a keyset cursor that skips or repeats is the whole risk", len(got), total)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %d = %s, want %s (newest first, no repeats)", i, got[i], want[i])
		}
	}
}

// Two purchases sharing a timestamp must still have a total order, or the row
// that loses the tie is dropped at a page boundary.
func TestCustomerOrdersBreakTimestampTiesDeterministically(t *testing.T) {
	db, ctx := outboxDB(t)
	account, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-tie"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	customer := uuid.NullUUID{UUID: account.ID, Valid: true}
	same := time.Now().Add(-time.Hour).Truncate(time.Microsecond)

	for range 4 {
		seedWalletOrder(t, db, ctx, customer, "completed", same)
	}

	seen := map[uuid.UUID]bool{}
	cursor := WalletCursor{}
	for range 4 {
		page, next, err := CustomerOrders(ctx, db, account.ID, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page {
			if seen[o.OrderID] {
				t.Fatalf("order %s came back twice across pages — the tie-break is not total", o.OrderID)
			}
			seen[o.OrderID] = true
		}
		if next.OrderID == uuid.Nil {
			break
		}
		cursor = next
	}
	if len(seen) != 4 {
		t.Fatalf("saw %d of 4 orders with identical timestamps — one was dropped at a page boundary", len(seen))
	}
}

// ADR-019's second proof: the SCAN is scoped, not merely the result.
//
// The technique is catalog's (season_smoke_test.go `explainGenericPlan`) and is
// duplicated here because commerce is a DIFFERENT GO MODULE and cannot import a
// test helper across it. ADR-019 warns that forking this is how one copy quietly
// stops asserting anything, so the parts that carry the value are kept verbatim
// and named:
//
//   - PREPARE + `SET LOCAL plan_cache_mode = force_generic_plan` + EXECUTE inside
//     ONE transaction. A `SET` on *sql.DB can land on a different pooled
//     connection than the statement it means to govern.
//   - The guard that the parameter survives as `$1`. `EXPLAIN <query with $1>`
//     through the driver plans with the value already bound — a CUSTOM plan — and
//     produces a confident, indexed, meaningless assertion.
var walletPlanProbe atomic.Uint64

func explainWalletPlan(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	stmt := "wallet_plan_probe_" + strconv.FormatUint(walletPlanProbe.Add(1), 10)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PREPARE %s(uuid, timestamptz, uuid, int) AS %s`, stmt, walletPageQuery)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`EXPLAIN EXECUTE %s('%s'::uuid, '9999-12-31'::timestamptz, '%s'::uuid, 20)`,
		stmt, uuid.New(), uuid.Max))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

func TestWalletReadScanIsScopedNotJustItsResult(t *testing.T) {
	db, ctx := outboxDB(t)
	account, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-plan"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	// A body of orders belonging to OTHER customers — and they must really be
	// other customers.
	//
	// The first version of this bound `other` to `account.ID`, so all forty rows
	// belonged to the customer being queried (ai-review [medium]). The variable
	// was named `other` and was not, which is what made it read as correct: the
	// plan could scan every completed row and the assertion would still pass,
	// because there was nothing to exclude. A fixture with one customer cannot
	// prove a per-customer scan is scoped.
	//
	// Without a body of rows at all, the planner may legitimately prefer a
	// sequential scan on a tiny table and the assertion becomes a statement about
	// the fixture (TKT-172/182).
	for range 40 {
		stranger, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-stranger"), "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		seedWalletOrder(t, db, ctx, uuid.NullUUID{UUID: stranger.ID, Valid: true}, "completed", time.Now().Add(-time.Hour))
	}
	// And a couple for the customer under test, so the query has something to
	// find and the plan is not trivially empty.
	for range 2 {
		seedWalletOrder(t, db, ctx, uuid.NullUUID{UUID: account.ID, Valid: true}, "completed", time.Now().Add(-time.Hour))
	}
	if _, err := db.ExecContext(ctx, `ANALYZE orders`); err != nil {
		t.Fatal(err)
	}

	plan := explainWalletPlan(t, db, ctx)

	if !strings.Contains(plan, "$1") {
		t.Fatalf("the plan substituted the parameter, so this is a CUSTOM plan and proves nothing "+
			"about what production executes:\n%s", plan)
	}
	if !strings.Contains(plan, "orders_customer_completed_idx") {
		t.Fatalf("the wallet read does not use its index — the result would still be correct while "+
			"scanning every order in the table, which is the defect ADR-019 exists to prevent:\n%s", plan)
	}
	// Using the index is not the same as being SCOPED by it: the customer must be
	// an Index Cond, not a filter applied after reading every completed entry.
	if !strings.Contains(plan, "Index Cond") || !strings.Contains(plan, "customer_id = $1") {
		t.Fatalf("customer_id is not an index CONDITION, so the scan reads every completed order "+
			"and filters afterwards — the result is scoped and the scan is not:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on orders") {
		t.Fatalf("the wallet read sequentially scans orders:\n%s", plan)
	}
}

// An order with no guest_order_ref must not reach the wallet (ai-review [high]).
//
// That is what an EXCHANGE replacement looks like: a second completed order that
// inherits the buyer's customer_id and has no reference of its own, because
// ADR-039/TKT-166 has the replacement tickets share the SOURCE order's link.
// Without the predicate the wallet renders a row pointing at the zero uuid — or
// fails the scan and 503s, hiding the customer's entire wallet because one of
// their purchases was exchanged.
func TestWalletExcludesOrdersWithNoTicketReference(t *testing.T) {
	db, ctx := outboxDB(t)
	account, err := RegisterCustomer(ctx, db, uniqueEmail("wallet-exchange"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	customer := uuid.NullUUID{UUID: account.ID, Valid: true}
	real := seedWalletOrder(t, db, ctx, customer, "completed", time.Now().Add(-time.Hour))

	// The exchange-replacement shape, inserted exactly as exchanges.go does:
	// completed, attributed, no guest_order_ref.
	reservation := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,1,1500,1500,1500,'EUR','completed')`,
		reservation, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,customer_id)
		VALUES($1,$2,'completed',$3,'exchange',$4)`,
		uuid.New(), reservation, "exchange:"+uuid.NewString(), customer); err != nil {
		t.Fatal(err)
	}

	page, _, err := CustomerOrders(ctx, db, account.ID, WalletCursor{}, WalletPageLimit)
	if err != nil {
		t.Fatalf("one unreferenced order must not fail the whole wallet: %v", err)
	}
	if len(page) != 1 || page[0].OrderID != real {
		t.Fatalf("page = %+v, want only the order that has a ticket reference (%s)", page, real)
	}
}
