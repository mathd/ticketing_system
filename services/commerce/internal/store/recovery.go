package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	// what makes the release restartable: 'declined', 'timeout', 'not_attempted'
	// (payments never bound a charge), or 'no_side_effect' (PSP status proved the hold
	// was released without a provider decision — TKT-115). Empty when no answer was
	// ever persisted.
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
// `payment_unknown` is claimable since TKT-115: the runner resolves it against real PSP
// status (GET /internal/psp/status), so the decision still rests on durable provider
// evidence, never on an inference about a failed transport. `reconciliation_required`
// is claimable only while UNPARKED — an unparked row is a queued compensation the
// runner drives through /internal/psp/refund; a parked one awaits a human.
func ClaimStuckOrders(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]StuckOrder, error) {
	claim := uuid.New()
	// The claimable set is chosen in a CTE under FOR UPDATE SKIP LOCKED, then joined to
	// reservations for the identifiers the re-drive needs. UPDATE ... FROM cannot follow
	// a WHERE ... IN (subquery), and the row lock has to sit on the selection, not the
	// join, or concurrent runners would contend on reservations too.
	rows, err := db.QueryContext(ctx, `
		WITH claimable AS (
			SELECT id FROM orders
			WHERE status IN ('created','payment_unknown','confirmation_pending','release_pending','reconciliation_required')
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
		WHERE id=$1 AND recovery_claim_id=$2 AND status IN ('created','payment_unknown','confirmation_pending','release_pending','reconciliation_required')`,
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
// are provider terminal answers, `not_attempted` means payments never bound a charge at
// all, and `no_side_effect` is a PSP-status-proven release without a provider decision —
// a void, an external cancellation, or a replay proving the charge was never created
// (TKT-115, ADR-032). A transport failure is not on this list and never becomes one.
func releasableOutcome(o string) bool {
	return o == "declined" || o == "timeout" || o == "not_attempted" || o == "no_side_effect"
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
//
// It establishes `release_pending` in the SAME statement as the outcome, fenced on the
// claim token, and requires exactly one affected row. All three matter:
//   - Writing the outcome without the status left the durable state unreachable: the
//     next pass saw `created` and re-ran the payments lookup it had already answered,
//     which is the un-restartable case this function exists to prevent. `release_pending`
//     is the state the runner reads to skip straight to the release.
//   - Without the claim fence a runner whose lease lapsed could overwrite the decision of
//     the successor that superseded it.
//   - Without the row check a no-op UPDATE reads as success, and the caller proceeds to
//     release a seat on the strength of an outcome that was never persisted.
func RecordTerminalOutcome(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID, outcome string) error {
	if !releasableOutcome(outcome) {
		return fmt.Errorf("terminal outcome %q does not prove absence of a side effect", outcome)
	}
	result, err := db.ExecContext(ctx, `
		UPDATE orders SET terminal_outcome=$2,status='release_pending',updated_at=now()
		WHERE id=$1 AND recovery_claim_id=$3 AND terminal_outcome IS NULL
		  AND status IN ('created','payment_unknown','release_pending','reconciliation_required')`,
		orderID, outcome, claimID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRecoveryConflict
	}
	return nil
}

// QueueForCompensation moves an order to reconciliation_required while KEEPING the
// recovery claim and staying unparked: the same pass continues into the refund, and a
// later failure backs off through ReleaseStuckOrder rather than waiting on a human.
// This is the TKT-115 successor to parking on ErrClaimGone — the compensation exists
// now, so the row is queued work, not an operator's inbox. Fenced on the claim token.
func QueueForCompensation(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID, reason string) error {
	result, err := db.ExecContext(ctx, `
		UPDATE orders
		SET status='reconciliation_required',
		    recovery_last_error=$3,
		    updated_at=now()
		WHERE id=$1 AND recovery_claim_id=$2 AND status IN ('created','payment_unknown','confirmation_pending')`,
		orderID, claimID, reason)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrRecoveryConflict
	}
	return nil
}

// MarkRefunded finishes a compensated order: captured money that could not buy its seat
// has been returned by payments (the runner calls this only after a refund 200 or
// status-proven refunded evidence — never on 409/502). The order reaches the terminal
// `refunded` status, the reservation fails, and the claim clears — atomically, fenced
// on the claim token like every recovery write.
func MarkRefunded(ctx context.Context, db *sql.DB, s StuckOrder) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE orders SET status='refunded',recovery_lease_until=NULL,recovery_claim_id=NULL,updated_at=now()
		WHERE id=$1 AND recovery_claim_id=$2 AND status IN ('created','payment_unknown','confirmation_pending','reconciliation_required')`,
		s.OrderID, s.ClaimID)
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

// RecordOrderFact writes the fact and returns its identity and time, mirroring the
// checkout path's fact write in internal/api/server.go.
//
// The id is derived from the order and fact type rather than generated, so a re-drive
// of the same order recomputes the same id and the insert collapses. Recovery re-drives
// by construction — a generated id would journal a second order.failed for the same
// order on every retry.
//
// occurred_at is read back rather than assumed: on the conflict path the stored row
// keeps the FIRST attempt's timestamp, and that is the one the journal must carry.
//
// ON CONFLICT DO UPDATE, not DO NOTHING. DO NOTHING returns no row on conflict, and
// recovering the timestamp from a second SELECT — even inside the same statement via a
// CTE — can return NOTHING at all: the statement snapshot is taken before ON CONFLICT
// waits on a concurrent uncommitted insert of the same id, so the SELECT still cannot
// see the row that insert just committed. Two runners re-driving one order is precisely
// the case this function exists to survive. DO UPDATE locks the conflicting row and
// returns it, which is why the no-op SET is not pointless.
func RecordOrderFact(ctx context.Context, db OutboxDB, s StuckOrder, factType string) (uuid.UUID, time.Time, error) {
	factID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(s.OrderID.String()+":"+factType))
	rows, err := db.QueryContext(ctx, `
		INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (fact_id) DO UPDATE SET fact_id=order_facts.fact_id
		RETURNING occurred_at`,
		factID, s.OrderID, s.OrganizerID, s.BuyerID, factType, s.Amount, s.Currency)
	if err != nil {
		return uuid.Nil, time.Time{}, err
	}
	defer func() { _ = rows.Close() }()
	var occurred time.Time
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return uuid.Nil, time.Time{}, err
		}
		return uuid.Nil, time.Time{}, fmt.Errorf("order fact %s returned no row", factID)
	}
	if err := rows.Scan(&occurred); err != nil {
		return uuid.Nil, time.Time{}, err
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, time.Time{}, err
	}
	return factID, occurred, nil
}

// AbandonRecoveryClaim hands back a claim whose order was never driven — a shutdown
// caught the pass mid-batch. It refunds the attempt ClaimStuckOrders charged up front.
//
// The refund is the point, and it is why this is not ClearRecoveryClaim: attempts are
// incremented at claim time, so an order abandoned by a rolling restart has paid for a
// re-drive it never received. Left uncounted, a few restarts would park a perfectly
// healthy order at MaxRecoveryAttempts without a single actual attempt having been made.
func AbandonRecoveryClaim(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE orders SET recovery_lease_until=NULL,recovery_claim_id=NULL,
		    recovery_attempts=GREATEST(recovery_attempts-1,0)
		WHERE id=$1 AND recovery_claim_id=$2`, orderID, claimID)
	return err
}

// ClearRecoveryClaim drops the lease after a successful re-drive, so the row stops
// being re-claimed once it has reached a terminal state by other means.
func ClearRecoveryClaim(ctx context.Context, db OutboxDB, orderID, claimID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE orders SET recovery_lease_until=NULL,recovery_claim_id=NULL,recovery_last_error=NULL
		WHERE id=$1 AND recovery_claim_id=$2`, orderID, claimID)
	return err
}

// --- The operator path out of a parked order (TKT-146) ---------------------------
//
// ReleaseStuckOrder parks a row once its attempts are exhausted and ClaimStuckOrders
// excludes parked rows, so nothing in this service revisited one: ADR-016 §Decision 1
// says as much ("nothing revisits a parked order") and its §Consequences names the
// population that hurts most — `confirmation_pending` whose confirm is terminally
// impossible parks as `reconciliation_required` holding CAPTURED money. This makes an
// operator the thing that revisits it. Deliberately an operator and not a scheduler:
// ADR-016 refuses to re-drive on a timer something that already failed its budget.

// ErrRecoveryOrderNotFound, ErrRecoveryOrderNotParked and ErrRecoveryOrderStatusNotClaimable
// are the three preconditions of UnparkOrder, kept DISTINCT rather than collapsed into one
// "cannot unpark". An operator acting during an incident needs to know which of "I typed the
// wrong id", "someone already did this" and "this row is in a state the runner cannot drive"
// they are looking at; the three call for three different next actions.
var (
	ErrRecoveryOrderNotFound           = errors.New("order not found")
	ErrRecoveryOrderNotParked          = errors.New("order is not parked")
	ErrRecoveryOrderStatusNotClaimable = errors.New("order status is not one the recovery runner can claim")
)

// claimableRecoveryStatuses is the status set ClaimStuckOrders selects on. Unparking a row
// outside it would clear the marker and achieve nothing, because the runner would still not
// select the row — a silent no-op wearing a success message.
//
// It duplicates the literal list in ClaimStuckOrders' SQL, which is a real cost. The
// alternative — building that query's IN clause from this slice — was rejected: the claim
// query is the hot path of the recovery runner and its SQL is meant to be read as one piece.
// TestUnparkedOrderIsClaimedAgainByTheRunner is what keeps the two honest, because it drives
// a real unpark through the real claim query.
var claimableRecoveryStatuses = map[string]bool{
	"created":                 true,
	"payment_unknown":         true,
	"confirmation_pending":    true,
	"release_pending":         true,
	"reconciliation_required": true,
}

// ParkedOrder is one row of the operator listing: everything needed to decide whether an
// order should be unparked, and nothing that would make the listing a buyer-facing read.
type ParkedOrder struct {
	OrderID   uuid.UUID
	Status    string
	Attempts  int
	ParkedAt  time.Time
	LastError sql.NullString
	// TerminalOutcome is the answer recorded before a release was attempted, empty when
	// none ever was. It is reported because it is the single most useful thing for
	// deciding what a parked order needs — never as an input to a decision this command
	// makes (ADR-016 §Decision 2 keeps that reasoning in the runner).
	TerminalOutcome sql.NullString
}

// ListParkedOrders reports every order excluded from recovery by a park marker.
//
// Takes OutboxDB rather than *sql.DB: it is a read and needs nothing wider. Unbounded and
// unpaged on purpose — the parked population is bounded by attempt exhaustion, so a listing
// long enough to need paging is itself the finding, and truncating it would hide exactly
// that. Ordered oldest-first because the oldest park is the one that has been waiting.
func ListParkedOrders(ctx context.Context, db OutboxDB) ([]ParkedOrder, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, status, recovery_attempts, recovery_parked_at, recovery_last_error, terminal_outcome
		FROM orders
		WHERE recovery_parked_at IS NOT NULL
		ORDER BY recovery_parked_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list parked orders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ParkedOrder
	for rows.Next() {
		var p ParkedOrder
		if err := rows.Scan(&p.OrderID, &p.Status, &p.Attempts, &p.ParkedAt,
			&p.LastError, &p.TerminalOutcome); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UnparkOrder returns one parked order to the claimable set and records why.
//
// It takes *sql.DB, not the OutboxDB every neighbour here takes, because it needs a
// transaction: the reset and its evidence row must land together or not at all. Widening
// OutboxDB to carry BeginTx was the alternative and was rejected — that interface is narrow
// so the outbox drainer can be exercised without a live PostgreSQL, and handing it a
// transaction surface for one caller's benefit gives that up. Do not "tidy" this signature
// back to match its neighbours.
//
// What it does NOT do is the load-bearing half — stated precisely, because the loose version
// of this sentence is wrong. It does not touch `status`, does not read or write
// `terminal_outcome`, does not call the PSP, and writes no money column. The decision about
// captured funds stays where ADR-016 §Decision 2 and ADR-032 put it: in the runner, on durable
// provider evidence.
//
// But it is NOT true that an unpark "decides nothing about money" (ai-review F2). A parked
// `reconciliation_required` order can hold captured funds, and clearing its marker is exactly
// what re-admits it to `resolveReconciliation`, which refunds on a `captured` PSP status and
// only afterwards discovers, via `inventory.Release`, whether the claim was already confirmed —
// re-parking as "refunded money against a confirmed claim" when it was. That ordering is the
// runner's, is reachable on `main` for any UNPARKED `reconciliation_required` row (migration
// 0005's backfill created a population of them), and is not this function's to change. What is
// this function's is to not pretend otherwise: an unpark is a decision to let the runner decide
// again, and an operator who has not established what the order actually needs should not make
// it. `list-parked` prints `recovery_last_error` first for that reason.
//
// It also leaves `updated_at` alone, which is not an omission. ClaimStuckOrders requires
// `updated_at < now() - interval '2 minutes'` to keep recovery off orders belonging to a live
// checkout; refreshing it here would exclude the order for two minutes for a reason unrelated
// to parking — the trap RecordTerminalOutcome already sprang.
//
// Adversary (ADR-021): the evidence row is about an HONEST operator. Anyone with commerce's
// database credentials can write, alter or delete these rows and nothing here would notice.
func UnparkOrder(ctx context.Context, db *sql.DB, orderID uuid.UUID, reason string) error {
	// Refused before the transaction opens: the reason is the only thing an unpark records
	// that could not be derived from the row itself, and a blank one leaves evidence that
	// looks complete and says nothing.
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("a reason is required: it is the only part of the record a later reader cannot reconstruct")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("unpark order: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// FOR UPDATE, so a second operator racing this one blocks here and then observes the
	// cleared marker rather than writing a second evidence row for one intervention.
	var (
		status   string
		parkedAt sql.NullTime
		attempts int
		lastErr  sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT status, recovery_parked_at, recovery_attempts, recovery_last_error
		FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&status, &parkedAt, &attempts, &lastErr)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrRecoveryOrderNotFound, orderID)
	}
	if err != nil {
		return fmt.Errorf("read order %s: %w", orderID, err)
	}
	// The three predicates, in this order and each reported separately. Order matters:
	// checking status first would report "not claimable" for an order that is simply not
	// parked, which sends an operator looking for a state problem that does not exist.
	if !parkedAt.Valid {
		return fmt.Errorf("%w: %s", ErrRecoveryOrderNotParked, orderID)
	}
	if !claimableRecoveryStatuses[status] {
		// No code path in this service produces a parked row with a status outside the
		// claimable set — ReleaseStuckOrder parks without touching status, and
		// ParkForReconciliation sets one that is in the set. This is a fail-closed guard
		// against a row someone wrote by hand, and clearing its marker would look like a
		// resolution while leaving the order just as unreachable.
		return fmt.Errorf("%w: %s is %q", ErrRecoveryOrderStatusNotClaimable, orderID, status)
	}

	// The reset mirrors migration 0005_psp_recovery.sql, which re-opened a bulk population
	// the same way and wrote down why: recovery_last_error is RETAINED as operator context,
	// and attempts reset so a newly-actionable row gets a full retry budget. That reset is
	// not cosmetic — ReleaseStuckOrder re-parks on `recovery_attempts>=MaxRecoveryAttempts`,
	// so clearing the marker alone buys exactly one re-drive before the row parks again.
	result, err := tx.ExecContext(ctx, `
		UPDATE orders
		SET recovery_parked_at=NULL,
		    recovery_attempts=0,
		    recovery_next_attempt_at=now(),
		    recovery_claim_id=NULL,
		    recovery_lease_until=NULL
		WHERE id=$1 AND recovery_parked_at IS NOT NULL`, orderID)
	if err != nil {
		return fmt.Errorf("unpark order %s: %w", orderID, err)
	}
	// Belt and braces behind the row lock: a zero-row update after a locked read that saw a
	// marker would mean the row changed under a lock that should have prevented it, and
	// committing the evidence row anyway would record an intervention that did not happen.
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrRecoveryConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_recovery_unparks
		  (id, order_id, reason, pre_recovery_attempts, pre_recovery_parked_at, pre_recovery_last_error)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		uuid.New(), orderID, reason, attempts, parkedAt.Time, lastErr); err != nil {
		return fmt.Errorf("record unpark of %s: %w", orderID, err)
	}
	return tx.Commit()
}
