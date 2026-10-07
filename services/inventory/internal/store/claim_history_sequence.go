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
// Preconditions it does not enforce, stated in docs/development.md: writers are stopped
// while it runs, and the sequence keeps 0012's settings (increment 1, no cache, no cycle).

// ErrAppendOrderSequenceBehind: the sequence's next value is not above max(append_order).
var ErrAppendOrderSequenceBehind = errors.New("claim_history append_order sequence is behind the stored maximum")

// ErrAppendOrderTriggerNotEnabled: the numbering trigger is missing, disabled, or fires only
// for replica sessions, so new rows would not be numbered. Never repaired by this guard.
var ErrAppendOrderTriggerNotEnabled = errors.New("claim_history numbering trigger is not enabled for ordinary writes")

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
// The repair is monotonic — setval(GREATEST(last_value, max), true) — so it never lowers the
// sequence and never rewrites a row. GREATEST differs from max only if a writer advances the
// sequence between the read and the setval, which the stopped-writers precondition excludes,
// so no test reaches it; it stays so a guard run against live writers cannot LOWER the
// sequence. NOTE: setval is not transactional; once it has run,
// rolling back does not undo it. A failure after it is reported as an error, and the operator
// re-runs the check, which reads the true state.
func CheckClaimHistoryAppendOrder(ctx context.Context, db *sql.DB, repair bool) (AppendOrderSequenceReport, error) {
	var enabled sql.NullString
	err := db.QueryRowContext(ctx, `SELECT tgenabled FROM pg_trigger
		WHERE tgrelid = 'claim_history'::regclass AND tgname = 'claim_history_set_append_order' AND NOT tgisinternal`).Scan(&enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AppendOrderSequenceReport{}, fmt.Errorf("read numbering trigger: %w", err)
	}
	// 'O' fires in origin and local sessions, 'A' always. 'D' is disabled, 'R' replica-only.
	if !enabled.Valid || (enabled.String != "O" && enabled.String != "A") {
		return AppendOrderSequenceReport{}, fmt.Errorf("%w (tgenabled=%q)", ErrAppendOrderTriggerNotEnabled, enabled.String)
	}

	report, err := readAppendOrderState(ctx, db)
	if err != nil || !report.NeedsResync {
		return report, err
	}
	if !repair {
		return report, ErrAppendOrderSequenceBehind
	}
	before := report
	if _, err := db.ExecContext(ctx, `SELECT setval('claim_history_append_order_seq',
		GREATEST((SELECT last_value FROM claim_history_append_order_seq), (SELECT max(append_order) FROM claim_history)), true)`); err != nil {
		return before, fmt.Errorf("repair the sequence (it may or may not have advanced; re-run the check): %w", err)
	}
	report, err = readAppendOrderState(ctx, db)
	if err != nil {
		return before, fmt.Errorf("re-read after repair (the sequence may have advanced; re-run the check): %w", err)
	}
	if report.NeedsResync {
		return report, fmt.Errorf("%w even after repair", ErrAppendOrderSequenceBehind)
	}
	report.Repaired = true
	report.BeforeLastValue, report.BeforeIsCalled = before.LastValue, before.IsCalled
	return report, nil
}

// readAppendOrderState compares the sequence with the stored maximum. The next nextval
// returns last_value itself when is_called is false and last_value+1 when it is true, so the
// sequence is behind when that next value is not above the maximum.
func readAppendOrderState(ctx context.Context, db *sql.DB) (AppendOrderSequenceReport, error) {
	var r AppendOrderSequenceReport
	err := db.QueryRowContext(ctx, `
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
