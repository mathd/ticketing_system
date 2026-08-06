//go:build smoke

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Event-cancellation bulk refunds, store half (TKT-159, ADR-040).
//
// The load-bearing property under test is that "exactly one outcome per order",
// resumability and no-double-refund are ONE mechanism: the ledger row keyed by
// (organizer, run, order). Everything else here is scoping.

// seedBook seeds `orders` completed orders sharing one organizer and slot, which
// seedCompleted cannot do — it mints a fresh organizer AND slot per call, so a book
// built from it would have every order on a different event.
func seedBook(t *testing.T, db *sql.DB, ctx context.Context, key string, org, slot uuid.UUID, orders int, quantity int32, unit int64) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, orders)
	for i := range orders {
		res, order := uuid.New(), uuid.New()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'EUR','completed')`,
			res, org, uuid.New(), slot, uuid.New(), uuid.New(), quantity, unit, unit*int64(quantity)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
			VALUES($1,$2,'completed',$3,'fingerprint',$4)`,
			order, res, key+"-"+order.String(), uuid.New()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM cancellation_refund_orders WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM order_refunds WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM order_facts WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id=$1`, order)
			_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, order)
			_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, res)
		})
		ids = append(ids, order)
		_ = i
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM cancellation_refund_runs WHERE slot_id=$1`, slot) })
	return ids
}

func runRequest(org, slot uuid.UUID, key string) CancellationRunRequest {
	return CancellationRunRequest{
		OrganizerID: org, SlotID: slot, IdempotencyKey: key,
		Actor: "ops@example.test", Reason: "event cancelled",
	}
}

// A1: the per-order refund key must NOT contain the run. Two runs over the same slot have
// to converge on ONE refund identity per order — a run-scoped key lets a second run bind a
// SECOND refund whose quantity trips BindOrderRefund's ceiling (which counts pending
// refunds), stranding the first attempt and reporting the order failed forever.
func TestCancellationRefundKeyIsRunIndependent(t *testing.T) {
	slot, order, otherSlot, otherOrder := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	first := CancellationRefundKey(slot, order)
	if first != CancellationRefundKey(slot, order) {
		t.Fatal("cancellation refund key is not deterministic")
	}
	if first == CancellationRefundKey(otherSlot, order) {
		t.Fatal("distinct slots must not collide")
	}
	if first == CancellationRefundKey(slot, otherOrder) {
		t.Fatal("distinct orders must not collide")
	}
}

// The run identity derives from (organizer, idempotency key) exactly as RefundID does, so
// a retry that lost its response re-derives the same run rather than starting a second one
// over the same book.
func TestCancellationRunIDIsDeterministic(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	first := CancellationRunID(org, "k")
	if first != CancellationRunID(org, "k") {
		t.Fatal("run identity is not deterministic")
	}
	if first == CancellationRunID(org, "k2") {
		t.Fatal("distinct idempotency keys must not collide")
	}
	if first == CancellationRunID(other, "k") {
		t.Fatal("distinct organizers must not collide")
	}
}

// AC 5 scoping + replay: the same key answers as itself; a different instruction under the
// same key is a conflict rather than a silent replay of someone else's run.
func TestBindCancellationRunReplaysAndConflicts(t *testing.T) {
	db, ctx := outboxDB(t)
	org, slot := uuid.New(), uuid.New()
	seedBook(t, db, ctx, "bind-replay", org, slot, 1, 2, 1000)

	run, err := BindCancellationRun(ctx, db, runRequest(org, slot, "run-key"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Replay {
		t.Fatal("first bind must not report a replay")
	}
	if run.CutoffAt.IsZero() {
		t.Fatal("cutoff must be recorded with the run")
	}
	again, err := BindCancellationRun(ctx, db, runRequest(org, slot, "run-key"))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replay || again.ID != run.ID || !again.CutoffAt.Equal(run.CutoffAt) {
		t.Fatalf("replay must answer as the same run with the same cutoff: %+v vs %+v", again, run)
	}

	different := runRequest(org, uuid.New(), "run-key")
	if _, err := BindCancellationRun(ctx, db, different); err != ErrCancellationRunConflict {
		t.Fatalf("reused key with a different slot = %v, want ErrCancellationRunConflict", err)
	}
}

// AC 1: the book is every completed order on THIS organizer's slot, at or before the
// cutoff — and nothing else. The negatives are the point: another organizer's order on the
// same slot id, another slot's order, and a non-completed order must not be enumerated.
func TestEnumerateCancellationBookScopesTheSlot(t *testing.T) {
	db, ctx := outboxDB(t)
	org, slot := uuid.New(), uuid.New()
	want := seedBook(t, db, ctx, "enum-in", org, slot, 3, 2, 1000)
	seedBook(t, db, ctx, "enum-other-org", uuid.New(), slot, 2, 2, 1000)
	seedBook(t, db, ctx, "enum-other-slot", org, uuid.New(), 2, 2, 1000)

	// A non-completed order on the same slot: counted as incomplete-at-cutoff, never
	// enumerated. It is the silent-under-refund case AC 6's report has to surface.
	stray := seedBook(t, db, ctx, "enum-stray", org, slot, 1, 2, 1000)
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='created' WHERE id=$1`, stray[0]); err != nil {
		t.Fatal(err)
	}

	run, err := BindCancellationRun(ctx, db, runRequest(org, slot, "enum-run"))
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, err := EnumerateCancellationBook(ctx, db, run.OrganizerID, run.ID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}

	got := map[uuid.UUID]bool{}
	rows, err := db.QueryContext(ctx, `SELECT order_id FROM cancellation_refund_orders WHERE organizer_id=$1 AND run_id=$2`, run.OrganizerID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("enumerated %d orders, want %d — the scan is not scoped to (organizer, slot, completed)", len(got), len(want))
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("order %s missing from the book", id)
		}
	}

	var incomplete int
	if err := db.QueryRowContext(ctx, `SELECT incomplete_at_enumeration FROM cancellation_refund_runs WHERE organizer_id=$1 AND id=$2`, run.OrganizerID, run.ID).Scan(&incomplete); err != nil {
		t.Fatal(err)
	}
	if incomplete != 1 {
		t.Fatalf("incomplete_at_enumeration = %d, want 1 — an order that could not be enumerated must not vanish silently", incomplete)
	}
}

// AC 6: the book is claimed in bounded batches. A claim larger than the batch, or a whole-
// book transaction, fails here.
func TestClaimCancellationOrdersIsBatchBounded(t *testing.T) {
	db, ctx := outboxDB(t)
	org, slot := uuid.New(), uuid.New()
	seedBook(t, db, ctx, "batch", org, slot, 5, 1, 1000)
	run := enumerated(t, db, ctx, org, slot, "batch-run")

	first, err := ClaimCancellationOrders(ctx, db, 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("claimed %d rows, want the batch bound of 2", len(first))
	}
	// A claimed row is leased, not locked: a second claimant skips it and makes progress
	// on the rest of the book while the first is still working.
	second, err := ClaimCancellationOrders(ctx, db, 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("second claimant got %d rows, want 2 — a lease must not block the rest of the book", len(second))
	}
	for _, w := range second {
		for _, f := range first {
			if w.OrderID == f.OrderID {
				t.Fatal("two claimants got the same order")
			}
		}
	}
	if run.ID == uuid.Nil {
		t.Fatal("run was not created")
	}
}

// AC 2 + AC 4: the first terminal verdict is the only verdict. A stale claimant — one
// whose lease lapsed and whose work was taken over — cannot overwrite its successor's
// outcome, which is what makes an interrupted run safe to resume.
func TestFinalizeCancellationOrderIsClaimFencedAndOnceOnly(t *testing.T) {
	db, ctx := outboxDB(t)
	org, slot := uuid.New(), uuid.New()
	seedBook(t, db, ctx, "fence", org, slot, 1, 1, 1000)
	run := enumerated(t, db, ctx, org, slot, "fence-run")

	claimed, err := ClaimCancellationOrders(ctx, db, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}
	w := claimed[0]

	stale := w
	stale.ClaimID = uuid.New()
	if err := FinalizeCancellationOrder(ctx, db, stale, CancellationOutcome{
		Outcome: "failed", FailureCode: "internal", FailureReason: "stale claimant",
	}); err != ErrCancellationClaimLost {
		t.Fatalf("stale claim finalize = %v, want ErrCancellationClaimLost", err)
	}

	if err := FixCancellationRequestedQuantity(ctx, db, w, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeCancellationOrder(ctx, db, w, CancellationOutcome{
		Outcome: "refunded", RefundID: uuid.New(),
		MoneyRefunded: true, TicketsVoided: true, CapacityReturned: true,
		RefundedQuantity: 1, RefundedAmount: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	// A second finalize by the SAME claimant is refused too: the row is terminal, and a
	// terminal outcome is not something a retry gets to revise.
	if err := FinalizeCancellationOrder(ctx, db, w, CancellationOutcome{
		Outcome: "failed", FailureCode: "internal", FailureReason: "second verdict",
	}); err != ErrCancellationClaimLost {
		t.Fatalf("re-finalize = %v, want ErrCancellationClaimLost", err)
	}

	var outcome string
	if err := db.QueryRowContext(ctx, `SELECT outcome FROM cancellation_refund_orders WHERE order_id=$1`, w.OrderID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "refunded" {
		t.Fatalf("outcome = %q, want the first verdict to be the only verdict", outcome)
	}
	if run.Status == "" {
		t.Fatal("run status missing")
	}
}

// ADR-039, enforced by the database rather than by the runner's good manners: a success
// means EVERY obligation is discharged. Money back with tickets still valid is `failed`.
func TestSuccessfulOutcomeCannotHideAnOutstandingObligation(t *testing.T) {
	db, ctx := outboxDB(t)
	org, slot := uuid.New(), uuid.New()
	seedBook(t, db, ctx, "obligation", org, slot, 1, 1, 1000)
	enumerated(t, db, ctx, org, slot, "obligation-run")

	claimed, err := ClaimCancellationOrders(ctx, db, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %d", err, len(claimed))
	}
	// Everything else about the row is valid — quantity fixed, refund bound — so the ONLY
	// constraint left to reject it is the obligation one. Asserting on the constraint by
	// name matters: without it this test passes for the wrong reason the moment some other
	// CHECK happens to fire first.
	if err := FixCancellationRequestedQuantity(ctx, db, claimed[0], 1, false); err != nil {
		t.Fatal(err)
	}
	err = FinalizeCancellationOrder(ctx, db, claimed[0], CancellationOutcome{
		Outcome: "refunded", RefundID: uuid.New(),
		MoneyRefunded: true, TicketsVoided: false, CapacityReturned: false,
	})
	if err == nil {
		t.Fatal("a `refunded` outcome with the reversal outstanding was accepted — the ADR-039 check is missing")
	}
	if !strings.Contains(err.Error(), "cancellation_refund_orders_success_is_complete") {
		t.Fatalf("rejected by %v, want the success_is_complete check", err)
	}
}

// AC 4 + AC 5: a run completes only when enumeration finished AND no row is unresolved,
// and the report is organizer-scoped.
func TestRunCompletesOnlyWhenEveryRowIsTerminal(t *testing.T) {
	db, ctx := outboxDB(t)
	org, slot := uuid.New(), uuid.New()
	seedBook(t, db, ctx, "complete", org, slot, 2, 1, 1000)
	run := enumerated(t, db, ctx, org, slot, "complete-run")

	if _, err := CompleteFinishedCancellationRuns(ctx, db); err != nil {
		t.Fatal(err)
	}
	report, err := CancellationReport(ctx, db, org, run.ID, 100, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Run.Status == "completed" {
		t.Fatal("run reported completed while rows are still unresolved")
	}
	if report.Counts.Pending != 2 {
		t.Fatalf("pending = %d, want 2", report.Counts.Pending)
	}

	for {
		claimed, err := ClaimCancellationOrders(ctx, db, 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) == 0 {
			break
		}
		for _, w := range claimed {
			if err := FinalizeCancellationOrder(ctx, db, w, CancellationOutcome{
				Outcome: "already_refunded", MoneyRefunded: true, TicketsVoided: true, CapacityReturned: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := CompleteFinishedCancellationRuns(ctx, db); err != nil {
		t.Fatal(err)
	}
	report, err = CancellationReport(ctx, db, org, run.ID, 100, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", report.Run.Status)
	}
	if report.Counts.Total != 2 || report.Counts.AlreadyRefunded != 2 || report.Counts.Pending != 0 {
		t.Fatalf("counts = %+v, want 2 total / 2 already_refunded / 0 pending", report.Counts)
	}
	if len(report.Orders) != 2 {
		t.Fatalf("report rows = %d, want 2", len(report.Orders))
	}

	if _, err := CancellationReport(ctx, db, uuid.New(), run.ID, 100, uuid.Nil); err != sql.ErrNoRows {
		t.Fatal("the same run id under another organizer must not be readable")
	}
}

// A2: the runner resolves an existing cancellation refund before computing anything. The
// lookup is what lets a resumed run replay its own earlier attempt instead of binding a
// second refund against a ceiling the first one already consumed.
func TestLookupRefundByIDFindsABoundRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "lookup-refund", 2, 1000)
	key := CancellationRefundKey(c.SlotID, order)

	if _, found, err := LookupRefundByID(ctx, db, c.OrganizerID, RefundID(c.OrganizerID, key)); err != nil || found {
		t.Fatalf("unbound refund: found=%v err=%v", found, err)
	}
	bound, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: order, OrganizerID: c.OrganizerID, Quantity: 2, IdempotencyKey: key,
		Actor: CancellationRefundActor, Reason: CancellationRefundReason,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := LookupRefundByID(ctx, db, c.OrganizerID, bound.ID)
	if err != nil || !found {
		t.Fatalf("bound refund not found: %v %v", found, err)
	}
	if got.ID != bound.ID || got.Quantity != 2 || got.OrderID != order {
		t.Fatalf("looked up %+v, want the bound refund", got)
	}
}

func enumerated(t *testing.T, db *sql.DB, ctx context.Context, org, slot uuid.UUID, key string) CancellationRun {
	t.Helper()
	run, err := BindCancellationRun(ctx, db, runRequest(org, slot, key))
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, err := EnumerateCancellationBook(ctx, db, run.OrganizerID, run.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return run
		}
	}
}
