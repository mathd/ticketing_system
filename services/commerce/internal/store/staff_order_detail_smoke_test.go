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
	// EACH QUERY REPORTS WHAT IT READ, on payments' verifyProbe pattern (TKT-254) — the
	// same problem, already solved and reviewed in this repo. Three weaker seams were
	// executed first and all three passed while the guarantee was gone; the shapes and why
	// are on staffOrderDetailProbe. The short version: a probe that reports the DATA cannot
	// be satisfied by moving or rewriting the probe, because the values come from the
	// statements under test.
	snapshotTaken := make(chan struct{})
	commitDone := make(chan struct{})
	readerExited := make(chan struct{})

	var countersAmount int64
	var countersStatus string
	var countersFired bool
	var refundsSeen []StaffOrderRefund
	var refundsFired bool
	// Whether the competing commit had already been released when the refunds query ran.
	// It must have been: that is the window this test exists to open.
	var refundsSawCommitAlreadyDone bool
	commitReleased := false

	probe := &staffOrderDetailProbe{
		afterCounters: func(amount int64, status string) {
			countersFired, countersAmount, countersStatus = true, amount, status
			// The window: the competing completion commits while the reader is parked
			// here, so it lands strictly between the two queries.
			close(snapshotTaken)
			<-commitDone
		},
		afterRefunds: func(refunds []StaffOrderRefund) {
			refundsFired = true
			refundsSawCommitAlreadyDone = commitReleased
			refundsSeen = append([]StaffOrderRefund(nil), refunds...)
		},
	}

	// Released once on EVERY exit path, and the reader is JOINED before teardown. Without
	// the join, a Fatal after the seam fires lets cleanup cancel the context and close the
	// database while the reader is still using both (ai-review pass 4).
	releaseReader := func() {
		if !commitReleased {
			commitReleased = true
			close(commitDone)
		}
	}
	t.Cleanup(func() {
		releaseReader()
		<-readerExited
	})

	type result struct {
		d   StaffOrderDetail
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer close(readerExited)
		d, err := readStaffOrderDetail(ctx, db, org, order, probe)
		done <- result{d, err}
	}()

	// Selected against the reader RETURNING: if its first query errors the seam never
	// fires, and a bare receive would block until the context expired and then report a
	// timeout, hiding the real error behind a hang.
	select {
	case <-snapshotTaken:
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the read returned before reaching the seam: %v", r.err)
		}
		t.Fatal("the read completed without reaching the seam between its two queries; " +
			"the interleave this test needs did not happen, so a pass would prove nothing")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err) // Cleanup releases and joins the reader.
	}
	releaseReader()

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}

	// Preconditions. Each guards a way this test could pass while observing nothing, which
	// is the failure mode this test is itself about.
	if !countersFired || !refundsFired {
		t.Fatalf("probes did not both fire (counters=%v refunds=%v); the seam did not run "+
			"where this test needs it", countersFired, refundsFired)
	}
	if len(refundsSeen) != 1 {
		t.Fatalf("the refunds query read %d rows, want 1; the fixture did not construct "+
			"the race", len(refundsSeen))
	}
	// THE WINDOW WAS ACTUALLY OPEN. Without this the test passes vacuously whenever the
	// barrier stops being between the two queries: the completion then commits before both
	// or after both, nothing is torn, and a consistent read proves nothing about snapshots.
	// Moving afterCounters below the refunds query is exactly that edit, and it is caught
	// here rather than by the consistency assertion, which it would satisfy honestly.
	if !refundsSawCommitAlreadyDone {
		t.Fatal("the refunds query ran BEFORE the competing completion was released, so the " +
			"commit never landed between the two queries; this run exercised no window and " +
			"a pass would prove nothing about reading one snapshot")
	}

	// THE INVARIANT, stated without naming the implementation: the two queries describe the
	// same instant. One transaction advances the counters and the refund row together, so
	// the completion is visible to BOTH queries or to NEITHER — a settled row read by the
	// second query beside unmoved counters read by the first is a state the database never
	// held.
	//
	// Asserted on what each QUERY read, not on the returned struct: that is what a
	// coordinated reversion cannot fake.
	settledRow := refundsSeen[0].Status == "completed"
	countersMoved := countersAmount == 1250 && countersStatus == "partial"
	if settledRow != countersMoved {
		t.Fatalf("TORN READ: the refunds query saw settled=%v (status=%q) while the counters "+
			"query saw moved=%v (refunded_amount=%d, refund_status=%q). The completion "+
			"committed between them, and only one snapshot can refuse to show half of it.",
			settledRow, refundsSeen[0].Status, countersMoved, countersAmount, countersStatus)
	}
	// And specifically the PRE-commit instant: the reader started before the commit, so a
	// repeatable-read snapshot must still show the state it started at.
	if settledRow || countersMoved {
		t.Errorf("the read observed a completion that committed AFTER its snapshot was taken "+
			"(row settled=%v, counters moved=%v)", settledRow, countersMoved)
	}
	// The returned response is the same read, so it cannot disagree with either query.
	if r.d.Totals.RefundedAmount != countersAmount || r.d.Totals.RefundStatus != countersStatus {
		t.Errorf("the response reports refunded_amount=%d/%q but the counters query read "+
			"%d/%q", r.d.Totals.RefundedAmount, r.d.Totals.RefundStatus, countersAmount, countersStatus)
	}
	if len(r.d.Refunds) != 1 || r.d.Refunds[0].Status != refundsSeen[0].Status {
		t.Errorf("the response's refund rows disagree with what the refunds query read")
	}
}
