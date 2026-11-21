//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-525 S1 (ADR-087): recording `refunded_outside` for a failed staff or bulk refund.
//
// The store half: scoped inspection, then one short transaction that locks the refund, then
// its order, rechecks what was inspected, appends the outcome, completes the refund with the
// outcome as its evidence, and advances the order projection once.

func outsideRequest(c Completion, refund uuid.UUID) RefundOutcomeRequest {
	return RefundOutcomeRequest{
		OrganizerID: c.OrganizerID, RefundID: refund, Kind: OutcomeRefundedOutside,
		OperatorName: "Ada Operator", ExternalRef: "SEPA-2026-11-20-001", Reason: "card closed; paid by bank transfer",
	}
}

// bindPending binds one real pending refund of `quantity` units through BindOrderRefund.
func bindPending(t *testing.T, ctx context.Context, db *sql.DB, c Completion, key string, quantity int32) Refund {
	t.Helper()
	r, err := BindOrderRefund(ctx, db, request(c, key, quantity))
	if err != nil {
		t.Fatalf("bind refund: %v", err)
	}
	return r
}

func recordOutside(t *testing.T, ctx context.Context, db *sql.DB, in RefundOutcomeRequest) (RefundOutcome, bool, error) {
	t.Helper()
	target, err := InspectRefundOutcomeTarget(ctx, db, in.OrganizerID, in.RefundID)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	return RecordRefundedOutside(ctx, db, in, target)
}

type projection struct {
	qty    int32
	amount int64
	status string
}

func readProjection(t *testing.T, ctx context.Context, db *sql.DB, order uuid.UUID) projection {
	t.Helper()
	var p projection
	if err := db.QueryRowContext(ctx, `SELECT refunded_quantity,refunded_amount,refund_status FROM orders WHERE id=$1`, order).
		Scan(&p.qty, &p.amount, &p.status); err != nil {
		t.Fatal(err)
	}
	return p
}

func countFacts(t *testing.T, ctx context.Context, db *sql.DB, order uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_facts WHERE order_id=$1 AND fact_type='order.refunded'`, order).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The full S1 write: exact evidence, an evidence-linked completion with no payments fact, one
// projection advance, and no Commerce order.refunded fact (D15).
func TestRecordRefundedOutsideCompletesWithTheOutcomeAsEvidence(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	r := bindPending(t, ctx, db, c, "outside-1", 2)
	in := outsideRequest(c, r.ID)

	before := time.Now().Add(-time.Minute)
	got, replay, err := recordOutside(t, ctx, db, in)
	if err != nil {
		t.Fatalf("record refunded_outside: %v", err)
	}
	if replay {
		t.Fatal("the first record reported a replay")
	}
	var sessionUser string
	if err := db.QueryRowContext(ctx, `SELECT session_user`).Scan(&sessionUser); err != nil {
		t.Fatal(err)
	}
	if got.Kind != OutcomeRefundedOutside || got.Amount != 2000 || got.Currency != "EUR" ||
		got.ExternalRef != in.ExternalRef || got.OperatorName != in.OperatorName || got.Reason != in.Reason ||
		got.SessionUser != sessionUser || got.RecordedAt.Before(before) {
		t.Fatalf("outcome = %+v, want the request's attribution, the refund's 2000 EUR and session_user %q", got, sessionUser)
	}

	var row RefundOutcome
	if err := db.QueryRowContext(ctx, `
		SELECT kind,amount,currency,external_ref,operator_name,session_user_name,reason
		FROM refund_outcomes WHERE organizer_id=$1 AND subject_kind='order_refund' AND subject_id=$2`, c.OrganizerID, r.ID).
		Scan(&row.Kind, &row.Amount, &row.Currency, &row.ExternalRef, &row.OperatorName, &row.SessionUser, &row.Reason); err != nil {
		t.Fatalf("outcome row: %v", err)
	}
	if row.Kind != got.Kind || row.Amount != 2000 || row.Currency != "EUR" || row.ExternalRef != in.ExternalRef ||
		row.OperatorName != in.OperatorName || row.SessionUser != sessionUser || row.Reason != in.Reason {
		t.Fatalf("persisted outcome = %+v", row)
	}
	if row.OperatorName == row.SessionUser {
		t.Fatal("operator name and session_user are the same value; the fixture cannot tell them apart")
	}

	var status string
	var fact uuid.NullUUID
	var outside sql.NullString
	var completedAt sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT status,payment_fact_id,outside_completion_kind,completed_at FROM order_refunds WHERE organizer_id=$1 AND id=$2`,
		c.OrganizerID, r.ID).Scan(&status, &fact, &outside, &completedAt); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || fact.Valid || outside.String != OutcomeRefundedOutside || !completedAt.Valid {
		t.Fatalf("refund after outside completion: status=%s fact=%v outside=%v completed_at=%v", status, fact, outside, completedAt)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{2, 2000, "full"}) {
		t.Fatalf("projection = %+v, want {2 2000 full}", p)
	}
	if n := countFacts(t, ctx, db, order); n != 0 {
		t.Fatalf("order.refunded facts = %d, want 0: an outside completion is not a payments fact (D15)", n)
	}
}

// An exact repeat returns the ORIGINAL record and changes nothing; any difference in kind,
// reason, operator or reference is a conflict that also changes nothing.
func TestRecordRefundedOutsideReplayAndConflict(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 3, 1000)
	r := bindPending(t, ctx, db, c, "outside-replay", 1)
	in := outsideRequest(c, r.ID)
	first, _, err := recordOutside(t, ctx, db, in)
	if err != nil {
		t.Fatal(err)
	}

	again, replay, err := recordOutside(t, ctx, db, in)
	if err != nil || !replay {
		t.Fatalf("exact repeat: replay=%t err=%v", replay, err)
	}
	if !again.RecordedAt.Equal(first.RecordedAt) || again.SessionUser != first.SessionUser || again.OperatorName != first.OperatorName {
		t.Fatalf("replay = %+v, want the original record %+v", again, first)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{1, 1000, "partial"}) {
		t.Fatalf("projection after replay = %+v, want one advance {1 1000 partial}", p)
	}

	for name, mutate := range map[string]func(*RefundOutcomeRequest){
		"reason":       func(x *RefundOutcomeRequest) { x.Reason += "!" },
		"operator":     func(x *RefundOutcomeRequest) { x.OperatorName = "Grace Operator" },
		"external ref": func(x *RefundOutcomeRequest) { x.ExternalRef = "SEPA-other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := in
			mutate(&changed)
			if _, _, err := recordOutside(t, ctx, db, changed); !errors.Is(err, ErrRefundOutcomeConflict) {
				t.Fatalf("a repeat with a different %s: err = %v, want ErrRefundOutcomeConflict", name, err)
			}
		})
	}
	var outcomes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, c.OrganizerID, r.ID).Scan(&outcomes); err != nil || outcomes != 1 {
		t.Fatalf("outcomes = %d err=%v, want 1", outcomes, err)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{1, 1000, "partial"}) {
		t.Fatalf("projection after conflicts = %+v", p)
	}
}

// A different KIND already recorded is a conflict, and the refund stays as it was.
func TestRecordRefundedOutsideConflictsWithAnAbandonedOutcome(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "abandoned-first", 1)
	in := outsideRequest(c, r.ID)
	// Every field but the kind is identical to the request, so only the kind can decide.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO refund_outcomes(organizer_id,subject_kind,subject_id,kind,amount,currency,external_ref,operator_name,session_user_name,reason)
		VALUES($1,'order_refund',$2,'abandoned',1000,'EUR',$3,$4,session_user,$5)`,
		c.OrganizerID, r.ID, in.ExternalRef, in.OperatorName, in.Reason); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordOutside(t, ctx, db, in); !errors.Is(err, ErrRefundOutcomeConflict) {
		t.Fatalf("err = %v, want ErrRefundOutcomeConflict", err)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{0, 0, "none"}) {
		t.Fatalf("projection = %+v, want untouched", p)
	}
}

// A refund payments already completed is not eligible, and nothing is written.
func TestRecordRefundedOutsideRefusesAnOrdinarilyCompletedRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "ordinary", 1)
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, r.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordOutside(t, ctx, db, outsideRequest(c, r.ID)); !errors.Is(err, ErrRefundOutcomeIneligible) {
		t.Fatalf("err = %v, want ErrRefundOutcomeIneligible", err)
	}
	var outcomes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, c.OrganizerID, r.ID).Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("outcomes = %d err=%v", outcomes, err)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{1, 1000, "full"}) {
		t.Fatalf("projection = %+v, want the one ordinary advance", p)
	}
}

// Scope. A refund in another organizer, a refund whose source reservation belongs to another
// organizer, and a refund that does not exist all give the same refusal, at inspection and at
// the locked write. Same raw refund id in two organizers resolves to each organizer's own row.
func TestRefundOutcomeTargetIsOrganizerScoped(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	r := bindPending(t, ctx, db, c, "scoped", 1)

	other, otherOrder := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 700)
	// The same raw refund id, in the other organizer, on the other organizer's order.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason)
		VALUES($1,$2,$3,'same-raw-id','fp',1,700,700,'EUR','ops@example.test','same id')`, other.OrganizerID, r.ID, otherOrder); err != nil {
		t.Fatal(err)
	}
	mine, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
	if err != nil || mine.Amount != 1000 || mine.OrderID != c.OrderID {
		t.Fatalf("own target = %+v err=%v", mine, err)
	}
	theirs, err := InspectRefundOutcomeTarget(ctx, db, other.OrganizerID, r.ID)
	if err != nil || theirs.Amount != 700 || theirs.OrderID != otherOrder {
		t.Fatalf("other organizer's target = %+v err=%v", theirs, err)
	}

	// Foreign-only: a refund that exists ONLY under another organizer.
	foreignOnly := bindPending(t, ctx, db, other, "foreign-only", 1)
	// Malformed: the queue row claims this organizer, the source reservation is another's.
	malformed := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason)
		VALUES($1,$2,$3,'malformed','fp',1,700,700,'EUR','ops@example.test','malformed')`, c.OrganizerID, malformed, otherOrder); err != nil {
		t.Fatal(err)
	}
	// The reverse: a queue row under ANOTHER organizer on THIS organizer's order. Only the
	// refund row's own organizer predicate refuses it.
	foreignRow := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason)
		VALUES($1,$2,$3,'foreign-row','fp',1,1000,1000,'EUR','ops@example.test','foreign row')`, other.OrganizerID, foreignRow, c.OrderID); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"missing": uuid.New(), "foreign-only": foreignOnly.ID,
		"source reservation elsewhere": malformed, "refund row in another organizer": foreignRow} {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, id); !errors.Is(err, ErrRefundOutcomeNotFound) {
				t.Fatalf("inspect: err = %v, want ErrRefundOutcomeNotFound", err)
			}
			forged := RefundOutcomeTarget{OrganizerID: c.OrganizerID, RefundID: id, OrderID: otherOrder, Amount: 700, Currency: "EUR", ChargeKey: "x"}
			in := outsideRequest(c, id)
			if _, _, err := RecordRefundedOutside(ctx, db, in, forged); !errors.Is(err, ErrRefundOutcomeNotFound) {
				t.Fatalf("locked write: err = %v, want ErrRefundOutcomeNotFound", err)
			}
		})
	}
	var foreignStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM order_refunds WHERE organizer_id=$1 AND id=$2`, other.OrganizerID, foreignOnly.ID).Scan(&foreignStatus); err != nil || foreignStatus != "pending" {
		t.Fatalf("the foreign refund changed: status=%q err=%v", foreignStatus, err)
	}
}

// The locked write rechecks what was inspected before the payments call. A change to the order's
// charge key or to the refund's money in between refuses, and writes nothing.
func TestRecordRefundedOutsideRechecksTheInspectedRow(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, ctx context.Context, db *sql.DB, c Completion, target *RefundOutcomeTarget){
		"charge key": func(_ *testing.T, _ context.Context, _ *sql.DB, _ Completion, target *RefundOutcomeTarget) {
			target.ChargeKey += "-inspected-elsewhere"
		},
		"amount": func(_ *testing.T, _ context.Context, _ *sql.DB, _ Completion, target *RefundOutcomeTarget) {
			target.Amount++
		},
		"currency": func(_ *testing.T, _ context.Context, _ *sql.DB, _ Completion, target *RefundOutcomeTarget) {
			target.Currency = "USD"
		},
		"order": func(_ *testing.T, _ context.Context, _ *sql.DB, _ Completion, target *RefundOutcomeTarget) {
			target.OrderID = uuid.New()
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, ctx := outboxDB(t)
			c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
			r := bindPending(t, ctx, db, c, "recheck", 1)
			target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			change(t, ctx, db, c, &target)
			if _, _, err := RecordRefundedOutside(ctx, db, outsideRequest(c, r.ID), target); !errors.Is(err, ErrRefundOutcomeStale) {
				t.Fatalf("err = %v, want ErrRefundOutcomeStale", err)
			}
			if p := readProjection(t, ctx, db, order); p != (projection{0, 0, "none"}) {
				t.Fatalf("projection = %+v, want untouched", p)
			}
		})
	}
}

// Two different refunds of one order complete concurrently, one ordinarily and one outside.
// Each advances the projection once; the sum and status are exact.
func TestOutsideAndOrdinaryCompletionsOnOneOrderSumExactly(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 3, 1000)
	outside := bindPending(t, ctx, db, c, "concurrent-outside", 1)
	ordinary := bindPending(t, ctx, db, c, "concurrent-ordinary", 2)
	target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, outside.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the order so both writers queue on it, then release them together.
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, c.OrderID); err != nil {
		t.Fatal(err)
	}
	outsideDone, ordinaryDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := RecordRefundedOutside(ctx, db, outsideRequest(c, outside.ID), target)
		outsideDone <- err
	}()
	waitForLockWait(t, ctx, db, outsideOrderLockSQL, outsideDone)
	go func() { ordinaryDone <- CompleteOrderRefund(ctx, db, c.OrganizerID, ordinary.ID, uuid.New()) }()
	waitForLockWaiters(t, ctx, db, c.OrderID, 2)
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-outsideDone; err != nil {
		t.Fatalf("outside completion: %v", err)
	}
	if err := <-ordinaryDone; err != nil {
		t.Fatalf("ordinary completion: %v", err)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{3, 3000, "full"}) {
		t.Fatalf("projection = %+v, want {3 3000 full}", p)
	}
}

// waitForLockWaiters waits until n backends in this database wait on a lock.
func waitForLockWaiters(t *testing.T, ctx context.Context, db *sql.DB, _ uuid.UUID, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d backends waited on a lock, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// S1 keeps both allowances consumed. An order of two units at 1000: one unit completed
// outside plus one ordinary pending unit leaves the quantity sum at 2, so a further unit is
// refused at the Commerce ceiling.
func TestOutsideCompletionKeepsTheQuantityCeilingConsumed(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	outside := bindPending(t, ctx, db, c, "ceiling-outside", 1)
	if _, _, err := recordOutside(t, ctx, db, outsideRequest(c, outside.ID)); err != nil {
		t.Fatal(err)
	}
	bindPending(t, ctx, db, c, "ceiling-pending", 1)
	var qty, amount int64
	if err := db.QueryRowContext(ctx, `SELECT sum(quantity), sum(amount) FROM order_refunds WHERE order_id=$1`, c.OrderID).Scan(&qty, &amount); err != nil {
		t.Fatal(err)
	}
	if qty != 2 || amount != 2000 {
		t.Fatalf("refund sums = %d units / %d, want 2 / 2000", qty, amount)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "ceiling-third", 1)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("a third unit: err = %v, want ErrRefundExceedsOrder", err)
	}
}

// The reversal reconciler claims an outside completion as it claims any completed refund, and
// still ignores an ordinary pending one.
func TestReversalClaimTakesAnOutsideCompletionAndNotAPendingRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	outside := bindPending(t, ctx, db, c, "claim-outside", 1)
	pending := bindPending(t, ctx, db, c, "claim-pending", 1)
	if _, _, err := recordOutside(t, ctx, db, outsideRequest(c, outside.ID)); err != nil {
		t.Fatal(err)
	}
	// Claim until this organizer's rows are found or the queue is drained; other tests share
	// this database.
	seen := map[uuid.UUID]bool{}
	for range 50 {
		// A 1ms lease, so rows other tests own are claimable again at once.
		claimed, err := ClaimOutstandingReversals(ctx, db, 100, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		for _, cl := range claimed {
			if cl.Refund.OrganizerID == c.OrganizerID {
				seen[cl.Refund.ID] = true
			}
		}
		if len(claimed) < 100 || (seen[outside.ID] && seen[pending.ID]) {
			break
		}
	}
	if !seen[outside.ID] {
		t.Fatal("the reversal claim did not take the outside completion; its tickets would stay valid")
	}
	if seen[pending.ID] {
		t.Fatal("the reversal claim took a pending refund, which would void tickets for money that never moved")
	}
}
