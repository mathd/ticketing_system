//go:build smoke

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	// The flake was `available=10 want 8`. 10 is full capacity, which decomposes as
	// capacity 10 − confirmed 0 − held 0 − reserved 0: reserved=0 proves the public read
	// HAD observed the release, so the two reads never disagreed about the boundary —
	// what vanished was the HOLD, whose liveness rode on a TTL clock this test neither
	// controls nor asserts anything about.
	//
	// The claim therefore stays `held` right across release_at, exactly as before, so
	// the post-release held→finalizing→confirmed path keeps its only coverage. What
	// changes is that its expiry is pinned by DATABASE time below instead of being left
	// to the host TTL and the machine's load.
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	// Establish the cutoff by DATABASE time, as the lock-wait test below does: host/DB
	// clock skew must not be a second moving boundary.
	var releaseAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() + interval '2 seconds'`).Scan(&releaseAt); err != nil {
		t.Fatal(err)
	}
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6, ReleaseAt: &releaseAt}})

	// Sell 2 of the 6 before release.
	c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "rel-sold")
	if err != nil {
		t.Fatal(err)
	}
	// Pin the hold's liveness by DATABASE time, so the allocation release is the only
	// moving boundary in a test named for exactly that. The host TTL is now irrelevant
	// to this claim: whatever the machine's load, expiry cannot decide the outcome.
	//
	// Shortening the TTL and finalizing early were both tried and both rejected — the
	// first raced CreateHold against Transition (separate transactions, so narrowing the
	// window cannot close it), and the second finalized BEFORE the cutoff, deleting the
	// post-release held→finalizing→confirmed coverage this test uniquely provides.
	if _, err := db.ExecContext(ctx,
		`UPDATE claims SET expires_at = clock_timestamp() + interval '1 hour' WHERE id=$1`, c.ID); err != nil {
		t.Fatalf("pin the hold beyond the test window by DB time: %v", err)
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
	// The whole point of holding the claim across the cutoff: a buyer claim taken
	// BEFORE release must still complete its lifecycle AFTER it, even though its
	// channel allocation is now inactive. Finalizing early would have deleted this.
	if _, err := st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
		t.Fatalf("pre-release hold must finalize after release: %v", err)
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

// Expiry is an explicit database-state transition here, never an elapsed duration: the
// original version built the store with a 50ms TTL and raced it, so under load exp-1
// expired before exp-2 ran, sweepExpired freed its headroom, and the cap-full assertion
// inverted while the production code was correct (TKT-233). The constructor TTL below is
// deliberately irrelevant — exp-1's expiry is pinned to 'infinity' and then backdated by
// hand, so no delay between any two statements can change the outcome.
func TestExpiredChannelHoldFreesItsCap(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 3}})
	exp1, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "presale", "exp-1")
	if err != nil {
		t.Fatal(err)
	}
	// Pin exp-1 live for the cap-full assertion. 'infinity' satisfies liveClaims
	// (expires_at > now()) and the claims_kind_shape CHECK (buyer expiry is non-NULL),
	// so the hold cannot expire out from under the next statement.
	//
	// Do not add anything that scans this claim's expires_at between here and the backdate
	// below: pgx hands 'infinity' back as a string, and scanning it into Claim.ExpiresAt's
	// *time.Time fails with "unsupported Scan, storing driver.Value type string" (verified
	// against the driver). Transition is the trap — it loads the claim before mutating it.
	// History is safe: it reads claim_history columns only (operational.go:346-347).
	mustAgeClaim(t, ctx, db, exp1.ID, "'infinity'::timestamptz")
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "exp-2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cap full: got %v want ErrUnavailable", err)
	}
	// Expire exp-1 explicitly. Status stays 'held' — only the sweep may flip it, and
	// that is the behaviour under test.
	mustAgeClaim(t, ctx, db, exp1.ID, "now() - interval '1 second'")
	// Read availability BEFORE anything sweeps. Without this read the test would prove far
	// less than it looks: CreateHold sweeps before it counts, so exp-3 alone still succeeds
	// with a broken predicate — the sweep flips exp-1 to 'expired' first and masks the
	// regression. Only a pre-sweep read sees the due-but-unswept row that liveClaims exists
	// to classify.
	ch, err := st.Availability(ctx, org, slot, "presale")
	if err != nil {
		t.Fatal(err)
	}
	// Held is asserted separately from Available on purpose. Available alone is
	// under-discriminating here: it is min(pool remaining, cap - consumed), so with pool
	// capacity 10 a regression that still counted the expired hold pool-side would leave
	// remaining at 7, and min(7,3) is still 3 — the channel arm dominates and hides it
	// (ai-review finding 1). Held reads the pool-level liveClaims sum directly, so the two
	// predicates now fail independently.
	if ch.Held != 0 {
		t.Fatalf("pool after explicit expiry: held=%d want 0 (expired hold must leave the pool-level live sum)", ch.Held)
	}
	if ch.Available != 3 {
		t.Fatalf("channel after explicit expiry: available=%d want 3 (expired hold must not consume the cap)", ch.Available)
	}
	// The expired hold frees channel headroom through the shared live-claims predicate.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "presale", "exp-3"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

// mustAgeClaim rewrites a claim's expires_at to the given SQL timestamp expression,
// leaving status untouched. It asserts exactly one row changed: a silent zero-row update
// would make the caller's fixture vacuously green.
func mustAgeClaim(t *testing.T, ctx context.Context, db *sql.DB, claim uuid.UUID, expiresAt string) {
	t.Helper()
	res, err := db.ExecContext(ctx, `UPDATE claims SET expires_at=`+expiresAt+` WHERE id=$1 AND status='held'`, claim)
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("aging claim to %s: %d rows affected, want 1", expiresAt, n)
	}
}

// ai-review finding 1: a channel-hold transaction that begins before release_at but queues
// on the pool lock across the cutoff must still reject — the release boundary is judged at
// decision time (clock_timestamp), not transaction start (now).
func TestReleaseCutoffHoldsUnderPoolLockContention(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	// The cutoff is established and crossed by DATABASE time — host/DB clock skew must
	// not decide the test.
	var releaseAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() + interval '3 seconds'`).Scan(&releaseAt); err != nil {
		t.Fatal(err)
	}
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
	// Handshake, not a sleep: the test is only meaningful if the hold's transaction is
	// observed WAITING on the pool lock while DB time is still before release_at —
	// otherwise a late transaction start would reject under the old broken predicate too.
	for {
		var waiting, before bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type='Lock' AND state='active'
				  AND query LIKE '%FROM inventory_pools%FOR UPDATE%' AND pid <> pg_backend_pid()
			), clock_timestamp() < $1`, releaseAt).Scan(&waiting, &before); err != nil {
			t.Fatal(err)
		}
		if waiting {
			if !before {
				t.Fatal("lock waiter observed only after release_at; widen the margin")
			}
			break
		}
		if !before {
			t.Fatal("hold transaction never blocked on the pool lock before release_at")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Cross the cutoff by DB time while the waiter stays queued, then let it through.
	for {
		var past bool
		if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() > $1`, releaseAt).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("hold completed while the pool lock was held: %v", err)
	default:
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("queued channel hold crossed the release cutoff: got %v want ErrUnavailable", err)
	}
}

// TKT-76: a capacity cut does not resize channel allocations — caps are upper bounds,
// not guaranteed inventory. The target bounds every claim path; replacement sets are
// validated against the requested target, not the materialized clamp floor.
func TestCapacityCutWithOversizedChannelAllocations(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 60}})
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 20, 0, "", "presale", "cut-pre"); err != nil {
		t.Fatal(err)
	}

	// Cut above demand (20) but below the allocation cap sum (60): applies fully.
	adj, _, err := st.AdjustCapacity(ctx, org, slot, 30, "staff", "reconfig", "cut-30")
	if err != nil || adj.Status != "applied" || adj.Capacity != 30 {
		t.Fatalf("cut: %v %+v", err, adj)
	}
	// The oversized allocation row is untouched.
	var cap32 int32
	if err = db.QueryRowContext(ctx, `SELECT cap FROM channel_allocations WHERE pool_id=$1 AND channel_code='presale'`, slot).Scan(&cap32); err != nil {
		t.Fatal(err)
	}
	if cap32 != 60 {
		t.Fatalf("allocation resized to %d", cap32)
	}
	// Channel headroom (60) no longer decides alone: the pool target (30) does.
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 15, 0, "", "presale", "cut-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hold past target: %v", err)
	}
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "presale", "cut-fits"); err != nil {
		t.Fatalf("hold within target: %v", err)
	}
	// A replacement set exceeding the requested capacity rejects.
	if _, err = st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 40}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversized replacement: %v", err)
	}
	if _, err = st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 30}}); err != nil {
		t.Fatalf("fitting replacement: %v", err)
	}
	// Availability stays clamped, never negative.
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Capacity != 30 || a.Available < 0 {
		t.Fatalf("availability %+v", a)
	}
}

// The channel registry (catalog, TKT-235) is a LOOKUP, NOT A CONSTRAINT — this
// is inventory's half of that guarantee.
//
// Catalog now defines what a channel is, but no column here references it and
// inventory never calls catalog to check one. A code that exists in no registry
// must sell exactly as it did before the registry existed. ADR-024 is why:
// historical attribution has to survive a channel being retired, so an FK from
// claims was refused, and the same argument extends to the rest.
//
// CHARACTERIZATION TEST, NOT TDD EVIDENCE. It was green before TKT-235 existed,
// because inventory has never consulted catalog on this path — that is the point.
// It is here to fail loudly if some future ticket adds a registry lookup to the
// claim path and quietly makes unregistered codes unsellable. It was never
// observed red, and it is not counted among the tests that were.
//
// Fixture note: the allocation is created for the arbitrary code deliberately. A
// code with NO allocation is refused for an unrelated reason (no active
// allocation, asserted above), so without this the test would be measuring
// "unallocated" and would pass against a build that did gate on the registry.
func TestAnUnregisteredChannelCodeStillSells(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	// A code no catalog registry has ever heard of. Inventory does not know, and
	// must not learn.
	const unregistered = "legacy-partner-2019"
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: unregistered, Cap: 4}})

	claim, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", unregistered, "unregistered-hold")
	if err != nil {
		t.Fatalf("hold on an unregistered channel code: %v — the registry is a lookup, not a constraint (ADR-024)", err)
	}
	if claim.Channel != unregistered {
		t.Fatalf("claim.Channel = %q, want %q verbatim — attribution must survive exactly as written", claim.Channel, unregistered)
	}
	// And it confirms, so the whole sale completes rather than only the hold —
	// a registry lookup added to the confirm path would be just as breaking as
	// one added to the hold path.
	if _, err := st.Transition(ctx, org, claim.ID, "finalizing"); err != nil {
		t.Fatalf("finalizing a hold on an unregistered channel code: %v", err)
	}
	confirmed, err := st.Transition(ctx, org, claim.ID, "confirmed")
	if err != nil {
		t.Fatalf("confirming a hold on an unregistered channel code: %v", err)
	}
	if confirmed.Channel != unregistered {
		t.Fatalf("confirmed claim.Channel = %q, want %q verbatim", confirmed.Channel, unregistered)
	}
}
