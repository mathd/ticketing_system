//go:build smoke

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustReplace(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, allocs []ChannelAllocation) {
	t.Helper()
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, allocs); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationReplacementValidatesAndIsAtomic(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 40}, {Channel: "reseller", Cap: 30}})

	// Sum above pool capacity is rejected and the prior set survives.
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 60}, {Channel: "reseller", Cap: 60}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("over-capacity sum: got %v want ErrUnavailable", err)
	}
	a, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Channels) != 2 || a.Channels[0].Cap != 40 || a.Channels[1].Cap != 30 {
		t.Fatalf("prior set did not survive rejected replacement: %+v", a.Channels)
	}

	// Lowering a cap below its consumption is rejected.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "presale", "alloc-consume"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 5}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cap below consumption: got %v want ErrConflict", err)
	}

	// Empty set clears all reservations: the whole remainder is public again.
	mustReplace(t, ctx, st, org, slot, nil)
	av, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if av.Available != 90 {
		t.Fatalf("after clearing allocations available=%d want 90", av.Available)
	}

	// Unknown pool is not found.
	if _, err := st.ReplaceChannelAllocations(ctx, org, uuid.New(), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown pool: got %v want ErrNotFound", err)
	}
}

func TestChannelCapAndPoolCapacityRejectIndependently(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})

	// A hold beyond the channel cap rejects even with pool headroom.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 7, 0, "", "presale", "ch-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("channel over cap: got %v want ErrUnavailable", err)
	}
	// A hold on a channel with no active allocation rejects.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "nosuch", "ch-none"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown channel: got %v want ErrUnavailable", err)
	}
	// Public holds cannot eat the unsold channel reservation.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 5, 0, "", "", "pub-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("public into reservation: got %v want ErrUnavailable", err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 4, 0, "", "", "pub-ok"); err != nil {
		t.Fatalf("public within headroom: %v", err)
	}
	// Pool exhaustion rejects a channel hold despite cap headroom: an operational hold
	// (pool-only, unchanneled) eats the remaining global capacity.
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 4, "house", "foh", "staff:amy", "ops", "op-eat"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "presale", "ch-starved"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pool exhausted: got %v want ErrUnavailable", err)
	}
	if c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "ch-fits"); err != nil || c.Channel != "presale" {
		t.Fatalf("channel within both limits: %v %+v", err, c)
	}
}

func TestChannelJoinsFingerprintAndLegacyIsPinned(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 50}})

	c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "fp-key")
	if err != nil {
		t.Fatal(err)
	}
	if c.Channel != "presale" {
		t.Fatalf("channel not carried: %+v", c)
	}
	// Exact replay with the same channel returns the original claim.
	r, replay, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "fp-key")
	if err != nil || !replay || r.ID != c.ID || r.Channel != "presale" {
		t.Fatalf("replay: %v replay=%v %+v", err, replay, r)
	}
	// Same key with a different or omitted channel is a key reuse.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "other", "fp-key"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("different channel: got %v want ErrIdempotency", err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "", "fp-key"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("omitted channel: got %v want ErrIdempotency", err)
	}

	// The channel-less fingerprint stays byte-identical to the pre-channel format, so
	// idempotency records created before this migration keep replaying (ADR-009).
	pub, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "", "fp-legacy")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT request_fingerprint FROM claims WHERE id=$1`, pub.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%d:%d:%s", org, slot, uuid.Nil, 3, 0, ""))))
	if stored != legacy {
		t.Fatalf("legacy fingerprint drifted: stored=%s want=%s", stored, legacy)
	}
}

func TestChannelSurvivesLifecycleAndConvertStaysPublic(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 50}})

	c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "life-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	got, err := st.Transition(ctx, org, c.ID, "confirmed")
	if err != nil || got.Channel != "presale" {
		t.Fatalf("confirm: %v channel=%q want presale", err, got.Channel)
	}
	// Confirmed consumption still counts against the cap.
	a, err := st.Availability(ctx, org, slot, "presale")
	if err != nil || a.Available != 48 {
		t.Fatalf("channel availability after confirm: %v %+v", err, a)
	}

	// A converted operational hold becomes a public buyer hold, never a channel one.
	op, _, err := st.PlaceOperationalHold(ctx, org, slot, 5, "house", "foh", "staff:amy", "ops", "conv-src")
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := st.ConvertOperational(ctx, org, op.ID, uuid.New(), slot, 1, 1000, "EUR", "staff:amy", "override", "conv-key")
	if err != nil {
		t.Fatal(err)
	}
	var childChannel *string
	if err := db.QueryRowContext(ctx, `SELECT channel_code FROM claims WHERE id=$1`, res.Child.ID).Scan(&childChannel); err != nil {
		t.Fatal(err)
	}
	if childChannel != nil {
		t.Fatalf("converted child carries channel %q, want public", *childChannel)
	}
}

func TestScheduledReleaseIsLazyAndObservable(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	releaseAt := time.Now().UTC().Add(2 * time.Second)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6, ReleaseAt: &releaseAt}})

	// Sell 2 of the 6 before release.
	c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "rel-sold")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := st.Availability(ctx, org, slot, "")
	if err != nil || pub.Available != 4 {
		t.Fatalf("public before release: %v %+v want available 4", err, pub)
	}
	ch, err := st.Availability(ctx, org, slot, "presale")
	if err != nil || ch.Available != 4 {
		t.Fatalf("channel before release: %v %+v want available 4", err, ch)
	}

	// Cross release_at by DB time. Reads only — no mutation, no sweeper.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ch, err = st.Availability(ctx, org, slot, "presale")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Available == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ch.Available != 0 {
		t.Fatalf("channel after release: available=%d want 0", ch.Available)
	}
	pub, err = st.Availability(ctx, org, slot, "")
	if err != nil || pub.Available != 8 {
		t.Fatalf("public after release: %v available=%d want 8 (unsold 4 returned)", err, pub.Available)
	}
	// New channel holds reject; the pre-release hold still finishes its lifecycle.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "rel-late"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("post-release channel hold: got %v want ErrUnavailable", err)
	}
	if _, err := st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(ctx, org, c.ID, "confirmed"); err != nil {
		t.Fatalf("pre-release hold must confirm after release: %v", err)
	}
}

func TestAllocationSumValidationDoesNotWrapInt32(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, math.MaxInt32)
	// Two caps whose int32 sum wraps negative; int64 math must still reject the total.
	over := []ChannelAllocation{{Channel: "a", Cap: math.MaxInt32 - 2}, {Channel: "b", Cap: math.MaxInt32 - 2}}
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, over); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrapping sum: got %v want ErrUnavailable", err)
	}
}

func TestExpiredChannelHoldFreesItsCap(t *testing.T) {
	ctx, st, _ := storeForTest(t, 50*time.Millisecond)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 3}})
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "presale", "exp-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "exp-2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cap full: got %v want ErrUnavailable", err)
	}
	time.Sleep(80 * time.Millisecond)
	// The expired hold frees channel headroom through the shared live-claims predicate.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "presale", "exp-3"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

// ai-review finding 1: a channel-hold transaction that begins before release_at but queues
// on the pool lock across the cutoff must still reject — the release boundary is judged at
// decision time (clock_timestamp), not transaction start (now).
func TestReleaseCutoffHoldsUnderPoolLockContention(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	releaseAt := time.Now().UTC().Add(400 * time.Millisecond)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6, ReleaseAt: &releaseAt}})

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, slot); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "cutoff-key")
		done <- err
	}()
	time.Sleep(900 * time.Millisecond) // hold the lock well past release_at
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("queued channel hold crossed the release cutoff: got %v want ErrUnavailable", err)
	}
}
