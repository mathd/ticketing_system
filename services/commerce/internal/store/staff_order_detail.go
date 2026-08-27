package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// StaffOrderLine is one line of an order. There is exactly one, structurally:
// `orders.reservation_id` is NOT NULL UNIQUE and a reservation names one ticket type.
type StaffOrderLine struct {
	TicketTypeID uuid.UUID
	Quantity     int32
	UnitAmount   int64
	FaceValue    int64
	TotalAmount  int64
	Currency     string
}

// StaffOrderTotals is the order's money position. Every field is integer minor units
// (ADR-001) and PassedOnFees is an exact integer difference rather than a computed share,
// so the three reconcile by construction instead of by rounding luck.
type StaffOrderTotals struct {
	TotalAmount      int64
	FaceValue        int64
	PassedOnFees     int64
	RefundedAmount   int64
	RefundedQuantity int32
	RefundStatus     string
	Currency         string
}

// StaffOrderRefund is one persisted refund attempt.
//
// CompletedAt is nullable and PENDING attempts are included. The pending case is the
// point: a refund whose request timed out may still have moved money, and the page's only
// safe next step is replaying the same IdempotencyKey — which it can now read instead of
// remembering.
type StaffOrderRefund struct {
	RefundID       uuid.UUID
	Status         string
	Quantity       int32
	Amount         int64
	Currency       string
	IdempotencyKey string
	Actor          string
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// StaffOrderDetail is what a credentialed staff caller may see about one order.
//
// No buyer contact, no buyer id, no customer id — see ReadStaffOrderDetail.
type StaffOrderDetail struct {
	OrderID     uuid.UUID
	OrganizerID uuid.UUID
	Status      string
	Line        StaffOrderLine
	Totals      StaffOrderTotals
	Refunds     []StaffOrderRefund
}

// ReadStaffOrderDetail reads one order's line, money totals and refund attempts, scoped by
// organizer through the reservation — `orders` carries no organizer_id of its own, exactly
// as ReadOrderCancellationState documents.
//
// `AND r.organizer_id=$2` IS THE AUTHORIZATION. Delete it and this function answers about
// another tenant's order; there is no second check above it that would notice, because the
// credential proves the caller is the back office and says nothing about which organizer
// it acts for. The test that protects it lives at this tier for that reason: an assertion
// one layer up would agree with a fake and prove nothing about the SQL.
//
// A miss is sql.ErrNoRows whether the order does not exist or belongs to someone else, and
// the caller turns both into the same 404. That is deliberate: *why* nothing came back is
// itself a disclosure channel, and a distinct answer would confirm the order exists in
// another tenant. The same reasoning is already written into the cancellation report.
//
// buyer_pii IS DELIBERATELY NOT JOINED. Not "we don't select those columns" — the table
// never enters the query, so there is nothing for a later refactor to accidentally map.
// Buyer contact is exposed at /internal/buyers/{id}/delivery-email, to the service that
// must send mail, and nowhere else (ADR-003).
// ONE SNAPSHOT, and that is not tidiness (ai-review). The order's refund COUNTERS and the
// refund ROWS are two sources that CompleteOrderRefund advances together, inside one
// transaction (refunds.go: it updates order_refunds and orders' refunded_amount /
// refunded_quantity / refund_status before a single Commit). Two autocommit reads can
// straddle that commit and return the OLD counters beside a NEWLY completed refund — a
// response that says "1250 refunded" next to a row saying two refunds have settled.
//
// That is worse here than a stale read usually is, because this endpoint exists to tell a
// staff member what has already happened to an order before they decide what to do next,
// and the two halves disagreeing is precisely the confusion it was built to end. So: one
// REPEATABLE READ transaction, read-only, and both queries see the same instant.
// staffOrderDetailProbe is a test-only seam, nil in production, modelled on payments'
// verifyProbe (TKT-254) — the same problem, already solved and reviewed in this repo.
//
// It exists because the consistency this function promises cannot be tested from outside.
// The failure it guards is a competing transaction committing BETWEEN the two queries, and
// both complete in microseconds, so a test that merely runs them concurrently sees the
// commit before both or after both and never the tear. A plain SELECT cannot be stalled
// with a row lock either — MVCC readers do not wait.
//
// EACH CALLBACK CARRIES WHAT ITS QUERY ACTUALLY READ. That is the whole design, and three
// weaker versions were executed and passed while the guarantee was gone (ai-review passes
// 2-4):
//
//   - eight goroutines racing a commit: probabilistic, and green on a fast database.
//   - a bare `func()`: proves a callback ran. Delete the transaction AND move the call
//     below both queries — green.
//   - `func(tx *sql.Tx)` with the TEST querying through the handle: the probe became the
//     transaction's FIRST statement whenever the counters query was reverted to `db`, so
//     it ESTABLISHED the snapshot it claimed to verify. Reverting only that query — green.
//
// Passing the VALUES closes all three, because they are produced by the statements under
// test rather than beside them. A test can then assert that what the refunds query saw
// agrees with what the counters query saw, and no edit to the instrumentation can fake
// that agreement — only reading both from one snapshot can.
type staffOrderDetailProbe struct {
	// afterCounters carries the order's refund position as the FIRST query read it.
	afterCounters func(refundedAmount int64, refundStatus string)
	// afterRefunds carries the refund rows as the SECOND query read them.
	afterRefunds func(refunds []StaffOrderRefund)
}

func ReadStaffOrderDetail(ctx context.Context, db *sql.DB, org, order uuid.UUID) (StaffOrderDetail, error) {
	return readStaffOrderDetail(ctx, db, org, order, nil)
}

// readStaffOrderDetail is ReadStaffOrderDetail with the probe seam above. Production
// passes nil and pays nothing; only TestStaffOrderDetailReadsOneSnapshotOfCountersAndRefunds
// passes a probe, because the interleaving it needs cannot be constructed from outside.
func readStaffOrderDetail(ctx context.Context, db *sql.DB, org, order uuid.UUID, probe *staffOrderDetailProbe) (StaffOrderDetail, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return StaffOrderDetail{}, err
	}
	// Read-only: nothing to lose by rolling back, and a deferred rollback is what makes
	// every early return below safe.
	defer func() { _ = tx.Rollback() }()

	d := StaffOrderDetail{OrderID: order, OrganizerID: org}
	if err := tx.QueryRowContext(ctx, `
		SELECT o.status,
		       r.ticket_type_id, r.quantity, r.unit_amount, r.face_value_amount, r.total_amount, r.currency,
		       o.refund_status, o.refunded_quantity, o.refunded_amount
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2`, order, org).
		Scan(&d.Status,
			&d.Line.TicketTypeID, &d.Line.Quantity, &d.Line.UnitAmount, &d.Line.FaceValue,
			&d.Line.TotalAmount, &d.Line.Currency,
			&d.Totals.RefundStatus, &d.Totals.RefundedQuantity, &d.Totals.RefundedAmount); err != nil {
		return StaffOrderDetail{}, err
	}
	d.Totals.TotalAmount = d.Line.TotalAmount
	d.Totals.FaceValue = d.Line.FaceValue
	// Exact integer difference. face_value_amount <= total_amount is a table CHECK
	// (migration 0014), so this is never negative.
	d.Totals.PassedOnFees = d.Line.TotalAmount - d.Line.FaceValue
	d.Totals.Currency = d.Line.Currency

	if probe != nil && probe.afterCounters != nil {
		probe.afterCounters(d.Totals.RefundedAmount, d.Totals.RefundStatus)
	}

	// Scoped by organizer here TOO, rather than trusting that the order was already
	// scoped above. order_refunds carries its own organizer_id, so the predicate is
	// available and costs nothing — and a read whose two halves are scoped by different
	// arguments is one refactor away from disagreeing.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, status, quantity, amount, currency, idempotency_key, actor, created_at, completed_at
		FROM order_refunds
		WHERE organizer_id=$1 AND order_id=$2
		ORDER BY created_at, id`, org, order)
	if err != nil {
		return StaffOrderDetail{}, err
	}
	defer func() { _ = rows.Close() }()
	// Non-nil so the response carries `"refunds": []` rather than `null`: an order with no
	// refunds and an order whose refunds failed to load must not look the same to a client.
	d.Refunds = []StaffOrderRefund{}
	for rows.Next() {
		var f StaffOrderRefund
		var completed sql.NullTime
		if err := rows.Scan(&f.RefundID, &f.Status, &f.Quantity, &f.Amount, &f.Currency,
			&f.IdempotencyKey, &f.Actor, &f.CreatedAt, &completed); err != nil {
			return StaffOrderDetail{}, err
		}
		if completed.Valid {
			f.CompletedAt = &completed.Time
		}
		d.Refunds = append(d.Refunds, f)
	}
	if err := rows.Err(); err != nil {
		return StaffOrderDetail{}, err
	}
	if probe != nil && probe.afterRefunds != nil {
		probe.afterRefunds(d.Refunds)
	}
	return d, nil
}
