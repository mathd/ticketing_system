package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// SlotPolicy is one slot's projected re_entry policy (ADR-005: catalog owns
// it, access enforces it at the gate). Fed by the performance.published
// consumer; the projection is additive — dropping it returns every slot to
// single, which is today's behavior.
type SlotPolicy struct {
	SlotID, OrganizerID uuid.UUID
	Policy              ReEntryPolicy
}

// UpsertSlotPolicy applies one publication event's policy, idempotently by
// envelope id (the Issue pattern): a redelivered envelope is a no-op, a new
// envelope for the same slot converges the row.
func (p *Postgres) UpsertSlotPolicy(ctx context.Context, eventID uuid.UUID, sp SlotPolicy) error {
	switch sp.Policy.Mode {
	case "single", "multi", "count_limited":
	default:
		return fmt.Errorf("unknown re_entry mode %q", sp.Policy.Mode)
	}
	if sp.Policy.Mode == "count_limited" && (sp.Policy.MaxEntries == nil || *sp.Policy.MaxEntries <= 0) {
		return errors.New("re_entry mode count_limited requires a positive max_entries")
	}
	if sp.Policy.Mode != "count_limited" && sp.Policy.MaxEntries != nil {
		return errors.New("max_entries is only valid for re_entry mode count_limited")
	}
	if sp.SlotID == uuid.Nil || sp.OrganizerID == uuid.Nil || eventID == uuid.Nil {
		return errors.New("slot policy requires slot, organizer and envelope ids")
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id) VALUES($1) ON CONFLICT DO NOTHING`, eventID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO slot_re_entry_policies(slot_id,organizer_id,mode,max_entries,requires_exit)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(slot_id) DO UPDATE SET organizer_id=EXCLUDED.organizer_id,mode=EXCLUDED.mode,
			max_entries=EXCLUDED.max_entries,requires_exit=EXCLUDED.requires_exit,updated_at=now()`,
		sp.SlotID, sp.OrganizerID, sp.Policy.Mode, sp.Policy.MaxEntries, sp.Policy.RequiresExit); err != nil {
		return err
	}
	return tx.Commit()
}

// slotPolicy resolves a slot's policy; no row means single — today's
// semantics exactly, never fail-open (COS 7). Runs on whatever querier the
// caller is in: the projection is written only by the consumer, so a plain
// read outside the ticket lock cannot race an admission decision into
// inconsistency — at worst a scan lands just before the policy row and takes
// the single path, which is the declared deploy-ordering behavior.
func (p *Postgres) slotPolicy(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, slotID uuid.UUID) (ReEntryPolicy, error) {
	var policy ReEntryPolicy
	err := q.QueryRowContext(ctx, `SELECT mode,max_entries,requires_exit FROM slot_re_entry_policies WHERE slot_id=$1`, slotID).
		Scan(&policy.Mode, &policy.MaxEntries, &policy.RequiresExit)
	if errors.Is(err, sql.ErrNoRows) {
		return ReEntryPolicy{Mode: "single"}, nil
	}
	if isUndefinedTable(err) {
		// A schema that predates 0006 (migration tests pin the store against
		// historical versions) has no projection at all — which means single,
		// the same answer as an absent row.
		return ReEntryPolicy{Mode: "single"}, nil
	}
	if err != nil {
		return ReEntryPolicy{}, err
	}
	return policy, nil
}

// isUndefinedTable reports Postgres undefined_table (SQLSTATE 42P01) without
// importing driver-specific error types (the isUniqueViolation pattern).
func isUndefinedTable(err error) bool {
	type coder interface{ SQLState() string }
	var c coder
	return errors.As(err, &c) && c.SQLState() == "42P01"
}
