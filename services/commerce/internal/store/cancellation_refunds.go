package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Event-cancellation bulk refunds, store half (TKT-159, ADR-040).
//
// A durable run plus a per-order ledger that is BOTH the work queue and the final report.
// One mechanism, so "exactly one outcome per order", resumability and no-double-refund
// cannot disagree with each other.
//
// Nothing here holds a transaction or a lock across the book: enumeration pages, claims
// are bounded and lease rather than lock, and every external call happens after the claim
// transaction has committed.

var (
	// ErrCancellationRunConflict reports the same idempotency key used with a different
	// instruction, exactly as ErrRefundConflict does for a single refund.
	ErrCancellationRunConflict = errors.New("cancellation run idempotency key reused with a different request")
	// ErrCancellationClaimLost reports that the row was finalized by someone else, or
	// that this claimant's lease lapsed and its work was taken over. The first terminal
	// verdict is the only verdict.
	ErrCancellationClaimLost = errors.New("cancellation work row is no longer this claimant's")
)

// The attribution cancellation refunds bind under. Deliberately FIXED rather than the
// operator's: order_refunds.request_fingerprint covers actor and reason, so a second run
// carrying a different operator would conflict with its own earlier attempt instead of
// replaying it (ADR-040 §3). The operator's attribution is recorded on the run row.
const (
	CancellationRefundActor  = "system:event-cancellation"
	CancellationRefundReason = "event cancelled"
)

// CancellationRunRequest is one operator instruction to refund a slot's book.
type CancellationRunRequest struct {
	OrganizerID, SlotID uuid.UUID
	IdempotencyKey      string
	Actor, Reason       string
}

// CancellationRun is a durable bulk-refund run.
type CancellationRun struct {
	ID, OrganizerID, SlotID uuid.UUID
	Status                  string
	CutoffAt, CreatedAt     time.Time
	CompletedAt             sql.NullTime
	IncompleteAtEnumeration int
	Replay                  bool
}

// CancellationWork is one claimed ledger row: the unit the runner refunds.
type CancellationWork struct {
	OrganizerID, RunID, OrderID, SlotID, ClaimID uuid.UUID
	// RequestedQuantity is fixed before the first external call and never recomputed —
	// recomputing after money moved reads a different remainder, which would change the
	// refund's request fingerprint and turn a crash-resume into a conflict with itself.
	RequestedQuantity sql.NullInt32
	Currency          string
	// PriorRun reports that a previous run had already refunded this order when this row
	// first resolved its quantity. Read back on every attempt so a resumed one attributes
	// the outcome the same way the first one would have.
	PriorRun bool
	// Attempts includes THIS claim. The runner uses it to bound retries of ambiguous
	// failures — the ones where the money may or may not have moved.
	Attempts int
}

// CancellationOutcome is a row's terminal verdict.
// The refunded quantity is NOT here: it is fixed by FixCancellationRequestedQuantity
// before the first external call, and a verdict is not allowed to revise it.
type CancellationOutcome struct {
	Outcome                    string
	RefundID                   uuid.UUID
	FailureCode, FailureReason string
	MoneyRefunded              bool
	TicketsVoided              bool
	CapacityReturned           bool
	RefundedQuantity           int32
	RefundedAmount             int64
}

// CancellationCounts is the report's tally, derived from the ledger rather than copied
// onto the run — a second projection is a second thing that can drift.
type CancellationCounts struct {
	Total, Refunded, AlreadyRefunded, Failed, Pending int
}

// CancellationOrderOutcome is one row of the report.
type CancellationOrderOutcome struct {
	OrderID                    uuid.UUID
	Outcome                    string
	RefundID                   uuid.NullUUID
	RefundedQuantity           int32
	RefundedAmount             int64
	Currency                   string
	MoneyRefunded              bool
	TicketsVoided              bool
	CapacityReturned           bool
	FailureCode, FailureReason string
}

// CancellationReportPage is a run plus one page of its outcomes.
type CancellationReportPage struct {
	Run                     CancellationRun
	Counts                  CancellationCounts
	IncompleteAtEnumeration int
	Orders                  []CancellationOrderOutcome
	NextAfterOrderID        uuid.NullUUID
}

// CancellationRunID derives a run's identity from its organizer and idempotency key, so a
// retry that lost its response re-derives the same run rather than starting a second one
// over the same book. Namespaced apart from RefundID so the two can never collide.
func CancellationRunID(org uuid.UUID, idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cancellation-run:"+org.String()+":"+idempotencyKey))
}

// CancellationRefundKey is the idempotency key a cancellation refund of one order binds
// under. It deliberately does NOT contain the run: every run over the slot must converge
// on ONE refund identity per order.
//
// Why it matters (ADR-040 §3): BindOrderRefund's ceiling counts PENDING refunds, while the
// order's refunded_quantity projection only moves on completion. A run that dies between
// the two leaves a pending row that consumes the ceiling but is invisible to the
// projection — so a second run with a run-scoped key would compute the full remaining
// quantity, bind a second refund, and trip ErrRefundExceedsOrder, stranding the first
// attempt and reporting the order failed forever. A run-independent key makes the second
// run REPLAY the first attempt, which is what the single-order path already does.
func CancellationRefundKey(slot, order uuid.UUID) string {
	return CancellationRefundKeyPrefix + slot.String() + ":" + order.String()
}

// CancellationRefundKeyPrefix is RESERVED: the staff refund endpoint rejects an
// Idempotency-Key carrying it. A staff refund under a derived key would produce the same
// refund identity while disagreeing with its request fingerprint, and every cancellation run
// would then report that order failed forever — even one whose staff refund fully succeeded.
const CancellationRefundKeyPrefix = "cancel:"

func cancellationRunFingerprint(in CancellationRunRequest) string {
	sum := sha256.Sum256([]byte(in.SlotID.String() + "\x00" + in.Actor + "\x00" + in.Reason))
	return hex.EncodeToString(sum[:])
}

// BindCancellationRun inserts-or-loads the one run for (organizer, idempotency key).
//
// The cutoff is stamped in the SAME transaction as the row, so the book's membership is
// durable before any work starts and a replay answers with the original cutoff rather than
// silently widening the book.
func BindCancellationRun(ctx context.Context, db *sql.DB, in CancellationRunRequest) (CancellationRun, error) {
	id := CancellationRunID(in.OrganizerID, in.IdempotencyKey)
	fingerprint := cancellationRunFingerprint(in)

	existing, found, err := lookupCancellationRun(ctx, db, in.OrganizerID, id)
	if err != nil {
		return CancellationRun{}, err
	}
	if found {
		if existing.fingerprint != fingerprint {
			return CancellationRun{}, ErrCancellationRunConflict
		}
		existing.run.Replay = true
		return existing.run, nil
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO cancellation_refund_runs(organizer_id,id,slot_id,idempotency_key,request_fingerprint,actor,reason,cutoff_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,now()) ON CONFLICT DO NOTHING`,
		in.OrganizerID, id, in.SlotID, in.IdempotencyKey, fingerprint, in.Actor, in.Reason); err != nil {
		return CancellationRun{}, err
	}
	// Re-read rather than trusting the INSERT: ON CONFLICT DO NOTHING means a concurrent
	// caller may have won, and its row is the one that counts.
	bound, found, err := lookupCancellationRun(ctx, db, in.OrganizerID, id)
	if err != nil {
		return CancellationRun{}, err
	}
	if !found {
		return CancellationRun{}, errors.New("cancellation run missing after bind")
	}
	if bound.fingerprint != fingerprint {
		return CancellationRun{}, ErrCancellationRunConflict
	}
	return bound.run, nil
}

type storedCancellationRun struct {
	run         CancellationRun
	fingerprint string
}

func lookupCancellationRun(ctx context.Context, q rowQuerier, org, id uuid.UUID) (storedCancellationRun, bool, error) {
	var s storedCancellationRun
	err := q.QueryRowContext(ctx, `
		SELECT id,organizer_id,slot_id,request_fingerprint,status,cutoff_at,created_at,completed_at,incomplete_at_enumeration
		FROM cancellation_refund_runs WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&s.run.ID, &s.run.OrganizerID, &s.run.SlotID, &s.fingerprint, &s.run.Status,
			&s.run.CutoffAt, &s.run.CreatedAt, &s.run.CompletedAt, &s.run.IncompleteAtEnumeration)
	if errors.Is(err, sql.ErrNoRows) {
		return storedCancellationRun{}, false, nil
	}
	if err != nil {
		return storedCancellationRun{}, false, err
	}
	return s, true, nil
}

// ClaimCancellationRuns leases nothing — it just lists unfinished runs, oldest first, for
// the runner to enumerate. Enumeration itself takes the run row lock for one page.
func ClaimCancellationRuns(ctx context.Context, db *sql.DB, limit int) ([]CancellationRun, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id,organizer_id,slot_id,status,cutoff_at,created_at,completed_at,incomplete_at_enumeration
		FROM cancellation_refund_runs WHERE status <> 'completed'
		ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CancellationRun
	for rows.Next() {
		var r CancellationRun
		if err := rows.Scan(&r.ID, &r.OrganizerID, &r.SlotID, &r.Status, &r.CutoffAt,
			&r.CreatedAt, &r.CompletedAt, &r.IncompleteAtEnumeration); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnumerateCancellationBook materializes ONE page of the run's book and reports whether
// enumeration is finished.
//
// The page is over `reservations` — orders carry neither organizer_id nor slot_id, and
// reach a slot only through the unique orders.reservation_id. The keyset cursor advances
// across the WHOLE page, including reservations with no completed order, so no later state
// change can leave a gap behind the cursor.
//
// Orders that existed at the cutoff but were not completed are COUNTED rather than
// enumerated: they cannot be refunded by this run, and a count is what keeps a possible
// under-refund from being invisible.
func EnumerateCancellationBook(ctx context.Context, db *sql.DB, org, runID uuid.UUID, batch int) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var slot uuid.UUID
	var cutoff time.Time
	var cursorAt sql.NullTime
	var cursorID uuid.NullUUID
	var enumerated sql.NullTime
	// FOR UPDATE on the run: two runner passes must not page the same cursor twice.
	if err := tx.QueryRowContext(ctx, `
		SELECT slot_id,cutoff_at,cursor_created_at,cursor_reservation_id,enumeration_completed_at
		FROM cancellation_refund_runs WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, org, runID).
		Scan(&slot, &cutoff, &cursorAt, &cursorID, &enumerated); err != nil {
		return false, err
	}
	if enumerated.Valid {
		return true, tx.Commit()
	}

	// A NULL cursor means "the first page" and is tested as NULL rather than as a
	// far-past sentinel: a sentinel silently skips any reservation older than it, which an
	// imported or backdated book can be.
	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.created_at, r.currency, o.id, o.status
		FROM reservations r LEFT JOIN orders o ON o.reservation_id = r.id
		WHERE r.organizer_id=$1 AND r.slot_id=$2 AND r.created_at <= $3
		  AND ($4::timestamptz IS NULL OR (r.created_at, r.id) > ($4, $5))
		ORDER BY r.created_at, r.id
		LIMIT $6`, org, slot, cutoff, cursorAt, cursorID, batch)
	if err != nil {
		return false, err
	}
	type row struct {
		resID, orderID uuid.UUID
		createdAt      time.Time
		currency       string
		hasOrder       bool
		completed      bool
	}
	var page []row
	for rows.Next() {
		var r row
		var orderID uuid.NullUUID
		var status sql.NullString
		if err := rows.Scan(&r.resID, &r.createdAt, &r.currency, &orderID, &status); err != nil {
			_ = rows.Close()
			return false, err
		}
		r.orderID, r.hasOrder = orderID.UUID, orderID.Valid
		r.completed = status.Valid && status.String == "completed"
		page = append(page, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	_ = rows.Close()

	incomplete := 0
	for _, r := range page {
		if !r.hasOrder {
			continue
		}
		if !r.completed {
			incomplete++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO cancellation_refund_orders(organizer_id,run_id,order_id,currency)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, org, runID, r.orderID, r.currency); err != nil {
			return false, err
		}
	}

	done := len(page) < batch
	if len(page) > 0 {
		last := page[len(page)-1]
		if _, err := tx.ExecContext(ctx, `
			UPDATE cancellation_refund_runs
			SET cursor_created_at=$3, cursor_reservation_id=$4,
			    incomplete_at_enumeration = incomplete_at_enumeration + $5,
			    status = CASE WHEN status='pending' THEN 'running' ELSE status END,
			    updated_at = now()
			WHERE organizer_id=$1 AND id=$2`, org, runID, last.createdAt, last.resID, incomplete); err != nil {
			return false, err
		}
	}
	if done {
		if _, err := tx.ExecContext(ctx, `
			UPDATE cancellation_refund_runs
			SET enumeration_completed_at=now(),
			    status = CASE WHEN status='pending' THEN 'running' ELSE status END,
			    updated_at = now()
			WHERE organizer_id=$1 AND id=$2`, org, runID); err != nil {
			return false, err
		}
	}
	return done, tx.Commit()
}

// ClaimCancellationOrders leases at most `limit` unresolved rows, oldest first.
//
// FOR UPDATE SKIP LOCKED inside a CTE, then a lease written to durable columns and
// COMMITTED before the caller does anything external — the claim is a lease, not a held
// lock, which is what keeps the rest of the book writable while one refund is in flight.
func ClaimCancellationOrders(ctx context.Context, db *sql.DB, limit int, lease time.Duration) ([]CancellationWork, error) {
	claim := uuid.New()
	rows, err := db.QueryContext(ctx, `
		WITH claimable AS (
			SELECT organizer_id, run_id, order_id
			FROM cancellation_refund_orders
			WHERE outcome IS NULL AND (lease_until IS NULL OR lease_until <= now())
			ORDER BY created_at, order_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE cancellation_refund_orders c
			SET claim_id=$3, lease_until = now() + make_interval(secs => $2), attempts = c.attempts + 1
			FROM claimable k
			WHERE c.organizer_id=k.organizer_id AND c.run_id=k.run_id AND c.order_id=k.order_id
			RETURNING c.organizer_id, c.run_id, c.order_id, c.requested_quantity, c.currency, c.attempts, c.prior_run
		)
		SELECT c.organizer_id, c.run_id, c.order_id, c.requested_quantity, c.currency, c.attempts, c.prior_run, r.slot_id
		FROM claimed c JOIN cancellation_refund_runs r ON r.organizer_id=c.organizer_id AND r.id=c.run_id`,
		limit, lease.Seconds(), claim)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CancellationWork
	for rows.Next() {
		w := CancellationWork{ClaimID: claim}
		if err := rows.Scan(&w.OrganizerID, &w.RunID, &w.OrderID, &w.RequestedQuantity, &w.Currency, &w.Attempts, &w.PriorRun, &w.SlotID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FixCancellationRequestedQuantity persists the quantity this row will refund, BEFORE any
// external call. Guarded on the column still being NULL so a resumed attempt keeps the
// original number: the refund's request fingerprint covers the quantity, and a recomputed
// one would conflict with its own earlier attempt.
func FixCancellationRequestedQuantity(ctx context.Context, db *sql.DB, w CancellationWork, quantity int32, priorRun bool) error {
	// COALESCE, not `IS NULL`: a row this claimant already fixed to the same number is a
	// success, while a row whose claim has moved on is ErrCancellationClaimLost. Ignoring
	// RowsAffected here let a claimant whose lease had lapsed keep driving the refund and
	// then lose its finalize to the successor, leaving a discharged order reported failed.
	res, err := db.ExecContext(ctx, `
		UPDATE cancellation_refund_orders SET requested_quantity=$5, prior_run=$6
		WHERE organizer_id=$1 AND run_id=$2 AND order_id=$3 AND claim_id=$4
		  AND outcome IS NULL AND COALESCE(requested_quantity,$5)=$5`,
		w.OrganizerID, w.RunID, w.OrderID, w.ClaimID, quantity, priorRun)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCancellationClaimLost
	}
	return nil
}

// ClearCancellationRequestedQuantity releases a fixed quantity so the next attempt
// recomputes it. Used for exactly one case: the order's refund ceiling moved under the
// runner (a staff refund landed between reading the remainder and binding), so the fixed
// number is now wrong and retrying it would fail forever.
func ClearCancellationRequestedQuantity(ctx context.Context, db *sql.DB, w CancellationWork) error {
	_, err := db.ExecContext(ctx, `
		UPDATE cancellation_refund_orders SET requested_quantity=NULL
		WHERE organizer_id=$1 AND run_id=$2 AND order_id=$3 AND claim_id=$4 AND outcome IS NULL`,
		w.OrganizerID, w.RunID, w.OrderID, w.ClaimID)
	return err
}

// FinalizeCancellationOrder writes the row's terminal verdict, fenced on the claim.
//
// `outcome IS NULL` in the predicate is what makes the FIRST verdict the ONLY verdict: a
// claimant whose lease lapsed mid-drive, and whose work was taken over, cannot overwrite
// its successor. The database's own CHECK refuses a successful outcome with an obligation
// outstanding (ADR-039), so a runner bug cannot report money-back-tickets-valid as done.
func FinalizeCancellationOrder(ctx context.Context, db *sql.DB, w CancellationWork, out CancellationOutcome) error {
	var refund uuid.NullUUID
	if out.RefundID != uuid.Nil {
		refund = uuid.NullUUID{UUID: out.RefundID, Valid: true}
	}
	var code, reason *string
	if out.FailureCode != "" {
		code, reason = &out.FailureCode, &out.FailureReason
	}
	res, err := db.ExecContext(ctx, `
		UPDATE cancellation_refund_orders
		SET outcome=$5, refund_id=COALESCE($6, refund_id), failure_code=$7, failure_reason=$8,
		    money_refunded=$9, tickets_voided=$10, capacity_returned=$11,
		    refunded_quantity=$12, refunded_amount=$13,
		    claim_id=NULL, lease_until=NULL, completed_at=now()
		WHERE organizer_id=$1 AND run_id=$2 AND order_id=$3 AND claim_id=$4 AND outcome IS NULL`,
		w.OrganizerID, w.RunID, w.OrderID, w.ClaimID, out.Outcome, refund, code, reason,
		out.MoneyRefunded, out.TicketsVoided, out.CapacityReturned,
		out.RefundedQuantity, out.RefundedAmount)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCancellationClaimLost
	}
	return nil
}

// AbandonCancellationClaim releases a lease without a verdict, so an interrupted runner's
// work is reclaimable immediately rather than after the lease expires. Context cancellation
// is an interruption, never a business outcome.
func AbandonCancellationClaim(ctx context.Context, db *sql.DB, w CancellationWork, refundAttempt bool) error {
	// The attempt charge is REFUNDED with the claim. Attempts are charged at claim time, so
	// a row claimed and released without being driven — a shutdown, a lapsed lease — would
	// otherwise spend its whole retry budget on work that never happened, and its first real
	// ambiguous failure would arrive already exhausted. Recovery's AbandonRecoveryClaim does
	// the same for the same reason.
	// Only an UNDRIVEN claim gets its charge back. A claim released after the refund unit was
	// already called has done real money-path work, and refunding its attempt would let a
	// cancellation window that recurs at exactly that point keep the row below the cap
	// forever — so the run would never complete and its report never become readable.
	_, err := db.ExecContext(ctx, `
		UPDATE cancellation_refund_orders
		SET claim_id=NULL, lease_until=NULL,
		    attempts = CASE WHEN $5 THEN greatest(attempts - 1, 0) ELSE attempts END
		WHERE organizer_id=$1 AND run_id=$2 AND order_id=$3 AND claim_id=$4 AND outcome IS NULL`,
		w.OrganizerID, w.RunID, w.OrderID, w.ClaimID, refundAttempt)
	return err
}

// CompleteFinishedCancellationRuns stamps runs whose enumeration has finished and whose
// every row is terminal. Both halves are required: a run with an unresolved row is still
// owed work, and a run still enumerating may not have materialized the whole book.
func CompleteFinishedCancellationRuns(ctx context.Context, db *sql.DB) (int, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE cancellation_refund_runs r
		SET status='completed', completed_at=now(), updated_at=now()
		WHERE r.status <> 'completed' AND r.enumeration_completed_at IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM cancellation_refund_orders c
		    WHERE c.organizer_id=r.organizer_id AND c.run_id=r.id AND c.outcome IS NULL
		  )`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// CancellationReport reads one page of a run's outcomes, organizer-scoped. A run id under
// the wrong organizer is sql.ErrNoRows — not an empty report, which would read as "this
// cancellation refunded nobody".
func CancellationReport(ctx context.Context, db *sql.DB, org, runID uuid.UUID, limit int, after uuid.UUID) (CancellationReportPage, error) {
	stored, found, err := lookupCancellationRun(ctx, db, org, runID)
	if err != nil {
		return CancellationReportPage{}, err
	}
	if !found {
		return CancellationReportPage{}, sql.ErrNoRows
	}
	page := CancellationReportPage{Run: stored.run, IncompleteAtEnumeration: stored.run.IncompleteAtEnumeration}

	if err := db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE outcome='refunded'),
		       count(*) FILTER (WHERE outcome='already_refunded'),
		       count(*) FILTER (WHERE outcome='failed'),
		       count(*) FILTER (WHERE outcome IS NULL)
		FROM cancellation_refund_orders WHERE organizer_id=$1 AND run_id=$2`, org, runID).
		Scan(&page.Counts.Total, &page.Counts.Refunded, &page.Counts.AlreadyRefunded,
			&page.Counts.Failed, &page.Counts.Pending); err != nil {
		return CancellationReportPage{}, err
	}
	// Rows only once the run is complete: a paginating reader must not have the
	// membership grow underneath it.
	if stored.run.Status != "completed" {
		return page, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT order_id,outcome,refund_id,refunded_quantity,refunded_amount,currency,
		       money_refunded,tickets_voided,capacity_returned,failure_code,failure_reason
		FROM cancellation_refund_orders
		WHERE organizer_id=$1 AND run_id=$2 AND order_id > $3
		ORDER BY order_id LIMIT $4`, org, runID, after, limit)
	if err != nil {
		return CancellationReportPage{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o CancellationOrderOutcome
		var code, reason sql.NullString
		if err := rows.Scan(&o.OrderID, &o.Outcome, &o.RefundID, &o.RefundedQuantity, &o.RefundedAmount,
			&o.Currency, &o.MoneyRefunded, &o.TicketsVoided, &o.CapacityReturned, &code, &reason); err != nil {
			return CancellationReportPage{}, err
		}
		o.FailureCode, o.FailureReason = code.String, reason.String
		page.Orders = append(page.Orders, o)
	}
	if err := rows.Err(); err != nil {
		return CancellationReportPage{}, err
	}
	if len(page.Orders) == limit && limit > 0 {
		page.NextAfterOrderID = uuid.NullUUID{UUID: page.Orders[len(page.Orders)-1].OrderID, Valid: true}
	}
	return page, nil
}
