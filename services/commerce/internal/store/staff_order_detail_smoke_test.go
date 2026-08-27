//go:build smoke

package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The staff order read (TKT-201), against real Postgres.
//
// These live at the STORE tier and not one layer up, because the thing worth proving is a
// SQL predicate. An assertion at the handler tier would run against whatever the store
// returned and pass just as happily with `AND r.organizer_id=$2` deleted — it would prove
// the handler and the store agree, which is not the claim.

// TestStaffOrderDetailScopesByTheReservationsOrganizer is COS 3, at the tier the scoping
// happens.
//
// THE MUTATION THAT IS EVIDENCE: delete `AND r.organizer_id=$2` from ReadStaffOrderDetail
// and the second read below returns organizer B's order to organizer A.
//
// The positive read is asserted FIRST and in this same function, deliberately. A
// cross-organizer miss on this join is a MISSING row, not a substituted one — o.reservation_id
// is a FK to reservations' primary key, so the join is many-to-one and a wrong organizer
// yields zero rows. That makes ErrNoRows a weak signal on its own: a store that always
// failed, a fixture that never seeded, and a correctly scoped read are indistinguishable by
// the negative alone. Proving the exact row IS readable by its owner, immediately before
// asking for it as someone else, is what makes the negative mean something.
func TestStaffOrderDetailScopesByTheReservationsOrganizer(t *testing.T) {
	db, ctx := outboxDB(t)

	owner := uuid.New()
	stranger := uuid.New()
	reservation, order, ticketType := uuid.New(), uuid.New(), uuid.New()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,4,1250,5300,5000,'EUR','completed')`,
		reservation, owner, uuid.New(), uuid.New(), ticketType, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'fingerprint')`,
		order, reservation, "detail-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	// The owner CAN read it, and reads exactly the seeded row. Without this the negative
	// below proves nothing.
	got, err := ReadStaffOrderDetail(ctx, db, owner, order)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if got.Line.Quantity != 4 || got.Line.UnitAmount != 1250 ||
		got.Line.FaceValue != 5000 || got.Line.TotalAmount != 5300 || got.Line.Currency != "EUR" {
		t.Fatalf("owner read = %+v; want the seeded line (qty 4, unit 1250, face 5000, total 5300, EUR)", got.Line)
	}
	if got.Line.TicketTypeID != ticketType {
		t.Errorf("ticket type = %v want %v", got.Line.TicketTypeID, ticketType)
	}

	// The same order, asked for by someone else. Missing, not substituted, not empty.
	if _, err := ReadStaffOrderDetail(ctx, db, stranger, order); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-organizer read err = %v want sql.ErrNoRows: delete `AND r.organizer_id=$2` "+
			"and this returns another tenant's order", err)
	}
}

// The money projection, asserted against values derived from the seed rather than read
// back from a run.
//
// passed_on_fees is the assertion that matters: it is the only DERIVED number in the
// response, and the invariant is that it is the exact integer difference between what the
// buyer paid and the face value — never a rounded share, and never a float (ADR-001).
func TestStaffOrderDetailReportsMoneyAsExactIntegers(t *testing.T) {
	db, ctx := outboxDB(t)

	org := uuid.New()
	reservation, order := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,3,1100,3450,3300,'CAD','completed')`,
		reservation, org, uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'fingerprint')`,
		order, reservation, "money-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	got, err := ReadStaffOrderDetail(ctx, db, org, order)
	if err != nil {
		t.Fatal(err)
	}
	// 3450 gross, 3300 face -> 150 passed on. Stated from the seed, not observed.
	if got.Totals.TotalAmount != 3450 || got.Totals.FaceValue != 3300 || got.Totals.PassedOnFees != 150 {
		t.Errorf("totals = %+v; want total 3450, face 3300, fees 150", got.Totals)
	}
	if got.Totals.Currency != "CAD" {
		t.Errorf("currency = %q want CAD — it must come from the reservation, not a default", got.Totals.Currency)
	}
	// An order nobody has refunded reports a zero position, not an absent one.
	if got.Totals.RefundStatus != "none" || got.Totals.RefundedAmount != 0 || got.Totals.RefundedQuantity != 0 {
		t.Errorf("unrefunded order totals = %+v; want refund_status none and zero amounts", got.Totals)
	}
	if len(got.Refunds) != 0 {
		t.Errorf("refunds = %v; want an empty slice on an order with none", got.Refunds)
	}
	if got.Refunds == nil {
		t.Error("refunds is nil; it must marshal as [] rather than null, so a client can tell " +
			"'no refunds' from 'the field is missing'")
	}
}

// Pending refunds are RETURNED, and that is the whole reason this read retires
// unresolved-refunds.ts.
//
// A refund whose request timed out may still have moved money, and the page's only safe
// next step is replaying the SAME idempotency key. A read that answered with completed
// refunds only would leave exactly that case invisible and the module would still be
// needed — so this asserts the pending row is present WITH its key, not merely that some
// refunds came back.
//
// Every seed here is load-bearing: delete the pending row and the ordering and
// pending-visibility assertions both fail; delete the completed row and the two-status
// assertion fails; change either created_at and the ordering assertion fails.
func TestStaffOrderDetailReturnsPendingRefundsWithTheirIdempotencyKeys(t *testing.T) {
	db, ctx := outboxDB(t)

	org := uuid.New()
	reservation, order := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,4,1250,5300,5000,'EUR','completed')`,
		reservation, org, uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,refund_status,refunded_quantity,refunded_amount)
		VALUES($1,$2,'completed',$3,'fingerprint','partial',1,1250)`,
		order, reservation, "refunded-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	older, newer := uuid.New(), uuid.New()
	// Completed first in time, so a correct ORDER BY created_at puts it first and the
	// assertion below can tell ordering from insertion order.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason,status,completed_at,payment_fact_id,created_at)
		VALUES($1,$2,$3,'key-settled','fp',1,1250,1250,'EUR','staff:amy','duplicate','completed',now(),$4,now() - interval '1 hour')`,
		org, older, order, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason,status,created_at)
		VALUES($1,$2,$3,'key-in-flight','fp',1,1250,1250,'EUR','staff:bo','goodwill','pending',now())`,
		org, newer, order); err != nil {
		t.Fatal(err)
	}

	got, err := ReadStaffOrderDetail(ctx, db, org, order)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Refunds) != 2 {
		t.Fatalf("refunds = %d want 2 (one completed, one PENDING): a completed-only read "+
			"leaves the in-flight key invisible, which is the case this read exists to answer", len(got.Refunds))
	}
	if got.Refunds[0].RefundID != older || got.Refunds[1].RefundID != newer {
		t.Errorf("refunds are not oldest-first: got %v then %v", got.Refunds[0].RefundID, got.Refunds[1].RefundID)
	}
	if got.Refunds[0].Status != "completed" || got.Refunds[0].IdempotencyKey != "key-settled" ||
		got.Refunds[0].Actor != "staff:amy" || got.Refunds[0].CompletedAt == nil {
		t.Errorf("settled refund = %+v; want completed, key-settled, staff:amy, a completion time", got.Refunds[0])
	}
	if got.Refunds[1].Status != "pending" || got.Refunds[1].IdempotencyKey != "key-in-flight" ||
		got.Refunds[1].Actor != "staff:bo" {
		t.Errorf("in-flight refund = %+v; want pending, key-in-flight, staff:bo", got.Refunds[1])
	}
	if got.Refunds[1].CompletedAt != nil {
		t.Errorf("a PENDING refund reports completed_at = %v; the table's CHECK ties completion "+
			"to the status, and a caller reading a time here would believe money had settled",
			got.Refunds[1].CompletedAt)
	}
	// The order's own counters, which are a different source from the refund rows.
	if got.Totals.RefundStatus != "partial" || got.Totals.RefundedAmount != 1250 || got.Totals.RefundedQuantity != 1 {
		t.Errorf("totals = %+v; want partial / 1250 / 1 from the order's counters", got.Totals)
	}
}

// A refund belonging to ANOTHER organizer on the same order id must not appear.
//
// Separate from the scoping test above because it exercises a SECOND predicate: the
// refund query has its own `organizer_id=$1`, and a guard with N predicates needs N tests.
// The first test passes with this one deleted — it never seeds a foreign refund — so
// deleting `WHERE organizer_id=$1` from the refund query is a mutation only this catches.
func TestStaffOrderDetailDoesNotLeakAnotherOrganizersRefund(t *testing.T) {
	db, ctx := outboxDB(t)

	org, other := uuid.New(), uuid.New()
	reservation, order := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,4,1250,5300,5000,'EUR','completed')`,
		reservation, org, uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'fingerprint')`,
		order, reservation, "leak-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	// order_refunds carries organizer_id of its own and does not constrain it against the
	// reservation, so this row is writable and would be returned by an unscoped read.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason,status)
		VALUES($1,$2,$3,'not-yours','fp',1,1250,1250,'EUR','staff:elsewhere','other tenant','pending')`,
		other, uuid.New(), order); err != nil {
		t.Fatal(err)
	}

	got, err := ReadStaffOrderDetail(ctx, db, org, order)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Refunds) != 0 {
		t.Fatalf("refunds = %+v; a refund row owned by another organizer must not appear — "+
			"delete `organizer_id=$1` from the refund query and this leaks it", got.Refunds)
	}
}

// The read is ONE SNAPSHOT of two sources that move together (ai-review).
//
// CompleteOrderRefund advances the refund row and the order's counters inside a single
// transaction. Two separate autocommit reads can straddle that commit and return the OLD
// counters beside a NEWLY completed refund — the response would say the order has 0
// refunded while listing a settled refund of 1250, which is exactly the contradiction a
// staff member consults this endpoint to avoid.
//
// THE FIXTURE MUST BE ABLE TO REACH THE FAILING STATE, which a static seed cannot: the
// competing transaction has to commit BETWEEN the two reads. This drives that from the
// database side — the refund completion is held open until the detail read has taken its
// snapshot, then committed while the read is still in flight.
//
// The mutation that is evidence: replace the transaction with two db.QueryContext calls
// and this goes red, reporting counters and rows from different instants.
func TestStaffOrderDetailReadsOneSnapshotOfCountersAndRefunds(t *testing.T) {
	db, ctx := outboxDB(t)

	org := uuid.New()
	reservation, order, refund := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,4,1250,5300,5000,'EUR','completed')`,
		reservation, org, uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	// The order starts with NO refund recorded, and one PENDING refund row.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'fingerprint')`,
		order, reservation, "snapshot-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason,status)
		VALUES($1,$2,$3,'snapshot-key','fp',1,1250,1250,'EUR','staff:amy','duplicate','pending')`,
		org, refund, order); err != nil {
		t.Fatal(err)
	}

	// A competing completion, prepared but NOT yet committed. Both halves of what
	// CompleteOrderRefund does, in one transaction, exactly as production does them.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE order_refunds SET status='completed',completed_at=now(),payment_fact_id=$2
		WHERE id=$1`, refund, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE orders SET refunded_quantity=1, refunded_amount=1250, refund_status='partial'
		WHERE id=$1`, order); err != nil {
		t.Fatal(err)
	}

	// The read starts while that transaction is open, so it necessarily takes its
	// snapshot BEFORE the commit. Committing while the read is in flight is what a torn
	// read needs, and a REPEATABLE READ snapshot is what refuses to show it.
	// THE INTERLEAVE IS FORCED, not hoped for (ai-review pass 2).
	//
	// The version this replaces ran eight readers in a loop and committed in the middle,
	// asserting no observation was torn. It went red on three of three mutation runs — and
	// the reviewer was right that this proves it USUALLY catches the defect, not that it
	// must: nothing synchronised the readers with the commit, so on a fast database every
	// observation can land before it, all of them valid, the count satisfied, and the test
	// green with the mechanism removed. A probabilistic regression test for a consistency
	// property is a test that will one day pass for the wrong reason. It was also flaky in
	// the other direction: a loaded runner could miss the throughput floor and fail correct
	// code.
	//
	// The two obvious deterministic barriers do not work either, and both are worth
	// recording because each looked fine:
	//   - SELECT ... FOR UPDATE on a row the reader touches: a plain SELECT never blocks on
	//     a row lock. That is what MVCC is for. The reader sails past.
	//   - the same on the orders row: additionally deadlocks, because the competing
	//     completion updates orders and the barrier stops the WRITER.
	//
	// So the seam is in the code, between the two queries, which is the only instant at
	// which the two isolation levels differ. The reader signals it has taken its snapshot
	// and waits; the completion commits; the reader proceeds to its second query. Under one
	// snapshot the second query still sees the first's instant. Under two autocommit reads
	// it sees a newer one — and the response reports a state the database never held.
	snapshotTaken := make(chan struct{})
	commitDone := make(chan struct{})
	betweenStaffOrderDetailQueries = func() {
		close(snapshotTaken)
		<-commitDone
	}
	t.Cleanup(func() { betweenStaffOrderDetailQueries = nil })

	type result struct {
		d   StaffOrderDetail
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := ReadStaffOrderDetail(ctx, db, org, order)
		done <- result{d, err}
	}()

	// The reader has run its counters query and is parked. Commit the completion now, so it
	// lands strictly between the two queries.
	<-snapshotTaken
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	close(commitDone)

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.d.Refunds) != 1 {
		t.Fatalf("refunds = %d want 1", len(r.d.Refunds))
	}
	// THE INVARIANT, without naming the implementation: the counters and the refund rows
	// describe the same instant. One transaction advances both, so the completion is
	// visible in BOTH or in NEITHER — a settled refund beside counters that have not moved
	// is a state the database never held.
	settledRow := r.d.Refunds[0].Status == "completed"
	countersMoved := r.d.Totals.RefundedAmount == 1250 && r.d.Totals.RefundStatus == "partial"
	if settledRow != countersMoved {
		t.Fatalf("TORN READ: refund row settled=%v but counters moved=%v "+
			"(status=%q, refunded_amount=%d, refund_status=%q). The completion committed "+
			"between the two queries, and only a single snapshot can refuse to show half of it.",
			settledRow, countersMoved, r.d.Refunds[0].Status,
			r.d.Totals.RefundedAmount, r.d.Totals.RefundStatus)
	}
	// And the snapshot must be the PRE-commit one specifically. Without this a read that
	// somehow saw both halves of the commit would also pass — consistent, but not the
	// instant this reader started at, and the assertion would be about consistency in
	// general rather than about this snapshot.
	if settledRow {
		t.Errorf("the read observed the completion that committed AFTER its snapshot was " +
			"taken; a repeatable-read reader must see the instant it started at")
	}
}
