package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrCatalogParkedCollision reports a parked (subject, event_id) that already holds different
// bytes. That is a producer invariant break (ADR-009 §5), and the first copy stays.
var ErrCatalogParkedCollision = errors.New("catalog event id already parked with different content")

// ParkedCatalogEvent is one catalog event that was terminated after its catalog answer stayed
// unusable (TKT-317 D2). Envelope holds the exact bytes the broker delivered.
type ParkedCatalogEvent struct {
	Subject     string
	EventID     uuid.UUID
	OrganizerID uuid.UUID
	SlotID      uuid.UUID
	Schema      int
	Envelope    []byte
	Deliveries  int64
	Reason      string
}

// ParkCatalogEvent persists a terminated catalog event. It reports whether the row is new. A
// duplicate with identical bytes is a silent no-op, so a crash between the park and the Term is
// safe. A duplicate with different bytes returns ErrCatalogParkedCollision and leaves the first
// copy as it was.
func (p *Postgres) ParkCatalogEvent(ctx context.Context, e ParkedCatalogEvent) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO catalog_event_parked(subject, event_id, schema, organizer_id, slot_id, envelope, delivery_count, reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (subject, event_id) DO NOTHING`,
		e.Subject, e.EventID, e.Schema, e.OrganizerID, e.SlotID, e.Envelope, e.Deliveries, e.Reason)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var stored []byte
		if err = tx.QueryRowContext(ctx, `SELECT envelope FROM catalog_event_parked WHERE subject=$1 AND event_id=$2`,
			e.Subject, e.EventID).Scan(&stored); err != nil {
			return false, err
		}
		if !bytes.Equal(stored, e.Envelope) {
			return false, ErrCatalogParkedCollision
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}
