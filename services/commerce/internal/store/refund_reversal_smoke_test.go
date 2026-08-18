//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Reversal reconciliation, SQL half (TKT-163, ADR-062).
//
// Everything here is a claim about a PREDICATE, which is why it lives against real
// PostgreSQL rather than against the runner's fakes: eligibility, the organizer/hold join,
// the lease, the claim fence, the backoff, parking and the capacity-after-void guard are all
// enforced by the shipped SQL, and a fake enforcing the same rules in Go would prove only
// that the fake and the runner agree. The runner's DECISIONS live in
// internal/reversal/runner_test.go instead.

// completedRefund seeds a completed order and a completed refund on it, with both
// obligations outstanding — the state a refund is left in when access is down.
//
// It writes `order_refunds` directly rather than going through BindOrderRefund +
// CompleteOrderRefund: this file's subject is which rows the claim query SELECTS, so the
// fixture must be able to express states the happy path cannot reach (a pending refund, a
// row already parked, a lease in the future). Money is never moved by these tests.
func completedRefund(t *testing.T, db *sql.DB, ctx context.Context, key string, mutate func(*refundSeed)) refundSeed {
	t.Helper()
	c, _ := seedCompleted(t, db, ctx, key, 3, 1250)
	s := refundSeed{
		ID: uuid.New(), OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: 2,
		Status: "completed",
	}
	if mutate != nil {
		mutate(&s)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,request_fingerprint,quantity,unit_amount,amount,currency,status,
		                          tickets_voided_at,capacity_returned_at,reversal_parked_at,reversal_next_attempt_at,
		                          reversal_attempts,reversal_claim_id,reversal_lease_until)
		VALUES($1,$2,$3,$4,$5,1250,2500,'EUR',$6,$7,$8,$9,coalesce($10,now()),$11,$12,$13)`,
		s.ID, s.OrderID, s.OrganizerID, "fingerprint-"+key, s.Quantity, s.Status,
		s.VoidedAt, s.ReturnedAt, s.ParkedAt, s.NextAttemptAt, s.Attempts, s.ClaimID, s.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	return s
}

type refundSeed struct {
	ID, OrderID, OrganizerID uuid.UUID
	Quantity                 int32
	Status                   string
	VoidedAt, ReturnedAt     *time.Time
	ParkedAt, NextAttemptAt  *time.Time
	LeaseUntil               *time.Time
	ClaimID                  *uuid.UUID
	Attempts                 int
}

func claimReversal(t *testing.T, db *sql.DB, ctx context.Context, want uuid.UUID) (ClaimedReversal, bool) {
	t.Helper()
	claimed, err := ClaimOutstandingReversals(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, c := range claimed {
		if c.Refund.ID == want {
			return c, true
		}
	}
	return ClaimedReversal{}, false
}

func reversalAgo(d time.Duration) *time.Time   { t := time.Now().Add(-d); return &t }
func reversalAhead(d time.Duration) *time.Time { t := time.Now().Add(d); return &t }

// The claim carries the organizer and the hold from the RESERVATION. This is the whole
// reason the query cannot reuse LookupRefundByID, which documents that it does not populate
// HoldID — and without the hold, DriveReversal's capacity leg short-circuits on
// `refund.HoldID == uuid.Nil` and the obligation is never discharged, silently, forever.
func TestClaimedReversalCarriesTheReservationsOrganizerAndHold(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-join", nil)

	var wantHold uuid.UUID
	if err := db.QueryRowContext(ctx, `
		SELECT r.hold_id FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`,
		s.OrderID).Scan(&wantHold); err != nil {
		t.Fatal(err)
	}

	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("an outstanding reversal was not claimable")
	}
	if c.Refund.HoldID != wantHold {
		t.Fatalf("hold = %v, want %v: without the reservation join the capacity leg "+
			"short-circuits on a nil hold and never returns the seat", c.Refund.HoldID, wantHold)
	}
	if c.Refund.OrganizerID != s.OrganizerID {
		t.Fatalf("organizer = %v, want %v", c.Refund.OrganizerID, s.OrganizerID)
	}
	if c.Refund.Quantity != s.Quantity {
		t.Fatalf("quantity = %d, want %d", c.Refund.Quantity, s.Quantity)
	}
	if c.ClaimID == uuid.Nil {
		t.Fatal("claimed without a claim token: nothing can be fenced")
	}
}

// A refund whose money has NOT moved is not eligible. Voiding its tickets would reverse a
// sale that has not happened.
func TestClaimSkipsARefundWhoseMoneyHasNotMoved(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-pending", func(s *refundSeed) {
		s.Status = "pending"
	})
	if _, ok := claimReversal(t, db, ctx, s.ID); ok {
		t.Fatal("a PENDING refund was claimed for reversal: its money has not moved, and " +
			"voiding its tickets reverses a sale that has not happened")
	}
}

// A refund with both obligations discharged is done, and must never be re-driven.
func TestClaimSkipsAFullyDischargedReversal(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-done", func(s *refundSeed) {
		s.VoidedAt, s.ReturnedAt = reversalAgo(time.Hour), reversalAgo(time.Hour)
	})
	if _, ok := claimReversal(t, db, ctx, s.ID); ok {
		t.Fatal("a reversal with both obligations discharged was claimed again")
	}
}

// Each obligation independently keeps the row eligible: a refund whose tickets are voided
// but whose capacity is still owed is exactly the state a seated or inventory-outage refund
// sits in, and it is the half a query written only around the ticket's headline case
// (access down) would drop.
func TestEitherOutstandingObligationKeepsARefundClaimable(t *testing.T) {
	db, ctx := outboxDB(t)
	voidedOnly := completedRefund(t, db, ctx, "reversal-cap-only", func(s *refundSeed) {
		s.VoidedAt = reversalAgo(time.Hour)
	})
	if _, ok := claimReversal(t, db, ctx, voidedOnly.ID); !ok {
		t.Fatal("a refund owing only its capacity return was not claimable")
	}
}

// A parked row has spent its budget and awaits a human. Claiming it again is what turns one
// permanently refused obligation into a runner that re-drives it forever, oldest-first,
// starving everything behind it.
func TestClaimSkipsAParkedReversal(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-parked", func(s *refundSeed) {
		s.ParkedAt = reversalAgo(time.Minute)
	})
	if _, ok := claimReversal(t, db, ctx, s.ID); ok {
		t.Fatal("a parked reversal was claimed: a permanently refused obligation would " +
			"retry forever and starve the queue behind it")
	}
}

// The backoff is real: a row whose next attempt is in the future is not claimable yet.
func TestClaimRespectsTheBackoff(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-backoff", func(s *refundSeed) {
		s.NextAttemptAt = reversalAhead(time.Hour)
	})
	if _, ok := claimReversal(t, db, ctx, s.ID); ok {
		t.Fatal("a backed-off reversal was claimed before its next attempt was due")
	}
}

// A live lease hides the row from a second runner (or a second replica); an expired one
// makes it reclaimable. Both directions, because a lease that never blocks and a lease that
// never expires are different defects and one test cannot see both.
func TestALiveLeaseHidesARowAndAnExpiredOneReleasesIt(t *testing.T) {
	db, ctx := outboxDB(t)
	other := uuid.New()
	live := completedRefund(t, db, ctx, "reversal-leased", func(s *refundSeed) {
		s.ClaimID, s.LeaseUntil = &other, reversalAhead(time.Hour)
	})
	if _, ok := claimReversal(t, db, ctx, live.ID); ok {
		t.Fatal("a row under a live lease was claimed by a second runner")
	}

	stale := uuid.New()
	expired := completedRefund(t, db, ctx, "reversal-expired", func(s *refundSeed) {
		s.ClaimID, s.LeaseUntil = &stale, reversalAgo(time.Hour)
	})
	if _, ok := claimReversal(t, db, ctx, expired.ID); !ok {
		t.Fatal("a row whose lease had expired stayed invisible: its obligation would be " +
			"stranded by whatever process died holding it")
	}
}

// Claiming charges an attempt. Without the charge nothing ever spends the budget and
// nothing ever parks.
func TestClaimingChargesAnAttempt(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-attempt", func(s *refundSeed) { s.Attempts = 4 })
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if c.Attempts != 5 {
		t.Fatalf("attempts = %d, want 5", c.Attempts)
	}
}

// The release fence: a claimant whose lease lapsed cannot disturb the successor that now
// holds the row.
func TestReleaseIsFencedByTheClaimToken(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-fence", nil)
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if err := ReleaseReversalClaim(ctx, db, s.ID, uuid.New(), false, "a stale claimant"); err != nil {
		t.Fatal(err)
	}
	var claim uuid.NullUUID
	if err := db.QueryRowContext(ctx, `SELECT reversal_claim_id FROM order_refunds WHERE id=$1`, s.ID).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	if !claim.Valid || claim.UUID != c.ClaimID {
		t.Fatalf("claim = %v, want the live holder %v: a stale claimant cleared its "+
			"successor's lease", claim, c.ClaimID)
	}
}

// A row that makes no progress spends its budget and parks. This is the mechanism that
// stops a permanently undischargeable obligation — inventory's partial-seated refusal
// (TKT-164) — from being retried forever, WITHOUT commerce having to predict the refusal
// from state it cannot read.
func TestAReversalThatNeverProgressesParks(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-parks", func(s *refundSeed) {
		s.VoidedAt = reversalAgo(time.Hour) // voided; capacity refused forever, as a seated partial is
		s.Attempts = MaxReversalAttempts - 1
	})
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if err := ReleaseReversalClaim(ctx, db, s.ID, c.ClaimID, false, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	var parked sql.NullTime
	var lastErr sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT reversal_parked_at,reversal_last_error FROM order_refunds WHERE id=$1`,
		s.ID).Scan(&parked, &lastErr); err != nil {
		t.Fatal(err)
	}
	if !parked.Valid {
		t.Fatalf("attempts reached %d without parking: the obligation retries forever",
			MaxReversalAttempts)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Fatal("parked with no recorded reason: an operator cannot tell what kept failing")
	}
	if _, ok := claimReversal(t, db, ctx, s.ID); ok {
		t.Fatal("a freshly parked row was immediately re-claimable")
	}
}

// Progress resets the budget, and it must reset it ALL the way: an access outage of any
// length costs one attempt per pass while nothing moves, and the first discharged obligation
// restores the full budget. Without this a bounded budget would retire a refund that is
// actively recovering — reintroducing the failure this ticket exists to close.
func TestProgressResetsTheAttemptBudgetAndClearsTheBackoff(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-progress", func(s *refundSeed) {
		s.Attempts = MaxReversalAttempts - 1
	})
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	// The voiding half landed; capacity is still owed.
	if _, err := db.ExecContext(ctx, `UPDATE order_refunds SET tickets_voided_at=now() WHERE id=$1`, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseReversalClaim(ctx, db, s.ID, c.ClaimID, true, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var parked sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT reversal_attempts,reversal_parked_at FROM order_refunds WHERE id=$1`,
		s.ID).Scan(&attempts, &parked); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d after progress, want 0: a refund that is recovering would "+
			"still park", attempts)
	}
	if parked.Valid {
		t.Fatal("a refund that made progress was parked anyway")
	}
	if _, ok := claimReversal(t, db, ctx, s.ID); !ok {
		t.Fatal("a refund that made progress was not immediately retryable, so the rest of " +
			"its reversal waits out a backoff it did not earn")
	}
}

// Abandoning an undriven claim refunds the attempt charged at claim time and leaves the row
// immediately reclaimable — a shutdown must not cost budget or park anything.
func TestAbandonRefundsTheAttemptAndReleasesImmediately(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-abandon", func(s *refundSeed) { s.Attempts = 3 })
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if err := AbandonReversalClaim(ctx, db, s.ID, c.ClaimID); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT reversal_attempts FROM order_refunds WHERE id=$1`, s.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d after abandoning an undriven claim, want the original 3: a "+
			"row must not reach its first real failure with budget spent on work that never ran", attempts)
	}
	if _, ok := claimReversal(t, db, ctx, s.ID); !ok {
		t.Fatal("an abandoned claim was not immediately reclaimable, so an orderly restart " +
			"costs a full lease of doing nothing")
	}
}

// Capacity cannot be recorded as returned while voiding is still outstanding. 0009 left this
// to application code, which was sufficient with ONE caller; TKT-163 adds a second, so it is
// the database's rule now. Freeing the seat while the ticket still admits is the one
// ordering that can OVERSELL (ADR-038 §1).
func TestCapacityCannotBeMarkedReturnedBeforeVoiding(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-ordering", nil)

	if err := MarkRefundCapacityReturned(ctx, db, s.OrganizerID, s.ID); err != nil {
		t.Fatalf("the guarded mark must be a no-op, not an error: %v", err)
	}
	var returned sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT capacity_returned_at FROM order_refunds WHERE id=$1`, s.ID).Scan(&returned); err != nil {
		t.Fatal(err)
	}
	if returned.Valid {
		t.Fatal("capacity was marked returned while the tickets were still valid: that is " +
			"the one ordering that can oversell")
	}

	// The database refuses it too, not just the WHERE clause — the second lock on the door,
	// for any future caller that writes the column directly.
	if _, err := db.ExecContext(ctx, `UPDATE order_refunds SET capacity_returned_at=now() WHERE id=$1`, s.ID); err == nil {
		t.Fatal("a direct write of capacity_returned_at before tickets_voided_at was accepted: " +
			"the CHECK constraint is missing")
	}

	// And it succeeds once voiding is recorded, so the guard refuses the wrong order rather
	// than refusing everything — a constraint that never admits anything passes the negative
	// test while breaking the feature.
	if _, err := db.ExecContext(ctx, `UPDATE order_refunds SET tickets_voided_at=now() WHERE id=$1`, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := MarkRefundCapacityReturned(ctx, db, s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT capacity_returned_at FROM order_refunds WHERE id=$1`, s.ID).Scan(&returned); err != nil {
		t.Fatal(err)
	}
	if !returned.Valid {
		t.Fatal("capacity was not marked returned even after voiding")
	}
}

// The backlog gauges count what the operator is told they count. `parked` is the number that
// makes parking honest, so a backlog read that cannot distinguish parked from retrying would
// report a stopped reconciler as a busy one.
func TestBacklogCountsOutstandingAndParkedSeparately(t *testing.T) {
	db, ctx := outboxDB(t)
	before, err := ReadReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	completedRefund(t, db, ctx, "reversal-backlog-live", nil)
	completedRefund(t, db, ctx, "reversal-backlog-parked", func(s *refundSeed) {
		s.ParkedAt = reversalAgo(time.Minute)
	})
	// Discharged: counted by neither.
	completedRefund(t, db, ctx, "reversal-backlog-done", func(s *refundSeed) {
		s.VoidedAt, s.ReturnedAt = reversalAgo(time.Hour), reversalAgo(time.Hour)
	})

	after, err := ReadReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Outstanding - before.Outstanding; got != 2 {
		t.Fatalf("outstanding grew by %d, want 2 (the discharged refund must not count)", got)
	}
	if got := after.Parked - before.Parked; got != 1 {
		t.Fatalf("parked grew by %d, want 1: a stopped reconciler would read as a busy one", got)
	}
}
