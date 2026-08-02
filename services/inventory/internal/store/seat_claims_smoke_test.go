//go:build smoke

package store

import (
	"context"
	"database/sql"
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
	if err := st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, capacity, false, nil); err != nil {
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

	// TKT-173: the error must name WHICH seats lost, not merely that one did.
	// A buyer whose selection partly collided needs to re-render the seats they
	// actually have to give up; "a seat was taken" is not enough to do that, and
	// the losing set is knowable only here, inside the transaction that arbitrated.
	//
	// Two overlaps, deliberately: the per-seat insert loop this replaces returned
	// on the FIRST unique violation, so a per-seat error would have reported one
	// seat where two were contended and looked correct while being wrong.
	if _, err = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/9"}, 0, "EUR", "k-extra"); err != nil {
		t.Fatalf("seed A/1/9: %v", err)
	}
	_, err = st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1", "A/1/5", "A/1/9"}, 0, "EUR", "k4")
	var taken *SeatTakenError
	if !errors.As(err, &taken) {
		t.Fatalf("err = %v (%T) want a *SeatTakenError carrying the contended identities", err, err)
	}
	if !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("the typed error must still unwrap to ErrSeatTaken, got %v", err)
	}
	// Sorted, exact: A/1/1 and A/1/9 are held by other claims; A/1/5 is free and
	// must NOT be named — telling a buyer to give up a seat they could have had is
	// its own defect.
	if len(taken.Seats) != 2 || taken.Seats[0] != "A/1/1" || taken.Seats[1] != "A/1/9" {
		t.Fatalf("contended seats = %v want [A/1/1 A/1/9]", taken.Seats)
	}
	// The losing claim leaves nothing behind: no claims row, no partial seat rows.
	var orphanSeats, orphanClaims int
	if err = dbOf(t, st).QueryRowContext(ctx,
		`SELECT count(*) FROM claim_seats WHERE seat_identity='A/1/5'`).Scan(&orphanSeats); err != nil {
		t.Fatal(err)
	}
	if err = dbOf(t, st).QueryRowContext(ctx,
		`SELECT count(*) FROM claims WHERE idempotency_key='k4'`).Scan(&orphanClaims); err != nil {
		t.Fatal(err)
	}
	if orphanSeats != 0 || orphanClaims != 0 {
		t.Fatalf("a refused seat hold must roll back entirely: %d stray seat rows, %d stray claims",
			orphanSeats, orphanClaims)
	}
	// Disjoint seats succeed.
	if _, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/3", "A/1/4"}, 0, "EUR", "k3"); err != nil {
		t.Fatalf("disjoint hold: %v", err)
	}
}

// dbOf reaches the store's connection for assertions about rows the API does not
// return. storeForTest already hands back the *sql.DB; this exists so the
// double-live-seat test can assert rollback without changing that helper's shape.
func dbOf(t *testing.T, st *Postgres) *sql.DB {
	t.Helper()
	return st.db
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

// TestReconcileSeatClaimStatesClassifiesAfterExpirySweep is the liveness verdict behind
// reconcile-pins (TKT-112). It asserts the exact verdict for every lifecycle a catalog
// `hold:` pin can point at, plus the two negative cases the reconciler must not confuse:
// a claim that is due but not yet swept (the verdict must MAKE it terminal, not predict it)
// and a claim id this database has never seen (unknown, never dead).
func TestReconcileSeatClaimStatesClassifiesAfterExpirySweep(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	hold := func(key string, seats ...string) uuid.UUID {
		t.Helper()
		sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), seats, 0, "EUR", key)
		if err != nil {
			t.Fatalf("hold %s: %v", key, err)
		}
		return sh.Claim.ID
	}

	held := hold("k-held", "A/1/1")
	finalizing := hold("k-finalizing", "A/1/2")
	if _, err := st.Transition(ctx, org, finalizing, "finalizing"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	confirmed := hold("k-confirmed", "A/1/3")
	if _, err := st.Transition(ctx, org, confirmed, "confirmed"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	released := hold("k-released", "A/1/4")
	if _, err := st.Transition(ctx, org, released, "released"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Due but unswept: backdate expires_at without touching status, exactly the state a
	// pool nobody has held against since the on-sale is left in.
	due := hold("k-due", "A/1/5")
	if _, err := db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1`, due); err != nil {
		t.Fatal(err)
	}
	var stillHeld string
	if err := db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1`, due).Scan(&stillHeld); err != nil {
		t.Fatal(err)
	}
	if stillHeld != "held" {
		t.Fatalf("fixture: due claim status = %s, want an unswept 'held'", stillHeld)
	}
	unknown := uuid.New()

	got, err := st.ReconcileSeatClaimStates(ctx, []uuid.UUID{held, finalizing, confirmed, released, due, unknown})
	if err != nil {
		t.Fatalf("reconcile states: %v", err)
	}
	want := map[uuid.UUID]SeatClaimState{
		held:       SeatClaimLive,
		finalizing: SeatClaimLive,
		confirmed:  SeatClaimLive,
		released:   SeatClaimDead,
		due:        SeatClaimDead,
		unknown:    SeatClaimUnknown,
	}
	if len(got) != len(want) {
		t.Fatalf("verdicts = %v want one per requested claim %v", got, want)
	}
	for id, wantState := range want {
		if got[id] != wantState {
			t.Fatalf("claim %s verdict = %q want %q (all: %v)", id, got[id], wantState, got)
		}
	}

	// The dead verdict must be a FACT the call created, not a prediction: the due claim is
	// now terminal with its seats released, so a finalizer queued behind the pool lock sees
	// a status it must refuse rather than a time comparison it can win.
	var status string
	var releasedAt *time.Time
	if err := db.QueryRowContext(ctx, `SELECT c.status, cs.released_at FROM claims c
		JOIN claim_seats cs ON cs.claim_id=c.id WHERE c.id=$1`, due).Scan(&status, &releasedAt); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || releasedAt == nil {
		t.Fatalf("due claim after verdict: status=%s released_at=%v, want expired + released", status, releasedAt)
	}
	// Every claim that survived must still hold its seats.
	for _, id := range []uuid.UUID{held, finalizing, confirmed} {
		var live int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim_seats WHERE claim_id=$1 AND released_at IS NULL`, id).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 1 {
			t.Fatalf("live claim %s has %d live seat rows want 1", id, live)
		}
	}
}

// TestReconcileSeatClaimStatesSerializesWithFinalize is the lock-queue race (ADR-010,
// docs/learnings/2026-07-16-lock-queue-time-cutoffs.md). A finalize that BEGAN before the
// TTL elapsed sits behind the pool lock; the reconciler must not be able to declare the
// claim dead and have the finalize then succeed anyway. Whichever order the lock grants,
// the two must agree — a dead verdict implies the finalize is refused.
func TestReconcileSeatClaimStatesSerializesWithFinalize(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)
	sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{"A/1/1"}, 0, "EUR", "k1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	claim := sh.Claim.ID
	if _, err = db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 second' WHERE id=$1`, claim); err != nil {
		t.Fatal(err)
	}

	// Hold the pool lock so both contenders queue behind a third party, then release it and
	// let them race for real.
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var verdicts map[uuid.UUID]SeatClaimState
	var verdictErr, finalizeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		verdicts, verdictErr = st.ReconcileSeatClaimStates(ctx, []uuid.UUID{claim})
	}()
	go func() {
		defer wg.Done()
		_, finalizeErr = st.Transition(ctx, org, claim, "finalizing")
	}()
	time.Sleep(150 * time.Millisecond) // let both block on the pool lock
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if verdictErr != nil {
		t.Fatalf("verdict: %v", verdictErr)
	}
	var status string
	if err = db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1`, claim).Scan(&status); err != nil {
		t.Fatal(err)
	}
	// The two outcomes must be consistent. A dead verdict may never coexist with a claim
	// that went on to finalize — that is the combination that would strand a pinned seat.
	if verdicts[claim] == SeatClaimDead && (finalizeErr == nil || status == "finalizing") {
		t.Fatalf("dead verdict but finalize succeeded (status=%s finalizeErr=%v) — the verdict raced the lock", status, finalizeErr)
	}
	if verdicts[claim] == SeatClaimLive && status == "expired" {
		t.Fatalf("live verdict for a claim that is now expired (finalizeErr=%v)", finalizeErr)
	}
}

// TestReconcileSeatClaimStatesKeepsLiveClaimsWithInconsistentSeatRows closes the fail-open hole
// ai-review F1 found. The first cut derived "dead" as the INVERSE of "has a live claim_seats row
// AND has a live status", so any live claim whose seat rows were missing or already released came
// out dead and had its catalog pin deleted. Nothing in the schema couples claim status to
// claim_seats.released_at, so that shape is representable — by a `hold:` pin naming a GA claim
// (no seat rows at all, and the batch-pin endpoint is manually callable), or by restore skew or an
// earlier defect. Those are exactly the degraded-data cases the unknown verdict fails closed for,
// so failing OPEN here was inconsistent with the design's own safety rule.
//
// Dead is now established positively from a terminal status. A live status is live whatever its
// child rows look like.
func TestReconcileSeatClaimStatesKeepsLiveClaimsWithInconsistentSeatRows(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 100)

	hold := func(key, seat string) uuid.UUID {
		t.Helper()
		sh, err := st.CreateSeatHold(ctx, org, slot, uuid.New(), []string{seat}, 0, "EUR", key)
		if err != nil {
			t.Fatalf("hold %s: %v", key, err)
		}
		return sh.Claim.ID
	}

	// Held, with its seat row released out from under it.
	heldReleased := hold("k-held-released", "A/2/1")
	if _, err := db.ExecContext(ctx, `UPDATE claim_seats SET released_at=now() WHERE claim_id=$1`, heldReleased); err != nil {
		t.Fatal(err)
	}
	// Finalizing, with its seat row deleted entirely.
	finalizingGone := hold("k-finalizing-gone", "A/2/2")
	if _, err := st.Transition(ctx, org, finalizingGone, "finalizing"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM claim_seats WHERE claim_id=$1`, finalizingGone); err != nil {
		t.Fatal(err)
	}
	// Confirmed — a SOLD seat — with its seat row released.
	confirmedReleased := hold("k-confirmed-released", "A/2/3")
	if _, err := st.Transition(ctx, org, confirmedReleased, "confirmed"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE claim_seats SET released_at=now() WHERE claim_id=$1`, confirmedReleased); err != nil {
		t.Fatal(err)
	}

	got, err := st.ReconcileSeatClaimStates(ctx, []uuid.UUID{heldReleased, finalizingGone, confirmedReleased})
	if err != nil {
		t.Fatalf("reconcile states: %v", err)
	}
	for _, c := range []struct {
		id   uuid.UUID
		what string
	}{
		{heldReleased, "held claim whose seat row was released"},
		{finalizingGone, "finalizing claim whose seat row is missing"},
		{confirmedReleased, "CONFIRMED (sold) claim whose seat row was released"},
	} {
		if got[c.id] != SeatClaimLive {
			t.Fatalf("%s: verdict = %q want %q — a live status must never be reclaimable", c.what, got[c.id], SeatClaimLive)
		}
	}
}
