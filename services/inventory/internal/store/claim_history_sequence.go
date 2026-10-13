package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The append_order sequence guard (TKT-295 D2, ADR-021 § Amendment).
//
// Since TKT-295, append_order LEADS claim_history's order. Its numbers come from
// claim_history_append_order_seq through the claim_history_set_append_order trigger (0012).
// A value-preserving restore runs with that trigger disabled, so the restored rows keep
// their numbers but the sequence does not move: the next append can then be numbered BELOW
// restored history and sort before it. This guard checks that the next number the sequence
// will hand out is above every stored one, and — only when asked — advances the sequence.
//
// It is a guard, not a restore tool, and it bounds only honest-writer consistency: it does
// not detect missing, duplicated or dishonest rows, and a writer that bypasses the trigger
// (session_replication_role=replica, a disabled trigger) supplies append_order itself.
//
// The whole check runs in ONE transaction holding claim_history in SHARE ROW EXCLUSIVE mode.
// Every append (the DEFAULT and the trigger call nextval inside an INSERT) holds ROW EXCLUSIVE
// on the table until it commits, so the lock waits for in-flight appends — including an
// uncommitted restore row the maximum must see — and admits no new one until the guard is
// done. A nextval called OUTSIDE an insert is not excluded: the runbook stops writers and
// maintenance sessions for that (docs/development.md § Restoring claim_history).

// ErrAppendOrderSequenceBehind: the sequence's next value is not above max(append_order).
var ErrAppendOrderSequenceBehind = errors.New("claim_history append_order sequence is behind the stored maximum")

// ErrAppendOrderTriggerNotEnabled: the numbering trigger is missing, disabled, or fires only
// for replica sessions, so new rows would not be numbered. Never repaired by this guard.
var ErrAppendOrderTriggerNotEnabled = errors.New("claim_history numbering trigger is not enabled for ordinary writes")

// ErrAppendOrderSequenceSettings: the sequence no longer has 0012's settings (increment 1,
// cache 1, no cycle, min_value >= 1). The comparison assumes them — a negative increment or a cycle hands out
// LOWER numbers, and a cache lets two sessions interleave allocation and append order — so
// the guard refuses rather than report ok. Never repaired by this guard.
var ErrAppendOrderSequenceSettings = errors.New("claim_history append_order sequence settings differ from migration 0012")

// ErrAppendOrderSequenceExhausted: the sequence cannot hand out the next append's numbers.
// An ordinary insert that omits append_order consumes TWO values (the column DEFAULT, then the
// trigger), so two must remain. Never repaired by this guard.
var ErrAppendOrderSequenceExhausted = errors.New("claim_history append_order sequence is exhausted")

// AppendOrderSequenceReport is what the guard observed (after a repair, the state it left).
type AppendOrderSequenceReport struct {
	LastValue      int64
	IsCalled       bool
	MaxAppendOrder sql.NullInt64 // NULL: no numbered rows (an empty or legacy-only table)
	NeedsResync    bool
	Repaired       bool
	// Before is the observed state when a repair ran, so the operator sees what changed.
	BeforeLastValue int64
	BeforeIsCalled  bool
}

// CheckClaimHistoryAppendOrder runs the guard; repair=true advances a behind sequence.
//
// The repair is setval(max(append_order), true). It runs only when the sequence is behind,
// that is last_value <= max under the table lock, so it never lowers the sequence; it never
// rewrites a row. NOTE: setval is not transactional — a failure after it does not undo it.
// Such a failure is reported as an error, and the operator re-runs the check.
func CheckClaimHistoryAppendOrder(ctx context.Context, db *sql.DB, repair bool) (AppendOrderSequenceReport, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return AppendOrderSequenceReport{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE claim_history IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return AppendOrderSequenceReport{}, fmt.Errorf("lock claim_history: %w", err)
	}

	var enabled sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT tgenabled FROM pg_trigger
		WHERE tgrelid = 'claim_history'::regclass AND tgname = 'claim_history_set_append_order' AND NOT tgisinternal`).Scan(&enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AppendOrderSequenceReport{}, fmt.Errorf("read numbering trigger: %w", err)
	}
	// 'O' fires in origin and local sessions, 'A' always. 'D' is disabled, 'R' replica-only;
	// a missing trigger reads as "".
	if enabled.String != "O" && enabled.String != "A" {
		return AppendOrderSequenceReport{}, fmt.Errorf("%w (tgenabled=%q)", ErrAppendOrderTriggerNotEnabled, enabled.String)
	}

	var increment, cache, seqMin, seqMax int64
	var cycle bool
	if err := tx.QueryRowContext(ctx, `SELECT seqincrement, seqcache, seqcycle, seqmin, seqmax FROM pg_sequence
		WHERE seqrelid = 'claim_history_append_order_seq'::regclass`).Scan(&increment, &cache, &cycle, &seqMin, &seqMax); err != nil {
		return AppendOrderSequenceReport{}, fmt.Errorf("read sequence settings: %w", err)
	}
	// min_value >= 1 is 0012's positive domain (append_order's CHECK refuses anything else), and
	// it keeps seqMax-2 below from underflowing (pass-2 finding).
	if increment != 1 || cache != 1 || cycle || seqMin < 1 {
		return AppendOrderSequenceReport{}, fmt.Errorf("%w (increment=%d cache=%d cycle=%t min_value=%d)", ErrAppendOrderSequenceSettings, increment, cache, cycle, seqMin)
	}

	report, err := readAppendOrderState(ctx, tx)
	if err != nil {
		return AppendOrderSequenceReport{}, err
	}
	// Two values must remain after whichever is higher: the restored maximum (a repair sets
	// the sequence to it, called) or the sequence's own next value.
	if (report.MaxAppendOrder.Valid && report.MaxAppendOrder.Int64 > seqMax-2) ||
		(report.IsCalled && report.LastValue > seqMax-2) || (!report.IsCalled && report.LastValue > seqMax-1) {
		return report, fmt.Errorf("%w (last_value=%d is_called=%t max_append_order=%s max_value=%d)",
			ErrAppendOrderSequenceExhausted, report.LastValue, report.IsCalled, nullInt(report.MaxAppendOrder), seqMax)
	}
	if !report.NeedsResync {
		return report, tx.Commit()
	}
	if !repair {
		return report, ErrAppendOrderSequenceBehind
	}
	before := report
	if _, err := tx.ExecContext(ctx, `SELECT setval('claim_history_append_order_seq', $1, true)`, report.MaxAppendOrder.Int64); err != nil {
		return before, fmt.Errorf("repair the sequence (it may or may not have advanced; re-run the check): %w", err)
	}
	report, err = readAppendOrderState(ctx, tx)
	if err != nil {
		return before, fmt.Errorf("re-read after repair (the sequence may have advanced; re-run the check): %w", err)
	}
	if report.NeedsResync {
		return report, fmt.Errorf("%w even after repair", ErrAppendOrderSequenceBehind)
	}
	if err := tx.Commit(); err != nil {
		return before, fmt.Errorf("commit after repair (the sequence has advanced; re-run the check): %w", err)
	}
	report.Repaired = true
	report.BeforeLastValue, report.BeforeIsCalled = before.LastValue, before.IsCalled
	return report, nil
}

func nullInt(v sql.NullInt64) string {
	if !v.Valid {
		return "null"
	}
	return fmt.Sprint(v.Int64)
}

// readAppendOrderState compares the sequence with the stored maximum. The next nextval
// returns last_value itself when is_called is false and last_value+1 when it is true, so the
// sequence is behind when that next value is not above the maximum.
func readAppendOrderState(ctx context.Context, tx *sql.Tx) (AppendOrderSequenceReport, error) {
	var r AppendOrderSequenceReport
	err := tx.QueryRowContext(ctx, `
		SELECT s.last_value, s.is_called, h.max_append_order,
		       CASE
		           WHEN h.max_append_order IS NULL THEN false
		           WHEN s.is_called THEN s.last_value < h.max_append_order
		           ELSE s.last_value <= h.max_append_order
		       END
		FROM claim_history_append_order_seq AS s
		CROSS JOIN (SELECT max(append_order) AS max_append_order FROM claim_history) AS h`).
		Scan(&r.LastValue, &r.IsCalled, &r.MaxAppendOrder, &r.NeedsResync)
	if err != nil {
		return AppendOrderSequenceReport{}, fmt.Errorf("read append_order sequence state: %w", err)
	}
	return r, nil
}
