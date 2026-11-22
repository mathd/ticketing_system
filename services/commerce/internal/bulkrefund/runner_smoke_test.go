//go:build smoke

package bulkrefund

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

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

// Void is the comped reversal (TKT-171). It binds a real order_voids row through
// the real store, so this smoke stub exercises the actual bind — including its
// guards — rather than asserting against a shape it made up. It moves NO money
// projection, which is the point: the order's refunded_* columns must be
// untouched by a void.
func (s stubRefunder) Void(_ context.Context, in store.VoidRequest) (refunds.VoidResult, error) {
	v, err := store.BindOrderVoid(s.ctx, s.db, in)
	if err != nil {
		return refunds.VoidResult{}, err
	}
	// The driver's two legs have no downstream to call here, so record both
	// obligations as discharged the way a successful drive would.
	if err := store.MarkVoidTicketsVoided(s.ctx, s.db, in.OrganizerID, v.ID); err != nil {
		return refunds.VoidResult{}, err
	}
	if err := store.MarkVoidCapacityReturned(s.ctx, s.db, in.OrganizerID, v.ID); err != nil {
		return refunds.VoidResult{}, err
	}
	v.TicketsVoided, v.CapacityReturned = true, true
	return refunds.VoidResult{Void: v}, nil
}

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

// TKT-598. RunOnce against real PostgreSQL for the two attempt rules that only fake-store
// tests covered before: a retryable failure BEFORE the drive is charged one attempt with the
// lease kept (TKT-521), and an interruption before the drive hands the claim back at no cost
// (TKT-300). The driven retryable case is covered by
// TestBulkRunRetriesWhenTheReadAfterAPaymentsRefusalFails and
// TestBulkRefundOfAnUnissuedReplacementNeverReachesPayments.

// isolatedRunnerDB opens a pool whose search_path is a fresh schema that only this test uses,
// migrated by store.Migrate. The claim query sees every claimable row in its schema, so rows
// left by other tests in the shared schema would join the claimed batch and change which rows
// the runner drives.
func isolatedRunnerDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	admin, ctx := runnerDB(t)
	schema := "bulkrefund_" + uuid.NewString()[:8]
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	cfg, err := pgx.ParseConfig(os.Getenv("COMMERCE_BULKREFUND_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}
	return db, ctx
}

// seedPaidOrders seeds n completed one-unit EUR orders at 1000 minor units in one organizer and
// slot, in the shape TestRunnerDrainsARealBook uses.
func seedPaidOrders(t *testing.T, ctx context.Context, db *sql.DB, org, slot uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	var orders []uuid.UUID
	for range n {
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
			_, _ = db.Exec(`DELETE FROM cancellation_refund_orders WHERE organizer_id=$1 AND order_id=$2`, org, order)
			_, _ = db.Exec(`DELETE FROM order_refunds WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, order)
			_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, res)
		})
	}
	return orders
}

func bindIsolatedRun(t *testing.T, ctx context.Context, db *sql.DB, org, slot uuid.UUID, key string) store.CancellationRun {
	t.Helper()
	run, err := store.BindCancellationRun(ctx, db, store.CancellationRunRequest{
		OrganizerID: org, SlotID: slot, IdempotencyKey: key, Actor: "ops", Reason: "cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	// This cleanup runs before the order cleanups (cleanups run in reverse), so it deletes the
	// run's ledger rows before the run row that they reference.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cancellation_refund_orders WHERE organizer_id=$1 AND run_id=$2`, org, run.ID)
		_, _ = db.Exec(`DELETE FROM cancellation_refund_runs WHERE organizer_id=$1 AND id=$2`, org, run.ID)
	})
	return run
}

// ledgerSnapshot is one cancellation_refund_orders row, read directly rather than through
// the code under test.
type ledgerSnapshot struct {
	attempts  int
	claim     uuid.NullUUID
	lease     sql.NullTime
	leaseLive sql.NullBool // lease_until > now(), by PostgreSQL's clock
	outcome   sql.NullString
	requested sql.NullInt32
}

func readLedger(t *testing.T, ctx context.Context, db *sql.DB, org, run, order uuid.UUID) ledgerSnapshot {
	t.Helper()
	var s ledgerSnapshot
	if err := db.QueryRowContext(ctx, `
		SELECT attempts, claim_id, lease_until, lease_until > now(), outcome, requested_quantity
		FROM cancellation_refund_orders WHERE organizer_id=$1 AND run_id=$2 AND order_id=$3`,
		org, run, order).Scan(&s.attempts, &s.claim, &s.lease, &s.leaseLive, &s.outcome, &s.requested); err != nil {
		t.Fatalf("read ledger row of order %s: %v", order, err)
	}
	return s
}

// requireActiveFreshClaim checks a row the runner has claimed and not yet driven.
func requireActiveFreshClaim(t *testing.T, s ledgerSnapshot, order uuid.UUID) {
	t.Helper()
	if s.attempts != 0 || !s.claim.Valid || !s.leaseLive.Valid || !s.leaseLive.Bool || s.outcome.Valid || s.requested.Valid {
		t.Fatalf("order %s at the lookup = %+v, want zero attempts, an active claim and lease, no outcome and no fixed quantity", order, s)
	}
}

// lookupFaultStore is the real DBStore with one seam: every LookupRefund runs `fault`, and a
// non-nil result is returned as the lookup's error.
type lookupFaultStore struct {
	DBStore
	fault func(ctx context.Context, org, refundID uuid.UUID) error
	calls int
}

func (s *lookupFaultStore) LookupRefund(ctx context.Context, org, refundID uuid.UUID) (store.Refund, bool, error) {
	s.calls++
	if err := s.fault(ctx, org, refundID); err != nil {
		return store.Refund{}, false, err
	}
	return s.DBStore.LookupRefund(ctx, org, refundID)
}

// attemptRefunder fails the test on any call: these passes must end before the drive.
type attemptRefunder struct {
	t     *testing.T
	calls int
}

func (r *attemptRefunder) Refund(_ context.Context, in store.RefundRequest) (refunds.Result, error) {
	r.calls++
	r.t.Errorf("Refund called for order %s; the pass must end before the drive", in.OrderID)
	return refunds.Result{}, errors.New("unexpected refund")
}

func (r *attemptRefunder) DriveReversal(_ context.Context, refund store.Refund) store.Refund {
	r.calls++
	r.t.Errorf("DriveReversal called for refund %s; the pass must end before the drive", refund.ID)
	return refund
}

func (r *attemptRefunder) Void(_ context.Context, in store.VoidRequest) (refunds.VoidResult, error) {
	r.calls++
	r.t.Errorf("Void called for order %s; the pass must end before the drive", in.OrderID)
	return refunds.VoidResult{}, errors.New("unexpected void")
}

// A fresh row whose refund lookup fails before the drive: RunOnce reaches `record` with a
// retryable `internal` failure and drove=false. The pass is the row's own failure, so it costs
// one attempt, and the lease stays as the backoff (TKT-521).
func TestRunnerChargesPreDriveRetryableFailureInPostgres(t *testing.T) {
	db, ctx := isolatedRunnerDB(t)
	org, slot := uuid.New(), uuid.New()
	order := seedPaidOrders(t, ctx, db, org, slot, 1)[0]
	run := bindIsolatedRun(t, ctx, db, org, slot, "pre-drive")
	wantRefund := store.RefundID(org, store.CancellationRefundKey(slot, order))

	var before ledgerSnapshot
	st := &lookupFaultStore{DBStore: DBStore{DB: db}}
	st.fault = func(lctx context.Context, gotOrg, gotRefund uuid.UUID) error {
		if lctx.Err() != nil {
			t.Fatalf("lookup ran on a finished context: %v", lctx.Err())
		}
		if gotOrg != org || gotRefund != wantRefund {
			t.Fatalf("lookup of %s/%s, want the fixture's %s/%s", gotOrg, gotRefund, org, wantRefund)
		}
		before = readLedger(t, ctx, db, org, run.ID, order)
		requireActiveFreshClaim(t, before, order)
		return errors.New("injected refund lookup failure")
	}
	refunder := &attemptRefunder{t: t}

	resolved := New(st, refunder, time.Minute, 2, 2*time.Minute).RunOnce(ctx)

	after := readLedger(t, ctx, db, org, run.ID, order)
	if after.attempts != 1 {
		t.Fatalf("attempts = %d after a retryable failure before the drive, want 1 charged", after.attempts)
	}
	if st.calls != 1 || refunder.calls != 0 || resolved != 0 {
		t.Fatalf("lookups = %d, refunder calls = %d, resolved = %d; want 1, 0, 0", st.calls, refunder.calls, resolved)
	}
	if after.outcome.Valid || after.requested.Valid {
		t.Fatalf("row = %+v, want no outcome and no fixed quantity", after)
	}
	if after.claim != before.claim || !after.lease.Valid || !after.lease.Time.Equal(before.lease.Time) ||
		!after.leaseLive.Valid || !after.leaseLive.Bool {
		t.Fatalf("claim/lease after the pass = %v/%v (live %v), want the original %v/%v still in the future",
			after.claim, after.lease, after.leaseLive, before.claim, before.lease)
	}
}

// Two orders in one claimed batch, and the runner's context ends inside the first order's
// fresh lookup. The first row reaches `record` with drove=false; the second reaches RunOnce's
// per-order context check. Both hand-backs release the claim and cost no attempt: the
// interruption belongs to the worker, not to the row (TKT-300).
func TestRunnerReleasesInterruptedUndrivenClaimWithoutChargeInPostgres(t *testing.T) {
	db, parent := isolatedRunnerDB(t)
	org, slot := uuid.New(), uuid.New()
	orders := seedPaidOrders(t, parent, db, org, slot, 2)
	run := bindIsolatedRun(t, parent, db, org, slot, "interrupted")
	orderOf := map[uuid.UUID]uuid.UUID{}
	for _, o := range orders {
		orderOf[store.RefundID(org, store.CancellationRefundKey(slot, o))] = o
	}

	runCtx, cancel := context.WithCancel(parent)
	defer cancel()
	var first uuid.UUID
	st := &lookupFaultStore{DBStore: DBStore{DB: db}}
	st.fault = func(lctx context.Context, gotOrg, gotRefund uuid.UUID) error {
		if lctx.Err() != nil {
			t.Fatalf("lookup ran on a finished context: %v", lctx.Err())
		}
		o, ok := orderOf[gotRefund]
		if !ok || gotOrg != org {
			t.Fatalf("lookup of %s/%s, not a fixture refund", gotOrg, gotRefund)
		}
		first = o
		var claim uuid.NullUUID
		for _, each := range orders {
			s := readLedger(t, parent, db, org, run.ID, each)
			requireActiveFreshClaim(t, s, each)
			if claim.Valid && s.claim != claim {
				t.Fatalf("claims %v and %v, want both orders in one claimed batch", claim, s.claim)
			}
			claim = s.claim
		}
		cancel()
		return context.Canceled
	}
	refunder := &attemptRefunder{t: t}

	resolved := New(st, refunder, time.Minute, 2, 2*time.Minute).RunOnce(runCtx)

	if first == uuid.Nil {
		t.Fatal("the runner never looked up a fixture refund")
	}
	second := orders[0]
	if second == first {
		second = orders[1]
	}
	for _, c := range []struct {
		order uuid.UUID
		path  string
	}{
		{first, "the first order, handed back by record"},
		{second, "the second order, handed back by RunOnce's per-order check"},
	} {
		s := readLedger(t, parent, db, org, run.ID, c.order)
		if s.attempts != 0 {
			t.Fatalf("%s: attempts = %d, want 0 for an undriven interruption", c.path, s.attempts)
		}
		if s.claim.Valid || s.lease.Valid || s.requested.Valid || s.outcome.Valid {
			t.Fatalf("%s: row = %+v, want the claim and lease released, no fixed quantity and no outcome", c.path, s)
		}
	}
	if st.calls != 1 || refunder.calls != 0 || resolved != 0 {
		t.Fatalf("lookups = %d, refunder calls = %d, resolved = %d; want 1, 0, 0", st.calls, refunder.calls, resolved)
	}
}
