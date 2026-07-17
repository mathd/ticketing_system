package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// Pool offering state (TKT-75 / US-012): inventory's mirror of the catalog slot's offer state.
// Archival is terminal; closure is a reversible attribute versioned by the catalog's monotonic
// closure counter. Both mutations follow the ADR-010 lock order (pool row first) and the
// consumed_events dedupe every catalog consumer uses. State only gates NEW holds — live claims
// keep their lifecycle untouched (spike TKT-50 §Case 3: forward-only).

var (
	ErrSlotArchived = errors.New("slot archived")
	ErrSlotClosed   = errors.New("slot closed")
)

// offeringStatus collapses the two axes for read models: archived wins over closed.
func offeringStatus(lifecycle, closure string) string {
	if lifecycle == "archived" {
		return "archived"
	}
	if closure == "closed" {
		return "closed"
	}
	return "open"
}

// guardOffering is the shared new-hold gate, called with the pool row already locked.
func guardOffering(lifecycle, closure string) error {
	switch offeringStatus(lifecycle, closure) {
	case "archived":
		return ErrSlotArchived
	case "closed":
		return ErrSlotClosed
	}
	return nil
}

// lockPoolOffering locks the pool row (ADR-010) and returns its offering axes.
// ErrNotFound is returned BEFORE any consumed_events write so the caller's event
// stays unconsumed and can be redelivered once the pool is provisioned.
func lockPoolOffering(ctx context.Context, tx *sql.Tx, pool uuid.UUID) (lifecycle, closure string, version int32, err error) {
	err = tx.QueryRowContext(ctx, `SELECT lifecycle_status,closure_status,closure_version FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool).
		Scan(&lifecycle, &closure, &version)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// consumeEvent inserts the event into the dedupe registry; done=false means this
// delivery is a replay and the mutation must not re-apply.
func consumeEvent(ctx context.Context, tx *sql.Tx, eventID uuid.UUID) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id) VALUES($1) ON CONFLICT DO NOTHING`, eventID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ApplyArchive marks the pool archived (terminal). Replays and events for
// already-archived pools commit as no-ops; a missing pool is ErrNotFound and
// leaves the event unconsumed.
func (p *Postgres) ApplyArchive(ctx context.Context, eventID, pool uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, _, err = lockPoolOffering(ctx, tx, pool); err != nil {
		return err
	}
	fresh, err := consumeEvent(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if !fresh {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET lifecycle_status='archived',updated_at=now() WHERE slot_id=$1`, pool); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyClosure applies a closed/reopened transition at the given catalog closure
// version. Stale versions (a delayed closed(v1) after reopened(v2)) consume their
// event but change nothing — the version counter is the ordering authority.
func (p *Postgres) ApplyClosure(ctx context.Context, eventID, pool uuid.UUID, closed bool, version int32) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, _, current, err := lockPoolOffering(ctx, tx, pool)
	if err != nil {
		return err
	}
	fresh, err := consumeEvent(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if !fresh || version <= current {
		return tx.Commit()
	}
	status := "open"
	if closed {
		status = "closed"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET closure_status=$1,closure_version=$2,updated_at=now() WHERE slot_id=$3`, status, version, pool); err != nil {
		return err
	}
	return tx.Commit()
}
