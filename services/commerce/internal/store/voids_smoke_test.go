//go:build smoke

package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Comped-order voids, commerce half (TKT-171).
//
// The invariant every test here serves, stated once so no assertion has to be
// reverse-engineered from the code: A VOID MOVES TICKETS AND CAPACITY AND NEVER
// MONEY. Its counterpart is that a void is the ONLY reversal a comped order can
// have, and the only reversal a PAID order cannot.

func voidRequest(c Completion, key string) VoidRequest {
	return VoidRequest{
		OrderID: c.OrderID, OrganizerID: c.OrganizerID,
		IdempotencyKey: key, Actor: "support@example.test", Reason: "event cancelled",
	}
}

// COS-1 + COS-2 together: a comped order binds a void, and binding it writes NO
// money.
//
// The money assertion is stated as an absolute — ZERO order_refunds rows — and
// derived from the requirement rather than from a run. A count copied from what
// the code produced would pin the behaviour; "a void never writes a refund row" is
// the rule, and it is false for any number but zero.
func TestBindOrderVoidRecordsAReversalAndNoMoney(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-basic", 3, 0)

	v, err := BindOrderVoid(ctx, db, voidRequest(c, "void-1"))
	if err != nil {
		t.Fatalf("bind void: %v", err)
	}
	if v.ID != VoidID(c.OrganizerID, c.OrderID) {
		t.Errorf("void id = %v, want the order-derived id", v.ID)
	}
	// Whole-order, from the reservation. The request carries no quantity at all.
	if v.Quantity != 3 {
		t.Errorf("quantity = %d, want the reservation's 3", v.Quantity)
	}
	if v.TicketsVoided || v.CapacityReturned {
		t.Error("binding records the INTENT; the obligations are discharged by the driver")
	}

	var refunds int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1`, c.OrderID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 0 {
		t.Fatalf("a void wrote %d order_refunds rows; a void never writes money", refunds)
	}
	var facts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_facts WHERE order_id=$1`, c.OrderID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("a void wrote %d order_facts; ADR-003 — the journal records what happened", facts)
	}
}

// The identity decision, at the tier that owns it: a replay under a DIFFERENT
// request key converges on the same void.
//
// This is what makes a staff retry, a cancellation-run retry and a process restart
// discharge ONE downstream operation. Deriving the id from the request key instead
// would give each its own, and the order would be reversed more than once.
func TestBindOrderVoidReplaysAcrossDifferentRequestKeys(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-replay", 2, 0)

	first, err := BindOrderVoid(ctx, db, voidRequest(c, "staff-key"))
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	second, err := BindOrderVoid(ctx, db, voidRequest(c, "cancel:run:different"))
	if err != nil {
		t.Fatalf("second bind: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("two keys produced two voids (%v, %v) — a replay must converge", first.ID, second.ID)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_voids WHERE order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("order_voids rows = %d, want exactly 1", rows)
	}
}

// A second caller with DIFFERENT attribution ADOPTS the existing void rather than
// conflicting with it — the case the deterministic id exists for.
//
// This is the staff-then-cancellation-run sequence: staff void an order by hand,
// the event is then cancelled, and the run reaches the same id carrying
// `system:event-cancellation`. Conflicting there made the run retry to its attempt
// limit and report permanent failure — unable to repair an outstanding capacity
// leg it held the correct id for (ai-review F3).
//
// Unlike a refund, a void has no parameters to disagree about: its quantity comes
// from the reservation and its identity is the order. Actor and reason are a label
// on the operation, not part of it — so the FIRST binder's attribution survives,
// because they are the one who decided to reverse it.
func TestBindOrderVoidIsAdoptedByASecondCaller(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-adopt", 1, 0)

	staff := voidRequest(c, "staff-key")
	staff.Actor, staff.Reason = "support@example.test", "customer request"
	first, err := BindOrderVoid(ctx, db, staff)
	if err != nil {
		t.Fatalf("staff bind: %v", err)
	}
	// Discharge one leg only, so the adopting caller has something to repair —
	// which is the whole reason adoption matters rather than being tidier.
	if err := MarkVoidTicketsVoided(ctx, db, c.OrganizerID, first.ID); err != nil {
		t.Fatal(err)
	}

	run := voidRequest(c, "cancel:run:1")
	run.Actor, run.Reason = "system:event-cancellation", "event cancelled"
	adopted, err := BindOrderVoid(ctx, db, run)
	if err != nil {
		t.Fatalf("the cancellation run must ADOPT a staff-bound void, not conflict: %v", err)
	}
	if adopted.ID != first.ID {
		t.Fatalf("adopted a different void (%v vs %v)", adopted.ID, first.ID)
	}
	// The progress it must be able to resume.
	if !adopted.TicketsVoided || adopted.CapacityReturned {
		t.Fatalf("the adopting caller must see the real progress: %+v", adopted)
	}
	// The first binder's attribution survives: they decided to reverse it, the run
	// found the decision already made.
	if adopted.Actor != "support@example.test" || adopted.Reason != "customer request" {
		t.Fatalf("adoption must keep the original attribution, got %q/%q", adopted.Actor, adopted.Reason)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_voids WHERE order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("order_voids rows = %d, want exactly 1", rows)
	}
}

// COS-5: a PAID order is not voidable by this path. It has money to return and
// must go through the refund.
//
// Its own test, with a fixture that passes every EARLIER predicate — completed,
// unexchanged, right organizer — so the only thing it can fail on is the money
// check. Delete `unit != 0` and this is the test that goes red.
func TestBindOrderVoidRefusesAPaidOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-paid", 2, 2500)

	_, err := BindOrderVoid(ctx, db, voidRequest(c, "void-paid-1"))
	if !errors.Is(err, ErrVoidHasMoney) {
		t.Fatalf("err = %v, want ErrVoidHasMoney", err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_voids WHERE order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a refused void wrote %d rows", rows)
	}
}

// THE FINDING (ai-review F1): a zero-FACE order can still have CAPTURED money,
// and it must not be voidable.
//
// `unit_amount` is the face value; migration 0014 establishes `total = face +
// passed_on`. So a ticket priced at 0 carrying a fixed passed-on fee has
// `unit_amount = 0` and a real charged total. Voiding it would return the tickets
// and the seat and keep the buyer's fee.
//
// The fixture is the point: it passes EVERY other predicate — completed,
// unexchanged, right organizer, unit_amount 0 — so the only thing it can fail on
// is the total. Delete `|| total != 0` and this is the single test that goes red.
// The original version of this ticket had exactly that gap, and its smoke test
// demonstrated the defect while asserting success.
func TestBindOrderVoidRefusesAZeroFaceOrderThatCapturedFees(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-face-zero-fees", 2, 0)
	// A comped ticket with a passed-on fee: face 0, total 600 — money the buyer
	// really paid. Written directly because no supported path produces one today
	// (TKT-285), and the point is what the GUARD does with the state, not how the
	// state arises.
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET total_amount=600
		WHERE id=$1`, c.ReservationID); err != nil {
		t.Fatal(err)
	}

	_, err := BindOrderVoid(ctx, db, voidRequest(c, "void-fees-1"))
	if !errors.Is(err, ErrVoidHasMoney) {
		t.Fatalf("err = %v, want ErrVoidHasMoney — a zero FACE with a captured TOTAL is not a comped order", err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_voids WHERE order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a refused void wrote %d rows", rows)
	}
}

// COS-4, pinned so a later ticket cannot relax it as a shortcut: BindOrderRefund
// still refuses a zero unit amount.
//
// The two halves are complementary and both are asserted, because the pair is the
// actual contract — every order is reversible by exactly one of the two paths, and
// a change that let both accept the same order would be a double reversal.
func TestBindOrderRefundStillRefusesACompedOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-refund-still-refuses", 1, 0)

	if _, err := BindOrderRefund(ctx, db, request(c, "refund-comped", 1)); !errors.Is(err, ErrRefundNoMoney) {
		t.Fatalf("err = %v, want ErrRefundNoMoney — a comped order still has no refund", err)
	}
}

// The remaining guards, one test each, every fixture passing the earlier
// predicates so a deleted predicate fails its OWN test rather than being masked by
// an earlier refusal.
func TestBindOrderVoidGuards(t *testing.T) {
	t.Run("a foreign organizer sees nothing", func(t *testing.T) {
		db, ctx := outboxDB(t)
		c, _ := seedCompleted(t, db, ctx, "void-org", 1, 0)
		in := voidRequest(c, "void-org-1")
		in.OrganizerID = uuid.New()
		if _, err := BindOrderVoid(ctx, db, in); err == nil {
			t.Fatal("a void bound across organizers")
		}
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_voids WHERE order_id=$1`, c.OrderID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("a cross-organizer attempt wrote %d rows", rows)
		}
	})

	t.Run("an incomplete order is not voidable", func(t *testing.T) {
		db, ctx := outboxDB(t)
		c, _ := seedCompleted(t, db, ctx, "void-incomplete", 1, 0)
		// 'declined', not 'failed': orders_status_check (migration 0004) enumerates
		// the statuses, and an invented one is refused by the database rather than
		// silently producing a non-completed order — which is how this fixture first
		// failed, correctly.
		if _, err := db.ExecContext(ctx, `UPDATE orders SET status='declined' WHERE id=$1`, c.OrderID); err != nil {
			t.Fatal(err)
		}
		if _, err := BindOrderVoid(ctx, db, voidRequest(c, "void-incomplete-1")); !errors.Is(err, ErrOrderNotVoidable) {
			t.Fatalf("err = %v, want ErrOrderNotVoidable", err)
		}
	})

	t.Run("an exchanged order is not voidable", func(t *testing.T) {
		db, ctx := outboxDB(t)
		c, _ := seedCompleted(t, db, ctx, "void-exchanged", 1, 0)
		// An order an exchange already owns would be reversed twice. Mirrors
		// BindOrderRefund's identical refusal.
		if _, err := db.ExecContext(ctx, `
			INSERT INTO order_exchanges(organizer_id,id,source_order_id,idempotency_key,request_fingerprint,
				target_ticket_type_id,quantity,status,actor,reason)
			VALUES($1,$2,$3,$4,'fp',$5,1,'switch_pending','staff','upgrade')`,
			c.OrganizerID, uuid.New(), c.OrderID, "exch-"+c.OrderID.String(), uuid.New()); err != nil {
			t.Skipf("exchange fixture unavailable: %v", err)
		}
		if _, err := BindOrderVoid(ctx, db, voidRequest(c, "void-exchanged-1")); !errors.Is(err, ErrOrderNotVoidable) {
			t.Fatalf("err = %v, want ErrOrderNotVoidable", err)
		}
	})
}

// ADR-038 §1 IN THE DATABASE. The driver enforces the order in its control flow;
// this proves the WRITE refuses it too, so a driver that stopped enforcing it
// cannot quietly free a seat against tickets that still admit.
//
// Two mechanisms deliberately, and each is tested: MarkVoidCapacityReturned's
// `tickets_voided_at IS NOT NULL` predicate, and migration 0025's CHECK.
func TestCapacityCannotBeRecordedBeforeTicketsAreVoided(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "void-ordering", 1, 0)
	v, err := BindOrderVoid(ctx, db, voidRequest(c, "void-ordering-1"))
	if err != nil {
		t.Fatalf("bind void: %v", err)
	}

	// The guarded write is a no-op rather than an error — it matches no row.
	if err := MarkVoidCapacityReturned(ctx, db, c.OrganizerID, v.ID); err != nil {
		t.Fatalf("mark capacity: %v", err)
	}
	after, found, err := LookupOrderVoid(ctx, db, c.OrganizerID, c.OrderID)
	if err != nil || !found {
		t.Fatalf("lookup: %v found=%v", err, found)
	}
	if after.CapacityReturned {
		t.Fatal("capacity was recorded as returned before the tickets were voided — " +
			"the ADR-038 §1 sequence that oversells")
	}

	// And the CHECK constraint refuses it even to a writer that bypasses the guard.
	_, err = db.ExecContext(ctx, `UPDATE order_voids SET capacity_returned_at=now() WHERE organizer_id=$1 AND id=$2`,
		c.OrganizerID, v.ID)
	if err == nil {
		t.Fatal("migration 0025's CHECK must refuse capacity_returned_at without tickets_voided_at")
	}

	// In the right order both markers land, so the guards are not simply refusing
	// everything — a test that only proved refusal would pass with the whole
	// mechanism broken shut.
	if err := MarkVoidTicketsVoided(ctx, db, c.OrganizerID, v.ID); err != nil {
		t.Fatalf("mark tickets: %v", err)
	}
	if err := MarkVoidCapacityReturned(ctx, db, c.OrganizerID, v.ID); err != nil {
		t.Fatalf("mark capacity after voiding: %v", err)
	}
	done, _, err := LookupOrderVoid(ctx, db, c.OrganizerID, c.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if !done.TicketsVoided || !done.CapacityReturned {
		t.Fatalf("both markers must land when discharged in order: %+v", done)
	}
}
