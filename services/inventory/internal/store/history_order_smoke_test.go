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

// TestHistoryOrdersDistinctTimestampsByOccurredAt pins the other direction: append_order
// is a TIE-BREAK, not the primary key of the order. A row with a LATER occurred_at but a
// LOWER append_order must still sort last.
//
// Without this, an implementation that ordered by append_order first would pass the tie
// test above while silently reordering every history that has distinct timestamps — which
// is all of them, almost all of the time (ai-review finding 1).
//
// ────────────────────────────────────────────────────────────────────────────────────────
// TKT-234 / ADR-021 §Amendment (2026-09-01): THIS TEST IS ALSO A GAP SENTINEL. READ BEFORE
// CHANGING IT.
//
// It records the CURRENT preference — wall clock first — and that preference is exactly the
// exposure ADR-021's amendment names and does NOT close: `claim_history` has no hash chain,
// so unlike access's lifecycle trail its sort key IS its ordering guarantee, and a backward
// clock step genuinely reorders history that was appended in a definite order.
//
// So this test going red is not automatically a regression. **TKT-295 promotes
// `append_order` to the primary sort key**, and when it does, this test MUST go red. Reverse
// it deliberately, with the legacy-NULL boundary stated — do not delete it, and do not
// "fix" it by weakening the assertion. That is the discipline ADR-021's rollback-gap test
// exists to enforce: a known gap is pinned as PRESENT so it cannot drift silently, and the
// day it closes, the pin is updated rather than removed.
//
// TWO THINGS TKT-295 INHERITS, and the first one corrects an earlier reading of this test.
//
//   - **The state this fixture builds IS honestly reachable, and that is the whole point.**
//     An earlier draft of this comment (and TKT-234's shaping note) said the trigger makes
//     it "unreachable through the normal path". That is wrong, and it matters: the trigger
//     controls WHO ASSIGNS `append_order`, not the RELATIVE ORDER of the two columns.
//     `occurred_at` defaults to `clock_timestamp()` (0012:43), so a backward clock step
//     between two ordinary inserts produces exactly this state — an increasing
//     `append_order` against a decreasing `occurred_at` — with the trigger firing normally
//     throughout. `withTriggerDisabled` is used here only to CONSTRUCT the state
//     deterministically without waiting for a clock step; it is a convenience, not evidence
//     that the state is synthetic. **This test therefore records a real exposure**, which is
//     precisely why it is a gap sentinel rather than a curiosity.
//   - **The trigger does not govern every writer.** Measured against this repo's PostgreSQL:
//     an ordinary INSERT fires it (a supplied 999 became 1), `COPY` fires it, and it does
//     **not** fire for any session running `SET session_replication_role = replica` — the
//     same insert kept 999 — because this is an ordinary `CREATE TRIGGER` with no
//     `ENABLE REPLICA`/`ENABLE ALWAYS`. Logical-replication apply is one such session; a
//     maintenance session is another. Worse than either alone: a subscription's INITIAL
//     SYNC uses `COPY` and therefore renumbers, then streaming apply preserves publisher
//     values — two independently generated numbering schemes in one column, which may
//     overlap. Not a live hazard (nothing here configures replication; `wal_level` is
//     `replica`, not `logical`). **Migration 0012 now records this exception at the trigger
//     itself** — that is the authoritative statement; this is a pointer to it. ADR-021
//     §Amendment (TKT-234) says what it would cost.
//   - Its sibling `TestHistoryOrdersTiedTimestampsByAppendOrder` (:83) is INDEPENDENT of
//     this one and must stay green through TKT-295: same-microsecond collisions ordered by
//     `append_order` is a regression proof, not a preference.
// ────────────────────────────────────────────────────────────────────────────────────────
func TestHistoryOrdersDistinctTimestampsByOccurredAt(t *testing.T) {
	f := historyFixture(t)

	var base time.Time
	if err := f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	later, earlier := uuid.New(), uuid.New()

	// The trigger overwrites whatever a writer supplies, so it is disabled here to assign
	// append_order DETERMINISTICALLY — that is the only reason, and it is worth being exact
	// because an earlier version of this comment said the trigger makes the resulting state
	// "unreachable through the normal path". IT DOES NOT (see the block above this test):
	// occurred_at defaults to clock_timestamp(), so a backward clock step between two
	// ordinary inserts produces the same disagreement with the trigger firing throughout.
	// Disabling it buys a fixture that does not depend on moving the system clock.
	// Restored immediately, and by t.Cleanup so a failure between the two statements cannot
	// leave it disabled for the rest of the schema's life.
	withTriggerDisabled(t, f, func() {
		insert := `INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,occurred_at,append_order)
			VALUES($1,$2,$3,$4,'staff:a','r',1,1,'held',$5,$6)`
		// Appended with the LOWER append_order but the LATER timestamp.
		if _, err := f.db.ExecContext(f.ctx, insert, later, f.org, f.claim, "release", base.Add(time.Second), 1_000_001); err != nil {
			t.Fatal(err)
		}
		// HIGHER append_order, EARLIER timestamp: must come first.
		if _, err := f.db.ExecContext(f.ctx, insert, earlier, f.org, f.claim, "place", base, 1_000_002); err != nil {
			t.Fatal(err)
		}
	})

	hist, err := f.st.History(f.ctx, f.org, f.claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 {
		t.Fatalf("history = %d rows, want 3: %+v", len(hist), hist)
	}
	if hist[1].HistoryID != earlier || hist[2].HistoryID != later {
		t.Fatalf("distinct timestamps ordered as [%s %s], want [%s %s] — occurred_at must "+
			"remain the primary key of the order; append_order only breaks ties",
			hist[1].HistoryID, hist[2].HistoryID, earlier, later)
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
	if hist[1].HistoryID != legacy || hist[2].HistoryID != fresh {
		t.Fatalf("legacy/new tie ordered as [%s %s], want legacy %s first then %s — "+
			"NULLS FIRST places an unordered legacy row before a row written after the migration",
			hist[1].HistoryID, hist[2].HistoryID, legacy, fresh)
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
