// TKT-380 DISPOSABLE PROTOTYPE. NOT PRODUCTION CODE.
//
// run.sh copies this file into a TEMPORARY copy of services/inventory as
// internal/store/tkt380_prototype.go. It is never compiled in the repository:
// docs/evidence is outside every Go module in go.work.
//
// It is a copy of Transition (services/inventory/internal/store/store.go:811)
// with the ADR-084 D13 invariant added for a claim that cites a code:
//
//  1. Take the code row lock AFTER the pool and claim locks (lock order pool ->
//     claim -> code; no pool or claim lock after the code lock).
//  2. Judge expiry on a fresh time read (clock_timestamp()) taken AFTER the code
//     lock, not on transaction-start now().
//
// The presale code row stands in for the future promotion code row. The three
// lines marked MUTATION-POINT are the ones run.sh edits for the mutation runs:
// "time" reads now() instead, "lock" deletes the lock, and "uncapped" skips the
// lock for a code that has no cap (the exception ADR-084 first proposed).
package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// TKT380AfterDecision is a probe-only hook. It runs inside the finalize
// transaction after the decision and before the commit, so the probe can
// force a schedule. Nil means no hook.
var TKT380AfterDecision func(ctx context.Context, tx *sql.Tx)

func tkt380LockCode(ctx context.Context, tx *sql.Tx, org uuid.UUID, channel, code string) error {
	_, err := tx.ExecContext(ctx, `SELECT 1 FROM presale_codes WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`, org, channel, code)
	return err
}

// TKT380PromotedTransition is Transition plus the D13 invariant.
func (p *Postgres) TKT380PromotedTransition(ctx context.Context, org, id uuid.UUID, target string) (Claim, error) {
	var pool uuid.UUID
	err := p.db.QueryRowContext(ctx, `SELECT pool_id FROM claims WHERE id=$1 AND organizer_id=$2`, id, org).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, pool); err != nil {
		return Claim{}, err
	}
	var c Claim
	var code sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,organizer_id,pool_id,quantity,status,COALESCE(channel_code,''),expires_at,now(),COALESCE(ticket_type_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(unit_amount,0),COALESCE(currency,''),claim_kind,presale_code FROM claims WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, org).
		Scan(&c.ID, &c.OrganizerID, &c.PoolID, &c.Quantity, &c.Status, &c.Channel, &c.ExpiresAt, &c.ServerTime, &c.TicketTypeID, &c.UnitAmount, &c.Currency, &c.Kind, &code)
	if err != nil {
		return Claim{}, err
	}
	if c.Kind != "buyer" {
		return Claim{}, ErrConflict
	}
	// D13: a path that KEEPS usage (finalize, direct confirm) takes the code
	// lock, then reads time afresh under it. It does so whether or not the code
	// is capped today: a cap can be added while this transaction is open (S3).
	if code.Valid && (target == "finalizing" || target == "confirmed") {
		var capped bool
		if err = tx.QueryRowContext(ctx, `SELECT max_redemptions IS NOT NULL FROM presale_codes WHERE organizer_id=$1 AND channel_code=$2 AND code=$3`, org, c.Channel, code.String).Scan(&capped); err != nil {
			return Claim{}, err
		}
		skipUncapped := false // MUTATION-POINT:uncapped
		if capped || !skipUncapped {
			err = tkt380LockCode(ctx, tx, org, c.Channel, code.String) // MUTATION-POINT:lock
		}
		if err != nil {
			return Claim{}, err
		}
		timeSQL := `SELECT clock_timestamp()` // MUTATION-POINT:time
		if err = tx.QueryRowContext(ctx, timeSQL).Scan(&c.ServerTime); err != nil {
			return Claim{}, err
		}
	}
	if c.expired() {
		c.Status = "expired"
		if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='expired',updated_at=now() WHERE id=$1`, id); err != nil {
			return Claim{}, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "expire", "system", "ttl_elapsed", c.Quantity, 0, "expired", nil, nil); err != nil {
			return Claim{}, err
		}
		if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if c.Status == target {
		return c, p.commitAvailability(tx, pool)
	}
	if target == "finalizing" && c.Status == "confirmed" {
		return c, p.commitAvailability(tx, pool)
	}
	if target == "released" && c.Status == "expired" {
		return c, p.commitAvailability(tx, pool)
	}
	if target == "finalizing" && c.Status == "held" {
		c.Status = target
		if _, err = tx.ExecContext(ctx, `UPDATE claims SET status='finalizing',updated_at=now() WHERE id=$1`, id); err != nil {
			return c, err
		}
		if err = appendHistory(ctx, tx, org, id, nil, "finalize", "commerce", "checkout", c.Quantity, c.Quantity, "finalizing", nil, nil); err != nil {
			return c, err
		}
		if TKT380AfterDecision != nil {
			TKT380AfterDecision(ctx, tx)
		}
		return c, p.commitAvailability(tx, pool)
	}
	if c.Status != "held" && c.Status != "finalizing" {
		return c, ErrConflict
	}
	if target == "confirmed" {
		if _, err = tx.ExecContext(ctx, `UPDATE inventory_pools SET confirmed_quantity=confirmed_quantity+$1,updated_at=now() WHERE slot_id=$2`, c.Quantity, pool); err != nil {
			return Claim{}, err
		}
	}
	c.Status = target
	if _, err = tx.ExecContext(ctx, `UPDATE claims SET status=$1,updated_at=now() WHERE id=$2`, target, id); err != nil {
		return Claim{}, err
	}
	action, after := "confirm", c.Quantity
	if target == "released" {
		action, after = "release", 0
		if err = releaseSeatsForTerminal(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
		if err = reconcileCapacity(ctx, tx, pool); err != nil {
			return Claim{}, err
		}
	}
	if err = appendHistory(ctx, tx, org, id, nil, action, "commerce", "checkout", c.Quantity, after, target, nil, nil); err != nil {
		return Claim{}, err
	}
	return c, p.commitAvailability(tx, pool)
}
