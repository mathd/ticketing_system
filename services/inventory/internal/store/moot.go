package store

import (
	"context"

	"github.com/google/uuid"
)

// A moot outcome is recorded before its event is acked (TKT-317, ADR-077). The record and the
// event's consumed_events row commit together, so an acked moot event always has its record. Both
// writes are idempotent. A replayed event, or a second moot event of the same kind for the same slot,
// changes nothing, and the record keeps its first source event.
//
// The record's source says which kind of event was moot. Only a publication record authorises a
// later archive or closure to be consumed without a pool (D7). A closure record is kept, and it
// authorises nothing: a closure's 404 does not prove that no publication will provision the slot.

// MootSource names the kind of event whose catalog answer was "not published".
type MootSource string

const (
	// MootSourcePublication is a publication whose catalog answer was 404. Its record authorises the
	// fallback for the slot.
	MootSourcePublication MootSource = "publication"
	// MootSourceClosure is a closure whose catalog answer was 404. Its record authorises nothing.
	MootSourceClosure MootSource = "closure"
)

// RecordMootSlot records that catalog did not publish the slot when eventID was handled. The key
// includes the source, so a closure record and a publication record for one slot are both kept, and
// neither shadows or overwrites the other.
func (p *Postgres) RecordMootSlot(ctx context.Context, eventID, organizerID, slotID uuid.UUID, source MootSource) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = consumeEvent(ctx, tx, eventID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO moot_slots(organizer_id, slot_id, source, source_event_id)
		VALUES($1,$2,$3,$4) ON CONFLICT (organizer_id, slot_id, source) DO NOTHING`,
		organizerID, slotID, string(source), eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeMootOffering consumes an archive or closure that found no pool, when a publication record
// for its organizer and slot exists (TKT-317 D1, D7). Pool absence and the record match are checked
// in the statement that inserts the consumption. A closure record never matches. When either check
// fails, it returns false and writes nothing. A replay of an event that an earlier call consumed
// returns true, while the same checks still hold.
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
		AND EXISTS(SELECT 1 FROM moot_slots WHERE organizer_id=$3 AND slot_id=$4 AND source=$5)
		ON CONFLICT DO NOTHING`, eventID, poolID, organizerID, slotID, string(MootSourcePublication))
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
			AND EXISTS(SELECT 1 FROM moot_slots WHERE organizer_id=$3 AND slot_id=$4 AND source=$5)`,
			eventID, poolID, organizerID, slotID, string(MootSourcePublication)).Scan(&replay); err != nil {
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
