//go:build smoke

package bulkrefund

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/store"
)

// The runner against real PostgreSQL (TKT-159). The unit tests answer "which outcome did
// the runner choose"; this one answers the question a fake store cannot be wrong about —
// whether the claim/finalize SQL actually drains a book. The whole-stack smoke test found
// a run that enumerated and then never resolved, which every fake passed.

var (
	migrateOnce sync.Once
	migrateErr  error
)

// runnerDB opens this package's OWN database, not the store package's.
//
// Its own because this is the third package that calls store.Migrate, and
// `go test ./internal/...` runs packages as separate, CONCURRENT test binaries: the
// sync.Once below serializes migration within this binary and nothing at all across
// them. Sharing commerce_store_smoke meant two goose runs against one database, the
// loser dying mid-migration on "relation ... already exists" and taking a different
// ~30-test subset down each run (TKT-198). ./internal/mailer took this same route in
// TKT-226, and payments before it for ./internal/api; scripts/smoke.sh creates
// commerce_bulkrefund_smoke.
func runnerDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_BULKREFUND_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_BULKREFUND_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	migrateOnce.Do(func() { migrateErr = store.Migrate(ctx, db) })
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	return db, ctx
}

// stubRefunder discharges everything, so the only thing that can fail this test is the
// runner's own SQL.
type stubRefunder struct {
	db  *sql.DB
	ctx context.Context
}

func (s stubRefunder) Refund(_ context.Context, in store.RefundRequest) (refunds.Result, error) {
	// Move the order's projection the way CompleteOrderRefund would, without money.
	if _, err := s.db.ExecContext(s.ctx, `
		UPDATE orders SET refunded_quantity=$2, refunded_amount=$3, refund_status='full' WHERE id=$1`,
		in.OrderID, in.Quantity, int64(in.Quantity)*1000); err != nil {
		return refunds.Result{}, err
	}
	return refunds.Result{Refund: store.Refund{ID: store.RefundID(in.OrganizerID, in.IdempotencyKey), OrderID: in.OrderID, Quantity: in.Quantity}}, nil
}

func (s stubRefunder) DriveReversal(_ context.Context, r store.Refund) store.Refund { return r }

func TestRunnerDrainsARealBook(t *testing.T) {
	db, ctx := runnerDB(t)
	org, slot := uuid.New(), uuid.New()

	var orders []uuid.UUID
	for range 3 {
		res, order := uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
			VALUES($1,$2,$3,$4,$5,$6,1,1000,1000,1000,'EUR','completed')`,
			res, org, uuid.New(), slot, uuid.New(), uuid.New()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
			VALUES($1,$2,'completed',$3,'fp',$4)`, order, res, "run-"+order.String(), uuid.New()); err != nil {
			t.Fatal(err)
		}
		orders = append(orders, order)
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM cancellation_refund_orders WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM order_refunds WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, order)
			_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, res)
		})
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM cancellation_refund_runs WHERE slot_id=$1`, slot) })

	run, err := store.BindCancellationRun(ctx, db, store.CancellationRunRequest{
		OrganizerID: org, SlotID: slot, IdempotencyKey: "real-book", Actor: "ops", Reason: "cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved := New(DBStore{DB: db}, stubRefunder{db: db, ctx: ctx}, time.Minute, 2, time.Minute).RunOnce(ctx)

	report, err := store.CancellationReport(ctx, db, org, run.ID, 100, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Pending != 0 {
		t.Fatalf("%d rows still pending after a full pass (resolved=%d) — the runner enumerated a book it cannot drain", report.Counts.Pending, resolved)
	}
	if report.Counts.Total != len(orders) || report.Counts.Refunded != len(orders) {
		t.Fatalf("counts = %+v, want %d refunded", report.Counts, len(orders))
	}
	if report.Run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", report.Run.Status)
	}
}
