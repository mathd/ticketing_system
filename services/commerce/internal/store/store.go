package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
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

// CompleteOrder atomically establishes the public guest reference. Concurrent
// callers serialize on the order row and all receive the persisted canonical
// value; a caller-generated candidate is never returned unless it commits.
func CompleteOrder(ctx context.Context, db *sql.DB, reservationID, orderID, candidate uuid.UUID) (uuid.UUID, error) {
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
	err = tx.QueryRowContext(ctx, `SELECT status,guest_order_ref FROM orders WHERE id=$1 AND reservation_id=$2 FOR UPDATE`, orderID, reservationID).Scan(&status, &existing)
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

	result, err := tx.ExecContext(ctx, `UPDATE reservations SET status='completed' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, reservationID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("complete reservation: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return uuid.Nil, fmt.Errorf("complete reservation rows: %w", err)
		}
		return uuid.Nil, fmt.Errorf("%w: reservation rows affected %d", ErrCompletionConflict, rows)
	}
	result, err = tx.ExecContext(ctx, `UPDATE orders SET status='completed',guest_order_ref=$2,updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, orderID, candidate)
	if err != nil {
		return uuid.Nil, fmt.Errorf("complete order: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return uuid.Nil, fmt.Errorf("complete order rows: %w", err)
		}
		return uuid.Nil, fmt.Errorf("%w: order rows affected %d", ErrCompletionConflict, rows)
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return candidate, nil
}
