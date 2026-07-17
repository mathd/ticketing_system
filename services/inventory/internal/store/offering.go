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
func lockPoolOffering(ctx context.Context, tx *sql.Tx, pool uuid.UUID) (lifecycle, closure string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT lifecycle_status,closure_status FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool).
		Scan(&lifecycle, &closure)
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
	if _, _, err = lockPoolOffering(ctx, tx, pool); err != nil {
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

// ApplyClosure applies a closed/reopened transition for one performance at that
// performance's monotonic catalog closure version. Versions are ordered PER SLOT —
// grouped festival days share a pool but never a counter — and the pool's
// closure_status is derived under the lock: any closed member closes the pool
// (owner decision at Gate 2; day-level offer state is TKT-14's). A stale version
// (a delayed closed(v1) after that slot's reopened(v2)) consumes its event but
// changes nothing.
func (p *Postgres) ApplyClosure(ctx context.Context, eventID, pool, performance uuid.UUID, closed bool, version int32) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err = lockPoolOffering(ctx, tx, pool); err != nil {
		return err
	}
	fresh, err := consumeEvent(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if !fresh {
		return tx.Commit()
	}
	status := "open"
	if closed {
		status = "closed"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pool_slot_closures(pool_id,performance_id,closure_status,closure_version)
		VALUES($1,$2,$3,$4)
		ON CONFLICT(pool_id,performance_id) DO UPDATE SET closure_status=EXCLUDED.closure_status,closure_version=EXCLUDED.closure_version,updated_at=now()
		WHERE pool_slot_closures.closure_version < EXCLUDED.closure_version`, pool, performance, status, version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET closure_status=CASE
			WHEN EXISTS(SELECT 1 FROM pool_slot_closures WHERE pool_id=$1 AND closure_status='closed') THEN 'closed'
			ELSE 'open' END, updated_at=now()
		WHERE slot_id=$1`, pool); err != nil {
		return err
	}
	return tx.Commit()
}
