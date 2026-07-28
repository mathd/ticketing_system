package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"ticketing/services/commerce/internal/events"
)

var ErrCompletionConflict = errors.New("order cannot be completed from its current state")

//go:embed all:migrations
var migrationsFS embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	f, e := fs.Sub(migrationsFS, "migrations")
	if e != nil {
		return e
	}
	p, e := goose.NewProvider(goose.DialectPostgres, db, f)
	if e != nil {
		return e
	}
	_, e = p.Up(ctx)
	return e
}

// Completion carries everything the order-completed envelope is built from. The
// envelope needs reservation-scoped identifiers, so completion cannot be expressed
// by order id alone.
type Completion struct {
	ReservationID, OrderID uuid.UUID
	OrganizerID, BuyerID   uuid.UUID
	SlotID, TicketTypeID   uuid.UUID
	Quantity               int32
}

// OutboxMessage is one owed publication, held under a lease. ClaimID identifies the
// holder: retirement and release are conditional on it, so a claimant whose lease
// lapsed mid-publish cannot disturb the drainer that superseded it.
type OutboxMessage struct {
	EventID  uuid.UUID
	OrderID  uuid.UUID
	Subject  string
	Envelope []byte
	Attempts int
	ClaimID  uuid.UUID
}

// MaxOutboxAttempts bounds retries before a row is quarantined. Claiming is
// oldest-first, so an unbounded retry of a permanently-failing row would starve every
// newer order behind it.
const MaxOutboxAttempts = 10

// completionEnvelope builds the canonical ADR-009 envelope for a completed order.
// Completion and backfill share it so the frozen bytes have exactly one definition
// -- and since TKT-126 that definition lives in the events package, which is where
// the subject and its payload type already live. This function is now only the
// Completion-to-payload mapping; it no longer re-declares the envelope.
func completionEnvelope(c Completion, guestRef uuid.UUID, occurredAt time.Time) (uuid.UUID, []byte, error) {
	id := events.EventID(c.OrderID)
	body, err := events.OrderCompletedEnvelope(id, events.OrderCompletedData{
		OrderID: c.OrderID, GuestOrderRef: guestRef, OrganizerID: c.OrganizerID,
		BuyerID: c.BuyerID, SlotID: c.SlotID, TicketTypeID: c.TicketTypeID, Quantity: c.Quantity,
	}, occurredAt.UTC())
	return id, body, err
}

// OutboxDB is the database surface the outbox functions need; *sql.DB satisfies it.
// Narrowed so the drainer can be exercised without a live PostgreSQL.
type OutboxDB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// CompleteOrder atomically establishes the public guest reference and owes the
// completion event. Concurrent callers serialize on the order row and all receive
// the persisted canonical value; a caller-generated candidate is never returned
// unless it commits.
//
// The outbox insert shares this transaction (ADR-016 §Decision 6): an order cannot
// become `completed` without owing its event, so a crash before publication leaves a
// claimable row rather than a paid order with no ticket. Publication happens after
// the commit, never inside it.
func CompleteOrder(ctx context.Context, db *sql.DB, c Completion, candidate uuid.UUID) (uuid.UUID, error) {
	if candidate == uuid.Nil {
		return uuid.Nil, errors.New("guest order reference is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	var existing uuid.NullUUID
	err = tx.QueryRowContext(ctx, `SELECT status,guest_order_ref FROM orders WHERE id=$1 AND reservation_id=$2 FOR UPDATE`, c.OrderID, c.ReservationID).Scan(&status, &existing)
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock order completion: %w", err)
	}
	if status == "completed" {
		if !existing.Valid || existing.UUID == uuid.Nil {
			return uuid.Nil, errors.New("completed order has no guest reference")
		}
		if err := tx.Commit(); err != nil {
			return uuid.Nil, err
		}
		return existing.UUID, nil
	}
	if status != "created" && status != "payment_unknown" && status != "confirmation_pending" {
		return uuid.Nil, fmt.Errorf("%w: order status %q", ErrCompletionConflict, status)
	}

	result, err := tx.ExecContext(ctx, `UPDATE reservations SET status='completed' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, c.ReservationID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("complete reservation: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return uuid.Nil, fmt.Errorf("complete reservation rows: %w", err)
		}
		return uuid.Nil, fmt.Errorf("%w: reservation rows affected %d", ErrCompletionConflict, rows)
	}
	result, err = tx.ExecContext(ctx, `UPDATE orders SET status='completed',guest_order_ref=$2,updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, c.OrderID, candidate)
	if err != nil {
		return uuid.Nil, fmt.Errorf("complete order: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return uuid.Nil, fmt.Errorf("complete order rows: %w", err)
		}
		return uuid.Nil, fmt.Errorf("%w: order rows affected %d", ErrCompletionConflict, rows)
	}

	// Freeze the envelope now. Rebuilding it per publish attempt would make the
	// payload a function of retry timing while the deterministic id stays fixed.
	//
	// OccurredAt is the completion *decision* time, taken just before this commit. It
	// is deliberately not the durable commit timestamp, which cannot be known before
	// committing; the gap is sub-millisecond and the value never changes afterwards.
	eventID, envelope, err := completionEnvelope(c, candidate, time.Now())
	if err != nil {
		return uuid.Nil, fmt.Errorf("freeze completion envelope: %w", err)
	}
	// The event id is derived from the order id, so a conflict means this order's own
	// row already exists — a completion racing its replay. Verify that rather than
	// assuming it: DO NOTHING on a row belonging to a *different* order would let this
	// order complete owing nothing, which is the bug the outbox exists to prevent.
	var owner uuid.UUID
	err = tx.QueryRowContext(ctx, `
		INSERT INTO completion_outbox(event_id,order_id,subject,envelope) VALUES($1,$2,$3,$4)
		ON CONFLICT (event_id) DO UPDATE SET event_id=completion_outbox.event_id
		RETURNING order_id`,
		eventID, c.OrderID, events.SubjectOrderCompleted, envelope).Scan(&owner)
	if err != nil {
		return uuid.Nil, fmt.Errorf("owe completion event: %w", err)
	}
	if owner != c.OrderID {
		return uuid.Nil, fmt.Errorf("completion event %s is owned by order %s, not %s", eventID, owner, c.OrderID)
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return candidate, nil
}

// BackfillCompletionOutbox owes an event for every order that reached `completed`
// before the outbox existed. CompleteOrder's already-completed short-circuit never
// inserts for those rows, so without this the paid-but-no-ticket window stays open
// forever for exactly the historical orders the outbox is meant to protect.
//
// Runs once per process, behind the outbox drainer (TKT-71) — not on the startup
// path. It is data repair, not schema (ADR-022), idempotent (ON CONFLICT DO NOTHING
// on the derived event id), and matches nothing on every boot after the first. It
// still sequentially scans orders per boot and buffers every match in memory, but
// as background work: a slow or failing pass is a logged error on a worker, never
// a readiness or deadline failure. A restarted container re-runs it (per-boot, not
// per-deploy); the next boot is also the retry path when a pass errors.
//
// Backfilled events are republished once. That is safe and deliberate: publication is
// at-least-once by design and access dedupes issuance transactionally on
// consumed_events(event_id), so redelivering an already-issued order is a no-op.
// Redelivery is strictly better than leaving a paid order with no ticket.
func BackfillCompletionOutbox(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.id, o.guest_order_ref, o.updated_at,
		       r.id, r.organizer_id, r.buyer_id, r.slot_id, r.ticket_type_id, r.quantity
		FROM orders o
		JOIN reservations r ON r.id = o.reservation_id
		LEFT JOIN completion_outbox x ON x.order_id = o.id
		WHERE o.status='completed' AND o.guest_order_ref IS NOT NULL AND x.order_id IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("scan orders needing backfill: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type pending struct {
		c        Completion
		guestRef uuid.UUID
		at       time.Time
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.c.OrderID, &p.guestRef, &p.at, &p.c.ReservationID,
			&p.c.OrganizerID, &p.c.BuyerID, &p.c.SlotID, &p.c.TicketTypeID, &p.c.Quantity); err != nil {
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var owed int
	for _, p := range todo {
		// occurred_at is when the order actually completed, not when this ran: the
		// event describes the completion, not the backfill.
		eventID, envelope, err := completionEnvelope(p.c, p.guestRef, p.at)
		if err != nil {
			return owed, fmt.Errorf("rebuild envelope for %s: %w", p.c.OrderID, err)
		}
		result, err := db.ExecContext(ctx, `
			INSERT INTO completion_outbox(event_id,order_id,subject,envelope,created_at)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
			eventID, p.c.OrderID, events.SubjectOrderCompleted, envelope, p.at)
		if err != nil {
			return owed, fmt.Errorf("backfill %s: %w", p.c.OrderID, err)
		}
		if n, _ := result.RowsAffected(); n == 1 {
			owed++
		}
	}
	return owed, nil
}

// ClaimOutbox leases up to limit claimable messages, oldest first, stamping each with
// a fresh claim id. SKIP LOCKED lets concurrent drainers and replicas take disjoint
// work without blocking each other.
//
// `lease` must exceed the worst-case publish duration the caller tolerates. When it
// lapses another drainer may take the row, and both may publish — publication is
// at-least-once, so that is a duplicate rather than a corruption, but the claim id is
// what stops the superseded claimant from *also* mutating the new claimant's row.
func ClaimOutbox(ctx context.Context, db OutboxDB, limit int, lease time.Duration) ([]OutboxMessage, error) {
	claim := uuid.New()
	rows, err := db.QueryContext(ctx, `
		UPDATE completion_outbox
		SET lease_until=now()+make_interval(secs => $2), claim_id=$3, attempts=attempts+1
		WHERE event_id IN (
			SELECT event_id FROM completion_outbox
			WHERE published_at IS NULL
			  AND dead_lettered_at IS NULL
			  AND next_attempt_at<=now()
			  AND (lease_until IS NULL OR lease_until<=now())
			ORDER BY next_attempt_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		RETURNING event_id,order_id,subject,envelope,attempts,claim_id`,
		limit, lease.Seconds(), claim)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.EventID, &m.OrderID, &m.Subject, &m.Envelope, &m.Attempts, &m.ClaimID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkPublished retires a message, but only if the caller still holds the claim.
// Called after the broker acked; an unacked publish stays claimable and is retried.
//
// Conditional on claim_id: a claimant whose lease expired mid-publish must not retire
// a row another drainer has since claimed, or it would mask a publication that never
// happened. Returns false when the claim was lost.
func MarkPublished(ctx context.Context, db OutboxDB, eventID, claimID uuid.UUID) (bool, error) {
	result, err := db.ExecContext(ctx, `
		UPDATE completion_outbox SET published_at=now(),lease_until=NULL,claim_id=NULL,last_error=NULL
		WHERE event_id=$1 AND claim_id=$2 AND published_at IS NULL`, eventID, claimID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// FrozenEnvelope returns an order's committed envelope bytes, or ok=false if the event
// is already published or absent. The checkout path uses it to publish the same bytes
// the drainer would, so inline and background delivery cannot disagree on the payload.
func FrozenEnvelope(ctx context.Context, db OutboxDB, orderID uuid.UUID) (uuid.UUID, string, []byte, bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT event_id,subject,envelope FROM completion_outbox
		WHERE order_id=$1 AND published_at IS NULL`, orderID)
	if err != nil {
		return uuid.Nil, "", nil, false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return uuid.Nil, "", nil, false, rows.Err()
	}
	var id uuid.UUID
	var subject string
	var envelope []byte
	if err := rows.Scan(&id, &subject, &envelope); err != nil {
		return uuid.Nil, "", nil, false, err
	}
	return id, subject, envelope, true, rows.Err()
}

// MarkPublishedByOrder retires an order's owed event without holding a claim. Used by
// the inline publish on the checkout path, which is not a drainer. It is safe because
// it only retires a row nobody currently holds: a leased row belongs to a drainer that
// is publishing it, and letting the request path retire that would mask its outcome.
func MarkPublishedByOrder(ctx context.Context, db OutboxDB, orderID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE completion_outbox SET published_at=now(),lease_until=NULL,claim_id=NULL,last_error=NULL
		WHERE order_id=$1 AND published_at IS NULL AND (lease_until IS NULL OR lease_until<=now())`, orderID)
	return err
}

// ReleaseOutbox returns a message to the claimable set after a failed publish, backing
// off before it may be retried and quarantining it once attempts are exhausted.
//
// Conditional on claim_id for the same reason as MarkPublished: a stale claimant must
// not clear the lease of the drainer that superseded it, which would let a third
// drainer publish concurrently with the second.
//
// The backoff is what keeps a poison row from starving the queue: claiming is
// oldest-first, so an immediately-retryable failure at the head is re-selected every
// pass forever. After MaxOutboxAttempts the row is dead-lettered — permanently
// excluded from claiming, still visible to an operator via last_error.
func ReleaseOutbox(ctx context.Context, db OutboxDB, eventID, claimID uuid.UUID, cause error) error {
	_, err := db.ExecContext(ctx, `
		UPDATE completion_outbox
		SET lease_until=NULL,
		    claim_id=NULL,
		    last_error=$3,
		    -- Exponential, capped: 2^attempts seconds up to 5 minutes.
		    next_attempt_at=now() + least(make_interval(secs => power(2, least(attempts, 8))::double precision), interval '5 minutes'),
		    dead_lettered_at=CASE WHEN attempts>=$4 THEN now() ELSE NULL END
		WHERE event_id=$1 AND claim_id=$2 AND published_at IS NULL`,
		eventID, claimID, cause.Error(), MaxOutboxAttempts)
	return err
}
