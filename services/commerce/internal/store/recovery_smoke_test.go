//go:build smoke

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// These tests assert ADR-016 §Decisions 1-3. Every one of them fails on the pre-slice-3
// code, where nothing re-drove a stuck order at all.

// seedStuck parks an order in a recoverable state, old enough to clear the in-flight
// grace period.
func seedStuck(t *testing.T, status string) StuckOrder {
	t.Helper()
	db, ctx := outboxDB(t)
	c, _ := seedCompletable(t, db, ctx, "recovery-"+uuid.New().String())
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET status='finalizing' WHERE id=$1`, c.ReservationID); err != nil {
		t.Fatal(err)
	}
	// updated_at back-dated past the grace period: a fresh order belongs to its own
	// in-flight request, not to recovery.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status=$2,updated_at=now()-interval '10 minutes' WHERE id=$1`, c.OrderID, status); err != nil {
		t.Fatal(err)
	}
	var holdID uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT hold_id FROM reservations WHERE id=$1`, c.ReservationID).Scan(&holdID); err != nil {
		t.Fatal(err)
	}
	return StuckOrder{
		OrderID: c.OrderID, ReservationID: c.ReservationID, OrganizerID: c.OrganizerID,
		HoldID: holdID, BuyerID: c.BuyerID, SlotID: c.SlotID, TicketTypeID: c.TicketTypeID,
		Quantity: c.Quantity, Status: status,
	}
}

// The core slice-3 invariant: a parked order is visible to the runner. Before this,
// nothing ever looked.
func TestStuckOrdersAreClaimable(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, status := range []string{"created", "confirmation_pending"} {
		s := seedStuck(t, status)
		claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
		if err != nil {
			t.Fatalf("claim stuck orders: %v", err)
		}
		var found bool
		for _, c := range claimed {
			if c.OrderID == s.OrderID {
				found = true
				if c.Status != status {
					t.Fatalf("claimed status = %q, want %q", c.Status, status)
				}
				if c.OrganizerID != s.OrganizerID || c.HoldID != s.HoldID {
					t.Fatal("claimed order must carry the reservation identifiers the re-drive needs")
				}
				if c.IdempotencyKey == "" {
					t.Fatal("claimed order must carry its idempotency key: it is the payments lookup key")
				}
				if c.ClaimID == uuid.Nil {
					t.Fatal("claimed order must carry a claim id")
				}
			}
		}
		if !found {
			t.Fatalf("a stuck %s order must be claimable", status)
		}
	}
}

// payment_unknown needs real-PSP status (TKT-56). Claiming it would invite exactly the
// unfounded inference ADR-016 §Decision 2 forbids.
func TestPaymentUnknownIsNotClaimed(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "payment_unknown")
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claimed {
		if c.OrderID == s.OrderID {
			t.Fatal("payment_unknown must not be claimed: resolving it needs PSP status, deferred to TKT-56")
		}
	}
}

// An order still being driven by its own request must not be re-driven underneath it.
func TestInFlightOrderIsNotClaimed(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompletable(t, db, ctx, "recovery-inflight-"+uuid.New().String())
	// Fresh updated_at: this order belongs to a live checkout.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='created',updated_at=now() WHERE id=$1`, c.OrderID); err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range claimed {
		if x.OrderID == c.OrderID {
			t.Fatal("an order updated seconds ago is in flight; recovery must not race its own coordinator")
		}
	}
}

// The claim token protects the same way it does in the outbox: a claimant whose lease
// lapsed mid-drive must not disturb its successor.
func TestStaleRecoveryClaimantCannotDisturbSuccessor(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "confirmation_pending")

	d1 := claimStuckOne(t, s.OrderID)
	if _, err := db.ExecContext(ctx, `UPDATE orders SET recovery_lease_until=now()-interval '1 second' WHERE id=$1`, s.OrderID); err != nil {
		t.Fatal(err)
	}
	d2 := claimStuckOne(t, s.OrderID)
	if d1.ClaimID == d2.ClaimID {
		t.Fatal("re-claim must carry a distinct claim id")
	}

	// The stale claimant's release must not clear the live claim.
	if err := ReleaseStuckOrder(ctx, db, s.OrderID, d1.ClaimID, errors.New("stale")); err != nil {
		t.Fatal(err)
	}
	var claim uuid.NullUUID
	if err := db.QueryRowContext(ctx, `SELECT recovery_claim_id FROM orders WHERE id=$1`, s.OrderID).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	if !claim.Valid || claim.UUID != d2.ClaimID {
		t.Fatalf("stale claimant cleared the live claim: got %v want %s", claim, d2.ClaimID)
	}
	// Nor may it park the order.
	if err := ParkForReconciliation(ctx, db, s.OrderID, d1.ClaimID, "stale"); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("stale claimant parked a row it no longer holds: err=%v", err)
	}
}

// An unrecoverable order must not starve the rest: claiming is oldest-first.
func TestExhaustedRecoveryParksAndStopsBlocking(t *testing.T) {
	db, ctx := outboxDB(t)
	poison := seedStuck(t, "confirmation_pending")

	for range MaxRecoveryAttempts + 1 {
		claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range claimed {
			if c.OrderID != poison.OrderID {
				continue
			}
			if err := ReleaseStuckOrder(ctx, db, c.OrderID, c.ClaimID, errors.New("inventory down")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `UPDATE orders SET recovery_next_attempt_at=now() WHERE id=$1 AND recovery_parked_at IS NULL`, poison.OrderID); err != nil {
			t.Fatal(err)
		}
	}

	var parked *time.Time
	if err := db.QueryRowContext(ctx, `SELECT recovery_parked_at FROM orders WHERE id=$1`, poison.OrderID).Scan(&parked); err != nil {
		t.Fatal(err)
	}
	if parked == nil {
		t.Fatalf("an order failing %d times must be parked, not retried forever", MaxRecoveryAttempts)
	}

	fresh := seedStuck(t, "confirmation_pending")
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var sawFresh, sawPoison bool
	for _, c := range claimed {
		if c.OrderID == fresh.OrderID {
			sawFresh = true
		}
		if c.OrderID == poison.OrderID {
			sawPoison = true
		}
	}
	if !sawFresh {
		t.Fatal("a newer stuck order must be claimable while an older parked one exists")
	}
	if sawPoison {
		t.Fatal("a parked order must never be claimed again")
	}
}

// The releasable-outcome guard is the teeth of ADR-016 §Decision 2: a transport failure
// never becomes evidence of no side effect.
//
// Asserted at the Go layer specifically, not just "the write fails": the DB CHECK also
// rejects these values, so a test that only checked err != nil would pass with the Go
// guard deleted. (It did — caught by mutation.) The two layers are deliberate defence in
// depth, but this test names which one it is testing: the store must refuse before it
// reaches PostgreSQL, so callers get "does not prove absence of a side effect" rather
// than a constraint violation, and so `""` is rejected rather than written as NULL.
func TestOnlyProvenOutcomesAreRecordable(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "created")
	for _, bad := range []string{"unknown", "captured", "network_error", ""} {
		err := RecordTerminalOutcome(ctx, db, s.OrderID, bad)
		if err == nil {
			t.Fatalf("outcome %q does not prove absence of a side effect and must be rejected", bad)
		}
		// The Go guard's message, not PostgreSQL's constraint error.
		if !strings.Contains(err.Error(), "does not prove absence of a side effect") {
			t.Fatalf("outcome %q must be refused by the store guard before reaching the database; got %v", bad, err)
		}
	}
	for _, good := range []string{"declined", "timeout", "not_attempted"} {
		s2 := seedStuck(t, "created")
		if err := RecordTerminalOutcome(ctx, db, s2.OrderID, good); err != nil {
			t.Fatalf("outcome %q must be recordable: %v", good, err)
		}
		var got string
		if err := db.QueryRowContext(ctx, `SELECT terminal_outcome FROM orders WHERE id=$1`, s2.OrderID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != good {
			t.Fatalf("recorded outcome = %q, want %q", got, good)
		}
	}
}

// Releasing must be restartable: the outcome is recorded BEFORE the release, so a crash
// in between leaves the next pass able to finish rather than guess.
func TestRecordedOutcomeSurvivesForTheNextPass(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "created")
	if err := RecordTerminalOutcome(ctx, db, s.OrderID, "declined"); err != nil {
		t.Fatal(err)
	}
	// RecordTerminalOutcome bumps updated_at, so the row re-enters the in-flight grace
	// period. Age it again: in production the next pass simply arrives later.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='release_pending',updated_at=now()-interval '10 minutes' WHERE id=$1`, s.OrderID); err != nil {
		t.Fatal(err)
	}
	// A later pass claims it and knows exactly what it is completing.
	got := claimStuckOne(t, s.OrderID)
	if got.TerminalOutcome != "declined" {
		t.Fatalf("re-claimed release_pending carries outcome %q; want declined", got.TerminalOutcome)
	}

	got.TerminalOutcome = "declined"
	if err := MarkReleased(ctx, db, got); err != nil {
		t.Fatalf("mark released: %v", err)
	}
	var orderStatus, resStatus string
	if err := db.QueryRowContext(ctx, `SELECT o.status,r.status FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`, s.OrderID).
		Scan(&orderStatus, &resStatus); err != nil {
		t.Fatal(err)
	}
	if orderStatus != "declined" || resStatus != "failed" {
		t.Fatalf("released order = %q/%q; want declined/failed", orderStatus, resStatus)
	}
}

// not_attempted presents to the buyer as a timeout but stays distinguishable in the
// audit column: nothing was charged either way, but only one of them called the PSP.
func TestNotAttemptedPresentsAsTimeoutButStaysDistinguishable(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "created")
	if err := RecordTerminalOutcome(ctx, db, s.OrderID, "not_attempted"); err != nil {
		t.Fatal(err)
	}
	// Age past the grace period the outcome write just reset.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes' WHERE id=$1`, s.OrderID); err != nil {
		t.Fatal(err)
	}
	got := claimStuckOne(t, s.OrderID)
	got.TerminalOutcome = "not_attempted"
	if err := MarkReleased(ctx, db, got); err != nil {
		t.Fatal(err)
	}
	var status, outcome string
	if err := db.QueryRowContext(ctx, `SELECT status,terminal_outcome FROM orders WHERE id=$1`, s.OrderID).Scan(&status, &outcome); err != nil {
		t.Fatal(err)
	}
	if status != "timeout" {
		t.Fatalf("not_attempted must present as timeout to the buyer, got %q", status)
	}
	if outcome != "not_attempted" {
		t.Fatalf("the audit column must keep not_attempted distinct, got %q", outcome)
	}
}

// Captured money whose claim is gone must not be silently resolved either way.
func TestParkForReconciliationHoldsCapturedMoneyVisibly(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "confirmation_pending")
	c := claimStuckOne(t, s.OrderID)

	if err := ParkForReconciliation(ctx, db, s.OrderID, c.ClaimID, "captured payment whose claim is gone"); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err := db.QueryRowContext(ctx, `SELECT status,recovery_last_error FROM orders WHERE id=$1`, s.OrderID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "reconciliation_required" {
		t.Fatalf("status = %q; want reconciliation_required", status)
	}
	if reason == "" {
		t.Fatal("a parked order must record why: it is the only signal a human gets")
	}
	// Never re-claimed: it awaits a capability that does not exist yet.
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range claimed {
		if x.OrderID == s.OrderID {
			t.Fatal("a reconciliation_required order must never be re-claimed")
		}
	}
}

// The status vocabulary is now closed. An unknown status would silently fall out of
// every claim query — which is how release_pending came to be told to buyers while
// being written nowhere.
func TestOrderStatusVocabularyIsEnforced(t *testing.T) {
	db, ctx := outboxDB(t)
	s := seedStuck(t, "created")
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='not_a_real_status' WHERE id=$1`, s.OrderID); err == nil {
		t.Fatal("orders.status must reject an unknown value")
	}
}

func claimStuckOne(t *testing.T, order uuid.UUID) StuckOrder {
	t.Helper()
	db, ctx := outboxDB(t)
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatalf("claim stuck orders: %v", err)
	}
	for _, c := range claimed {
		if c.OrderID == order {
			return c
		}
	}
	t.Fatalf("order %s not claimable", order)
	return StuckOrder{}
}
