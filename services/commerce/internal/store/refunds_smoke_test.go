//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Post-purchase refunds, commerce half (TKT-156). Everything here is about the two
// invariants a money path cannot get wrong: the amount comes from the persisted unit
// price (never a division), and the cumulative refund can never exceed the order.
//
// The ADR-016 trap is pinned by TestRefundLeavesCheckoutStatusCompleted: orders.status
// keeps meaning "how the checkout ended". A refunded purchase is still a completed
// checkout, and the refund lives on its own dimension.

// seedCompleted returns a completed order whose reservation has a deliberately
// non-divisible total, so any implementation that divides instead of multiplying is
// visible in the assertion rather than accidentally right.
func seedCompleted(t *testing.T, db *sql.DB, ctx context.Context, key string, quantity int32, unit int64) (Completion, uuid.UUID) {
	t.Helper()
	c := Completion{
		ReservationID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(),
		BuyerID: uuid.New(), SlotID: uuid.New(), TicketTypeID: uuid.New(), Quantity: quantity,
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'EUR','completed')`,
		c.ReservationID, c.OrganizerID, uuid.New(), c.SlotID, c.TicketTypeID, c.BuyerID,
		quantity, unit, unit*int64(quantity)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`,
		c.OrderID, c.ReservationID, key, uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_refunds WHERE order_id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM order_facts WHERE order_id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, c.ReservationID)
	})
	return c, c.OrderID
}

func request(c Completion, key string, quantity int32) RefundRequest {
	return RefundRequest{
		OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: quantity,
		IdempotencyKey: key, Actor: "support@example.test", Reason: "buyer cannot attend",
	}
}

// AC1: q × unit_amount, taken from the persisted unit price. 3 × 1667 = 5001; a
// proportional split of the 5001 total across 3 would be 1667 by luck, so the fixture
// refunds 2 of 3 — 3334, which no rounding of a divided total reaches by accident.
func TestBindOrderRefundDerivesAmountFromPersistedUnitAmount(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "refund-amount", 3, 1667)

	r, err := BindOrderRefund(ctx, db, request(c, "refund-1", 2))
	if err != nil {
		t.Fatalf("bind refund: %v", err)
	}
	if r.Amount != 3334 || r.UnitAmount != 1667 || r.Quantity != 2 {
		t.Fatalf("refund = %+v, want 2 × 1667 = 3334", r)
	}
	if r.Currency != "EUR" {
		t.Fatalf("currency = %q", r.Currency)
	}
}

// AC3, commerce half: the same idempotency key returns the same refund identity and
// writes one row.
func TestBindOrderRefundReplayReturnsTheSameRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "refund-replay", 2, 1250)

	first, err := BindOrderRefund(ctx, db, request(c, "refund-1", 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := BindOrderRefund(ctx, db, request(c, "refund-1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("replay must return the same refund: %+v vs %+v", first, second)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}

	// Same key, different body: a conflict, exactly as claimOrder treats a reused
	// checkout key.
	conflicting := request(c, "refund-1", 2)
	if _, err := BindOrderRefund(ctx, db, conflicting); !errors.Is(err, ErrRefundConflict) {
		t.Fatalf("err = %v, want ErrRefundConflict", err)
	}
}

// AC4: cumulative quantity is the ceiling, and bound-but-uncompleted refunds count.
func TestBindOrderRefundRejectsCumulativeOverRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "refund-ceiling", 3, 1000)

	if _, err := BindOrderRefund(ctx, db, request(c, "refund-1", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "refund-2", 2)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("err = %v, want ErrRefundExceedsOrder", err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "refund-2", 1)); err != nil {
		t.Fatalf("the exact remainder must fit: %v", err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "refund-3", 1)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("err = %v, want ErrRefundExceedsOrder once fully refunded", err)
	}
}

// AC4, the race: two individually-valid refunds that together over-refund, raced. The
// order-row lock is what makes exactly one commit.
func TestConcurrentOrderRefundsCannotOverRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "refund-race", 2, 1000)

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, key := range []string{"race-a", "race-b"} {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			start.Wait()
			_, errs[i] = BindOrderRefund(ctx, db, request(c, key, 2))
		}(i, key)
	}
	start.Done()
	wg.Wait()

	var ok int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrRefundExceedsOrder):
		default:
			t.Fatalf("refund %d failed unexpectedly: %v", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d refunds bound, want exactly 1", ok)
	}
}

// AC5: only a completed checkout can be refunded, and the whole orders.status vocabulary
// is walked so a new token cannot silently become refundable. Recovery's `refunded` is in
// the list on purpose — it means a FAILED checkout whose money was returned.
func TestBindOrderRefundRejectsNonCompletedOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, status := range []string{
		"created", "payment_unknown", "confirmation_pending", "release_pending",
		"declined", "timeout", "reconciliation_required", "refunded",
	} {
		t.Run(status, func(t *testing.T) {
			c, _ := seedCompleted(t, db, ctx, "refund-status-"+status, 2, 1000)
			if _, err := db.ExecContext(ctx, `UPDATE orders SET status=$2 WHERE id=$1`, c.OrderID, status); err != nil {
				t.Fatal(err)
			}
			if _, err := BindOrderRefund(ctx, db, request(c, "refund-1", 1)); !errors.Is(err, ErrOrderNotRefundable) {
				t.Fatalf("err = %v, want ErrOrderNotRefundable", err)
			}
		})
	}
}

// A free order has no captured money, and no provider issues a zero refund
// (compensationAllowed requires CapturedAmount > 0). Refuse rather than fabricate.
func TestBindOrderRefundRejectsZeroAmountOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "refund-free", 2, 0)
	if _, err := BindOrderRefund(ctx, db, request(c, "refund-1", 1)); !errors.Is(err, ErrRefundNoMoney) {
		t.Fatalf("err = %v, want ErrRefundNoMoney", err)
	}
}

// AC6: the ADR-016 trap. A refunded purchase is still a completed checkout — the refund
// state is a separate dimension, and no new orders.status token exists.
func TestRefundLeavesCheckoutStatusCompleted(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "refund-projection", 3, 1000)

	partial, err := BindOrderRefund(ctx, db, request(c, "refund-1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, partial.ID, uuid.New()); err != nil {
		t.Fatalf("complete refund: %v", err)
	}
	assertOrderRefundState(t, db, ctx, c.OrderID, "completed", "partial", 1, 1000)

	rest, err := BindOrderRefund(ctx, db, request(c, "refund-2", 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, rest.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	assertOrderRefundState(t, db, ctx, c.OrderID, "completed", "full", 3, 3000)

	// Completion is once-only: replaying it must not double the projection.
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, rest.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	assertOrderRefundState(t, db, ctx, c.OrderID, "completed", "full", 3, 3000)

	// A7: the reservation still records the sale that happened.
	var reservationStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM reservations WHERE id=$1`, c.ReservationID).Scan(&reservationStatus); err != nil {
		t.Fatal(err)
	}
	if reservationStatus != "completed" {
		t.Fatalf("reservations.status = %q, want completed (TKT-156 does not touch it)", reservationStatus)
	}
}

func assertOrderRefundState(t *testing.T, db *sql.DB, ctx context.Context, order uuid.UUID, wantStatus, wantRefund string, wantQty int32, wantAmount int64) {
	t.Helper()
	var status, refundStatus string
	var qty int32
	var amount int64
	if err := db.QueryRowContext(ctx, `SELECT status,refund_status,refunded_quantity,refunded_amount FROM orders WHERE id=$1`, order).
		Scan(&status, &refundStatus, &qty, &amount); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || refundStatus != wantRefund || qty != wantQty || amount != wantAmount {
		t.Fatalf("order state = (%s,%s,%d,%d), want (%s,%s,%d,%d)",
			status, refundStatus, qty, amount, wantStatus, wantRefund, wantQty, wantAmount)
	}
}

// A5: the whole refund path assumes orders.idempotency_key IS the key payments bound its
// charge operation under — checkout passes the same header string to both. Nothing in
// either service's types says so, so it is pinned here.
func TestRefundReadsThePaymentsSourceKeyFromTheOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-key-of-record", 1, 900)
	r, err := BindOrderRefund(ctx, db, request(c, "refund-1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if r.PaymentSourceKey != "charge-key-of-record" {
		t.Fatalf("payment source key = %q, want the order's idempotency_key", r.PaymentSourceKey)
	}
}
