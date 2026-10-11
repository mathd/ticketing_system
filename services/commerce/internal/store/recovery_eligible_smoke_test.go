//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests check the population behind the TKT-321 gauge. The population is the set of
// unparked orders that ClaimStuckOrders selects now. Each test uses its own schema from
// migratedDB. The shared store-test database is not used, because an absolute count is valid
// only when no other rows exist.
//
// A count alone does not prove a population. Two different sets can have the same size. The
// membership tests also compare the set that the real claim returns. Every expected value comes
// from the requirement, not from a run of the code.

// eligibleRow is one order, as the fixture writes it. Every field is set. No column default
// stands in for a value that the test must choose.
type eligibleRow struct {
	status        string
	createdAt     time.Time
	updatedAt     time.Time
	nextAttemptAt time.Time
	leaseUntil    sql.NullTime
	parkedAt      sql.NullTime
	// claimID is the recovery claim token. A claimant that crashed after its claim leaves it
	// set, with the lease that will expire.
	claimID uuid.NullUUID
}

// eligibleBase is claimable on every predicate, with margin on each. Its three timestamps are
// different. So the age origin is visible: a wrong origin gives a different age.
func eligibleBase(now time.Time) eligibleRow {
	return eligibleRow{
		status:        "created",
		createdAt:     now.Add(-2 * time.Hour),
		updatedAt:     now.Add(-10 * time.Minute),
		nextAttemptAt: now.Add(-time.Hour),
	}
}

// dbExec is the surface a fixture needs. *sql.DB and *sql.Tx both satisfy it. One seed function
// serves committed fixtures and transaction-scoped cases.
type dbExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbNow reads the database clock. Age bounds use database time, not the clock of the test process.
func dbNow(t *testing.T, ctx context.Context, x dbExec) time.Time {
	t.Helper()
	var now time.Time
	if err := x.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// insertEligibleRow seeds one reservation and one order. It returns the order id.
func insertEligibleRow(t *testing.T, ctx context.Context, x dbExec, r eligibleRow) uuid.UUID {
	t.Helper()
	resID, orderID := uuid.New(), uuid.New()
	if _, err := x.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1250,2500,2500,'EUR','finalizing')`,
		resID, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := x.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,
		                   created_at,updated_at,recovery_next_attempt_at,recovery_lease_until,recovery_parked_at,
		                   recovery_claim_id)
		VALUES($1,$2,$3,$4,'fingerprint',$5,$6,$7,$8,$9,$10)`,
		orderID, resID, r.status, "eligible-"+uuid.NewString(),
		r.createdAt, r.updatedAt, r.nextAttemptAt, r.leaseUntil, r.parkedAt, r.claimID); err != nil {
		t.Fatal(err)
	}
	return orderID
}

// clearOrders empties orders and reservations. Orders go first, because they reference reservations.
func clearOrders(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `DELETE FROM orders`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM reservations`); err != nil {
		t.Fatal(err)
	}
}

// requireStored checks a fixture value as the database stored it. The statement takes the order id
// as $1. A failure means the seed does not hold the value that the test assumes.
func requireStored(t *testing.T, ctx context.Context, x dbExec, what, statement string, id uuid.UUID) {
	t.Helper()
	var ok bool
	if err := x.QueryRowContext(ctx, statement, id).Scan(&ok); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			t.Errorf("fixture: %s is missing from the table", what)
			return
		}
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("fixture: %s is not stored as the test assumes (%s)", what, statement)
	}
}

// ageBounds returns the range for the age of a timestamp that lies offset before `before`. The read
// ran between `before` and `after`. The upper bound adds the elapsed database time, plus one second
// for the rounding in extract(epoch)::bigint.
func ageBounds(offset time.Duration, before, after time.Time) (lo, hi int64) {
	lo = int64(offset / time.Second)
	hi = lo + int64(after.Sub(before)/time.Second) + 1
	return lo, hi
}

// orderIDs lists the order ids that a claim returned.
func orderIDs(claimed []StuckOrder) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(claimed))
	for _, s := range claimed {
		ids = append(ids, s.OrderID)
	}
	return ids
}

// claimInRolledBackTx runs the real ClaimStuckOrders in a transaction that is rolled back. So the
// lease that the claim writes cannot change the next case. It returns the ids that the claim selected.
func claimInRolledBackTx(t *testing.T, ctx context.Context, db *sql.DB) []uuid.UUID {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	claimed, err := ClaimStuckOrders(ctx, tx, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return orderIDs(claimed)
}

// expectClaimed checks the claim against one candidate. The claim must select exactly that
// candidate when the candidate is eligible, and nothing when it is not.
func expectClaimed(t *testing.T, claimed []uuid.UUID, id uuid.UUID, eligible bool) {
	t.Helper()
	if !eligible {
		if len(claimed) != 0 {
			t.Errorf("the claim selected %v, want none", claimed)
		}
		return
	}
	if len(claimed) != 1 || claimed[0] != id {
		t.Errorf("the claim selected %v, want exactly %s", claimed, id)
	}
}

// promoteRow changes one field of an excluded row to its allowed value. The eligible count must
// then be two. The field is restored, and the count must return to one. If the seed is missing,
// the count stays at one, so the promotion fails.
func promoteRow(t *testing.T, ctx context.Context, db *sql.DB, name string, id uuid.UUID, set string, allowed, excluded any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, set, id, allowed); err != nil {
		t.Fatal(err)
	}
	promoted, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Count != 2 {
		t.Errorf("%s made eligible: count = %d, want 2", name, promoted.Count)
	}
	if _, err := db.ExecContext(ctx, set, id, excluded); err != nil {
		t.Fatal(err)
	}
	restored, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Count != 1 {
		t.Errorf("after restoring %s: count = %d, want 1", name, restored.Count)
	}
}

// COS1, six-row baseline (TKT-321).
//
// The baseline has one eligible row and five excluded rows. Each excluded row differs from the
// eligible row by one blocking field. Every other predicate holds for it. The count must be one,
// through the standalone read and through the combined read.
//
// Each excluded seed has its own promotion check. Only its blocking field changes, to the allowed
// value. The count must then be two.
func TestEligibleUnparkedBacklogSixRowBaseline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)

	before := dbNow(t, ctx, db)
	base := eligibleBase(before)
	eligibleID := insertEligibleRow(t, ctx, db, base)

	parked := base
	parked.parkedAt = sql.NullTime{Time: before.Add(-5 * time.Minute), Valid: true}
	parkedID := insertEligibleRow(t, ctx, db, parked)

	leased := base
	leased.leaseUntil = sql.NullTime{Time: before.Add(24 * time.Hour), Valid: true}
	leasedID := insertEligibleRow(t, ctx, db, leased)

	// Updated at the instant the test started. The row leaves the grace two minutes later, which
	// is more than the 90-second deadline of this test. So it cannot drift into the population
	// while the test runs.
	inGrace := base
	inGrace.updatedAt = before
	inGraceID := insertEligibleRow(t, ctx, db, inGrace)

	notDue := base
	notDue.nextAttemptAt = before.Add(24 * time.Hour)
	notDueID := insertEligibleRow(t, ctx, db, notDue)

	outside := base
	outside.status = "completed"
	outsideID := insertEligibleRow(t, ctx, db, outside)

	// Check the stored fixture before any count is read.
	requireStored(t, ctx, db, "eligible seed",
		`SELECT status = 'created' AND recovery_parked_at IS NULL AND recovery_lease_until IS NULL
		        AND recovery_next_attempt_at <= now() AND updated_at < now() - interval '2 minutes'
		   FROM orders WHERE id=$1`, eligibleID)
	requireStored(t, ctx, db, "parked seed",
		`SELECT recovery_parked_at IS NOT NULL FROM orders WHERE id=$1`, parkedID)
	requireStored(t, ctx, db, "live-lease seed",
		`SELECT recovery_lease_until > now() FROM orders WHERE id=$1`, leasedID)
	requireStored(t, ctx, db, "inside-grace seed",
		`SELECT updated_at >= now() - interval '2 minutes' FROM orders WHERE id=$1`, inGraceID)
	requireStored(t, ctx, db, "not-yet-due seed",
		`SELECT recovery_next_attempt_at > now() FROM orders WHERE id=$1`, notDueID)
	requireStored(t, ctx, db, "outside-status seed",
		`SELECT status = 'completed' FROM orders WHERE id=$1`, outsideID)

	standalone, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := ReadRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// Read after both reads, so the upper bound covers the time they took.
	after := dbNow(t, ctx, db)

	if standalone.Count != 1 {
		t.Errorf("standalone eligible count = %d, want 1", standalone.Count)
	}
	// The age is measured from recovery_next_attempt_at, one hour back. From updated_at it would be
	// ten minutes, and from created_at two hours. Neither is inside the bounds.
	lo, hi := ageBounds(time.Hour, before, after)
	if standalone.OldestAgeSeconds < lo || standalone.OldestAgeSeconds > hi {
		t.Errorf("standalone oldest age = %ds, want within [%d, %d] (from recovery_next_attempt_at)",
			standalone.OldestAgeSeconds, lo, hi)
	}
	if combined.EligibleUnparked.Count != 1 {
		t.Errorf("combined EligibleUnparked.Count = %d, want 1", combined.EligibleUnparked.Count)
	}
	if combined.EligibleUnparked.OldestAgeSeconds < lo || combined.EligibleUnparked.OldestAgeSeconds > hi {
		t.Errorf("combined EligibleUnparked.OldestAgeSeconds = %ds, want within [%d, %d]",
			combined.EligibleUnparked.OldestAgeSeconds, lo, hi)
	}
	// The parked population must not change. It holds the one parked seed and nothing else.
	if combined.Parked != 1 || combined.ReconciliationRequired != 0 || combined.Total != 1 {
		t.Errorf("combined parked split = (parked %d, reconciliation %d, total %d), want (1, 0, 1)",
			combined.Parked, combined.ReconciliationRequired, combined.Total)
	}
	plo, phi := ageBounds(5*time.Minute, before, after)
	if combined.OldestAgeSeconds < plo || combined.OldestAgeSeconds > phi {
		t.Errorf("combined parked oldest age = %ds, want within [%d, %d] (from recovery_parked_at)",
			combined.OldestAgeSeconds, plo, phi)
	}

	// The real claim selects exactly the eligible row, and its size matches the count.
	claimed := claimInRolledBackTx(t, ctx, db)
	expectClaimed(t, claimed, eligibleID, true)
	if int64(len(claimed)) != standalone.Count {
		t.Errorf("the count (%d) and the claim (%d) disagree on the population's size", standalone.Count, len(claimed))
	}

	// One promotion per excluded seed. Each one changes only the field that blocks its row.
	promoteRow(t, ctx, db, "parked row", parkedID,
		`UPDATE orders SET recovery_parked_at=$2 WHERE id=$1`, nil, parked.parkedAt.Time)
	promoteRow(t, ctx, db, "live-lease row", leasedID,
		`UPDATE orders SET recovery_lease_until=$2 WHERE id=$1`, nil, leased.leaseUntil.Time)
	promoteRow(t, ctx, db, "inside-grace row", inGraceID,
		`UPDATE orders SET updated_at=$2 WHERE id=$1`, base.updatedAt, inGrace.updatedAt)
	promoteRow(t, ctx, db, "not-yet-due row", notDueID,
		`UPDATE orders SET recovery_next_attempt_at=$2 WHERE id=$1`, base.nextAttemptAt, notDue.nextAttemptAt)
	promoteRow(t, ctx, db, "outside-status row", outsideID,
		`UPDATE orders SET status=$2 WHERE id=$1`, "created", "completed")
}

// COS1, the age and the empty population (TKT-321).
//
// The age is the age of the oldest eligible row. Two eligible rows of different ages catch a
// minimum taken where a maximum belongs. A live-lease row that is older than both must not set the
// age, because it is outside the population. An empty population reads zero.
func TestEligibleUnparkedAgeIsTheOldestEligibleRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)

	empty, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if empty != (EligibleUnparkedRecoveryBacklog{}) {
		t.Errorf("empty population reads %+v, want zero", empty)
	}

	before := dbNow(t, ctx, db)
	base := eligibleBase(before)

	younger := base
	younger.nextAttemptAt = before.Add(-time.Hour)
	insertEligibleRow(t, ctx, db, younger)

	older := base
	older.nextAttemptAt = before.Add(-2 * time.Hour)
	insertEligibleRow(t, ctx, db, older)

	locked := base
	locked.nextAttemptAt = before.Add(-5 * time.Hour)
	locked.leaseUntil = sql.NullTime{Time: before.Add(24 * time.Hour), Valid: true}
	lockedID := insertEligibleRow(t, ctx, db, locked)
	requireStored(t, ctx, db, "live-lease seed",
		`SELECT recovery_lease_until > now() FROM orders WHERE id=$1`, lockedID)

	got, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	after := dbNow(t, ctx, db)

	if got.Count != 2 {
		t.Errorf("eligible count = %d, want 2", got.Count)
	}
	lo, hi := ageBounds(2*time.Hour, before, after)
	if got.OldestAgeSeconds < lo || got.OldestAgeSeconds > hi {
		t.Errorf("oldest eligible age = %ds, want within [%d, %d]: the two-hour row, not the one-hour row (minimum), and not the five-hour locked row (outside the population)",
			got.OldestAgeSeconds, lo, hi)
	}

	// The locked seed must earn its place. Without this step, deleting it changes nothing above.
	// When its lease expires it joins the population, and then it is the oldest row, so the age
	// becomes five hours. If the seed is missing, the count stays at two and the age at two hours.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET recovery_lease_until=NULL WHERE id=$1`, lockedID); err != nil {
		t.Fatal(err)
	}
	unlocked, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	afterUnlock := dbNow(t, ctx, db)
	if unlocked.Count != 3 {
		t.Errorf("after the lease is cleared: eligible count = %d, want 3", unlocked.Count)
	}
	ulo, uhi := ageBounds(5*time.Hour, before, afterUnlock)
	if unlocked.OldestAgeSeconds < ulo || unlocked.OldestAgeSeconds > uhi {
		t.Errorf("after the lease is cleared: oldest eligible age = %ds, want within [%d, %d] (the five-hour row)",
			unlocked.OldestAgeSeconds, ulo, uhi)
	}
}

// COS1, membership by input class (TKT-321).
//
// A count can match while the sets differ. So each class is seeded alone. The count must equal the
// expected value. The real claim, in a rolled-back transaction, must select the candidate only when
// the class is eligible.
//
// Each other class differs from the eligible base by one field. So a predicate that is dropped or
// narrowed changes the answer for the class that tests it.
func TestEligibleUnparkedMembershipByInputClass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)

	classes := []struct {
		name     string
		change   func(r *eligibleRow, now time.Time)
		eligible bool
	}{
		{"base: created, lease NULL, due past, outside grace", func(*eligibleRow, time.Time) {}, true},
		{"status payment_unknown", func(r *eligibleRow, _ time.Time) { r.status = "payment_unknown" }, true},
		{"status confirmation_pending", func(r *eligibleRow, _ time.Time) { r.status = "confirmation_pending" }, true},
		{"status release_pending", func(r *eligibleRow, _ time.Time) { r.status = "release_pending" }, true},
		{"status reconciliation_required, unparked", func(r *eligibleRow, _ time.Time) { r.status = "reconciliation_required" }, true},
		{"status completed, outside the set", func(r *eligibleRow, _ time.Time) { r.status = "completed" }, false},
		{"status declined, outside the set", func(r *eligibleRow, _ time.Time) { r.status = "declined" }, false},
		{"status timeout, outside the set", func(r *eligibleRow, _ time.Time) { r.status = "timeout" }, false},
		{"status refunded, outside the set", func(r *eligibleRow, _ time.Time) { r.status = "refunded" }, false},
		{"lease expired", func(r *eligibleRow, now time.Time) {
			r.leaseUntil = sql.NullTime{Time: now.Add(-time.Minute), Valid: true}
		}, true},
		// The crash-after-claim shape: the claim token stays, and the lease has expired. The
		// predicate does not read the token, so the row is eligible again. The ADR-016 TKT-321
		// amendment says this work becomes visible after lease expiry; this case is that claim.
		{"lease expired, claim token retained", func(r *eligibleRow, now time.Time) {
			r.leaseUntil = sql.NullTime{Time: now.Add(-time.Minute), Valid: true}
			r.claimID = uuid.NullUUID{UUID: uuid.New(), Valid: true}
		}, true},
		{"lease live", func(r *eligibleRow, now time.Time) {
			r.leaseUntil = sql.NullTime{Time: now.Add(24 * time.Hour), Valid: true}
		}, false},
		{"due in the future", func(r *eligibleRow, now time.Time) {
			r.nextAttemptAt = now.Add(24 * time.Hour)
		}, false},
		{"inside the two-minute grace", func(r *eligibleRow, now time.Time) {
			r.updatedAt = now.Add(-30 * time.Second)
		}, false},
		{"parked", func(r *eligibleRow, now time.Time) {
			r.parkedAt = sql.NullTime{Time: now.Add(-5 * time.Minute), Valid: true}
		}, false},
		// Unparked reconciliation_required is queued compensation and is eligible (above). Parked,
		// it waits for a human and is not.
		{"status reconciliation_required, parked", func(r *eligibleRow, now time.Time) {
			r.status = "reconciliation_required"
			r.parkedAt = sql.NullTime{Time: now.Add(-5 * time.Minute), Valid: true}
		}, false},
	}
	for _, c := range classes {
		t.Run(c.name, func(t *testing.T) {
			clearOrders(t, ctx, db)
			now := dbNow(t, ctx, db)
			r := eligibleBase(now)
			c.change(&r, now)
			id := insertEligibleRow(t, ctx, db, r)

			got, err := ReadEligibleUnparkedRecoveryBacklog(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if c.eligible {
				want = 1
			}
			if got.Count != want {
				t.Errorf("eligible count = %d, want %d", got.Count, want)
			}

			claimed := claimInRolledBackTx(t, ctx, db)
			expectClaimed(t, claimed, id, c.eligible)
			if int64(len(claimed)) != got.Count {
				t.Errorf("the count (%d) and the claim (%d) disagree", got.Count, len(claimed))
			}
		})
	}
}

// COS1, exact boundaries (TKT-321).
//
// The fixture timestamps are read from the transaction clock, now(). In Postgres, now() is the start
// time of the transaction. So the fixture, the aggregate and the claim see one instant, and the
// equalities hold exactly. Each case is rolled back.
//
// Each boundary is tested on both sides, one microsecond apart. The lease and due checks use <=.
// The grace check uses <. Only the exact instant separates a strict check from a non-strict one.
// A grace period or a lease that differs by a little changes the answer on one side of these cases.
func TestEligibleUnparkedBoundariesAreExactInOneTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)

	const micro = time.Microsecond
	cases := []struct {
		name   string
		change func(r *eligibleRow, now time.Time)
		// holds must be true of the stored row. It shows that the value sits where the case says.
		holds    string
		eligible bool
	}{
		{"lease exactly now is expired",
			func(r *eligibleRow, now time.Time) { r.leaseUntil = sql.NullTime{Time: now, Valid: true} },
			`SELECT recovery_lease_until = now() FROM orders WHERE id=$1`, true},
		{"lease one microsecond before now is expired",
			func(r *eligibleRow, now time.Time) { r.leaseUntil = sql.NullTime{Time: now.Add(-micro), Valid: true} },
			`SELECT recovery_lease_until = now() - interval '1 microsecond' FROM orders WHERE id=$1`, true},
		{"lease one microsecond after now is live",
			func(r *eligibleRow, now time.Time) { r.leaseUntil = sql.NullTime{Time: now.Add(micro), Valid: true} },
			`SELECT recovery_lease_until = now() + interval '1 microsecond' FROM orders WHERE id=$1`, false},
		{"due exactly now is due",
			func(r *eligibleRow, now time.Time) { r.nextAttemptAt = now },
			`SELECT recovery_next_attempt_at = now() FROM orders WHERE id=$1`, true},
		{"due one microsecond before now is due",
			func(r *eligibleRow, now time.Time) { r.nextAttemptAt = now.Add(-micro) },
			`SELECT recovery_next_attempt_at = now() - interval '1 microsecond' FROM orders WHERE id=$1`, true},
		{"due one microsecond after now is not due",
			func(r *eligibleRow, now time.Time) { r.nextAttemptAt = now.Add(micro) },
			`SELECT recovery_next_attempt_at = now() + interval '1 microsecond' FROM orders WHERE id=$1`, false},
		{"updated exactly two minutes ago is inside grace",
			func(r *eligibleRow, now time.Time) { r.updatedAt = now.Add(-2 * time.Minute) },
			`SELECT updated_at = now() - interval '2 minutes' FROM orders WHERE id=$1`, false},
		{"updated one microsecond older than two minutes is outside grace",
			func(r *eligibleRow, now time.Time) { r.updatedAt = now.Add(-2*time.Minute - micro) },
			`SELECT updated_at = now() - interval '2 minutes' - interval '1 microsecond' FROM orders WHERE id=$1`, true},
		{"updated one microsecond newer than two minutes is inside grace",
			func(r *eligibleRow, now time.Time) { r.updatedAt = now.Add(-2*time.Minute + micro) },
			`SELECT updated_at = now() - interval '2 minutes' + interval '1 microsecond' FROM orders WHERE id=$1`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()

			now := dbNow(t, ctx, tx)
			r := eligibleBase(now)
			c.change(&r, now)
			id := insertEligibleRow(t, ctx, tx, r)
			requireStored(t, ctx, tx, c.name, c.holds, id)

			var count, age int64
			if err := tx.QueryRowContext(ctx, eligibleUnparkedRecoveryBacklogSQL).Scan(&count, &age); err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if c.eligible {
				want = 1
			}
			if count != want {
				t.Errorf("eligible count = %d, want %d", count, want)
			}

			claimed, err := ClaimStuckOrders(ctx, tx, 100, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			expectClaimed(t, orderIDs(claimed), id, c.eligible)
			if int64(len(claimed)) != count {
				t.Errorf("the count (%d) and the claim (%d) disagree", count, len(claimed))
			}
		})
	}
}
