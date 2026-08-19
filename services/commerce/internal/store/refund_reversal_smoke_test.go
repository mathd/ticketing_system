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
	// The row has to satisfy the table's own shape, not just this file's interests:
	// idempotency_key/actor/reason are NOT NULL, and 0007's status CHECK ties
	// `completed` to a non-null completed_at AND payment_fact_id (and `pending` to both
	// being null). Getting that wrong is how a fixture fails for a reason the test is
	// not about — which is what the first run of this file did.
	var completedAt any
	var factID any
	if s.Status == "completed" {
		completedAt, factID = time.Now(), uuid.New()
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,
		                          actor,reason,status,completed_at,payment_fact_id,
		                          tickets_voided_at,capacity_returned_at,reversal_parked_at,reversal_next_attempt_at,
		                          reversal_attempts,reversal_claim_id,reversal_lease_until)
		VALUES($1,$2,$3,$4,$5,$6,1250,2500,'EUR',
		       'ops@example.test','reversal reconciliation test',$7,$8,$9,
		       $10,$11,$12,coalesce($13,now()),$14,$15,$16)`,
		s.ID, s.OrderID, s.OrganizerID, "key-"+key, "fingerprint-"+key, s.Quantity,
		s.Status, completedAt, factID,
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
// The attempt is charged by the RELEASE, not by the claim (ai-review F4).
//
// Charging at claim time means a crash, an OOM or a SIGKILL between claiming and driving
// spends budget on rows that were never attempted — and since parking only ever happens on
// release, repeated crash-after-claim cycles could walk a row most of the way to its limit
// without one real failure, then park it on the first transient one. Both halves are
// asserted, because "the claim does not charge" alone would be satisfied by a version that
// never charges at all and therefore never parks.
func TestTheAttemptIsChargedByTheReleaseNotTheClaim(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-attempt", func(s *refundSeed) { s.Attempts = 4 })

	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if c.Attempts != 4 {
		t.Fatalf("attempts = %d after claiming, want the original 4: a claim that charges "+
			"spends budget on work a crash may never perform", c.Attempts)
	}

	if err := ReleaseReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID, false, false,
		"ticket voiding outstanding"); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT reversal_attempts FROM order_refunds WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d after a failed drive, want 5: nothing spends the budget, so "+
			"a permanently refused obligation never parks", attempts)
	}
}

// A lease that simply LAPSES — the crash case — costs the row nothing. This is what makes
// the bound honest: an expired claim is indistinguishable from one that never happened, so
// a process that dies mid-batch cannot walk its rows toward parking.
func TestAnExpiredLeaseCostsNoBudget(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-lapsed", func(s *refundSeed) {
		s.Attempts = 3
		dead := uuid.New()
		s.ClaimID, s.LeaseUntil = &dead, reversalAgo(time.Hour) // a claimant that died holding it
	})
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("a row whose claimant died was not reclaimable")
	}
	if c.Attempts != 3 {
		t.Fatalf("attempts = %d, want the original 3: a crash between claim and drive must "+
			"not spend the row's failure budget", c.Attempts)
	}
}

// ai-review F3: every statement keys on the FULL composite (organizer_id, id).
//
// `order_refunds`' primary key is (organizer_id, id) — `id` alone is not unique by schema.
// A claim matching on `id` alone would hand one eligible row's claim token to every same-id
// row in another tenant, INCLUDING rows that satisfied none of the eligibility predicates,
// and the runner would then drive a pending refund's reversal: tickets voided before the
// money moved.
//
// The fixture constructs exactly that: the same refund id under two organizers, one eligible
// and one PENDING. It is unreachable through the product path today (a refund id is a SHA-1
// over its organizer), which is precisely why it has to be seeded directly — the defect is in
// the SQL's shape, not in the data the happy path happens to produce.
func TestClaimingIsScopedByTheCompositeKeyNotTheRefundIDAlone(t *testing.T) {
	db, ctx := outboxDB(t)
	shared := uuid.New()

	eligible := completedRefund(t, db, ctx, "reversal-tenant-a", func(s *refundSeed) {
		s.ID = shared
	})
	// Same refund id, different organizer, and NOT eligible: its money has not moved.
	victim := completedRefund(t, db, ctx, "reversal-tenant-b", func(s *refundSeed) {
		s.ID = shared
		s.Status = "pending"
	})

	claimed, err := ClaimOutstandingReversals(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var sawEligible bool
	for _, c := range claimed {
		if c.Refund.ID == shared && c.Refund.OrganizerID == victim.OrganizerID {
			t.Fatal("a PENDING refund belonging to another organizer was claimed because it " +
				"shares a refund id with an eligible one: its tickets would be voided before " +
				"its money moved")
		}
		if c.Refund.ID == shared && c.Refund.OrganizerID == eligible.OrganizerID {
			sawEligible = true
		}
	}
	if !sawEligible {
		t.Fatal("the eligible row was not claimed at all")
	}

	// The victim's lease must be untouched — an unscoped UPDATE would have stamped it with
	// the same claim token even though it was never returned to the caller.
	var claim uuid.NullUUID
	if err := db.QueryRowContext(ctx, `SELECT reversal_claim_id FROM order_refunds WHERE organizer_id=$1 AND id=$2`,
		victim.OrganizerID, shared).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	if claim.Valid {
		t.Fatalf("another organizer's row was leased by this claim (%v): the UPDATE matched "+
			"on id alone", claim.UUID)
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
	if err := ReleaseReversalClaim(ctx, db, s.OrganizerID, s.ID, uuid.New(), false, false, "a stale claimant"); err != nil {
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
	if err := ReleaseReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID, true, false, "capacity return outstanding"); err != nil {
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
	// The voiding half lands from OUTSIDE this claim — a staff replay or a cancellation run,
	// neither of which takes this lease. The release is then told what THIS claimant saw at
	// claim time (nothing discharged), which is exactly the ai-review F2 case: a verdict
	// computed from the claimant's own before/after would read "no progress" here and, at
	// this attempt count, park a refund that just advanced.
	if _, err := db.ExecContext(ctx, `UPDATE order_refunds SET tickets_voided_at=now() WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID, false, false, "capacity return outstanding"); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var parked sql.NullTime
	var next time.Time
	if err := db.QueryRowContext(ctx, `SELECT reversal_attempts,reversal_parked_at,reversal_next_attempt_at
		FROM order_refunds WHERE organizer_id=$1 AND id=$2`, s.OrganizerID, s.ID).
		Scan(&attempts, &parked, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d after progress, want 0: a refund that is recovering would "+
			"still park", attempts)
	}
	if parked.Valid {
		t.Fatal("a refund that made progress was parked anyway — this is the concurrent-replay " +
			"case, and parking it strands the capacity return with nothing driving it")
	}
	// Prompt, but NOT instantly re-claimable (own finding S1): `RunOnce` drains in a loop, so
	// a zero backoff lets the same row be re-driven inside the same pass, hammering a
	// downstream that just half-failed and resetting its budget each time.
	if !next.After(time.Now()) {
		t.Fatal("a progressed refund became claimable immediately: the drain loop will " +
			"re-drive it within the same pass, with no backoff at all")
	}
	if next.After(time.Now().Add(time.Minute)) {
		t.Fatalf("a progressed refund waits until %s — it is recovering, and its remaining "+
			"obligation should not sit out a full backoff", next)
	}
}

// A row that became FULLY discharged between the drive and the release is not parked, even
// at the budget boundary. Parking a complete reversal would put a permanent "needs a human"
// marker on work that is finished — and because the claim query skips parked rows, nothing
// would ever look at it again to notice.
func TestAReversalCompletedBySomeoneElseIsNotParked(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-completed-elsewhere", func(s *refundSeed) {
		s.Attempts = MaxReversalAttempts - 1
	})
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	// A replay outside this lease discharges BOTH obligations while this claimant fails.
	if _, err := db.ExecContext(ctx, `
		UPDATE order_refunds SET tickets_voided_at=now(), capacity_returned_at=now()
		WHERE organizer_id=$1 AND id=$2`, s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID, false, false,
		"ticket voiding outstanding"); err != nil {
		t.Fatal(err)
	}
	// The WHOLE terminal state, not just parked_at (ai-review pass 2). The first version of
	// this test constructed exactly this sequence and asserted only that the row was not
	// parked — which it was not — while the release wrote this claimant's failure onto a row
	// that had just SUCCEEDED. Nothing ever clears that: the claim query skips discharged
	// rows, so Finish never runs on it, and 0021's rollback guard reads any non-null
	// last_error as failed reconciliation and refuses a legitimate rollback. A test that
	// builds the right scenario and looks at the wrong column is the shape that hides a
	// defect while appearing to cover it.
	var parked sql.NullTime
	var lastErr sql.NullString
	var attempts int
	if err := db.QueryRowContext(ctx, `
		SELECT reversal_parked_at, reversal_last_error, reversal_attempts
		FROM order_refunds WHERE organizer_id=$1 AND id=$2`,
		s.OrganizerID, s.ID).Scan(&parked, &lastErr, &attempts); err != nil {
		t.Fatal(err)
	}
	if parked.Valid {
		t.Fatal("a fully discharged reversal was parked: it would carry a permanent " +
			"needs-a-human marker for work that is already done")
	}
	if lastErr.Valid {
		t.Fatalf("a completed reversal kept a failure message (%q): nothing will ever clear "+
			"it, and 0021's rollback guard reads it as failed reconciliation", lastErr.String)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d on a completed reversal, want 0: it is finished, not failing", attempts)
	}
}

// The other half of the same defect, asserted where it actually bites: a refund that
// completed while someone else held the claim must not block the down migration. The guard
// is deliberately broad (any parked row, any attempts, any error), so a row that succeeds
// through this path has to come out genuinely clean or the guard misfires on healthy data.
func TestAConcurrentlyCompletedReversalDoesNotTripTheRollbackGuard(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-rollback-guard", nil)
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE order_refunds SET tickets_voided_at=now(), capacity_returned_at=now()
		WHERE organizer_id=$1 AND id=$2`, s.OrganizerID, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID, false, false,
		"ticket voiding outstanding"); err != nil {
		t.Fatal(err)
	}

	// The guard's own predicate, run against this row.
	var trips bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM order_refunds
			WHERE organizer_id=$1 AND id=$2
			  AND (reversal_parked_at IS NOT NULL OR reversal_attempts > 0 OR reversal_last_error IS NOT NULL)
		)`, s.OrganizerID, s.ID).Scan(&trips); err != nil {
		t.Fatal(err)
	}
	if trips {
		t.Fatal("a reversal that COMPLETED trips 0021's rollback guard, so a deploy that " +
			"needs undoing is refused on the strength of work that succeeded")
	}
}

// Abandoning an undriven claim leaves the row immediately reclaimable and costs it nothing —
// a shutdown must not spend budget or park anything.
func TestAbandonCostsNoBudgetAndReleasesImmediately(t *testing.T) {
	db, ctx := outboxDB(t)
	s := completedRefund(t, db, ctx, "reversal-abandon", func(s *refundSeed) { s.Attempts = 3 })
	c, ok := claimReversal(t, db, ctx, s.ID)
	if !ok {
		t.Fatal("not claimable")
	}
	if err := AbandonReversalClaim(ctx, db, s.OrganizerID, s.ID, c.ClaimID); err != nil {
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
