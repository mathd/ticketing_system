package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
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
	ReservationID, OrderID             uuid.UUID
	OrganizerID, BuyerID               uuid.UUID
	SlotID, TicketTypeID               uuid.UUID
	Quantity                           int32
}

// OutboxMessage is one owed publication, claimed under a lease.
type OutboxMessage struct {
	EventID  uuid.UUID
	OrderID  uuid.UUID
	Subject  string
	Envelope []byte
	Attempts int
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
	eventID := events.EventID(c.OrderID)
	envelope, err := json.Marshal(events.Envelope{
		ID:         eventID,
		Type:       events.SubjectOrderCompleted,
		OccurredAt: time.Now().UTC(),
		Schema:     1,
		Data: events.OrderCompletedData{
			OrderID: c.OrderID, GuestOrderRef: candidate, OrganizerID: c.OrganizerID,
			BuyerID: c.BuyerID, SlotID: c.SlotID, TicketTypeID: c.TicketTypeID, Quantity: c.Quantity,
		},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("freeze completion envelope: %w", err)
	}
	// ON CONFLICT DO NOTHING: the deterministic event id is the PK, so a completion
	// racing its own replay owes exactly one event.
	if _, err := tx.ExecContext(ctx, `INSERT INTO completion_outbox(event_id,order_id,subject,envelope) VALUES($1,$2,$3,$4) ON CONFLICT (event_id) DO NOTHING`,
		eventID, c.OrderID, events.SubjectOrderCompleted, envelope); err != nil {
		return uuid.Nil, fmt.Errorf("owe completion event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return candidate, nil
}

// ClaimOutbox leases up to limit unpublished messages, oldest first. SKIP LOCKED lets
// concurrent drainers (and replicas) take disjoint work without blocking each other;
// the lease bounds how long a crashed drainer can strand a row.
func ClaimOutbox(ctx context.Context, db OutboxDB, limit int) ([]OutboxMessage, error) {
	rows, err := db.QueryContext(ctx, `
		UPDATE completion_outbox SET lease_until=now()+interval '30 seconds', attempts=attempts+1
		WHERE event_id IN (
			SELECT event_id FROM completion_outbox
			WHERE published_at IS NULL AND (lease_until IS NULL OR lease_until<=now())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		RETURNING event_id,order_id,subject,envelope,attempts`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.EventID, &m.OrderID, &m.Subject, &m.Envelope, &m.Attempts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkPublished retires a message. Called only after the broker acked, so an
// unacked publish stays claimable and is retried (at-least-once; the deterministic
// Nats-Msg-Id makes the duplicate harmless).
func MarkPublished(ctx context.Context, db OutboxDB, eventID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `UPDATE completion_outbox SET published_at=now(),lease_until=NULL,last_error=NULL WHERE event_id=$1`, eventID)
	return err
}

// ReleaseOutbox returns a message to the claimable set after a failed publish,
// recording why. Keeping the failure visible is what stops a poison row from being
// silently retried forever with no operator signal.
func ReleaseOutbox(ctx context.Context, db OutboxDB, eventID uuid.UUID, cause error) error {
	_, err := db.ExecContext(ctx, `UPDATE completion_outbox SET lease_until=NULL,last_error=$2 WHERE event_id=$1 AND published_at IS NULL`, eventID, cause.Error())
	return err
}
