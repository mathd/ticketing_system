//go:build smoke

package store

import (
	"context"
	"database/sql"
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

	// The fixture's own PlaceOperationalHold wrote a 'create'/'place' row first; the two
	// rows under test are the last two. Assert on the tail so the fixture's own history
	// does not have to be enumerated here.
	if len(hist) < 2 {
		t.Fatalf("history too short: %+v", hist)
	}
	tail := hist[len(hist)-2:]
	if tail[0].Action != "place" || tail[1].Action != "release" {
		t.Fatalf("tied rows returned in append order = [%s %s], want [place release] — "+
			"a tie on occurred_at is being broken by the random uuid, not by append order",
			tail[0].Action, tail[1].Action)
	}
}

// TestClaimHistoryAssignsAppendOrderOnExplicitNull proves the TRIGGER, not the DEFAULT.
//
// A column DEFAULT does not apply when a writer names the column and passes NULL — verified
// directly against PostgreSQL at plan-review. Without a BEFORE INSERT trigger such a row
// would be stored unordered, and the ordering guarantee would have a silent hole that no
// existing writer happens to step in. This test is the only thing that proves the trigger
// does anything.
func TestClaimHistoryAssignsAppendOrderOnExplicitNull(t *testing.T) {
	f := historyFixture(t)

	id := uuid.New()
	if _, err := f.db.ExecContext(f.ctx,
		`INSERT INTO claim_history(id,organizer_id,claim_id,action,actor,reason,quantity,quantity_after,status_after,append_order)
		 VALUES($1,$2,$3,'place','staff:a','r',1,1,'held',NULL)`, id, f.org, f.claim); err != nil {
		t.Fatal(err)
	}

	var appendOrder *int64
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT append_order FROM claim_history WHERE id=$1`, id).Scan(&appendOrder); err != nil {
		t.Fatal(err)
	}
	if appendOrder == nil {
		t.Fatal("an explicit NULL append_order was stored as NULL — the DEFAULT does not apply " +
			"to an explicitly-supplied NULL, so a BEFORE INSERT trigger must assign one")
	}
	if *appendOrder <= 0 {
		t.Fatalf("append_order = %d, want > 0", *appendOrder)
	}
}
