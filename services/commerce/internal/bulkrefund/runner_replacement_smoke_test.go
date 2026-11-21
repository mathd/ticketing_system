//go:build smoke

package bulkrefund

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/refunds"
	"ticketing/services/commerce/internal/store"
)

// TKT-498 (plan v2 Build order 4, D10, D14, D18). A cancellation of the exchange's TARGET slot
// enumerates the exchange's replacement order and refunds it through the shared refund unit.
// The real refunds.New runs against a real Commerce database; its HTTP is the recording
// transport below.

// replacementTransport answers Access's admission read with `issued` tickets and lets payments
// refund the replacement's alias key. Every call is recorded with its body.
type replacementTransport struct {
	mu     sync.Mutex
	issued int32
	calls  []string
}

func (c *replacementTransport) call(_ context.Context, method, u, _ string, in any, _ bool) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, _ := json.Marshal(in)
	c.calls = append(c.calls, method+" "+u+" "+string(body))
	switch {
	case strings.HasPrefix(u, "http://access.test/internal/orders/") && strings.Contains(u, "/admission?"):
		return 200, []byte(`{"admitted":false,"issued_count":` + strconv.Itoa(int(c.issued)) + `}`), nil
	case strings.HasSuffix(u, "/internal/psp/partial-refund"):
		return 200, []byte(`{"status":"refunded","fact_id":"` + uuid.NewString() + `","amount":3000,"currency":"EUR","replay":false}`), nil
	}
	return 200, []byte(`{}`), nil
}

func (c *replacementTransport) matching(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, call := range c.calls {
		if strings.HasPrefix(call, prefix) {
			out = append(out, call)
		}
	}
	return out
}

type settledReplacement struct {
	org, exchange, targetSlot      uuid.UUID
	replacementOrder, replacement  uuid.UUID
	sourceOrder, sourceReservation uuid.UUID
}

// seedSettledReplacement writes a settled, unswitched exchange whose replacement order sits on
// the target slot, as settlement leaves it.
func seedSettledReplacement(t *testing.T, ctx context.Context, db *sql.DB) settledReplacement {
	t.Helper()
	f := settledReplacement{org: uuid.New(), exchange: uuid.New(), targetSlot: uuid.New(),
		replacementOrder: uuid.New(), replacement: uuid.New(), sourceOrder: uuid.New(), sourceReservation: uuid.New()}
	buyer, targetHold, targetType := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1000,2000,2000,'EUR','completed')`, f.sourceReservation, f.org, uuid.New(), uuid.New(), uuid.New(), buyer)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fp',$4)`, f.sourceOrder, f.sourceReservation, "src-"+f.sourceOrder.String(), uuid.New())
	exec(`INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1500,3000,3000,'EUR','completed')`, f.replacement, f.org, targetHold, f.targetSlot, targetType, buyer)
	exec(`INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'exchange')`, f.replacementOrder, f.replacement, "exchange:"+f.exchange.String())
	exec(`INSERT INTO order_exchanges(organizer_id,id,source_order_id,replacement_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,
		                            target_total,delta_amount,target_unit_amount,target_hold_id,
		                            replacement_reservation_id,target_slot_id,basis_at,
		                            currency,actor,reason,created_at,settled_at)
		VALUES($1,$2,$3,$4,$5,$6,'fp',2,2000,2000,3000,1000,1500,$7,$8,$9,now() - interval '2 minutes',
		       'EUR','ops','buyer asked',now() - interval '2 minutes',now() - interval '1 minute')`,
		f.org, f.exchange, f.sourceOrder, f.replacementOrder, targetType, "xkey-"+f.exchange.String(),
		targetHold, f.replacement, f.targetSlot)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE organizer_id=$1`, f.org)
		_, _ = db.Exec(`DELETE FROM cancellation_refund_orders WHERE organizer_id=$1`, f.org)
		_, _ = db.Exec(`DELETE FROM cancellation_refund_runs WHERE organizer_id=$1`, f.org)
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, f.org)
		_, _ = db.Exec(`DELETE FROM order_refunds WHERE organizer_id=$1`, f.org)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id IN (SELECT id FROM reservations WHERE organizer_id=$1)`, f.org)
		_, _ = db.Exec(`DELETE FROM reservations WHERE organizer_id=$1`, f.org)
	})
	return f
}

func cancelTargetSlot(t *testing.T, ctx context.Context, db *sql.DB, f settledReplacement) {
	t.Helper()
	if _, err := store.BindCancellationRun(ctx, db, store.CancellationRunRequest{
		OrganizerID: f.org, SlotID: f.targetSlot, IdempotencyKey: "cancel-" + f.targetSlot.String(),
		Actor: "ops", Reason: "target slot cancelled",
	}); err != nil {
		t.Fatal(err)
	}
}

func replacementVerdict(t *testing.T, ctx context.Context, db *sql.DB, f settledReplacement) (outcome, code string, attempts int) {
	t.Helper()
	var o, c sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT outcome, failure_code, attempts FROM cancellation_refund_orders WHERE organizer_id=$1 AND order_id=$2`,
		f.org, f.replacementOrder).Scan(&o, &c, &attempts); err != nil {
		t.Fatalf("read the replacement's cancellation row: %v", err)
	}
	return o.String, c.String, attempts
}

// With no issued replacement ticket, the bulk refund of the replacement asks Access once and
// sends nothing to payments; the row stays unresolved with one attempt charged, which is how the
// runner records the retryable `unavailable` (runner.record). Its refund stays pending. A switched replacement,
// whose tickets Access reports, passes and reaches payments under the replacement's own key.
func TestBulkRefundOfAnUnissuedReplacementNeverReachesPayments(t *testing.T) {
	db, ctx := runnerDB(t)
	f := seedSettledReplacement(t, ctx, db)
	cancelTargetSlot(t, ctx, db, f)
	transport := &replacementTransport{issued: 0}
	refunder := refunds.New(db, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")
	New(DBStore{DB: db}, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

	if calls := transport.matching("POST http://payments.test"); len(calls) != 0 {
		t.Fatalf("the bulk refund reached payments with no issued replacement ticket: %v", calls)
	}
	admission := "GET http://access.test/internal/orders/" + f.replacementOrder.String() + "/admission?organizer_id=" + f.org.String()
	if calls := transport.matching(admission); len(calls) != 1 {
		t.Fatalf("the replacement's scoped admission was read %d time(s), want 1; calls: %v", len(calls), transport.matching(""))
	}
	if outcome, code, attempts := replacementVerdict(t, ctx, db, f); outcome != "" || code != "" || attempts != 1 {
		t.Fatalf("verdict = %q/%q after %d attempt(s), want an unresolved row with one attempt charged", outcome, code, attempts)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND order_id=$2`,
		f.org, f.replacementOrder).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("replacement refund status = %q (err %v), want pending", status, err)
	}

	// The same replacement after Access switched its tickets: the next pass reaches payments.
	if _, err := db.ExecContext(ctx, `UPDATE cancellation_refund_orders SET lease_until = now() - interval '1 second'
		WHERE organizer_id=$1 AND order_id=$2`, f.org, f.replacementOrder); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	transport.issued = 2
	transport.mu.Unlock()
	New(DBStore{DB: db}, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)
	refundsSent := transport.matching("POST http://payments.test/internal/psp/partial-refund")
	if len(refundsSent) != 1 {
		t.Fatalf("a switched replacement made %d partial refund(s), want 1: %v", len(refundsSent), transport.matching(""))
	}
	if !strings.Contains(refundsSent[0], `"idempotency_key":"exchange:`+f.exchange.String()+`"`) ||
		!strings.Contains(refundsSent[0], `"organizer_id":"`+f.org.String()+`"`) {
		t.Fatalf("partial refund = %s, want the replacement's own key in its organizer", refundsSent[0])
	}
}

// After the exchange's abandonment reserves, the bulk bind of the replacement is refused with
// the existing `not_refundable` classification (D18), and nothing is sent downstream.
func TestBulkRefundOfAReplacementAfterTheAbandonmentReservationIsNotRefundable(t *testing.T) {
	db, ctx := runnerDB(t)
	f := seedSettledReplacement(t, ctx, db)
	e, err := store.LoadSettledExchange(ctx, db, f.org, f.exchange)
	if err != nil {
		t.Fatal(err)
	}
	attribution, err := store.NewAbandonmentAttribution("Ada Operator", "source admitted before the switch")
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := store.ReserveExchangeAbandonment(ctx, db, store.AbandonmentReservation{Expected: e, Attribution: attribution}); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	cancelTargetSlot(t, ctx, db, f)
	transport := &replacementTransport{issued: 2}
	refunder := refunds.New(db, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")
	New(DBStore{DB: db}, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

	if calls := transport.matching(""); len(calls) != 0 {
		t.Fatalf("a refused bulk refund made dependency calls: %v", calls)
	}
	if outcome, code, _ := replacementVerdict(t, ctx, db, f); outcome != "failed" || code != "not_refundable" {
		t.Fatalf("verdict = %q/%q, want failed/not_refundable", outcome, code)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_refunds WHERE organizer_id=$1`, f.org).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d refund rows (err %v), want none bound after the reservation", n, err)
	}
}
