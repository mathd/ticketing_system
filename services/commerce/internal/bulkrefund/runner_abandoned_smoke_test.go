//go:build smoke

package bulkrefund

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/store"
)

// TKT-525 S2 (ADR-087, owner decision 2026-10-12): a bulk run that finds its own cancellation
// refund recorded as `abandoned` makes ZERO payments calls, in both quantity branches. It
// classifies from the order projection and the reversal state: remaining quantity reports
// `refund_refused`; at zero, the existing reversal precedence applies — `already_refunded` when
// every reversal is discharged, `reversal_outstanding` when an obligation remains (0012's success
// CHECK forbids a success with an obligation outstanding).
//
// The real refunds.New is wired with a recording transport, AND the refunder is wrapped to count
// its Refund calls. Both are asserted: the shared service refuses an abandoned refund too, so a
// removed runner skip would still make zero payments calls, and only the refunder count shows it.

// recordingTransport is the refund service's HTTP. Payments answers a terminal provider failure,
// which is the only way a real cancellation refund ends up abandoned.
type recordingTransport struct {
	mu    sync.Mutex
	calls []string
}

func (c *recordingTransport) call(_ context.Context, method, u, _ string, in any, _ bool) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, _ := json.Marshal(in)
	c.calls = append(c.calls, method+" "+u+" "+string(body))
	if strings.HasSuffix(u, "/internal/psp/partial-refund") {
		return 422, []byte(`{"code":"provider_refund_failed","error":"provider refund failed","provider_ref":"re_failed_bulk"}`), nil
	}
	return 200, []byte(`{}`), nil
}

func (c *recordingTransport) payments() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if strings.Contains(call, "http://payments.test") {
			n++
		}
	}
	return n
}

// countingRefunder counts calls into the shared refund unit.
type countingRefunder struct {
	*refunds.Service
	mu      sync.Mutex
	refunds int
}

func (c *countingRefunder) Refund(ctx context.Context, in store.RefundRequest) (refunds.Result, error) {
	c.mu.Lock()
	c.refunds++
	c.mu.Unlock()
	return c.Service.Refund(ctx, in)
}

func (c *countingRefunder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refunds
}

// lateAbandonStore records the abandonment AFTER the runner's refund lookup has read the row:
// the window between the bulk lookup and the Commerce bind.
type lateAbandonStore struct {
	DBStore
	ctx     context.Context
	abandon func()
	once    sync.Once
}

func (s *lateAbandonStore) LookupRefund(ctx context.Context, org, refundID uuid.UUID) (store.Refund, bool, error) {
	r, found, err := s.DBStore.LookupRefund(ctx, org, refundID)
	s.once.Do(s.abandon)
	return r, found, err
}

type abandonedOrder struct {
	org, slot, order uuid.UUID
	bulkRefund       uuid.UUID
}

// seedAbandonedCancellation seeds a two-unit order at 1000, binds its cancellation refund for
// both units under the (slot, order) key, and records the operator's `abandoned` outcome for it.
func seedAbandonedCancellation(t *testing.T, ctx context.Context, db *sql.DB, abandon bool) abandonedOrder {
	t.Helper()
	o := abandonedOrder{org: uuid.New(), slot: uuid.New(), order: uuid.New()}
	res := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1000,2000,2000,'EUR','completed')`,
		res, o.org, uuid.New(), o.slot, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fp',$4)`, o.order, res, "checkout-"+o.order.String(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	r, err := store.BindOrderRefund(ctx, db, store.RefundRequest{
		OrderID: o.order, OrganizerID: o.org, Quantity: 2,
		IdempotencyKey: store.CancellationRefundKey(o.slot, o.order),
		Actor:          store.CancellationRefundActor, Reason: store.CancellationRefundReason,
	})
	if err != nil {
		t.Fatal(err)
	}
	o.bulkRefund = r.ID
	if abandon {
		recordAbandonedOutcome(t, ctx, db, o)
	}
	return o
}

func recordAbandonedOutcome(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
	t.Helper()
	in := store.RefundOutcomeRequest{OrganizerID: o.org, RefundID: o.bulkRefund, Kind: store.OutcomeAbandoned,
		OperatorName: "Ada Operator", Reason: "nobody will refund this attempt"}
	target, err := store.InspectRefundOutcomeTarget(ctx, db, o.org, o.bulkRefund)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordAbandoned(ctx, db, in, target, "re_failed_bulk"); err != nil {
		t.Fatal(err)
	}
}

// refundTheRestByStaff completes a staff refund of both units (possible because the abandoned
// unit is released), and optionally discharges its reversal.
func refundTheRestByStaff(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder, discharge bool) {
	t.Helper()
	r, err := store.BindOrderRefund(ctx, db, store.RefundRequest{
		OrderID: o.order, OrganizerID: o.org, Quantity: 2, IdempotencyKey: "staff-" + o.order.String(),
		Actor: "support@example.test", Reason: "replacement after abandonment",
	})
	if err != nil {
		t.Fatalf("staff replacement refund: %v", err)
	}
	if err := store.CompleteOrderRefund(ctx, db, o.org, r.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if discharge {
		if err := store.MarkRefundTicketsVoided(ctx, db, o.org, r.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkRefundCapacityReturned(ctx, db, o.org, r.ID); err != nil {
			t.Fatal(err)
		}
	}
}

type verdict struct {
	outcome, failureCode    string
	money, voided, returned bool
	refundedQuantity        int32
	refundedAmount          int64
	attempts                int
	requestedQuantity       sql.NullInt32
}

func readVerdict(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) verdict {
	t.Helper()
	var v verdict
	var outcome, code sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT outcome, failure_code, money_refunded, tickets_voided, capacity_returned, refunded_quantity, refunded_amount, attempts, requested_quantity
		FROM cancellation_refund_orders WHERE organizer_id=$1 AND order_id=$2`, o.org, o.order).
		Scan(&outcome, &code, &v.money, &v.voided, &v.returned, &v.refundedQuantity, &v.refundedAmount, &v.attempts, &v.requestedQuantity); err != nil {
		t.Fatal(err)
	}
	v.outcome, v.failureCode = outcome.String, code.String
	return v
}

func startRun(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder, fixQuantity bool) {
	t.Helper()
	run, err := store.BindCancellationRun(ctx, db, store.CancellationRunRequest{
		OrganizerID: o.org, SlotID: o.slot, IdempotencyKey: "cancel-" + o.order.String(), Actor: "ops", Reason: "event cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fixQuantity {
		return
	}
	// The fixed branch: a previous attempt of THIS run fixed the quantity before it was
	// interrupted.
	if _, err := store.EnumerateCancellationBook(ctx, db, o.org, run.ID, 100); err != nil {
		t.Fatal(err)
	}
	if res, err := db.ExecContext(ctx, `UPDATE cancellation_refund_orders SET requested_quantity=2 WHERE organizer_id=$1 AND run_id=$2 AND order_id=$3`,
		o.org, run.ID, o.order); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("fixed the quantity on %d rows", n)
	}
}

func TestBulkRunSkipsAnAbandonedRefundWithoutCallingPayments(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder)
		want  verdict
	}{
		{"remaining quantity", func(*testing.T, context.Context, *sql.DB, abandonedOrder) {},
			verdict{outcome: "failed", failureCode: "refund_refused"}},
		{"zero quantity, reversal discharged", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			refundTheRestByStaff(t, ctx, db, o, true)
		}, verdict{outcome: "already_refunded", money: true, voided: true, returned: true, refundedQuantity: 2, refundedAmount: 2000}},
		{"zero quantity, reversal outstanding", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			refundTheRestByStaff(t, ctx, db, o, false)
		}, verdict{outcome: "failed", failureCode: "reversal_outstanding", money: true, refundedQuantity: 2, refundedAmount: 2000}},
	}
	for _, branch := range []struct {
		name  string
		fixed bool
	}{{"unfixed quantity", false}, {"fixed quantity", true}} {
		for _, c := range cases {
			t.Run(branch.name+"/"+c.name, func(t *testing.T) {
				db, ctx := runnerDB(t)
				o := seedAbandonedCancellation(t, ctx, db, true)
				c.setup(t, ctx, db, o)
				startRun(t, ctx, db, o, branch.fixed)

				transport := &recordingTransport{}
				refunder := &countingRefunder{Service: refunds.New(db, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
				New(DBStore{DB: db}, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

				if n := transport.payments(); n != 0 {
					t.Fatalf("the run made %d payments calls for an abandoned refund", n)
				}
				if n := refunder.count(); n != 0 {
					t.Fatalf("the run called the refund unit %d times; the abandoned row must be skipped before it", n)
				}
				got := readVerdict(t, ctx, db, o)
				want := c.want
				want.attempts = 1
				want.requestedQuantity = got.requestedQuantity
				if got != want {
					t.Fatalf("verdict = %+v, want %+v", got, want)
				}
				if branch.fixed && (!got.requestedQuantity.Valid || got.requestedQuantity.Int32 != 2) {
					t.Fatalf("the fixed quantity changed: %+v", got.requestedQuantity)
				}
				var status string
				if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, o.org, o.bulkRefund).Scan(&status); err != nil || status != "pending" {
					t.Fatalf("abandoned refund status = %q err=%v", status, err)
				}
			})
		}
	}
}

// The abandonment commits between the runner's lookup and the Commerce bind. The shared refund
// service refuses before calling payments, and the runner applies the same classification.
func TestBulkRunClassifiesAnAbandonmentThatLandsAfterItsLookup(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		name := "unfixed quantity"
		if fixed {
			name = "fixed quantity"
		}
		t.Run(name, func(t *testing.T) {
			db, ctx := runnerDB(t)
			o := seedAbandonedCancellation(t, ctx, db, false)
			startRun(t, ctx, db, o, fixed)
			s := &lateAbandonStore{DBStore: DBStore{DB: db}, ctx: ctx}
			s.abandon = func() { recordAbandonedOutcome(t, ctx, db, o) }

			transport := &recordingTransport{}
			refunder := &countingRefunder{Service: refunds.New(db, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
			New(s, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

			if n := transport.payments(); n != 0 {
				t.Fatalf("the run made %d payments calls for a refund abandoned after its lookup", n)
			}
			got := readVerdict(t, ctx, db, o)
			if got.outcome != "failed" || got.failureCode != "refund_refused" || got.money || got.attempts != 1 {
				t.Fatalf("verdict = %+v, want a terminal refund_refused", got)
			}
		})
	}
}
