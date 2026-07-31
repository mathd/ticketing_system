//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Refund capacity return (TKT-161, ADR-038). A confirmed claim's whole quantity was added
// to inventory_pools.confirmed_quantity in one step and there was no vocabulary for
// giving part of it back. `claims.returned_quantity` is that vocabulary, orthogonal to
// `status`, and `claim_history` is the idempotency receipt.

// confirmedClaim provisions a pool and drives a buyer hold all the way to confirmed —
// through the real transitions, so the counters start the way production leaves them.
func confirmedClaim(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, qty int32, channel, key string) Claim {
	t.Helper()
	c, _, err := st.CreateHold(ctx, org, slot, uuid.New(), qty, 1000, "EUR", channel, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	confirmed, err := st.Transition(ctx, org, c.ID, "confirmed")
	if err != nil {
		t.Fatal(err)
	}
	return confirmed
}

func availability(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, channel string) Availability {
	t.Helper()
	a, err := st.Availability(ctx, org, slot, channel)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// AC1, the ordinary case the AC's amended wording is about: an open GA pool with no
// draining cut and no channel masking gains exactly q.
func TestReturnRefundedCapacityGivesBackExactlyQ(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	c := confirmedClaim(t, ctx, st, org, slot, 3, "", "ret-exact")

	before := availability(t, ctx, st, org, slot, "")
	out, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 2)
	if err != nil {
		t.Fatalf("return: %v", err)
	}
	if out.Replay || out.UnreturnedQuantity != 1 {
		t.Fatalf("return = %+v, want a fresh application leaving 1 unreturned", out)
	}
	after := availability(t, ctx, st, org, slot, "")
	if after.Available != before.Available+2 {
		t.Fatalf("available %d → %d, want +2", before.Available, after.Available)
	}
	if after.Confirmed != before.Confirmed-2 {
		t.Fatalf("confirmed %d → %d, want -2", before.Confirmed, after.Confirmed)
	}
	var returned int32
	if err := db.QueryRowContext(ctx, `SELECT returned_quantity FROM claims WHERE id=$1`, c.ID).Scan(&returned); err != nil {
		t.Fatal(err)
	}
	if returned != 2 {
		t.Fatalf("returned_quantity = %d, want 2", returned)
	}
}

// AC2: the receipt is the idempotency, and a replay must not move a counter.
func TestReturnRefundedCapacityReplayIsSideEffectFree(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	c := confirmedClaim(t, ctx, st, org, slot, 3, "", "ret-replay")
	refundID := uuid.New()

	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, refundID, 2); err != nil {
		t.Fatal(err)
	}
	mid := availability(t, ctx, st, org, slot, "")

	replay, err := st.ReturnRefundedCapacity(ctx, org, c.ID, refundID, 2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replay || replay.UnreturnedQuantity != 1 {
		t.Fatalf("replay = %+v, want Replay with 1 unreturned", replay)
	}
	if got := availability(t, ctx, st, org, slot, ""); got.Available != mid.Available || got.Confirmed != mid.Confirmed {
		t.Fatalf("replay moved the counters: %+v → %+v", mid, got)
	}
	var receipts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim_history WHERE claim_id=$1 AND action='refund_return'`, c.ID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("refund_return receipts = %d, want 1", receipts)
	}

	// Same refund identity, different quantity: a replay must be the same request or no
	// request at all.
	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, refundID, 1); !errors.Is(err, ErrRefundReturnConflict) {
		t.Fatalf("err = %v, want ErrRefundReturnConflict", err)
	}
}

// The ceiling. A return can never take back more than the claim still owes.
func TestReturnRefundedCapacityRefusesMoreThanUnreturned(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	c := confirmedClaim(t, ctx, st, org, slot, 3, "", "ret-ceiling")

	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 4); !errors.Is(err, ErrRefundReturnExceedsClaim) {
		t.Fatalf("err = %v, want ErrRefundReturnExceedsClaim", err)
	}
	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 2); !errors.Is(err, ErrRefundReturnExceedsClaim) {
		t.Fatalf("err = %v, want ErrRefundReturnExceedsClaim once only 1 is left", err)
	}
	// Exactly the remainder fits.
	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 1); err != nil {
		t.Fatalf("the exact remainder must fit: %v", err)
	}
}

// AC3, deterministically. The pool lock is the serialization, so the test HOLDS the pool
// row and asserts the call blocks on it — rather than starting goroutines and hoping for
// an interleaving. Two racing-goroutine tests in this epic passed against deliberately
// broken code (TKT-156, TKT-157); when the fix is a lock, hold the lock.
func TestReturnRefundedCapacitySerializesOnThePoolLock(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	c := confirmedClaim(t, ctx, st, org, slot, 3, "", "ret-lock")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot).Scan(&one); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 2)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("returned (%v) without taking the pool lock — the contention path is not serialized", err)
	case <-time.After(750 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("return after the lock cleared: %v", err)
	}

	var confirmed, returned int32
	if err := db.QueryRowContext(ctx, `SELECT p.confirmed_quantity, c.returned_quantity FROM inventory_pools p JOIN claims c ON c.pool_id=p.slot_id WHERE c.id=$1`, c.ID).
		Scan(&confirmed, &returned); err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 || returned != 2 {
		t.Fatalf("confirmed=%d returned=%d, want 1 and 2", confirmed, returned)
	}
}

// AC4: every aggregate that counts a confirmed claim's consumption must count it NET of
// what came back. Six call sites share one expression; this proves the channel-visible
// ones, which are where a stale variant would let a channel over- or under-sell.
func TestReturnedQuantityIsSubtractedFromChannelConsumption(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "resale", Cap: 5}}); err != nil {
		t.Fatal(err)
	}
	c := confirmedClaim(t, ctx, st, org, slot, 4, "resale", "ret-channel")

	before := availability(t, ctx, st, org, slot, "resale")
	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 3); err != nil {
		t.Fatal(err)
	}
	after := availability(t, ctx, st, org, slot, "resale")
	if after.Available != before.Available+3 {
		t.Fatalf("channel available %d → %d, want +3: the channel aggregate still counts returned capacity as sold",
			before.Available, after.Available)
	}

	// A fresh hold on that channel must fit in the headroom the return created. This is
	// the admission-side aggregate, which is a different call site from Availability.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.New(), 3, 1000, "EUR", "resale", "ret-channel-refill"); err != nil {
		t.Fatalf("a hold must fit in the returned headroom: %v", err)
	}
}

// AC1's exceptional case, from the plan's worked fixture. A draining capacity cut is in
// flight: the return lowers demand AND effective capacity together, so availability does
// NOT rise — and never exceeds the ceiling. This is why the AC's original "exactly +q"
// wording was amended.
func TestReturnDuringADrainingCutFollowsTheClampInsteadOfRaisingAvailability(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 8)
	c := confirmedClaim(t, ctx, st, org, slot, 8, "", "ret-drain")

	if _, _, err := st.AdjustCapacity(ctx, org, slot, 5, "staff:amy", "cut", "cut-1"); err != nil {
		t.Fatal(err)
	}
	assertPool := func(wantCapacity, wantConfirmed int32, wantTarget sql.NullInt32, wantAvailable int32) {
		t.Helper()
		var capacity, confirmed int32
		var target sql.NullInt32
		if err := db.QueryRowContext(ctx, `SELECT capacity,confirmed_quantity,target_capacity FROM inventory_pools WHERE slot_id=$1`, slot).
			Scan(&capacity, &confirmed, &target); err != nil {
			t.Fatal(err)
		}
		a := availability(t, ctx, st, org, slot, "")
		if capacity != wantCapacity || confirmed != wantConfirmed || target != wantTarget || a.Available != wantAvailable {
			t.Fatalf("pool = (capacity %d, confirmed %d, target %v, available %d), want (%d, %d, %v, %d)",
				capacity, confirmed, target, a.Available, wantCapacity, wantConfirmed, wantTarget, wantAvailable)
		}
	}
	assertPool(8, 8, sql.NullInt32{Int32: 5, Valid: true}, 0)

	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 2); err != nil {
		t.Fatal(err)
	}
	// Demand 6 still exceeds the target: capacity follows demand down, availability stays 0.
	assertPool(6, 6, sql.NullInt32{Int32: 5, Valid: true}, 0)

	if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	// Demand 5 has reached the target: the cut settles and the target clears.
	assertPool(5, 5, sql.NullInt32{}, 0)
}

// Only a confirmed BUYER claim can be returned. Operational holds and group reservations
// transition through their own staff endpoints, exactly as Transition already refuses them.
func TestReturnRefundedCapacityRefusesNonBuyerAndUnconfirmedClaims(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	held, _, err := st.CreateHold(ctx, org, slot, uuid.New(), 2, 1000, "EUR", "", "ret-held")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReturnRefundedCapacity(ctx, org, held.ID, uuid.New(), 1); !errors.Is(err, ErrRefundReturnNotConfirmed) {
		t.Fatalf("err = %v, want ErrRefundReturnNotConfirmed for a held claim", err)
	}

	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 2, "house", "front-of-house", "staff:amy", "allotment", "ret-op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReturnRefundedCapacity(ctx, org, op.ID, uuid.New(), 1); !errors.Is(err, ErrRefundReturnNotConfirmed) {
		t.Fatalf("err = %v, want ErrRefundReturnNotConfirmed for an operational hold", err)
	}
}

// ADR-002. Another organizer's claim is invisible, not merely refused.
func TestReturnRefundedCapacityIsOrganizerScoped(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	c := confirmedClaim(t, ctx, st, org, slot, 2, "", "ret-scope")

	if _, err := st.ReturnRefundedCapacity(ctx, uuid.New(), c.ID, uuid.New(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// AC5. Seated claims were the ticket's hard part and had no test at all until the
// adversarial review said so — the ACs were asserted in prose and nowhere else.

// A FULL seated return releases every live seat in the same transaction. It has to be
// the same transaction: the seats and the capacity are one fact, and a caller that saw
// capacity back while the seats were still held could resell a seat twice.
func TestFullSeatedReturnReleasesEverySeat(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/2", "A/1/3"}, 4500, "EUR", "seat-full")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.ReturnRefundedCapacity(ctx, org, sh.Claim.ID, uuid.New(), 3); err != nil {
		t.Fatalf("full seated return: %v", err)
	}
	var live int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, sh.Claim.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("%d seats still held after a full return", live)
	}

	// And the claim reads as DEAD to pin reconciliation, even though its status is still
	// `confirmed`. Without that, a catalog unpin that failed after the commit would leak a
	// pin `reconcile-pins` could never reclaim — the claim never reaches a terminal status.
	states, err := st.ReconcileSeatClaimStates(ctx, []uuid.UUID{sh.Claim.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[sh.Claim.ID] != SeatClaimDead {
		t.Fatalf("fully returned confirmed claim = %v, want dead so reconcile-pins can reclaim its pin", states[sh.Claim.ID])
	}
}

// A PARTIAL seated return has no correct answer and must refuse rather than guess.
// `claim_seats` is per claim; `tickets` records no seat. Nothing can say which two of
// three seats come back (TKT-164).
func TestPartialSeatedReturnIsRefusedAndChangesNothing(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"B/1/1", "B/1/2", "B/1/3"}, 4500, "EUR", "seat-partial")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	before := availability(t, ctx, st, org, slot, "")

	if _, err := st.ReturnRefundedCapacity(ctx, org, sh.Claim.ID, uuid.New(), 2); !errors.Is(err, ErrPartialSeatedReturn) {
		t.Fatalf("err = %v, want ErrPartialSeatedReturn", err)
	}
	// Nothing moved: not the pool, not the claim, not the seats, not the history.
	if got := availability(t, ctx, st, org, slot, ""); got.Available != before.Available || got.Confirmed != before.Confirmed {
		t.Fatalf("a refused return moved the counters: %+v → %+v", before, got)
	}
	var returned, live, receipts int
	if err := db.QueryRowContext(ctx, `SELECT returned_quantity FROM claims WHERE id=$1`, sh.Claim.ID).Scan(&returned); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, sh.Claim.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim_history WHERE claim_id=$1 AND action='refund_return'`, sh.Claim.ID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if returned != 0 || live != 3 || receipts != 0 {
		t.Fatalf("refused return left returned=%d live_seats=%d receipts=%d, want 0/3/0", returned, live, receipts)
	}

	// A partially returned claim stays LIVE for pin reconciliation — it still holds every
	// seat. Only a fully returned one is dead.
	states, err := st.ReconcileSeatClaimStates(ctx, []uuid.UUID{sh.Claim.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[sh.Claim.ID] != SeatClaimLive {
		t.Fatalf("claim with all seats still held = %v, want live", states[sh.Claim.ID])
	}
}

// ai-review pass 2. Both of these cover a state the schema permits but the happy path
// never produces: `claims.status`/`returned_quantity` and `claim_seats.released_at` are
// not coupled, so they can disagree after a repair, a restore skew, or a future defect.
// `classifySeatClaimsInPool` already says so in its own comment — these are that rule
// applied to the return path.

// A seated claim whose seat rows are ALREADY released must still be treated as seated.
// Inferring seatedness from live rows would let a partial return through as if it were
// GA — and then unpin every seat, because SeatPinRef reads released rows too, stripping
// catalog protection from the seats the remaining live tickets occupy.
func TestPartialSeatedReturnIsRefusedEvenWhenSeatRowsWereAlreadyReleased(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"C/1/1", "C/1/2", "C/1/3"}, 4500, "EUR", "seat-released")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	// The degraded state: rows present, all released, claim still confirmed.
	if _, err = db.ExecContext(ctx, `UPDATE claim_seats SET released_at=now() WHERE claim_id=$1`, sh.Claim.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := st.ReturnRefundedCapacity(ctx, org, sh.Claim.ID, uuid.New(), 2); !errors.Is(err, ErrPartialSeatedReturn) {
		t.Fatalf("err = %v, want ErrPartialSeatedReturn — a claim with released seat rows is still a SEATED claim", err)
	}
}

// A fully returned confirmed claim that still holds live seats must NOT be called dead.
// `reconcile-pins` deletes a dead claim's catalog pin, and deleting one while inventory
// still holds the seats lets a seat-map edit orphan them. Dead is established positively
// or not at all.
func TestFullyReturnedClaimWithLiveSeatsIsNotReconcileDead(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"D/1/1", "D/1/2"}, 4500, "EUR", "seat-skew")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReturnRefundedCapacity(ctx, org, sh.Claim.ID, uuid.New(), 2); err != nil {
		t.Fatal(err)
	}
	// The skew: accounting says fully returned, a seat row says otherwise.
	if _, err = db.ExecContext(ctx, `UPDATE claim_seats SET released_at=NULL WHERE claim_id=$1 AND seat_identity='D/1/1'`, sh.Claim.ID); err != nil {
		t.Fatal(err)
	}

	states, err := st.ReconcileSeatClaimStates(ctx, []uuid.UUID{sh.Claim.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[sh.Claim.ID] == SeatClaimDead {
		t.Fatal("a fully returned claim that still holds a live seat was called dead; reconcile-pins would delete a pin inventory still needs")
	}
}
