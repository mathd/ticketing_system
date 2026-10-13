//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-295 D2: the append_order sequence guard, against a real PostgreSQL.
//
// Once append_order LEADS claim_history's order, a value-preserving restore (trigger
// disabled) that leaves the sequence BEHIND the restored maximum would number the next
// append BELOW restored history — a visible reordering. The guard detects that and, only
// with an explicit repair, advances the sequence. Every expectation below is derived from
// that requirement, never from a run.

type seqState struct {
	lastValue int64
	isCalled  bool
}

func readSeq(t *testing.T, f historyFixtureData) seqState {
	t.Helper()
	var s seqState
	if err := f.db.QueryRowContext(f.ctx, `SELECT last_value, is_called FROM claim_history_append_order_seq`).Scan(&s.lastValue, &s.isCalled); err != nil {
		t.Fatal(err)
	}
	return s
}

func maxAppendOrder(t *testing.T, f historyFixtureData) int64 {
	t.Helper()
	var m int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT max(append_order) FROM claim_history`).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func setSeq(t *testing.T, f historyFixtureData, value int64, called bool) {
	t.Helper()
	if _, err := f.db.ExecContext(f.ctx, `SELECT setval('claim_history_append_order_seq', $1, $2)`, value, called); err != nil {
		t.Fatal(err)
	}
}

// historyRows snapshots every row's identity and append_order, to prove the guard never
// rewrites history.
func historyRows(t *testing.T, f historyFixtureData) map[uuid.UUID]sql.NullInt64 {
	t.Helper()
	rows, err := f.db.QueryContext(f.ctx, `SELECT id, append_order FROM claim_history`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[uuid.UUID]sql.NullInt64{}
	for rows.Next() {
		var id uuid.UUID
		var n sql.NullInt64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameRows(a, b map[uuid.UUID]sql.NullInt64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A sequence BEHIND the maximum is refused without --repair and changes nothing; repaired, it
// lands at the maximum, history is untouched, and the next append is numbered max+1.
func TestAppendOrderGuardDetectsAndRepairsASequenceBehindHistory(t *testing.T) {
	f := historyFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after)
			VALUES($1,$2,$3,'place','staff:a','r',1,1,'held')`, uuid.New(), f.org, f.claim); err != nil {
			t.Fatal(err)
		}
	}
	max := maxAppendOrder(t, f)
	setSeq(t, f, 1, true) // what a value-preserving restore can leave behind
	before, rowsBefore := readSeq(t, f), historyRows(t, f)

	report, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, false)
	if !errors.Is(err, ErrAppendOrderSequenceBehind) || !report.NeedsResync {
		t.Fatalf("check-only on a sequence behind history: report=%+v err=%v, want ErrAppendOrderSequenceBehind", report, err)
	}
	if readSeq(t, f) != before || !sameRows(rowsBefore, historyRows(t, f)) {
		t.Fatal("a check without --repair changed the sequence or history")
	}

	report, err = CheckClaimHistoryAppendOrder(f.ctx, f.db, true)
	if err != nil || !report.Repaired {
		t.Fatalf("repair: report=%+v err=%v", report, err)
	}
	if got := readSeq(t, f); got.lastValue != max || !got.isCalled {
		t.Fatalf("after repair the sequence is %+v, want last_value=%d is_called=true", got, max)
	}
	if !sameRows(rowsBefore, historyRows(t, f)) {
		t.Fatal("repair changed history rows")
	}
	if report, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, false); err != nil || report.NeedsResync {
		t.Fatalf("a check after repair: report=%+v err=%v, want clean", report, err)
	}
	// The real allocation: an explicit NULL is numbered by the trigger alone (an omitted
	// column would consume the DEFAULT too), so the next number is exactly max+1.
	var next int64
	if err := f.db.QueryRowContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,append_order)
		VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',NULL) RETURNING append_order`, uuid.New(), f.org, f.claim).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != max+1 {
		t.Fatalf("the next append was numbered %d, want %d (above all restored history)", next, max+1)
	}
}

// Each sequence state against the requirement "the next nextval is above max(append_order)".
// is_called=false means the next nextval returns last_value ITSELF, so equality is behind.
func TestAppendOrderGuardSequenceStates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		offset      int64 // last_value = max + offset
		called      bool
		needsResync bool
	}{
		{"called, equal to max", 0, true, false},
		{"called, ahead of max", 5, true, false},
		{"called, behind max", -1, true, true},
		{"never called, equal to max", 0, false, true},
		{"never called, ahead of max", 1, false, false},
		{"never called, behind max", -1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := historyFixture(t)
			max := maxAppendOrder(t, f)
			setSeq(t, f, max+tc.offset, tc.called)
			before := readSeq(t, f)

			report, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, false)
			if report.NeedsResync != tc.needsResync || (err != nil) != tc.needsResync {
				t.Fatalf("report=%+v err=%v, want needsResync=%v", report, err, tc.needsResync)
			}
			// --repair on a healthy sequence is a no-op; on a behind one it never LOWERS it.
			if _, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, true); err != nil {
				t.Fatal(err)
			}
			after := readSeq(t, f)
			if !tc.needsResync && after != before {
				t.Fatalf("--repair changed a healthy sequence: %+v → %+v", before, after)
			}
			if tc.needsResync && (after.lastValue < max || !after.isCalled) {
				t.Fatalf("repair left %+v, want last_value>=%d and is_called", after, max)
			}
		})
	}
}

// No numbered rows (an empty or legacy-only table): nothing to be behind; the sequence is
// left exactly as it was, even with --repair.
func TestAppendOrderGuardLeavesTheSequenceAloneWithNoNumberedRows(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	f := historyFixtureData{ctx: ctx, st: st, db: db}
	// storeForTest migrates a fresh schema: claim_history is empty.
	before := readSeq(t, f)
	for _, repair := range []bool{false, true} {
		report, err := CheckClaimHistoryAppendOrder(ctx, db, repair)
		if err != nil || report.NeedsResync || report.MaxAppendOrder.Valid {
			t.Fatalf("repair=%v on an empty table: report=%+v err=%v", repair, report, err)
		}
	}
	if readSeq(t, f) != before {
		t.Fatal("the guard changed the sequence of an empty table")
	}
}

// The maximum is GLOBAL: a restored row under another organizer, pool-shaped, still counts.
func TestAppendOrderGuardIsGlobalAcrossOrganizersAndRowShapes(t *testing.T) {
	f := historyFixture(t)
	otherOrg, otherSlot := provisioned(t, f.ctx, f.st, 10)
	withTriggerDisabled(t, f, func() {
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,pool_id,action,actor,reason,quantity,quantity_after,status_after,idempotency_key,request_fingerprint,append_order)
			VALUES($1,$2,$3,'adjust_capacity','staff','restored',10,10,'applied','k-restored','fp',1000000)`, uuid.New(), otherOrg, otherSlot); err != nil {
			t.Fatal(err)
		}
	})
	// A finished restore re-enables the trigger (withTriggerDisabled only does so at cleanup).
	if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, false); !errors.Is(err, ErrAppendOrderSequenceBehind) {
		t.Fatalf("a restored maximum under another organizer was not detected: %v", err)
	}
}

// A restore that leaves the numbering trigger disabled — or replica-only — is refused even
// with --repair: the guard's comparison means nothing if new rows will not be numbered.
func TestAppendOrderGuardRefusesADisabledNumberingTrigger(t *testing.T) {
	for name, stmt := range map[string]string{
		"disabled":     `ALTER TABLE claim_history DISABLE TRIGGER claim_history_set_append_order`,
		"replica only": `ALTER TABLE claim_history ENABLE REPLICA TRIGGER claim_history_set_append_order`,
		"missing":      `DROP TRIGGER claim_history_set_append_order ON claim_history`,
	} {
		t.Run(name, func(t *testing.T) {
			f := historyFixture(t)
			if _, err := f.db.ExecContext(f.ctx, stmt); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = f.db.ExecContext(f.ctx, `ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`)
			})
			before := readSeq(t, f)
			for _, repair := range []bool{false, true} {
				if _, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, repair); !errors.Is(err, ErrAppendOrderTriggerNotEnabled) {
					t.Fatalf("repair=%v with the trigger %s: err=%v, want ErrAppendOrderTriggerNotEnabled", repair, name, err)
				}
			}
			if readSeq(t, f) != before {
				t.Fatal("a refused guard changed the sequence")
			}
		})
	}
}

// A table holding only LEGACY rows (NULL append_order) has nothing to be behind: the sequence
// is left exactly as it was, even with --repair.
func TestAppendOrderGuardLeavesTheSequenceAloneWithOnlyLegacyRows(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	f := historyFixtureData{ctx: ctx, st: st, db: db, org: org}
	withTriggerDisabled(t, f, func() { capacityRow(t, f, slot, uuid.New(), time.Now(), true) })
	if _, err := db.ExecContext(ctx, `ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`); err != nil {
		t.Fatal(err)
	}
	var numbered, legacy int
	if err := db.QueryRowContext(ctx, `SELECT count(append_order), count(*) - count(append_order) FROM claim_history`).Scan(&numbered, &legacy); err != nil || numbered != 0 || legacy == 0 {
		t.Fatalf("setup: numbered=%d legacy=%d err=%v, want only legacy rows", numbered, legacy, err)
	}
	before := readSeq(t, f)
	for _, repair := range []bool{false, true} {
		report, err := CheckClaimHistoryAppendOrder(ctx, db, repair)
		if err != nil || report.NeedsResync || report.MaxAppendOrder.Valid {
			t.Fatalf("repair=%v on a legacy-only table: report=%+v err=%v", repair, report, err)
		}
	}
	if readSeq(t, f) != before {
		t.Fatal("the guard changed the sequence of a legacy-only table")
	}
}

// The comparison assumes 0012's settings. Each altered setting is refused — never reported ok,
// never "repaired" — because each makes the next number able to land at or below history.
func TestAppendOrderGuardRefusesAlteredSequenceSettings(t *testing.T) {
	for name, stmt := range map[string]string{
		"negative increment": `ALTER SEQUENCE claim_history_append_order_seq INCREMENT BY -1 MINVALUE 1`,
		"cache":              `ALTER SEQUENCE claim_history_append_order_seq CACHE 10`,
		"cycle":              `ALTER SEQUENCE claim_history_append_order_seq CYCLE`,
		"non-positive range": `ALTER SEQUENCE claim_history_append_order_seq MINVALUE -10`,
	} {
		t.Run(name, func(t *testing.T) {
			f := historyFixture(t)
			if _, err := f.db.ExecContext(f.ctx, stmt); err != nil {
				t.Fatal(err)
			}
			before := readSeq(t, f)
			for _, repair := range []bool{false, true} {
				if _, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, repair); !errors.Is(err, ErrAppendOrderSequenceSettings) {
					t.Fatalf("repair=%v with %s: err=%v, want ErrAppendOrderSequenceSettings", repair, name, err)
				}
			}
			if readSeq(t, f) != before {
				t.Fatal("a refused guard changed the sequence")
			}
		})
	}
}

// An ordinary append consumes TWO sequence values (the column DEFAULT, then the trigger), so
// the guard refuses unless two remain — and must not "repair" into exhaustion. The ok rows sit
// exactly one value inside each refused boundary.
func TestAppendOrderGuardRefusesAnExhaustedSequence(t *testing.T) {
	const top = int64(math.MaxInt64) // the bigint sequence's max_value
	for _, tc := range []struct {
		name      string
		restored  int64 // a restored row's append_order; 0 = none
		seq       int64
		called    bool
		exhausted bool
	}{
		{"restored max leaves one value", top - 1, 1, true, true},
		{"restored max leaves two values", top - 2, 1, true, false},
		{"called sequence leaves one value", 0, top - 1, true, true},
		{"called sequence leaves two values", 0, top - 2, true, false},
		{"uncalled sequence leaves one value", 0, top, false, true},
		{"uncalled sequence leaves two values", 0, top - 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := historyFixture(t)
			if tc.restored != 0 {
				withTriggerDisabled(t, f, func() {
					if _, err := f.db.ExecContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,append_order)
						VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',$4)`, uuid.New(), f.org, f.claim, tc.restored); err != nil {
						t.Fatal(err)
					}
				})
				if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`); err != nil {
					t.Fatal(err)
				}
			}
			setSeq(t, f, tc.seq, tc.called)
			before := readSeq(t, f)
			_, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, true)
			if tc.exhausted {
				if !errors.Is(err, ErrAppendOrderSequenceExhausted) {
					t.Fatalf("err=%v, want ErrAppendOrderSequenceExhausted", err)
				}
				if readSeq(t, f) != before {
					t.Fatal("a refused guard changed the sequence")
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v, want ok (two values remain)", err)
			}
			// The proof that two values really remain: an ordinary append (column omitted)
			// succeeds, and is numbered by the trigger with the second of them.
			var got int64
			if err := f.db.QueryRowContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after)
				VALUES($1,$2,$3,'place','staff:a','r',1,1,'held') RETURNING append_order`, uuid.New(), f.org, f.claim).Scan(&got); err != nil {
				t.Fatalf("an ordinary append after an ok guard failed: %v", err)
			}
			if got != top {
				t.Fatalf("the append was numbered %d, want %d (the last value)", got, top)
			}
		})
	}
}

// The guard holds claim_history locked for the whole check, so an UNCOMMITTED restore row is
// waited for and counted — not missed and then committed above a "repaired" sequence. The
// restore session runs as replica, so its supplied number survives the trigger. The wait is
// observed through pg_locks, not a sleep.
func TestAppendOrderGuardWaitsForAnInFlightRestore(t *testing.T) {
	f := historyFixture(t)
	restore, err := f.db.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restore.Close() }()
	rtx, err := restore.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rtx.Rollback() }()
	if _, err := rtx.ExecContext(f.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	const restored = 1000
	if _, err := rtx.ExecContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,append_order)
		VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',$4)`, uuid.New(), f.org, f.claim, restored); err != nil {
		t.Fatal(err)
	}

	type result struct {
		report AppendOrderSequenceReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		r, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, true)
		done <- result{r, err}
	}()

	// Wait until the guard is BLOCKED on claim_history — or fail if it finishes first.
	waitCtx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := f.db.QueryRowContext(waitCtx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE relation = 'claim_history'::regclass AND mode = 'ShareRowExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatalf("the guard never blocked on the in-flight restore: %v", err)
		}
		if waiting {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the guard finished while a restore row was uncommitted: report=%+v err=%v", r.report, r.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := rtx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || !r.report.Repaired || r.report.LastValue != restored {
		t.Fatalf("after the restore committed: report=%+v err=%v, want repaired to %d", r.report, r.err, restored)
	}
}

// The headroom check reads the sequence's OWN max_value, not the bigint ceiling: a lowered
// maximum is refused one value early and accepted at exactly two values left.
func TestAppendOrderGuardHeadroomUsesTheSequencesMaxValue(t *testing.T) {
	for _, tc := range []struct {
		seq       int64
		exhausted bool
	}{{999, true}, {998, false}} {
		f := historyFixture(t)
		if _, err := f.db.ExecContext(f.ctx, `ALTER SEQUENCE claim_history_append_order_seq MAXVALUE 1000`); err != nil {
			t.Fatal(err)
		}
		setSeq(t, f, tc.seq, true)
		_, err := CheckClaimHistoryAppendOrder(f.ctx, f.db, false)
		if got := errors.Is(err, ErrAppendOrderSequenceExhausted); got != tc.exhausted {
			t.Fatalf("max_value=1000 last_value=%d: err=%v, want exhausted=%v", tc.seq, err, tc.exhausted)
		}
	}
}
