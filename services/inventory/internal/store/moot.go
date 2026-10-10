package store

import (
	"context"

	"github.com/google/uuid"
)

// A moot outcome is recorded before its event is acked (TKT-317, ADR-077). The tombstone and the
// event's consumed_events row commit together, so an acked moot event always has its tombstone.
// Both writes are idempotent. A replayed event, or a second moot event for the same slot, changes
// nothing, and the tombstone keeps its first source event.

// RecordMootSlot records that catalog did not publish the slot when eventID was handled.
func (p *Postgres) RecordMootSlot(ctx context.Context, eventID, organizerID, slotID uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = consumeEvent(ctx, tx, eventID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO moot_slots(organizer_id, slot_id, source_event_id)
		VALUES($1,$2,$3) ON CONFLICT (organizer_id, slot_id) DO NOTHING`, organizerID, slotID, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeMootOffering consumes an archive or closure that found no pool, when a tombstone for its
// organizer and slot exists (TKT-317 D1). Pool absence and the tombstone match are checked in the
// statement that inserts the consumption. When either check fails, it returns false and writes
// nothing. A replay of an event that an earlier call consumed returns true, while the same checks
// still hold.
//
// This is a predicate, not a lock. The pool row does not exist, so there is no row to lock. A
// provisioning that commits after this statement's snapshot is not excluded here.
func (p *Postgres) ConsumeMootOffering(ctx context.Context, eventID, organizerID, slotID, poolID uuid.UUID) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id)
		SELECT $1 WHERE NOT EXISTS(SELECT 1 FROM inventory_pools WHERE slot_id=$2)
		AND EXISTS(SELECT 1 FROM moot_slots WHERE organizer_id=$3 AND slot_id=$4)
		ON CONFLICT DO NOTHING`, eventID, poolID, organizerID, slotID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Nothing was inserted. Either a check failed, or an earlier moot delivery consumed this
		// event. Only the second case counts as consumed.
		var replay bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM consumed_events WHERE event_id=$1)
			AND NOT EXISTS(SELECT 1 FROM inventory_pools WHERE slot_id=$2)
			AND EXISTS(SELECT 1 FROM moot_slots WHERE organizer_id=$3 AND slot_id=$4)`,
			eventID, poolID, organizerID, slotID).Scan(&replay); err != nil {
			return false, err
		}
		if !replay {
			return false, nil
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
