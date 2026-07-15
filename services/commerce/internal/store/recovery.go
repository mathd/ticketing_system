package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StuckOrder is an order parked in a non-terminal state with nothing driving it.
// Everything the recovery runner needs to decide comes from these durable columns —
// never from an inference about what a failed transport did (ADR-016 §Decision 2).
type StuckOrder struct {
	OrderID        uuid.UUID
	ReservationID  uuid.UUID
	OrganizerID    uuid.UUID
	HoldID         uuid.UUID
	BuyerID        uuid.UUID
	SlotID         uuid.UUID
	TicketTypeID   uuid.UUID
	Quantity       int32
	Amount         int64
	Currency       string
	Status         string
	IdempotencyKey string
	// TerminalOutcome is the result recorded before a release was attempted, and is
	// what makes the release restartable: 'declined', 'timeout', or 'not_attempted'
	// (payments never bound a charge). Empty when no answer was ever persisted.
	TerminalOutcome string
	ClaimID         uuid.UUID
	Attempts        int
}

// Completion projects the order onto the completion payload.
func (s StuckOrder) Completion() Completion {
	return Completion{
		ReservationID: s.ReservationID, OrderID: s.OrderID, OrganizerID: s.OrganizerID,
		BuyerID: s.BuyerID, SlotID: s.SlotID, TicketTypeID: s.TicketTypeID, Quantity: s.Quantity,
	}
}

// MaxRecoveryAttempts bounds re-drives before an order is parked for a human. Claiming
// is oldest-first, so an unbounded retry of one unrecoverable order would starve the
// rest — the same starvation the outbox guards against with dead-lettering.
const MaxRecoveryAttempts = 10

// ErrRecoveryConflict reports that an order moved on between claim and transition.
var ErrRecoveryConflict = errors.New("order changed state during recovery")

// ClaimStuckOrders leases orders that need re-driving, oldest first.
//
// `created` is included deliberately: it is the default row value and covers several
// materially different situations (payment never attempted; inventory finalized before
// a crash; captured before a crash; declined before a crash; released with the response
// lost). The row alone cannot distinguish them, so the runner resolves it against
// payments (ADR-016 §Decision 3) rather than guessing.
//
// `payment_unknown` is deliberately EXCLUDED: resolving it needs real-PSP status, which
// is deferred to TKT-56. Claiming it here would let the runner make exactly the
// unfounded inference this design exists to prevent.
func ClaimStuckOrders(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]StuckOrder, error) {
	claim := uuid.New()
	// The claimable set is chosen in a CTE under FOR UPDATE SKIP LOCKED, then joined to
	// reservations for the identifiers the re-drive needs. UPDATE ... FROM cannot follow
	// a WHERE ... IN (subquery), and the row lock has to sit on the selection, not the
	// join, or concurrent runners would contend on reservations too.
	rows, err := db.QueryContext(ctx, `
		WITH claimable AS (
			SELECT id FROM orders
			WHERE status IN ('created','confirmation_pending','release_pending')
			  AND recovery_parked_at IS NULL
			  AND recovery_next_attempt_at<=now()
			  AND (recovery_lease_until IS NULL OR recovery_lease_until<=now())
			  -- Grace period: a checkout in flight owns its own order. Re-driving it
			  -- underneath the request would race the coordinator for no reason.
			  AND updated_at < now() - interval '2 minutes'
			ORDER BY recovery_next_attempt_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		), claimed AS (
			UPDATE orders o
			SET recovery_lease_until=now()+make_interval(secs => $2),
			    recovery_claim_id=$3,
			    recovery_attempts=o.recovery_attempts+1
			WHERE o.id IN (SELECT id FROM claimable)
			RETURNING o.id, o.reservation_id, o.status, o.idempotency_key,
			          o.terminal_outcome, o.recovery_claim_id, o.recovery_attempts
		)
		SELECT c.id, c.reservation_id, r.organizer_id, r.hold_id, r.buyer_id, r.slot_id,
		       r.ticket_type_id, r.quantity, r.total_amount, r.currency, c.status,
		       c.idempotency_key, coalesce(c.terminal_outcome,''), c.recovery_claim_id,
		       c.recovery_attempts
		FROM claimed c JOIN reservations r ON r.id = c.reservation_id`,
		limit, lease.Seconds(), claim)
	if err != nil {
		return nil, fmt.Errorf("claim stuck orders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StuckOrder
	for rows.Next() {
		var s StuckOrder
		if err := rows.Scan(&s.OrderID, &s.ReservationID, &s.OrganizerID, &s.HoldID, &s.BuyerID,
			&s.SlotID, &s.TicketTypeID, &s.Quantity, &s.Amount, &s.Currency, &s.Status,
			&s.IdempotencyKey, &s.TerminalOutcome, &s.ClaimID, &s.Attempts); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReleaseStuckOrder returns an order to the claimable set after a failed re-drive,
// backing off and parking it once attempts are exhausted. Conditional on the claim id,
// so a claimant whose lease lapsed mid-drive cannot disturb its successor.
func ReleaseStuckOrder(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID, cause error) error {
	_, err := db.ExecContext(ctx, `
		UPDATE orders
		SET recovery_lease_until=NULL,
		    recovery_claim_id=NULL,
		    recovery_last_error=$3,
		    recovery_next_attempt_at=now() + least(make_interval(secs => power(2, least(recovery_attempts, 8))::double precision), interval '5 minutes'),
		    recovery_parked_at=CASE WHEN recovery_attempts>=$4 THEN now() ELSE NULL END
		WHERE id=$1 AND recovery_claim_id=$2`,
		orderID, claimID, cause.Error(), MaxRecoveryAttempts)
	return err
}

// ParkForReconciliation moves an order to a terminal-for-now state that only a human or
// a later compensation pass can clear. Used when the money is known captured but the
// claim can never be confirmed: completing would sell a seat that is gone, and releasing
// would strand the buyer's money. Refunding needs the PSP port (TKT-56), so the order
// waits, visibly, rather than being silently resolved either way.
func ParkForReconciliation(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID, reason string) error {
	result, err := db.ExecContext(ctx, `
		UPDATE orders
		SET status='reconciliation_required',
		    recovery_lease_until=NULL,
		    recovery_claim_id=NULL,
		    recovery_parked_at=now(),
		    recovery_last_error=$3,
		    updated_at=now()
		WHERE id=$1 AND recovery_claim_id=$2 AND status IN ('created','confirmation_pending','release_pending')`,
		orderID, claimID, reason)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrRecoveryConflict
	}
	return nil
}

// releasableOutcome reports whether an outcome PROVES no side effect, which is the only
// basis on which a claim may be released (ADR-016 §Decision 2): `declined` and `timeout`
// are the fake PSP's terminal answers, and `not_attempted` means payments never bound a
// charge at all. A transport failure is not on this list and never becomes one.
func releasableOutcome(o string) bool {
	return o == "declined" || o == "timeout" || o == "not_attempted"
}

// TerminalStatus maps a releasable outcome to the order's terminal status. Both
// `timeout` and `not_attempted` present to the buyer as a timed-out checkout — from
// their side nothing was charged either way — but the outcome column keeps the two
// distinguishable for anyone auditing what actually happened.
func TerminalStatus(outcome string) string {
	if outcome == "declined" {
		return "declined"
	}
	return "timeout"
}

// RecordTerminalOutcome persists the terminal answer BEFORE a release is attempted, so
// a later pass knows what it is completing (ADR-016 §Decision 2). Without it the release
// is un-restartable: a crash after releasing but before marking the order leaves no
// evidence the outcome was ever known.
func RecordTerminalOutcome(ctx context.Context, db OutboxDB, orderID uuid.UUID, outcome string) error {
	if !releasableOutcome(outcome) {
		return fmt.Errorf("terminal outcome %q does not prove absence of a side effect", outcome)
	}
	_, err := db.ExecContext(ctx, `
		UPDATE orders SET terminal_outcome=$2,updated_at=now()
		WHERE id=$1 AND terminal_outcome IS NULL AND status IN ('created','release_pending')`,
		orderID, outcome)
	return err
}

// MarkReleased finishes a release-driven failure: the claim is gone, so the order and
// its reservation reach their terminal state. Conditional on the claim id.
func MarkReleased(ctx context.Context, db *sql.DB, s StuckOrder) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE orders SET status=$2,recovery_lease_until=NULL,recovery_claim_id=NULL,updated_at=now()
		WHERE id=$1 AND recovery_claim_id=$3 AND status IN ('created','release_pending')`,
		s.OrderID, TerminalStatus(s.TerminalOutcome), s.ClaimID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrRecoveryConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reservations SET status='failed' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, s.ReservationID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearRecoveryClaim drops the lease after a successful re-drive, so the row stops
// being re-claimed once it has reached a terminal state by other means.
func ClearRecoveryClaim(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE orders SET recovery_lease_until=NULL,recovery_claim_id=NULL,recovery_last_error=NULL
		WHERE id=$1 AND recovery_claim_id=$2`, orderID, claimID)
	return err
}
