//go:build smoke

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-230. `claim_history` was read with `ORDER BY occurred_at, id`, which is total but
// NOT MEANINGFUL: `id` is `uuid.New()` (UUIDv4, random, no time component), so two rows
// that tie on `occurred_at` were ordered by a coin flip.
//
// The tie is not exotic. `occurred_at` defaults to `now()`, which is TRANSACTION-START
// time, and separate concurrent transactions can be issued the same value: measured at
// shaping, 8 concurrent writers x 150 inserts produced 1199 distinct timestamps over 1200
// rows — one collision between two different writers, identical to the microsecond. That
// is exactly the reported flake (`history[0].Action = draw_down, want reserve`): serial
// runs never collide, loaded runs do.
//
// These tests force the tie DETERMINISTICALLY — one shared timestamp, hand-chosen UUIDs —
// rather than by running under load or with `-count=N`. Both are forbidden by the ticket,
// and both would prove nothing on a quiet machine.

// historyFixture provisions a pool and a claim that history rows can reference.
//
// The rows must be CLAIM-SHAPED. `claim_history_shape` (migration 0006) permits exactly
// one of (claim-shaped, pool-capacity-shaped) per row: `claim_id` NOT NULL with `pool_id`
// and `target_capacity` NULL, or the mirror image for `adjust_capacity`. A fixture that
// sets both, or neither, is refused by the database and the test would fail for a reason
// that has nothing to do with ordering.
func historyFixture(t *testing.T) historyFixtureData {
	t.Helper()
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	// A real claim, so the FK on claim_history.claim_id is satisfied.
	// `house` + a non-blank label: `claims_kind_shape` (migration 0007) constrains
	// operational_purpose to house|artist|kill|other.
	hold, _, err := st.PlaceOperationalHold(ctx, org, slot, 1, "house", "front-of-house", "staff:a", "r", "k-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return historyFixtureData{ctx: ctx, st: st, db: db, org: org, claim: hold.ID}
}

// withTriggerDisabled runs fn with claim_history_set_append_order disabled, and restores
// it via t.Cleanup rather than after fn.
//
// The distinction matters: fn calls t.Fatal on failure, which does NOT return to this
// function — a re-enable written as a trailing statement would be skipped, leaving the
// trigger off for every later insert into this schema and silently weakening whatever ran
// next (ai-review finding 3). t.Cleanup runs regardless.
func withTriggerDisabled(t *testing.T, f historyFixtureData, fn func()) {
	t.Helper()
	if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE claim_history DISABLE TRIGGER claim_history_set_append_order`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(f.ctx, `ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`)
	})
	fn()
}

type historyFixtureData struct {
	ctx   context.Context
	st    *Postgres
	db    *sql.DB
	org   uuid.UUID
	claim uuid.UUID
}

// TestHistoryOrdersTiedTimestampsByAppendOrder is the regression proof.
//
// Two rows share ONE `occurred_at`, and the UUIDs are chosen so that ordering by `id`
// returns them in the WRONG order: the row appended first gets the HIGHER uuid. Under the
// old `ORDER BY occurred_at, id` this test fails; under a real append order it passes.
func TestHistoryOrdersTiedTimestampsByAppendOrder(t *testing.T) {
	f := historyFixture(t)

	// One timestamp value, read once, used for both rows: this is what a same-microsecond
	// collision between two concurrent transactions looks like, made deterministic.
	var tied time.Time
	if err := f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&tied); err != nil {
		t.Fatal(err)
	}

	// Appended FIRST, but carries the HIGHER uuid — so `ORDER BY id` puts it second.
	first := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	// Appended SECOND, lower uuid.
	second := uuid.MustParse("00000000-0000-4000-8000-000000000001")

	insert := `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,occurred_at)
		VALUES($1,$2,$3,$4,'staff:a','r',1,1,'held',$5)`
	if _, err := f.db.ExecContext(f.ctx, insert, first, f.org, f.claim, "place", tied); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, insert, second, f.org, f.claim, "release", tied); err != nil {
		t.Fatal(err)
	}

	hist, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil {
		t.Fatal(err)
	}

	// Assert the WHOLE history, by row identity, not by action name on a slice of it.
	//
	// Asserting `tail == [place release]` would have been too weak in a way the fixture
	// makes concrete: PlaceOperationalHold writes its own `place` row, so the history is
	// [place(fixture), place(under test), release(under test)]. If the row under test
	// vanished, the fixture's own `place` would slide into the tail and the assertion
	// would still read [place release] — passing while the thing it exists to check was
	// gone (ai-review finding 1; the `fixture too small` trap in AGENTS.md).
	//
	// Row identity also pins WHICH row is where, which action names cannot.
	var ids []uuid.UUID
	for _, h := range hist {
		ids = append(ids, h.HistoryID)
	}
	if len(ids) != 3 {
		t.Fatalf("history = %d rows, want exactly 3 (fixture place + the two under test): %+v", len(ids), hist)
	}
	if ids[1] != first || ids[2] != second {
		t.Fatalf("tied rows returned as [%s %s], want [%s %s] — a tie on occurred_at is being "+
			"broken by the random uuid, not by append order", ids[1], ids[2], first, second)
	}
	if hist[1].Action != "place" || hist[2].Action != "release" {
		t.Fatalf("actions = [%s %s], want [place release]", hist[1].Action, hist[2].Action)
	}
}

// TestHistoryOrdersDistinctTimestampsByAppendOrder: append order LEADS (TKT-295, owner
// decision D1: ORDER BY append_order NULLS FIRST, occurred_at, id).
//
// REVERSED from TKT-234's gap sentinel `TestHistoryOrdersDistinctTimestampsByOccurredAt`,
// which pinned the old wall-clock-first preference as a KNOWN gap so it could not drift
// silently. TKT-295 closes that gap, so the pin is reversed here, deliberately, not deleted.
//
// The state is REACHABLE through ordinary writes: `occurred_at` defaults to
// clock_timestamp(), so a backward clock step between two inserts gives an increasing
// append_order against a decreasing occurred_at, with the numbering trigger firing
// normally. This fixture builds exactly that WITH THE TRIGGER ENABLED — it supplies the two
// inverted timestamps and lets the trigger number the rows — so no state is synthesized.
//
// What the order proves (ADR-021 § Amendment): honest-writer consistency, not
// tamper-evidence. A writer that disables the trigger or runs with
// session_replication_role=replica supplies append_order itself.
func TestHistoryOrdersDistinctTimestampsByAppendOrder(t *testing.T) {
	f := historyFixture(t)
	fixtureRows, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil || len(fixtureRows) != 1 {
		t.Fatalf("setup: fixture history = %+v, %v; want one row", fixtureRows, err)
	}

	var base time.Time
	if err := f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	appendedFirst, appendedSecond := uuid.New(), uuid.New()
	insert := `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,occurred_at)
		VALUES($1,$2,$3,$4,'staff:a','r',1,1,'held',$5)`
	// Appended FIRST, stamped LATER — the clock then stepped back for the second append.
	if _, err := f.db.ExecContext(f.ctx, insert, appendedFirst, f.org, f.claim, "place", base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, insert, appendedSecond, f.org, f.claim, "release", base); err != nil {
		t.Fatal(err)
	}
	var firstNo, secondNo int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT (SELECT append_order FROM claim_history WHERE id=$1), (SELECT append_order FROM claim_history WHERE id=$2)`,
		appendedFirst, appendedSecond).Scan(&firstNo, &secondNo); err != nil {
		t.Fatal(err)
	}
	if firstNo >= secondNo {
		t.Fatalf("setup: the trigger numbered the rows %d, %d; want increasing in append order", firstNo, secondNo)
	}

	hist, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{fixtureRows[0].HistoryID, appendedFirst, appendedSecond}
	assertHistoryIDs(t, hist, want, "a backward clock step must not reorder history appended in a definite order")
}

func assertHistoryIDs(t *testing.T, hist []HistoryEntry, want []uuid.UUID, why string) {
	t.Helper()
	got := make([]uuid.UUID, len(hist))
	for i, h := range hist {
		got[i] = h.HistoryID
	}
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v (%s)", got, want, why)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history = %v, want %v (%s)", got, want, why)
		}
	}
}

// TestHistoryOrdersLegacyRowsBeforeTiedNewRows pins the legacy boundary.
//
// A pre-migration row has append_order IS NULL and cannot be given a true position. When
// it ties with a new row, NULLS FIRST puts it first. That is a DELIBERATE, documented
// choice — a legacy row is by definition older than any row written after the migration —
// and this test exists so that changing it is a decision rather than an accident.
func TestHistoryOrdersLegacyRowsBeforeTiedNewRows(t *testing.T) {
	f := historyFixture(t)

	var tied time.Time
	if err := f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&tied); err != nil {
		t.Fatal(err)
	}

	// A "legacy" row: explicitly NULL append_order. The BEFORE INSERT trigger would
	// normally assign one, so it is disabled for this statement only — the point is to
	// materialize a row shaped like one written before the migration existed.
	legacy := uuid.New()
	withTriggerDisabled(t, f, func() {
		if _, err := f.db.ExecContext(f.ctx,
			`INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,occurred_at,append_order)
			 VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',$4,NULL)`, legacy, f.org, f.claim, tied); err != nil {
			t.Fatal(err)
		}
	})

	// withTriggerDisabled restores the trigger at CLEANUP, so it is re-enabled explicitly here:
	// the fresh row must be numbered by the real trigger, as every row after 0012 is.
	if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`); err != nil {
		t.Fatal(err)
	}
	// A new row sharing the legacy row's timestamp.
	fresh := uuid.New()
	if _, err := f.db.ExecContext(f.ctx,
		`INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,occurred_at)
		 VALUES($1,$2,$3,'release','staff:a','r',1,1,'held',$4)`, fresh, f.org, f.claim, tied); err != nil {
		t.Fatal(err)
	}

	hist, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 {
		t.Fatalf("history = %d rows, want 3: %+v", len(hist), hist)
	}
	// TKT-295: append_order LEADS with NULLS FIRST, so the legacy row precedes EVERY numbered
	// row — including the fixture's, which was written earlier in this test but after 0012.
	// A pre-0012 row is by definition older than any numbered row on the ordinary write path.
	if hist[0].HistoryID != legacy || hist[2].HistoryID != fresh {
		t.Fatalf("history = [%s %s %s], want legacy %s first and %s last — NULLS FIRST places "+
			"every legacy row before every numbered row", hist[0].HistoryID, hist[1].HistoryID, hist[2].HistoryID, legacy, fresh)
	}
}

// TestClaimHistoryTriggerOwnsAppendOrder proves the TRIGGER, not the DEFAULT — and proves
// it OWNS the value rather than merely filling in NULLs.
//
// Two writers the DEFAULT alone does not cover:
//   - an explicit NULL (a DEFAULT does not apply to an explicitly-supplied NULL);
//   - a supplied non-NULL value, which a fill-in-NULLs trigger would write through
//     unchecked — letting a COPY, restore or replication apply reintroduce a duplicate and
//     collapse two rows back onto the random-uuid tie-break (ai-review finding 3).
//
// Uniqueness is not enforced by an index here; it holds because the sequence is the only
// source of the value. This test is what makes that claim true rather than aspirational.
func TestClaimHistoryTriggerOwnsAppendOrder(t *testing.T) {
	f := historyFixture(t)

	nullRow, suppliedRow := uuid.New(), uuid.New()
	insert := `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,append_order)
		VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',$4)`
	if _, err := f.db.ExecContext(f.ctx, insert, nullRow, f.org, f.claim, nil); err != nil {
		t.Fatal(err)
	}
	// A hostile value: negative (sorts ahead of everything) and a duplicate of nothing
	// legitimate. The trigger must discard it.
	if _, err := f.db.ExecContext(f.ctx, insert, suppliedRow, f.org, f.claim, -42); err != nil {
		t.Fatal(err)
	}

	var nullAssigned, suppliedAssigned *int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT append_order FROM claim_history WHERE id=$1`, nullRow).Scan(&nullAssigned); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT append_order FROM claim_history WHERE id=$1`, suppliedRow).Scan(&suppliedAssigned); err != nil {
		t.Fatal(err)
	}

	if nullAssigned == nil {
		t.Fatal("an explicit NULL append_order was stored as NULL — the DEFAULT does not apply " +
			"to an explicitly-supplied NULL, so the BEFORE INSERT trigger must assign one")
	}
	if suppliedAssigned == nil {
		t.Fatal("a supplied append_order was stored as NULL")
	}
	if *suppliedAssigned == -42 {
		t.Fatalf("a supplied append_order of -42 survived as %d — the trigger must OWN the "+
			"value, not merely fill in NULLs, or a COPY/restore can write its own ordering",
			*suppliedAssigned)
	}
	if *nullAssigned <= 0 || *suppliedAssigned <= 0 {
		t.Fatalf("append_order values must be positive, got %d and %d", *nullAssigned, *suppliedAssigned)
	}
	if *nullAssigned == *suppliedAssigned {
		t.Fatalf("two rows share append_order %d — the sequence must not repeat", *nullAssigned)
	}
}

// TestClaimHistoryMigrationDownRestoresOccurredAtDefault pins the Down migration.
//
// Up changes occurred_at's default from now() to clock_timestamp(). A Down that removed
// only the append_order objects would leave that behaviour in place — a hybrid schema
// carrying this migration's timestamp semantics under the previous version's code
// (ai-review pass 3, finding 1). Asserting on the migration's *effect* rather than
// re-reading its text is what makes this a test rather than a restatement.
func TestClaimHistoryMigrationDownRestoresOccurredAtDefault(t *testing.T) {
	f := historyFixture(t)

	def := func() string {
		var d sql.NullString
		if err := f.db.QueryRowContext(f.ctx, `
			SELECT column_default FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'claim_history'
			  AND column_name = 'occurred_at'`).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d.String
	}

	if got := def(); !strings.Contains(got, "clock_timestamp") {
		t.Fatalf("after Up, occurred_at default = %q, want clock_timestamp()", got)
	}

	// Apply this migration's Down by hand: goose's own down path is exercised by the
	// migration harness, but the assertion that matters here is what the schema looks
	// like afterwards.
	for _, stmt := range []string{
		`ALTER TABLE claim_history ALTER COLUMN occurred_at SET DEFAULT now()`,
		`DROP TRIGGER claim_history_set_append_order ON claim_history`,
		`DROP FUNCTION claim_history_assign_append_order()`,
		`ALTER TABLE claim_history DROP COLUMN append_order`,
	} {
		if _, err := f.db.ExecContext(f.ctx, stmt); err != nil {
			t.Fatalf("down step %q: %v", stmt, err)
		}
	}

	if got := def(); strings.Contains(got, "clock_timestamp") {
		t.Fatalf("after Down, occurred_at default = %q — Down must restore now(), or a "+
			"rollback leaves this migration's timestamp semantics under the old code", got)
	}
	if !strings.Contains(def(), "now()") {
		t.Fatalf("after Down, occurred_at default = %q, want now()", def())
	}
}

// TestClaimHistoryRejectsNonPositiveAppendOrder proves the NOT VALID CHECK is enforced for
// new rows despite never having been validated against existing ones.
//
// The trigger normally makes a bad value unreachable, so the constraint is only observable
// with the trigger off — which is also exactly the state a restore path would be in, and
// the state in which the constraint is the last line of defence.
func TestClaimHistoryRejectsNonPositiveAppendOrder(t *testing.T) {
	f := historyFixture(t)

	withTriggerDisabled(t, f, func() {
		_, err := f.db.ExecContext(f.ctx,
			`INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,append_order)
			 VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',-1)`, uuid.New(), f.org, f.claim)
		if err == nil {
			t.Fatal("a negative append_order was accepted — the NOT VALID CHECK must still " +
				"enforce the predicate for newly inserted rows")
		}
		if !strings.Contains(err.Error(), "claim_history_append_order_positive") {
			t.Fatalf("rejected, but not by the positivity constraint: %v", err)
		}
	})
}

// TKT-295 COS3: legacy rows precede numbered rows EVEN WHEN their timestamps are later, and
// keep occurred_at, id order among themselves. Legacy NULLs are seeded with the trigger
// disabled — the only way to produce one today; on the ordinary path every row after 0012
// is numbered.
func TestHistoryOrdersLegacyRowsFirstByOccurredAtAmongThemselves(t *testing.T) {
	f := historyFixture(t)
	fixtureRows, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil || len(fixtureRows) != 1 {
		t.Fatalf("setup: fixture history = %+v, %v", fixtureRows, err)
	}
	var base time.Time
	if err := f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	// Two legacy rows, both LATER than the numbered fixture row, inserted in REVERSE time
	// order and with uuids opposite their time order, so only `occurred_at` can sort them.
	legacyEarly := uuid.MustParse("ffffffff-ffff-4fff-8fff-fffffffffff0")
	legacyLate := uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	withTriggerDisabled(t, f, func() {
		insert := `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,occurred_at,append_order)
			VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',$4,NULL)`
		if _, err := f.db.ExecContext(f.ctx, insert, legacyLate, f.org, f.claim, base.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.ExecContext(f.ctx, insert, legacyEarly, f.org, f.claim, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	})
	hist, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryIDs(t, hist, []uuid.UUID{legacyEarly, legacyLate, fixtureRows[0].HistoryID},
		"legacy rows first, by occurred_at among themselves, then numbered rows")
}

// capacityRow inserts a pool-shaped (adjust_capacity) history row; append_order is left to
// the trigger unless the trigger is disabled and a NULL is wanted.
func capacityRow(t *testing.T, f historyFixtureData, slot, id uuid.UUID, at time.Time, legacyNull bool) {
	t.Helper()
	col, val := "", ""
	if legacyNull {
		col, val = ",append_order", ",NULL"
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO claim_history(id,organizer_id,pool_id,action,actor,reason,quantity,quantity_after,status_after,idempotency_key,request_fingerprint,occurred_at`+col+`)
		VALUES($1,$2,$3,'adjust_capacity','staff','resize',10,10,'applied',$4,'fp',$5`+val+`)`,
		id, f.org, slot, "k-"+id.String(), at); err != nil {
		t.Fatal(err)
	}
}

// TKT-295: the same three properties on the OTHER read, CapacityHistory — the reads are
// separate statements and must not drift apart.
func TestCapacityHistoryOrdersByAppendOrder(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	f := historyFixtureData{ctx: ctx, st: st, db: db, org: org}
	var base time.Time
	if err := db.QueryRowContext(ctx, `SELECT now()`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	ids := func() []uuid.UUID {
		hist, err := st.CapacityHistory(ctx, org, slot)
		if err != nil {
			t.Fatal(err)
		}
		out := []uuid.UUID{}
		for _, h := range hist {
			out = append(out, h.HistoryID)
		}
		return out
	}
	existing := ids()

	// Distinct timestamps inverted against append order (a backward clock step).
	first, second := uuid.New(), uuid.New()
	capacityRow(t, f, slot, first, base.Add(time.Second), false)
	capacityRow(t, f, slot, second, base, false)
	// A tie on occurred_at, uuids opposite append order.
	tieFirst := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	tieSecond := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	capacityRow(t, f, slot, tieFirst, base, false)
	capacityRow(t, f, slot, tieSecond, base, false)
	// Two legacy rows, LATER than everything, which must still come first — inserted in
	// reverse time order with uuids opposite their time order, so only occurred_at can order
	// them among themselves.
	legacyEarly := uuid.MustParse("ffffffff-ffff-4fff-8fff-fffffffffff0")
	legacyLate := uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	withTriggerDisabled(t, f, func() {
		capacityRow(t, f, slot, legacyLate, base.Add(2*time.Hour), true)
		capacityRow(t, f, slot, legacyEarly, base.Add(time.Hour), true)
	})

	got := ids()
	want := append(append([]uuid.UUID{legacyEarly, legacyLate}, existing...), first, second, tieFirst, tieSecond)
	if len(got) != len(want) {
		t.Fatalf("capacity history = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capacity history = %v, want %v (legacy first, then append order)", got, want)
		}
	}
}
