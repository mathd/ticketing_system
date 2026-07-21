//go:build smoke

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// provisionedSeated seeds a seated pool and returns (org, slot, seatMapID).
func provisionedSeated(t *testing.T, ctx context.Context, st *Postgres, capacity int32) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, capacity); err != nil {
		t.Fatalf("provision seated: %v", err)
	}
	return org, slot, seatMap
}

func TestSeatHoldBasicPersistsClaimAndSeats(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, seatMap := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/3", "A/1/1", "A/1/2"}, 4500, "EUR", "k1")
	if err != nil {
		t.Fatalf("create seat hold: %v", err)
	}
	if sh.Replay {
		t.Fatal("first hold must not be a replay")
	}
	if sh.Claim.Quantity != 3 || sh.Claim.Status != "held" {
		t.Fatalf("claim = %+v", sh.Claim)
	}
	if sh.SeatMapID != seatMap {
		t.Fatalf("seat map = %v want %v", sh.SeatMapID, seatMap)
	}
	// canonical order (sorted): A/1/1, A/1/2, A/1/3
	want := []string{"A/1/1", "A/1/2", "A/1/3"}
	if len(sh.Seats) != 3 || sh.Seats[0] != want[0] || sh.Seats[2] != want[2] {
		t.Fatalf("seats = %v want %v", sh.Seats, want)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, sh.Claim.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("live claim_seats = %d want 3", n)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claim_history WHERE claim_id=$1 AND action='create'`, sh.Claim.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("history create rows = %d want 1", n)
	}
}

func TestSeatHoldRejectsDoubleLiveSeat(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/2"}, 0, "EUR", "k1"); err != nil {
		t.Fatalf("first hold: %v", err)
	}
	// Overlaps on A/1/2 → rejected by the partial unique index.
	_, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/2", "A/1/3"}, 0, "EUR", "k2")
	if !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("overlapping hold err = %v want ErrSeatTaken", err)
	}
	// Disjoint seats succeed.
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/3", "A/1/4"}, 0, "EUR", "k3"); err != nil {
		t.Fatalf("disjoint hold: %v", err)
	}
}

// TestFinalizingSeatStaysExclusive pins the finalizing hole: a seat whose claim is
// mid-checkout (finalizing) is still live and must reject a competing hold. A status
// predicate that omitted 'finalizing' would double-hold here.
func TestFinalizingSeatStaysExclusive(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "finalizing"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k2"); !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("hold during finalizing err = %v want ErrSeatTaken", err)
	}
}

func TestConfirmedSeatStaysExclusive(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "confirmed"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k2"); !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("hold on confirmed seat err = %v want ErrSeatTaken", err)
	}
	// A confirmed seat's pin must NOT be released (still sold).
	var released int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NOT NULL`, sh.Claim.ID).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if released != 0 {
		t.Fatalf("confirmed claim seats released = %d want 0", released)
	}
}

func TestReleaseFreesSeatsInSameTxn(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err = st.Transition(ctx, org, sh.Claim.ID, "released"); err != nil {
		t.Fatalf("release: %v", err)
	}
	var live int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, sh.Claim.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live seats after release = %d want 0", live)
	}
	// Seat is now re-holdable.
	if _, err = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k2"); err != nil {
		t.Fatalf("re-hold after release: %v", err)
	}
}

// TestExpiredSeatReleasedInSameTxnAndSurfaced: a TTL-expired seat frees on the next
// pool mutation (lazy sweep), in the same txn as the expiry flip, and the swept pin is
// surfaced for the caller to unpin.
func TestExpiredSeatReleasedInSameTxnAndSurfaced(t *testing.T) {
	ctx, st, db := storeForTest(t, -time.Second) // every hold is born expired
	org, slot, seatMap := provisionedSeated(t, ctx, st, 100)

	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// A second hold on a different seat sweeps the pool; the first (expired) hold's seat
	// releases and its pin is surfaced.
	sh2, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"B/1/1"}, 0, "EUR", "k2")
	if err != nil {
		t.Fatalf("second hold: %v", err)
	}
	if len(sh2.ExpiredPins) != 1 || sh2.ExpiredPins[0].SeatMapID != seatMap || sh2.ExpiredPins[0].PinnedBy != pinnedBy(sh.Claim.ID) {
		t.Fatalf("expired pins = %+v", sh2.ExpiredPins)
	}
	var live int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, sh.Claim.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("expired claim live seats = %d want 0", live)
	}
	// The expired seat is re-holdable.
	if _, err = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k3"); err != nil {
		t.Fatalf("re-hold expired seat: %v", err)
	}
}

func TestSeatHoldIdempotencyCanonicalizesOrder(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	tt := uuid.New()

	first, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/1", "A/1/2"}, 100, "EUR", "same-key")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same key, seats in a different order → replay of the same claim, not a conflict.
	replay, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/2", "A/1/1"}, 100, "EUR", "same-key")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replay || replay.Claim.ID != first.Claim.ID {
		t.Fatalf("expected replay of %v, got %+v", first.Claim.ID, replay)
	}
	// Same key, different seat set → conflict.
	if _, err = st.CreateSeatHold(ctx, org, slot, tt, []string{"A/1/9"}, 100, "EUR", "same-key"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("different set same key err = %v want ErrIdempotency", err)
	}
}

// TestSeatHoldFingerprintIsInjectiveOverCommas: two DIFFERENT seat sets that a
// comma-join would collide (["A","B,C"] vs ["A,B","C"]) must not replay each other.
func TestSeatHoldFingerprintIsInjectiveOverCommas(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	tt := uuid.New()

	if _, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A", "B,C"}, 0, "EUR", "same"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same key, a genuinely different set that comma-joins identically → must conflict.
	if _, err := st.CreateSeatHold(ctx, org, slot, tt, []string{"A,B", "C"}, 0, "EUR", "same"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("comma-colliding different set, same key err = %v want ErrIdempotency", err)
	}
}

func TestSeatHoldRejectsGaPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot := provisioned(t, ctx, st, 100) // GA pool
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1"); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("seat hold on GA pool err = %v want ErrPoolKindMismatch", err)
	}
}

func TestGaHoldRejectsSeatedPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.New(), 1, 0, "", "", "k1"); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("GA hold on seated pool err = %v want ErrPoolKindMismatch", err)
	}
}

func TestOperationalAndReservationRejectSeatedPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 1, "house", "vip", "staff:x", "r", "op-k"); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("operational hold on seated pool err = %v want ErrPoolKindMismatch", err)
	}
	if _, _, err := st.PlaceGroupReservation(ctx, org, slot, 1, "acme", time.Now().Add(time.Hour), "", "staff:x", "r", "grp-k"); !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("group reservation on seated pool err = %v want ErrPoolKindMismatch", err)
	}
}

func TestSeatHoldCapacityCeiling(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 2) // ceiling of 2 admissions
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/2"}, 0, "EUR", "k1"); err != nil {
		t.Fatalf("hold 2 seats: %v", err)
	}
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/3"}, 0, "EUR", "k2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hold past ceiling err = %v want ErrUnavailable", err)
	}
}

// TestConcurrentOverlappingSeatedHoldsNeverDoubleAllocate is the AC4 contention proof:
// many claimants race for overlapping seat sets; no seat is ever held by two live
// claims. Mirrors TestClosureSerializesAgainstConcurrentHolds (barrier + WaitGroup).
func TestConcurrentOverlappingSeatedHoldsNeverDoubleAllocate(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 1000)

	const claimants = 40
	// Each claimant targets a 2-seat window over a shared 8-seat block → heavy overlap.
	block := []string{"S/1/1", "S/1/2", "S/1/3", "S/1/4", "S/1/5", "S/1/6", "S/1/7", "S/1/8"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, claimants)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := block[i%len(block)]
			b := block[(i+1)%len(block)]
			<-start
			_, errs[i] = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{a, b}, 0, "EUR", uuid.NewString())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrSeatTaken) && !errors.Is(err, ErrUnavailable) {
			t.Fatalf("claimant %d unexpected err: %v", i, err)
		}
	}
	// The invariant: no seat identity is held by more than one live claim.
	var maxDup int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(max(c),0) FROM (
		SELECT count(*) c FROM claim_seats WHERE pool_id=$1 AND released_at IS NULL GROUP BY seat_identity) x`, slot).Scan(&maxDup); err != nil {
		t.Fatal(err)
	}
	if maxDup > 1 {
		t.Fatalf("a seat is held by %d live claims — double-allocation", maxDup)
	}
}
