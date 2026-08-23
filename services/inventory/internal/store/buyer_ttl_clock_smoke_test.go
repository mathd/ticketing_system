//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-148: a buyer TTL must be anchored to INSERT time, not transaction-start time.
//
// PostgreSQL `now()` freezes at transaction start. Every buyer-grant transaction takes the
// pool row lock (ADR-010) BEFORE it inserts, so under contention the lock wait sits between
// the two. A grant written as `now()+ttl` therefore spends its TTL queueing, and a hold that
// waited longer than its TTL is handed to the buyer already dead: `liveClaims` will not count
// it, and the seat is free for someone else. `clock_timestamp()` advances within the
// transaction and is the correct anchor for a duration GRANTED to a buyer.
//
// `liveClaims`, `sweepExpired` and every capacity READ deliberately stay on `now()` — a read
// wants one consistent snapshot, a grant wants real time. That split is ADR-024's, and this
// ticket widens it rather than flattening it.
//
// The RETURNING clause moves with the anchor, and that is not cosmetic. `server_time` is the
// buyer's clock-skew reference: the storefront countdown is `expires_at - server_time`
// (web/storefront/src/components/HoldPicker.tsx), commerce gates conversion on the same pair,
// and Claim.expired() is that comparison. Anchoring `expires_at` while returning a
// transaction-start `server_time` would replace a hold that is born dead with one that
// OVERSTATES its remaining time by exactly the lock wait. Hence the two-sided band in
// assertGrantSurvivedWait: a one-sided "still alive" floor passes that half-fix.

// grantTTL is short enough that the enforced lock wait below outlives it several times over,
// which is the entire point: under the pre-fix anchor the returned hold is already expired.
const grantTTL = 500 * time.Millisecond

// dbNow reads DATABASE time. Every boundary in these tests is database time — host/DB clock
// skew must never be a second moving boundary (the discipline
// TestReleaseCutoffHoldsUnderPoolLockContention established for TKT-78).
func dbNow(t *testing.T, ctx context.Context, db *sql.DB) time.Time {
	t.Helper()
	var now time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`, ).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// blockPool opens a transaction holding the pool row lock and returns it plus a release func.
// The caller's grant cannot proceed until release runs, which is what manufactures the wait.
func blockPool(t *testing.T, ctx context.Context, db *sql.DB, slot uuid.UUID) *sql.Tx {
	t.Helper()
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback() })
	if _, err := blocker.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}
	return blocker
}

// awaitLockWaiter is the handshake, and it is the load-bearing part of every test here.
//
// It is NOT a sleep. The proof is only meaningful if the grant's transaction is OBSERVED
// blocked on the pool lock while database time is still before the TTL boundary — otherwise a
// late-starting transaction would carry a fresh `now()` and pass under the broken code too.
// That is TKT-78's rule.
//
// TKT-76's sharper corollary (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md)
// is that it must prove WHICH waiter: a predicate matching a table-name substring latched onto
// the wrong statement and the test passed with the statement under test deleted. So the
// predicate here is three-part:
//
//  1. the exact lock-statement text for the site under test (`want`);
//  2. `pg_locks.granted = false` — the backend is BLOCKED, not merely running that query; and
//  3. a different backend than our own.
//
// (1) alone is not sufficient here and that is a real constraint rather than caution:
// CreateSeatHold (seat_claims.go:676) and CreateBestAvailableSeatHold (seat_claims.go:1554)
// issue BYTE-IDENTICAL lock statements, and ConvertOperational's text is a prefix of
// DrawDownGroupReservation's. What separates those pairs is that each subtest runs against its
// own schema and its own freshly-generated slot UUID, and only one grant is ever in flight.
//
// PRECONDITION: these subtests must stay SERIAL. Adding t.Parallel() to them would let the
// seated pair observe each other's waiter, and both tests would go quietly vacuous — exactly
// the TKT-76 shape, one level removed.
func awaitLockWaiter(t *testing.T, ctx context.Context, db *sql.DB, want string, deadline time.Time) {
	t.Helper()
	for {
		var waiting, before bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM pg_stat_activity a
				JOIN pg_locks l ON l.pid = a.pid AND NOT l.granted
				WHERE a.wait_event_type='Lock' AND a.state='active'
				  AND a.backend_type='client backend'
				  AND a.query LIKE $1 AND a.pid <> pg_backend_pid()
			), clock_timestamp() < $2`, "%"+want+"%", deadline).Scan(&waiting, &before); err != nil {
			t.Fatal(err)
		}
		if waiting {
			if !before {
				t.Fatal("lock waiter observed only after the boundary; widen the margin")
			}
			return
		}
		if !before {
			// Fail the SETUP, loudly, rather than falling through to an assertion that
			// would then be judging an uncontended grant.
			t.Fatalf("grant transaction never blocked on the pool lock before the boundary (want query %q)", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdUntil keeps the blocker's lock until database time passes mark+d, then releases it.
func holdUntil(t *testing.T, ctx context.Context, db *sql.DB, blocker *sql.Tx, mark time.Time, d time.Duration) {
	t.Helper()
	target := mark.Add(d)
	for {
		var past bool
		if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() > $1`, target).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// assertGrantSurvivedWait is the contract, and it deliberately asserts what the CODE OBSERVED
// rather than facts about the harness. "The goroutine returned" and "a row was inserted" are
// true under the broken anchor too.
//
// The band on expires_at - server_time is TWO-SIDED on purpose. The lower bound catches the
// unfixed anchor (the pair collapses toward zero or goes negative). The UPPER bound catches
// the half-fix where the anchor moved to clock_timestamp() but RETURNING still says now():
// that pair reads as TTL PLUS the whole lock wait, is comfortably "still alive", and would
// sail past any one-sided floor while lying to the buyer about their remaining time.
func assertGrantSurvivedWait(t *testing.T, ctx context.Context, db *sql.DB, expiresAt *time.Time, serverTime time.Time, waitedPast time.Time) {
	t.Helper()
	if expiresAt == nil {
		t.Fatal("grant returned a nil expires_at")
	}
	// The grant was decided after the wait: a transaction-start server_time predates it.
	if !serverTime.After(waitedPast) {
		t.Fatalf("server_time %v is not after the enforced wait boundary %v: the RETURNING clause is still anchored to transaction start", serverTime, waitedPast)
	}
	// The buyer actually holds something: still live against real database time.
	now := dbNow(t, ctx, db)
	if !expiresAt.After(now) {
		t.Fatalf("hold was born expired: expires_at %v is not after database time %v", expiresAt, now)
	}
	// The returned pair describes one instant, and describes it honestly.
	granted := expiresAt.Sub(serverTime)
	if granted <= 0 {
		t.Fatalf("returned pair is inconsistent: expires_at %v is not after server_time %v", expiresAt, serverTime)
	}
	// The band is two-sided but ASYMMETRIC, and both bounds are load-insensitive by
	// construction rather than by hope (ai-review finding 2 argued a loaded CI box could
	// drift the two clock_timestamp() evaluations past a tight tolerance).
	//
	// Both evaluations happen inside ONE statement's execution, so their separation is not a
	// scheduling gap: measured on this database over 2000 rows, max drift was ~1us, and a
	// single INSERT ... RETURNING showed 186us. The lower bound therefore only has to absorb
	// that, and `slack` is four orders of magnitude above what was measured.
	//
	// The UPPER bound is the load-bearing one and it is NOT a tolerance -- it is the
	// half-fix detector. If the anchor moved to clock_timestamp() while RETURNING still said
	// now(), `granted` reads as TTL PLUS the whole lock wait. That is why it is compared
	// against the wait rather than against slack: the wait is 4x the TTL, so the check has
	// enormous margin and still cannot be satisfied by the half-fix.
	const slack = 250 * time.Millisecond
	if granted < grantTTL-slack {
		t.Fatalf("buyer was granted %v, want at least %v: the grant anchor and the RETURNING clause disagree", granted, grantTTL-slack)
	}
	if granted > grantTTL+waitPast/2 {
		t.Fatalf("buyer was granted %v, want ~%v: server_time is behind the grant, so the reported countdown is inflated by the lock wait", granted, grantTTL)
	}
}

// liveInDB re-reads the persisted row rather than trusting the returned struct, and judges it
// with the SAME predicate production uses.
func liveInDB(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) {
	t.Helper()
	var live bool
	if err := db.QueryRowContext(ctx, `SELECT `+liveClaims+` FROM claims WHERE id=$1`, id).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("persisted claim does not satisfy liveClaims: the buyer holds nothing")
	}
}

const (
	gaPoolLock    = "inventory_kind,closure_status FROM inventory_pools WHERE slot_id=$1 AND organizer_id=$2 FOR UPDATE"
	convertLock   = "SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE"
	drawDownLock  = "SELECT 1 /* grp-draw pool lock */ FROM inventory_pools WHERE slot_id=$1 FOR UPDATE"
	seatedPoolLck = "orphan_prevention_enabled,closure_status"
)

// waitPast is how long the blocker keeps the lock beyond the mark: 4x the TTL, so that under
// the pre-fix anchor the returned hold is not merely borderline but decisively dead.
const waitPast = 2 * time.Second

func TestBuyerGrantTTLSurvivesPoolLockWait(t *testing.T) {
	// Each subtest builds its own store: storeForTest installs a 30s context and its own
	// schema, and five subtests sharing one budget would race the timeout. Separate schemas
	// are also what makes the seated pair's identical lock statements distinguishable.

	t.Run("CreateHold", func(t *testing.T) {
		ctx, st, db := storeForTest(t, grantTTL)
		org, slot := provisioned(t, ctx, st, 10)
		mark := dbNow(t, ctx, db)
		blocker := blockPool(t, ctx, db, slot)

		type res struct {
			c   Claim
			err error
		}
		done := make(chan res, 1)
		go func() {
			c, _, err := st.CreateHold(ctx, org, slot, uuid.New(), 1, 1000, "EUR", "", "ttl-ga")
			done <- res{c, err}
		}()
		awaitLockWaiter(t, ctx, db, gaPoolLock, mark.Add(waitPast))
		holdUntil(t, ctx, db, blocker, mark, waitPast)

		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		assertGrantSurvivedWait(t, ctx, db, r.c.ExpiresAt, r.c.ServerTime, mark.Add(waitPast))
		liveInDB(t, ctx, db, r.c.ID)
	})

	t.Run("ConvertOperational", func(t *testing.T) {
		ctx, st, db := storeForTest(t, grantTTL)
		org, slot := provisioned(t, ctx, st, 10)
		src, _, err := st.PlaceOperationalHold(ctx, org, slot, 4, "house", "press", "staff", "seed", "op-seed")
		if err != nil {
			t.Fatal(err)
		}
		mark := dbNow(t, ctx, db)
		blocker := blockPool(t, ctx, db, slot)

		type res struct {
			c   ConvertResult
			err error
		}
		done := make(chan res, 1)
		go func() {
			c, _, err := st.ConvertOperational(ctx, org, src.ID, uuid.New(), slot, 2, 1000, "EUR", "staff", "sell", "ttl-conv")
			done <- res{c, err}
		}()
		awaitLockWaiter(t, ctx, db, convertLock, mark.Add(waitPast))
		holdUntil(t, ctx, db, blocker, mark, waitPast)

		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		assertGrantSurvivedWait(t, ctx, db, r.c.Child.ExpiresAt, r.c.Child.ServerTime, mark.Add(waitPast))
		liveInDB(t, ctx, db, r.c.Child.ID)
	})

	t.Run("DrawDownGroupReservation", func(t *testing.T) {
		ctx, st, db := storeForTest(t, grantTTL)
		org, slot := provisioned(t, ctx, st, 10)
		// The source reservation carries an ABSOLUTE staff-chosen expiry (ADR-027) and is
		// deliberately long-lived: this test is about the CHILD's granted duration, and a
		// source that lapsed mid-wait would refuse the draw-down for an unrelated reason.
		var srcExpiry time.Time
		if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() + interval '1 hour'`).Scan(&srcExpiry); err != nil {
			t.Fatal(err)
		}
		src, _, err := st.PlaceGroupReservation(ctx, org, slot, 4, "school", srcExpiry, "", "staff", "seed", "grp-seed")
		if err != nil {
			t.Fatal(err)
		}
		mark := dbNow(t, ctx, db)
		blocker := blockPool(t, ctx, db, slot)

		type res struct {
			c   ConvertResult
			err error
		}
		done := make(chan res, 1)
		go func() {
			c, _, err := st.DrawDownGroupReservation(ctx, org, src.ID, uuid.New(), slot, 2, 1000, "EUR", "staff", "sell", "ttl-draw")
			done <- res{c, err}
		}()
		awaitLockWaiter(t, ctx, db, drawDownLock, mark.Add(waitPast))
		holdUntil(t, ctx, db, blocker, mark, waitPast)

		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		assertGrantSurvivedWait(t, ctx, db, r.c.Child.ExpiresAt, r.c.Child.ServerTime, mark.Add(waitPast))
		liveInDB(t, ctx, db, r.c.Child.ID)
	})

	t.Run("CreateSeatHold", func(t *testing.T) {
		ctx, st, db := storeForTest(t, grantTTL)
		org, slot, _ := provisionedSeated(t, ctx, st, 50)
		mark := dbNow(t, ctx, db)
		blocker := blockPool(t, ctx, db, slot)

		type res struct {
			h   SeatHold
			err error
		}
		done := make(chan res, 1)
		go func() {
			h, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/2"}, 1000, "EUR", "ttl-seat")
			done <- res{h, err}
		}()
		awaitLockWaiter(t, ctx, db, seatedPoolLck, mark.Add(waitPast))
		holdUntil(t, ctx, db, blocker, mark, waitPast)

		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		assertGrantSurvivedWait(t, ctx, db, r.h.Claim.ExpiresAt, r.h.Claim.ServerTime, mark.Add(waitPast))
		liveInDB(t, ctx, db, r.h.Claim.ID)
	})

	t.Run("CreateBestAvailableSeatHold", func(t *testing.T) {
		ctx, st, db := storeForTest(t, grantTTL)
		org, slot, _ := seededBestAvailablePool(t, ctx, st, 1, 10)
		mark := dbNow(t, ctx, db)
		blocker := blockPool(t, ctx, db, slot)

		type res struct {
			h   SeatHold
			err error
		}
		done := make(chan res, 1)
		go func() {
			h, err := st.CreateBestAvailableSeatHold(ctx, org, slot, uuid.New(), 2, 1000, "EUR", "ttl-best")
			done <- res{h, err}
		}()
		awaitLockWaiter(t, ctx, db, seatedPoolLck, mark.Add(waitPast))
		holdUntil(t, ctx, db, blocker, mark, waitPast)

		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		assertGrantSurvivedWait(t, ctx, db, r.h.Claim.ExpiresAt, r.h.Claim.ServerTime, mark.Add(waitPast))
		liveInDB(t, ctx, db, r.h.Claim.ID)
	})
}

// TKT-148 ai-review finding 1: a REPLAY must report the buyer's countdown on the same clock a
// fresh grant does.
//
// The replay path reads an existing claim under the pool lock, so it queues behind contention
// exactly as a grant does. It returned `now()` as server_time, which is transaction-start
// time, so a retry that waited on the lock reported a countdown inflated by the whole wait —
// on the same wire field (`server_time`, commerce server.go:822) that a first attempt fills
// with insert-time. Before this ticket both paths were wrong in the same direction and so
// agreed; fixing only the grant would have made a hold's reported countdown depend on whether
// the buyer happened to retry.
//
// What this test deliberately does NOT assert: that the replay's LIVENESS decision moved.
// `expired()` still judges on transaction-start time (Claim.snapshotTime) — it writes, and
// moving it would kill more in-flight holds under contention, which is a money-path semantics
// change and its own ticket. This test pins the split: the reference advances, the decision
// does not.
func TestBuyerHoldReplayReportsAdvancingServerTime(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Hour) // long TTL: this is about the reference, not expiry
	org, slot := provisioned(t, ctx, st, 10)

	first, replayed1, err := st.CreateHold(ctx, org, slot, uuid.New(), 1, 1000, "EUR", "", "replay-key")
	if err != nil {
		t.Fatal(err)
	}
	if replayed1 {
		t.Fatal("first call reported a replay")
	}

	mark := dbNow(t, ctx, db)
	blocker := blockPool(t, ctx, db, slot)

	type res struct {
		c        Claim
		replayed bool
		err      error
	}
	done := make(chan res, 1)
	go func() {
		c, r, err := st.CreateHold(ctx, org, slot, first.TicketTypeID, 1, 1000, "EUR", "", "replay-key")
		done <- res{c, r, err}
	}()
	awaitLockWaiter(t, ctx, db, gaPoolLock, mark.Add(waitPast))
	holdUntil(t, ctx, db, blocker, mark, waitPast)

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !r.replayed {
		t.Fatal("second call with the same idempotency key was not a replay")
	}
	if r.c.ID != first.ID {
		t.Fatalf("replay returned a different claim: %v want %v", r.c.ID, first.ID)
	}
	// The whole point: the reference reflects when the answer was given, not when the
	// transaction happened to begin.
	if !r.c.ServerTime.After(mark.Add(waitPast)) {
		t.Fatalf("replay server_time %v is not after the enforced wait boundary %v: the replay read is still anchored to transaction start, so the buyer's countdown is inflated by the lock wait", r.c.ServerTime, mark.Add(waitPast))
	}
	// And it is a real reference, not a value copied from the grant.
	if !r.c.ServerTime.After(first.ServerTime) {
		t.Fatalf("replay server_time %v did not advance past the original grant's %v", r.c.ServerTime, first.ServerTime)
	}
}
