//go:build smoke

package bulkrefund

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	abandonRefund(t, ctx, db, o.org, o.bulkRefund)
}

// abandonRefund records an `abandoned` outcome the way the command does, with payments' answer
// assumed: reserve, then finish.
func abandonRefund(t *testing.T, ctx context.Context, db *sql.DB, org, refund uuid.UUID) {
	t.Helper()
	in := store.RefundOutcomeRequest{OrganizerID: org, RefundID: refund, Kind: store.OutcomeAbandoned,
		OperatorName: "Ada Operator", Reason: "nobody will refund this attempt"}
	target, err := store.InspectRefundOutcomeTarget(ctx, db, org, refund)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReserveAbandonment(ctx, db, in, target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishAbandonment(ctx, db, in, target, "re_failed_bulk"); err != nil {
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
				refunder := &countingRefunder{Service: refunds.New(db, transport.call, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
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
			refunder := &countingRefunder{Service: refunds.New(db, transport.call, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
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

// refusingTransport is payments refusing the partial refund with its generic 409, after running
// `during` inside the call: the window in which an abandonment and a staff replacement commit.
type refusingTransport struct {
	recordingTransport
	during func()
}

func (c *refusingTransport) call(ctx context.Context, method, u, token string, in any, idem bool) (int, []byte, error) {
	if strings.HasSuffix(u, "/internal/psp/partial-refund") {
		_, _, _ = c.recordingTransport.call(ctx, method, u, token, in, idem)
		c.during()
		return 409, []byte(`{"error":"refund leg abandoned"}`), nil
	}
	return c.recordingTransport.call(ctx, method, u, token, in, idem)
}

// R2-5: payments refuses the bulk refund because an abandonment committed after the bind, and a
// staff replacement refunded the order before the runner handled the refusal. The runner re-reads
// its own refund and classifies as a skip does, with no further payments call. A refusal with no
// abandonment keeps the terminal refund_refused.
func TestBulkRunClassifiesAPaymentsRefusalAfterAnAbandonment(t *testing.T) {
	for _, c := range []struct {
		name   string
		during func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder)
		want   verdict
		reason string
	}{
		{"abandoned, replacement discharged", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			recordAbandonedOutcome(t, ctx, db, o)
			refundTheRestByStaff(t, ctx, db, o, true)
		}, verdict{outcome: "already_refunded", money: true, voided: true, returned: true, refundedQuantity: 2, refundedAmount: 2000}, ""},
		{"abandoned, replacement reversal outstanding", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			recordAbandonedOutcome(t, ctx, db, o)
			refundTheRestByStaff(t, ctx, db, o, false)
		}, verdict{outcome: "failed", failureCode: "reversal_outstanding", money: true, refundedQuantity: 2, refundedAmount: 2000}, ""},
		// The payments refusal itself is reported, not the abandonment classification.
		{"not abandoned", func(*testing.T, context.Context, *sql.DB, abandonedOrder) {},
			verdict{outcome: "failed", failureCode: "refund_refused"}, refunds.ErrPaymentsRefused.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, ctx := runnerDB(t)
			o := seedAbandonedCancellation(t, ctx, db, false)
			startRun(t, ctx, db, o, false)
			transport := &refusingTransport{}
			transport.during = func() { c.during(t, ctx, db, o) }
			refunder := &countingRefunder{Service: refunds.New(db, transport.call, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
			New(DBStore{DB: db}, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

			if n := transport.payments(); n != 1 {
				t.Fatalf("payments calls = %d, want only the refused one", n)
			}
			got := readVerdict(t, ctx, db, o)
			want := c.want
			want.attempts = 1
			want.requestedQuantity = got.requestedQuantity
			if got != want {
				t.Fatalf("verdict = %+v, want %+v", got, want)
			}
			if c.reason != "" {
				var reason sql.NullString
				if err := db.QueryRowContext(ctx, `SELECT failure_reason FROM cancellation_refund_orders WHERE organizer_id=$1 AND order_id=$2`, o.org, o.order).Scan(&reason); err != nil || reason.String != c.reason {
					t.Fatalf("failure reason = %q err=%v, want %q", reason.String, err, c.reason)
				}
			}
		})
	}
}

// failOneLookup fails the next LookupRefund once it is armed, and only that one.
type failOneLookup struct {
	DBStore
	mu    sync.Mutex
	armed bool
}

func (f *failOneLookup) arm() {
	f.mu.Lock()
	f.armed = true
	f.mu.Unlock()
}

func (f *failOneLookup) LookupRefund(ctx context.Context, org, refundID uuid.UUID) (store.Refund, bool, error) {
	f.mu.Lock()
	armed := f.armed
	f.armed = false
	f.mu.Unlock()
	if armed {
		return store.Refund{}, false, errors.New("lookup failed after the payments refusal")
	}
	return f.DBStore.LookupRefund(ctx, org, refundID)
}

// D35 (RA3-3), ADR-087 §15: the runner's re-read after a payments refusal fails, and only that
// read fails. The pass is a charged, retryable `internal` failure: one attempt, no verdict, the
// lease kept as the backoff. It is not a terminal refund_refused built on a read that did not
// happen. After the lease, the next pass decides the row: with no abandonment it calls payments
// again under the SAME key, and that refusal is refund_refused; with an abandonment committed
// during the first call it skips with no payments call.
func TestBulkRunRetriesWhenTheReadAfterAPaymentsRefusalFails(t *testing.T) {
	for _, c := range []struct {
		name     string
		during   func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder)
		payments int
	}{
		{"not abandoned", func(*testing.T, context.Context, *sql.DB, abandonedOrder) {}, 2},
		{"abandoned during the call", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			recordAbandonedOutcome(t, ctx, db, o)
		}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, ctx := runnerDB(t)
			o := seedAbandonedCancellation(t, ctx, db, false)
			startRun(t, ctx, db, o, false)
			st := &failOneLookup{DBStore: DBStore{DB: db}}
			transport := &refusingTransport{}
			first := true
			// The claim and lease as the drive saw them. A charged hand-back in place of the
			// charge releases the claim, and the same RunOnce can claim the row again with a new
			// claim and a new lease. Only these exact values show that the drive's claim stayed.
			var claimAtDrive uuid.NullUUID
			var leaseAtDrive sql.NullTime
			transport.during = func() {
				if first {
					first = false
					if err := db.QueryRowContext(ctx, `SELECT claim_id, lease_until FROM cancellation_refund_orders WHERE organizer_id=$1 AND order_id=$2`,
						o.org, o.order).Scan(&claimAtDrive, &leaseAtDrive); err != nil {
						t.Errorf("read the claim during the payments call: %v", err)
					}
					c.during(t, ctx, db, o)
					st.arm()
				}
			}
			refunder := &countingRefunder{Service: refunds.New(db, transport.call, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
			runner := New(st, refunder, time.Minute, 10, time.Minute)

			runner.RunOnce(ctx)
			if n := transport.payments(); n != 1 {
				t.Fatalf("first pass: payments calls = %d, want the refused one", n)
			}
			if !claimAtDrive.Valid || !leaseAtDrive.Valid {
				t.Fatalf("during the payments call: claim %v, lease %v, want both set", claimAtDrive, leaseAtDrive)
			}
			got := readVerdict(t, ctx, db, o)
			if got.outcome != "" || got.failureCode != "" || got.attempts != 1 {
				t.Fatalf("first pass: verdict = %+v, want no verdict and one charged attempt", got)
			}
			var claimAfter uuid.NullUUID
			var leaseAfter sql.NullTime
			var leased sql.NullBool
			if err := db.QueryRowContext(ctx, `SELECT claim_id, lease_until, lease_until > now() FROM cancellation_refund_orders WHERE organizer_id=$1 AND order_id=$2`,
				o.org, o.order).Scan(&claimAfter, &leaseAfter, &leased); err != nil {
				t.Fatal(err)
			}
			if claimAfter != claimAtDrive || !leaseAfter.Valid || !leaseAfter.Time.Equal(leaseAtDrive.Time) {
				t.Fatalf("first pass: claim/lease = %v/%v, want the drive's %v/%v unchanged", claimAfter, leaseAfter, claimAtDrive, leaseAtDrive)
			}
			if !leased.Valid || !leased.Bool {
				t.Fatalf("first pass: lease live = %v, want the lease kept as the backoff", leased)
			}

			if _, err := db.ExecContext(ctx, `UPDATE cancellation_refund_orders SET lease_until=now()-interval '1 minute' WHERE organizer_id=$1 AND order_id=$2`, o.org, o.order); err != nil {
				t.Fatal(err)
			}
			runner.RunOnce(ctx)

			if n := transport.payments(); n != c.payments {
				t.Fatalf("after the retry: payments calls = %d, want %d", n, c.payments)
			}
			if c.payments == 2 {
				transport.mu.Lock()
				var posts []string
				for _, call := range transport.calls {
					if strings.Contains(call, "/internal/psp/partial-refund") {
						posts = append(posts, call)
					}
				}
				transport.mu.Unlock()
				if len(posts) != 2 || posts[0] != posts[1] {
					t.Fatalf("the retry changed the payments request: %q", posts)
				}
			}
			got = readVerdict(t, ctx, db, o)
			if got.outcome != "failed" || got.failureCode != "refund_refused" || got.money || got.attempts != 2 {
				t.Fatalf("after the retry: verdict = %+v, want a terminal refund_refused on the second attempt", got)
			}
		})
	}
}

// staffOrder is a two-unit order at 1000 with no cancellation refund.
func staffOrder(t *testing.T, ctx context.Context, db *sql.DB) abandonedOrder {
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
	return o
}

func staffRefund(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder, key string, quantity int32) uuid.UUID {
	t.Helper()
	r, err := store.BindOrderRefund(ctx, db, store.RefundRequest{
		OrderID: o.order, OrganizerID: o.org, Quantity: quantity, IdempotencyKey: key,
		Actor: "support@example.test", Reason: "staff refund",
	})
	if err != nil {
		t.Fatalf("staff refund %s: %v", key, err)
	}
	return r.ID
}

func completeStaffRefund(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder, refund uuid.UUID, discharge bool) {
	t.Helper()
	if err := store.CompleteOrderRefund(ctx, db, o.org, refund, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if discharge {
		if err := store.MarkRefundTicketsVoided(ctx, db, o.org, refund); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkRefundCapacityReturned(ctx, db, o.org, refund); err != nil {
			t.Fatal(err)
		}
	}
}

// R1-1 and the owner's staff-only rule: an order with ANY abandoned refund gets no fresh
// cancellation bind, also when the abandoned refund was a staff refund and the run has no leg
// of its own, and also after staff completed a replacement. Through the real runner and refund
// service: the runner reaches the shared refund unit, the bind refuses under the order lock,
// and the row is classified like the own-key skip, with zero payments calls and no new
// cancellation refund.
func TestBulkRunRefusesAFreshBindOnAnOrderWithAnAbandonedStaffRefund(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder)
		want  verdict
	}{
		{"staff R1 abandoned, R2 replaced, one unit left", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			abandonRefund(t, ctx, db, o.org, staffRefund(t, ctx, db, o, "staff-r1", 1))
			completeStaffRefund(t, ctx, db, o, staffRefund(t, ctx, db, o, "staff-r2", 1), true)
		}, verdict{outcome: "failed", failureCode: "refund_refused"}},
		{"staff refund abandoned, no cancellation leg, no replacement", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			abandonRefund(t, ctx, db, o.org, staffRefund(t, ctx, db, o, "staff-r1", 2))
		}, verdict{outcome: "failed", failureCode: "refund_refused"}},
		{"zero left, reversal discharged", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			abandonRefund(t, ctx, db, o.org, staffRefund(t, ctx, db, o, "staff-r1", 1))
			completeStaffRefund(t, ctx, db, o, staffRefund(t, ctx, db, o, "staff-r2", 2), true)
		}, verdict{outcome: "already_refunded", money: true, voided: true, returned: true, refundedQuantity: 2, refundedAmount: 2000}},
		{"zero left, reversal outstanding", func(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
			abandonRefund(t, ctx, db, o.org, staffRefund(t, ctx, db, o, "staff-r1", 1))
			completeStaffRefund(t, ctx, db, o, staffRefund(t, ctx, db, o, "staff-r2", 2), false)
		}, verdict{outcome: "failed", failureCode: "reversal_outstanding", money: true, refundedQuantity: 2, refundedAmount: 2000}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, ctx := runnerDB(t)
			o := staffOrder(t, ctx, db)
			c.setup(t, ctx, db, o)
			startRun(t, ctx, db, o, false)

			transport := &recordingTransport{}
			refunder := &countingRefunder{Service: refunds.New(db, transport.call, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
			New(DBStore{DB: db}, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

			if n := transport.payments(); n != 0 {
				t.Fatalf("the run made %d payments calls", n)
			}
			got := readVerdict(t, ctx, db, o)
			want := c.want
			want.attempts = 1
			want.requestedQuantity = got.requestedQuantity
			if got != want {
				t.Fatalf("verdict = %+v, want %+v", got, want)
			}
			assertNoCancellationRefund(t, ctx, db, o)
		})
	}
}

func assertNoCancellationRefund(t *testing.T, ctx context.Context, db *sql.DB, o abandonedOrder) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1 AND idempotency_key LIKE 'cancel:%'`, o.order).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cancellation refunds = %d err=%v, want none", n, err)
	}
}

// A staff refund is abandoned after the runner's lookup and before its locked bind. The bind
// sees the abandonment under the order lock and refuses; the row is refund_refused.
func TestBulkRunRefusesAStaffAbandonmentThatLandsBeforeItsBind(t *testing.T) {
	db, ctx := runnerDB(t)
	o := staffOrder(t, ctx, db)
	staff := staffRefund(t, ctx, db, o, "staff-r1", 1)
	startRun(t, ctx, db, o, false)
	s := &lateAbandonStore{DBStore: DBStore{DB: db}, ctx: ctx}
	s.abandon = func() { abandonRefund(t, ctx, db, o.org, staff) }

	transport := &recordingTransport{}
	refunder := &countingRefunder{Service: refunds.New(db, transport.call, transport.call, "http://payments.test", "http://access.test", "http://inventory.test")}
	New(s, refunder, time.Minute, 10, time.Minute).RunOnce(ctx)

	if n := transport.payments(); n != 0 {
		t.Fatalf("the run made %d payments calls", n)
	}
	if n := refunder.count(); n != 1 {
		t.Fatalf("refund unit calls = %d, want 1 (the refused bind)", n)
	}
	got := readVerdict(t, ctx, db, o)
	if got.outcome != "failed" || got.failureCode != "refund_refused" || got.money || got.attempts != 1 {
		t.Fatalf("verdict = %+v, want a terminal refund_refused", got)
	}
	assertNoCancellationRefund(t, ctx, db, o)
}
