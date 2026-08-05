package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Capacity adjustment (TKT-76 / ADR-026, behaviour per the ADR-005 amendment): raises
// apply freely; a cut below live demand clamps to max(new, confirmed + held), records the
// requested target, and blocks new claims until demand drains to it — never
// force-releasing anything (forward-only). Blocking is derived: every admission path
// checks demand against COALESCE(target_capacity, capacity), so no flag column exists.

type CapacityAdjustment struct {
	SlotID         uuid.UUID `json:"slot_id"`
	CapacityBefore int32     `json:"capacity_before"`
	Capacity       int32     `json:"capacity"`
	TargetCapacity *int32    `json:"target_capacity,omitempty"`
	Status         string    `json:"status"` // applied | clamped
	ServerTime     time.Time `json:"server_time"`
}

func (p *Postgres) AdjustCapacity(ctx context.Context, org, slot uuid.UUID, newCap int32, actor, reason, key string) (CapacityAdjustment, bool, error) {
	if newCap <= 0 {
		return CapacityAdjustment{}, false, fmt.Errorf("capacity must be positive")
	}
	fp := opFingerprint("adjust-capacity", org, slot, newCap)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return CapacityAdjustment{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var capacity, confirmed int32
	var lifecycle string
	err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,lifecycle_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE`, slot, org).Scan(&capacity, &confirmed, &lifecycle)
	if errors.Is(err, sql.ErrNoRows) {
		return CapacityAdjustment{}, false, ErrNotFound
	}
	if err != nil {
		return CapacityAdjustment{}, false, err
	}
	prior, found, err := registryLookup(ctx, tx, org, key, fp)
	if err != nil {
		return CapacityAdjustment{}, false, err
	}
	if found {
		adj := CapacityAdjustment{SlotID: slot, CapacityBefore: prior.quantity, Capacity: prior.quantityAfter, TargetCapacity: prior.targetCapacity, Status: prior.statusAfter, ServerTime: time.Now().UTC()}
		return adj, true, p.commitAvailability(tx, slot)
	}
	// Replay-then-guard, like every staff op. Archival is terminal; a closed pool stays
	// adjustable — closure is reversible and a capacity fix may precede reopening.
	if lifecycle == "archived" {
		return CapacityAdjustment{}, false, ErrSlotArchived
	}
	if err = sweepExpired(ctx, tx, slot); err != nil {
		return CapacityAdjustment{}, false, err
	}
	// The sweep reconciles a draining cut, so the pre-sweep read is stale: re-read under
	// the same lock so capacity_before records the settled, operator-visible value
	// (ai-review finding 2).
	if err = tx.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity FROM inventory_pools WHERE slot_id=$1`, slot).Scan(&capacity, &confirmed); err != nil {
		return CapacityAdjustment{}, false, err
	}
	var held int32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0) FROM claims WHERE pool_id=$1 AND `+liveClaims, slot).Scan(&held); err != nil {
		return CapacityAdjustment{}, false, err
	}
	// Demand never exceeds int32 capacity (pool invariant), so the cast is safe;
	// the comparison stays int64 like every other capacity check.
	demand := int64(confirmed) + int64(held)
	adj := CapacityAdjustment{SlotID: slot, CapacityBefore: capacity, Capacity: newCap, Status: "applied"}
	if int64(newCap) < demand {
		adj.Capacity, adj.TargetCapacity, adj.Status = int32(demand), &newCap, "clamped"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET capacity=$1,target_capacity=$2,updated_at=now() WHERE slot_id=$3`, adj.Capacity, adj.TargetCapacity, slot); err != nil {
		return CapacityAdjustment{}, false, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO claim_history(id,organizer_id,pool_id,action,actor,reason,quantity,quantity_after,status_after,target_capacity,idempotency_key,request_fingerprint)
		VALUES($1,$2,$3,'adjust_capacity',$4,$5,$6,$7,$8,$9,$10,$11) RETURNING occurred_at`,
		uuid.New(), org, slot, actor, reason, adj.CapacityBefore, adj.Capacity, adj.Status, adj.TargetCapacity, key, fp).Scan(&adj.ServerTime)
	if err != nil {
		return CapacityAdjustment{}, false, err
	}
	return adj, false, p.commitAvailability(tx, slot)
}

// effectiveCapacity is the read-side clamp floor: max(target-or-capacity, demand). It
// re-derives what reconcileCapacity materializes, so reads never depend on a write running.
func effectiveCapacity(capacity int32, target sql.NullInt32, confirmed, held int32) int32 {
	limit := int64(capacity)
	if target.Valid {
		limit = int64(target.Int32)
	}
	if d := int64(confirmed) + int64(held); d > limit {
		limit = d
	}
	return int32(limit)
}

// reconcileCapacity settles a draining cut under the caller's pool lock: while demand
// still exceeds the target, capacity follows demand down; once demand reaches the target,
// capacity lands there and the target clears. Bookkeeping only — reads and admission
// checks re-derive from liveClaims, so correctness never depends on this running.
func reconcileCapacity(ctx context.Context, tx *sql.Tx, pool uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `UPDATE inventory_pools p SET
			capacity=GREATEST(p.target_capacity, p.confirmed_quantity + d.held),
			target_capacity=CASE WHEN p.confirmed_quantity + d.held <= p.target_capacity THEN NULL ELSE p.target_capacity END,
			updated_at=now()
		FROM (SELECT COALESCE(sum(quantity),0)::int AS held FROM claims WHERE pool_id=$1 AND `+liveClaims+`) d
		WHERE p.slot_id=$1 AND p.target_capacity IS NOT NULL`, pool)
	return err
}

// CapacityHistory lists the pool's adjustment audit trail (who/when/from→to, and the
// requested target when a cut clamped). Staff read: no-store at the API layer.
func (p *Postgres) CapacityHistory(ctx context.Context, org, slot uuid.UUID) ([]HistoryEntry, error) {
	var one int
	err := p.db.QueryRowContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2`, slot, org).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT action,actor,reason,quantity,quantity_after,status_after,target_capacity,occurred_at
		FROM claim_history WHERE organizer_id=$1 AND pool_id=$2 ORDER BY occurred_at, id`, org, slot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		if err = rows.Scan(&e.Action, &e.Actor, &e.Reason, &e.Quantity, &e.QuantityAfter, &e.StatusAfter, &e.TargetCapacity, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
