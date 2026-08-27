//go:build smoke

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustReplace(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, allocs []ChannelAllocation) {
	t.Helper()
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, allocs, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationReplacementValidatesAndIsAtomic(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 40}, {Channel: "reseller", Cap: 30}})

	// Sum above pool capacity is rejected and the prior set survives.
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 60}, {Channel: "reseller", Cap: 60}}, nil); !errors.Is(err, ErrUnavailable) {
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
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 5}}, nil); !errors.Is(err, ErrConflict) {
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
	if _, err := st.ReplaceChannelAllocations(ctx, org, uuid.New(), nil, nil); !errors.Is(err, ErrNotFound) {
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
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, over, nil); !errors.Is(err, ErrUnavailable) {
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
	if _, err = st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 40}}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversized replacement: %v", err)
	}
	if _, err = st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 30}}, nil); err != nil {
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
// SCOPE, STATED NARROWLY (ai-review). This covers the INVENTORY-STORE portion of
// the invariant and nothing more. It calls the store directly, so it does not
// traverse commerce orchestration, catalog fee or split resolution, payments, or
// any API boundary — a registry lookup added at one of those layers would reject
// the sale earlier and leave this test green. The platform-wide claim ("an
// unregistered channel still sells, end to end") is therefore NOT proven here;
// TKT-241 owns the gateway-level version. Catalog's companion test
// (TestNothingReferencesTheChannelRegistry) is similarly bounded: it proves FK
// absence in catalog's schema, not the absence of application-level gating.
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

// setWindow writes a sales window directly, in DB time.
//
// Bounds are expressed as clock_timestamp() arithmetic, never a Go-side literal
// and never a time.Sleep. Two reasons, both learned here: a wall-clock fixture
// rots the moment the suite runs slower than it did at merge (TKT-233's flake),
// and a bound computed in Go can disagree with the server's clock by enough to
// straddle the boundary under load — which is the exact condition the window is
// supposed to decide correctly.
func setWindow(t *testing.T, ctx context.Context, db *sql.DB, slot uuid.UUID, channel, opensExpr, closesExpr string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`UPDATE channel_allocations SET opens_at=`+opensExpr+`, closes_at=`+closesExpr+
			` WHERE pool_id=$1 AND channel_code=$2`, slot, channel); err != nil {
		t.Fatal(err)
	}
}

// The window gates claims, and its refusal is DISTINGUISHABLE from a sellout.
//
// Both halves matter. If it did not refuse, the feature does nothing; if it
// refused with ErrUnavailable, a caller could not tell "not selling yet" from
// "sold out" — opposite actions, and the whole reason COS-3 asks for a distinct
// refusal.
func TestChannelSalesWindowGatesHoldsAndIsDistinguishable(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})

	// Not open yet.
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() + interval '1 hour'", "NULL")
	_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "win-early")
	if !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("before the window opens: got %v want ErrChannelWindowClosed — and NOT "+
			"ErrUnavailable, which would read as sold out", err)
	}
	// The pool and the cap both have room; only the window refused.
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("the window refusal is also an ErrUnavailable — the two must be distinct sentinels")
	}

	// Already closed.
	setWindow(t, ctx, db, slot, "presale", "NULL", "clock_timestamp() - interval '1 second'")
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "win-late"); !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("after the window closes: got %v want ErrChannelWindowClosed", err)
	}

	// Open. Same allocation, same pool, same quantity — only the window moved.
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() - interval '1 hour'", "clock_timestamp() + interval '1 hour'")
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "win-open"); err != nil {
		t.Fatalf("inside the window: %v", err)
	}

	// No window at all is always open — every allocation that existed before this
	// migration has NULL bounds, so this is the regression that matters most.
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "win-none"); err != nil {
		t.Fatalf("no window configured must behave exactly as before TKT-238: %v", err)
	}
}

// The window is half-open [opens_at, closes_at) — asserted against the PREDICATE
// itself, because a claim-path fixture cannot AIM at the boundary.
//
// Why not through CreateHold: setWindow writes the bounds as clock_timestamp()
// arithmetic in an UPDATE, and CreateHold reads them back in a LATER, SEPARATE
// statement. Nothing in that arrangement lets the test say where the bound
// falls relative to the instant the predicate evaluates — the gap is whatever
// the wall clock did in between, and it is neither controllable nor
// reproducible. Note what is NOT being claimed: not that the bound is always in
// the past, and not that landing exactly on it is impossible. Both would be
// guarantees about a non-monotonic wall clock, which cannot give them (see
// docs/learnings/2026-08-09-a-total-order-is-not-a-meaningful-one.md §2 and
// TKT-234 — clock_timestamp() narrows the window, it does not close it).
//
// The weaker statement is the one that condemns the fixture: it cannot place
// the bound on the boundary DELIBERATELY. So for `>` vs `>=` on the close and
// `<` vs `<=` on the open, the mutant's verdict is decided by whatever the
// clock did, not by the test. Both failure directions follow, and the quiet one
// is the dangerous one: such a fixture may fail on an accidental equality, and
// — far more often — pass green while the mutant is still live, because a bound
// that lands clearly before or after the evaluation instant is answered the
// same way by either operator. A green run is therefore not evidence that the
// boundary operator is correct. That is why the first version of this test left
// those two mutants alive: not because equality is impossible, but because
// nothing in the fixture could aim at it.
//
// The third mutant of that first version, `now()` for `clock_timestamp()`, is a
// different problem with a different answer, and is pinned by a PAIR of tests
// below rather than by this one. Each holds a transaction open across a bound so
// the frozen and moving clocks diverge, and each covers ONE of the two
// clock_timestamp() occurrences in windowOpen:
// TestWindowPredicateDecidesAtDecisionTimeNotTransactionStart the close side,
// TestWindowPredicateOpenSideDecidesAtDecisionTimeNotTransactionStart the open
// side (TKT-275).
//
// The pair is not duplication, and neither half is redundant. The close-side
// case sets opens_at to NULL, and `NULL IS NULL` short-circuits the open half to
// true under EITHER clock — so it is structurally blind to a substitution on the
// open side, and a mutation of that occurrence alone leaves it green. That is
// what the open-side case exists to catch, and what makes the close-side case
// its control: red there and green here is the evidence the substitution was
// caught by the new occurrence rather than by coverage that already existed.
// Deleting either one leaves an occurrence of the shipped predicate unpinned.
//
// Say exactly what the pair does and does not pin, because the two are easy to
// conflate. Both evaluate the shipped const in an ad hoc SELECT, so what they
// pin is the CLOCK SEMANTICS OF THE EXPRESSION — that windowOpen reads
// clock_timestamp() and not now(), on both bounds. They do NOT pin the WIRING:
// that the claim paths interpolate this const at all, or embed it with the right
// scope and ordering. windowOpen is a shared const spliced into the claim paths
// (store.go, reservations.go), which is what carries the expression's semantics
// into production — but a path that stopped using it, or used it wrongly, would
// leave both of these green. That gap is covered where it belongs, at the claim
// paths — and there are TWO of them, each with its own windowOpen query, so
// naming only one would repeat in miniature the mistake this paragraph exists to
// correct. CreateHold (store.go) is driven through not-yet-open, closed, open and
// no-window by TestChannelSalesWindowGatesHoldsAndIsDistinguishable;
// PlaceGroupReservation (reservations.go) has its own query and its own refusal,
// pinned by TestGroupDrawDownSurvivesAClosedWindowButPlacementDoesNot and
// TestClosedWindowRefusesAsAWindowEvenOnAnExhaustedPool. The sibling release_at
// cutoff is pinned under a real lock wait by
// TestReleaseCutoffHoldsUnderPoolLockContention.
//
// Do not reach for a within-statement argument here either: adjacent
// clock_timestamp() calls in ONE expression barely move. Measured on one
// machine on one run, over 20,000 rows evaluating it twice per row, the two
// calls were equal 19,653 times and differed by exactly 1µs the other 347; the
// clock is also coarse across rows (2,000 rows produced 86 distinct values).
// Observations, not guarantees — re-run them before building on them, and do
// not turn them into a rule about ordering.
//
// Evaluating the predicate against literal bounds makes the boundary
// expressible: the instant is fixed, so "at the bound" is a real case rather
// than a race with the clock.
func TestChannelSalesWindowIsHalfOpen(t *testing.T) {
	ctx, _, db := storeForTest(t, time.Minute)

	// `at` is the evaluation instant; each row sets the bounds relative to it.
	// The predicate under test is the shipped const, not a copy — a hand-copied
	// reduction would drift from what the claim path runs.
	eval := func(t *testing.T, opens, closes string) bool {
		t.Helper()
		var open bool
		q := `SELECT ` + windowOpen + ` FROM (SELECT ` + opens + `::timestamptz AS opens_at, ` +
			closes + `::timestamptz AS closes_at) w`
		// The predicate reads clock_timestamp(); pin it to a literal so the bounds
		// can be placed exactly ON it.
		q = strings.ReplaceAll(q, "clock_timestamp()", "$1::timestamptz")
		if err := db.QueryRowContext(ctx, q, "2026-08-10T12:00:00Z").Scan(&open); err != nil {
			t.Fatal(err)
		}
		return open
	}

	const at = `'2026-08-10T12:00:00Z'`
	const before = `'2026-08-10T11:00:00Z'`
	const after = `'2026-08-10T13:00:00Z'`

	cases := []struct {
		name          string
		opens, closes string
		want          bool
	}{
		// The two that matter, and the two no clock-relative fixture can express.
		{"opens_at EXACTLY at the instant is OPEN (inclusive lower bound)", at, "NULL", true},
		{"closes_at EXACTLY at the instant is CLOSED (exclusive upper bound)", "NULL", at, false},

		{"no bounds at all is always open", "NULL", "NULL", true},
		{"opened in the past", before, "NULL", true},
		{"opens in the future", after, "NULL", false},
		{"closes in the future", "NULL", after, true},
		{"closed in the past", "NULL", before, false},
		{"inside a bounded window", before, after, true},
		{"before a bounded window", at, after, true},
		{"after a bounded window", before, at, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eval(t, tc.opens, tc.closes); got != tc.want {
				t.Fatalf("windowOpen(opens=%s, closes=%s) = %v, want %v", tc.opens, tc.closes, got, tc.want)
			}
		})
	}
}

// clock_timestamp(), not now(): the predicate must decide at DECISION time, not
// at transaction-start time.
//
// This is the property ADR-024 wrote down for release_at and that
// TestReleaseCutoffHoldsUnderPoolLockContention pins for it. A window written
// with now() reintroduces a bug already litigated in this file — and a
// clock-relative fixture cannot tell the two apart, because in a short
// transaction they are microseconds apart.
//
// Made observable by holding a transaction open across the boundary: inside one
// transaction now() is frozen at its start while clock_timestamp() keeps moving,
// so a window that closed DURING the transaction is still "open" to now() and
// correctly "closed" to clock_timestamp(). Verified against this database:
// after a 50ms sleep inside a transaction, `now() < clock_timestamp()` is true.
func TestWindowPredicateDecidesAtDecisionTimeNotTransactionStart(t *testing.T) {
	ctx, _, db := storeForTest(t, time.Minute)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Freeze the transaction's now() well before the bound we are about to set.
	var frozen time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_sleep(0.05)`); err != nil {
		t.Fatal(err)
	}

	// A window that closed AFTER this transaction began but BEFORE now: frozen
	// now() still thinks it is open; clock_timestamp() knows it is not.
	var byClock, byNow bool
	err = tx.QueryRowContext(ctx, `
		SELECT `+windowOpen+`,
		       (opens_at IS NULL OR opens_at <= now()) AND (closes_at IS NULL OR closes_at > now())
		FROM (SELECT NULL::timestamptz AS opens_at, $1::timestamptz + interval '10 milliseconds' AS closes_at) w`,
		frozen).Scan(&byClock, &byNow)
	if err != nil {
		t.Fatal(err)
	}
	if byNow != true {
		t.Fatalf("the fixture did not reproduce the divergence: now() says closed already, " +
			"so this test cannot distinguish the two clocks")
	}
	if byClock {
		t.Fatal("the shipped predicate reports OPEN for a window that closed before decision time — " +
			"it is reading now(), which freezes at transaction start. A hold queued on the pool lock " +
			"across the cutoff would sell a closed channel (ADR-024's reasoning for release_at).")
	}
}

// The same property, on the OPEN side — the occurrence the case above cannot
// see (TKT-275).
//
// windowOpen names clock_timestamp() twice, and the case above sets opens_at to
// NULL. `NULL IS NULL` short-circuits the open half to true under EITHER clock,
// so that case is structurally blind to a now()-for-clock_timestamp()
// substitution on the open side: mutate that occurrence alone and it stays green. The
// pair is therefore not decoration. This case is the open-side evidence and the
// case above is its control — a run where this one goes red and that one stays
// green is what proves the substitution was caught HERE and not somewhere that
// already covered it. Deleting either one leaves an occurrence unpinned.
//
// The mirror, not the copy: the guard flips direction. Above, now() must say
// OPEN (the window closed during the transaction). Here, now() must say
// NOT-YET-OPEN (the window opened during the transaction) while the shipped
// predicate says OPEN. A guard copied across without flipping would assert
// something the fixture always satisfies, and would not be a guard.
//
// Why the open side is not symmetric decoration: it governs ON-SALE. Written
// with now(), "has the sale opened?" is answered at transaction-start time, so
// a hold that queues on the pool row lock (ADR-010) before the on-sale instant
// and acquires it after reads the frozen start time and is refused as
// not-yet-open — at the highest-contention moment the system has. Same class as
// the release_at bug ADR-024 litigated, on the other bound.
//
// Scope, stated so it is not read as more than it is: this evaluates the shipped
// const directly, so it pins the expression's CLOCK CHOICE, not the claim path's
// use of it. The lock wait above is the motivation, not the fixture — the
// transaction is held open to manufacture the same divergence a lock wait
// produces, without a second session. The claim paths themselves are covered by
// TestChannelSalesWindowGatesHoldsAndIsDistinguishable (CreateHold) and
// TestGroupDrawDownSurvivesAClosedWindowButPlacementDoesNot (the separate
// PlaceGroupReservation query), and a real lock wait by
// TestReleaseCutoffHoldsUnderPoolLockContention.
func TestWindowPredicateOpenSideDecidesAtDecisionTimeNotTransactionStart(t *testing.T) {
	ctx, _, db := storeForTest(t, time.Minute)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Freeze the transaction's now() just before the bound we are about to set.
	var frozen time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_sleep(0.05)`); err != nil {
		t.Fatal(err)
	}

	// A window that opened AFTER this transaction began but BEFORE now: frozen
	// now() still thinks it has not opened; clock_timestamp() knows it has. The
	// bound sits 10ms past the freeze and the sleep is 50ms, so the decision-time
	// clock clears it with room to spare. closes_at is NULL so nothing but the
	// open-side comparison can decide the result.
	var byClock, byNow bool
	err = tx.QueryRowContext(ctx, `
		SELECT `+windowOpen+`,
		       (opens_at IS NULL OR opens_at <= now()) AND (closes_at IS NULL OR closes_at > now())
		FROM (SELECT $1::timestamptz + interval '10 milliseconds' AS opens_at, NULL::timestamptz AS closes_at) w`,
		frozen).Scan(&byClock, &byNow)
	if err != nil {
		t.Fatal(err)
	}
	if byNow != false {
		t.Fatalf("the fixture did not reproduce the divergence: now() says open already, " +
			"so this test cannot distinguish the two clocks")
	}
	if !byClock {
		t.Fatal("the shipped predicate reports NOT-YET-OPEN for a window that opened before decision " +
			"time — it is reading now(), which freezes at transaction start. A hold that queued on the " +
			"pool lock before the on-sale instant and acquired it after would be refused (ADR-024's " +
			"reasoning for release_at, on the bound that governs on-sale).")
	}
}

// A closed window does NOT release the channel's capacity to the public
// (ADR-054 §Decision B).
//
// This is the decision that produces different public availability numbers with
// no test failing either way, so it is pinned explicitly. A presale's cap is a
// promise about capacity; a promise that evaporates until its window opens is
// not one, and the public on-sale would eat the presale's allocation overnight.
//
// Deliberately NOT symmetric with release_at, which DOES release
// (TestScheduledReleaseIsLazyAndObservable). The two answer different questions:
// release_at says the allocation is over, a window says it is not its turn yet.
func TestClosedWindowAllocationRemainsReservedFromPublic(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() + interval '1 hour'", "NULL")

	// The public sees 4, not 10: the presale's 6 are still reserved.
	av, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if av.Available != 4 {
		t.Fatalf("public available=%d want 4 — a closed window must not hand the presale's "+
			"capacity to the public sale before it opens", av.Available)
	}

	// And the public cannot claim into it.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 5, 0, "", "", "pub-into-closed"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("public hold into a closed channel's reservation: got %v want ErrUnavailable", err)
	}

	// The channel itself reports nothing claimable while shut.
	chav, err := st.Availability(ctx, org, slot, "presale")
	if err != nil {
		t.Fatal(err)
	}
	if chav.Available != 0 {
		t.Fatalf("closed channel available=%d want 0", chav.Available)
	}

	// Opening it changes the channel's answer and leaves the public's alone.
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() - interval '1 hour'", "NULL")
	chav, err = st.Availability(ctx, org, slot, "presale")
	if err != nil {
		t.Fatal(err)
	}
	if chav.Available != 6 {
		t.Fatalf("opened channel available=%d want 6", chav.Available)
	}
	av, err = st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if av.Available != 4 {
		t.Fatalf("public available=%d want 4 — opening a window must not move the public number", av.Available)
	}
}

// An existing hold finishes its lifecycle after its window closes (COS-4).
//
// Symmetric with ADR-024's rule for release_at, and the same reasoning: the
// window gates the creation of NEW consumption, not the completion of consumption
// already granted. A buyer who got a hold inside the window must be able to pay.
func TestHoldTakenInsideTheWindowSurvivesItsClose(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() - interval '1 hour'", "clock_timestamp() + interval '1 hour'")

	c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "survives-close")
	if err != nil {
		t.Fatal(err)
	}

	// The window shuts under the live hold.
	setWindow(t, ctx, db, slot, "presale", "NULL", "clock_timestamp() - interval '1 second'")

	if _, err := st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
		t.Fatalf("finalizing after the window closed: %v", err)
	}
	if _, err := st.Transition(ctx, org, c.ID, "confirmed"); err != nil {
		t.Fatalf("confirming after the window closed: %v — a window gates NEW claims, not the "+
			"completion of one already granted", err)
	}

	// But a NEW hold on that channel is refused.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "new-after-close"); !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("a new hold after the close: got %v want ErrChannelWindowClosed", err)
	}
}

// A group reservation granted inside the window still draws down after it closes
// (ADR-054, citing ADR-027).
//
// The plan draft proposed gating draw-down; plan-review rejected it. ADR-027
// already settled the analogous case for release_at, on the clause that transfers
// exactly: "the source already consumed it". A draw-down is quantity-neutral —
// it inserts a child and decrements the source in one pool-locked transaction —
// so it consumes nothing new. Gating it would strand capacity an agency was
// granted inside the window, and nobody would report that as a window bug.
func TestGroupDrawDownSurvivesAClosedWindowButPlacementDoesNot(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() - interval '1 hour'", "clock_timestamp() + interval '1 hour'")

	res, _, err := st.PlaceGroupReservation(ctx, org, slot, 4, "agency-a",
		time.Now().Add(24*time.Hour), "presale", "staff:amy", "group", "grp-win")
	if err != nil {
		t.Fatalf("placing inside the window: %v", err)
	}

	// The window shuts.
	setWindow(t, ctx, db, slot, "presale", "NULL", "clock_timestamp() - interval '1 second'")

	// Draw-down still works: the capacity is already theirs and already counted.
	if _, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, uuid.New(), slot, 2, 2500, "EUR",
		"staff:amy", "batch after close", "draw-after-close"); err != nil {
		t.Fatalf("drawing down after the window closed: %v — the source already consumed this "+
			"capacity (ADR-027); gating it would strand what the agency was granted", err)
	}

	// A NEW placement on that channel is refused, because it would consume anew.
	if _, _, err := st.PlaceGroupReservation(ctx, org, slot, 1, "agency-b",
		time.Now().Add(24*time.Hour), "presale", "staff:amy", "group", "grp-win-2"); !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("placing after the close: got %v want ErrChannelWindowClosed", err)
	}
}

// A closed window refuses as a CLOSED WINDOW even when the pool is exhausted.
//
// Found at ai-review, and it is a precedence bug rather than a missing check.
// The window was evaluated after the pool-capacity arithmetic, so the identical
// request answered `channel_window_closed` while the pool had room and the
// code-less "insufficient capacity" once it did not — a closed presale reading
// as a sellout exactly when the on-sale was busiest, and a caller told to join a
// waitlist when it should wait ninety seconds.
//
// It also defeated the load harness's fail-closed policy in the wrong direction:
// ClassifyHold409 counts a code-less 409 as an EXPECTED capacity rejection, so a
// load run against a closed channel would have been accepted as contention
// evidence rather than failing loudly.
//
// The pool is exhausted with an OPERATIONAL hold on purpose. Operational holds
// are pool-only and unchanneled (ADR-024 says so explicitly: they can exhaust the
// pool while a channel has nominal cap headroom), which is the only way to reach
// "pool full, channel cap free, window shut" — the state the original tests could
// not construct, because filling the pool through the public channel leaves the
// presale's reservation intact.
func TestClosedWindowRefusesAsAWindowEvenOnAnExhaustedPool(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})
	setWindow(t, ctx, db, slot, "presale", "clock_timestamp() + interval '1 hour'", "NULL")

	// With headroom: the window refusal, as before.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "prec-headroom"); !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("with pool headroom: got %v want ErrChannelWindowClosed", err)
	}

	// Exhaust the pool without touching the presale's allocation.
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 10, "house", "foh", "staff:amy", "ops", "prec-eat"); err != nil {
		t.Fatal(err)
	}

	// The SAME request must give the SAME answer. Capacity is a property of the
	// pool; a window is a property of the requested channel, and the second does
	// not stop being true because the first ran out.
	_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "prec-exhausted")
	if !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("with the pool exhausted: got %v want ErrChannelWindowClosed — a closed window "+
			"must not read as a sellout just because the pool filled up, and a code-less 409 is "+
			"counted by the load harness as capacity evidence", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("the refusal is also ErrUnavailable — the two must stay distinct sentinels")
	}

	// Same precedence for a group reservation.
	if _, _, err := st.PlaceGroupReservation(ctx, org, slot, 1, "agency-a",
		time.Now().Add(24*time.Hour), "presale", "staff:amy", "group", "prec-grp"); !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("group placement with the pool exhausted: got %v want ErrChannelWindowClosed", err)
	}
}

// An absent allocation stays the code-less capacity refusal — the other side of
// the precedence fix.
//
// Hoisting the window check above the capacity arithmetic must not turn "this
// channel has no allocation" into a window refusal. There is no channel there to
// be closed, and a caller that named a channel nobody configured should hear the
// same thing it always did.
func TestAChannelWithNoAllocationStillRefusesAsCapacity(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})

	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "nosuch", "no-alloc"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unconfigured channel: got %v want ErrUnavailable — no allocation means no "+
			"channel to be closed, so this is not a window refusal", err)
	}
}

// TKT-176. A channel allocation is a quantity cap, and a seated pool does not admit
// against quantity — CreateSeatHold consults capacity alone and never reads
// channel_allocations. So an allocation on a seated pool is subtracted by the public
// availability read while binding nobody who can actually claim a seat: the read and the
// claim disagree, and the allocation reserves inventory that is still freely sellable.
//
// The owner's decision (2026-08-26) is to make that state unrepresentable rather than to
// teach the seated claim path about channels. A seat-set-shaped allocation — one naming
// WHICH seats rather than how many — is a different design and a different ticket.
//
// THE INVARIANT THIS PINS, stated without naming the implementation: a refused replace
// leaves the allocation set and its revision exactly as it found them. The expected
// values below follow from that sentence, not from watching what the code produced, and
// every one of them reads the DATABASE back rather than trusting a return value.
func TestChannelAllocationsAreRefusedOnASeatedPool(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 10)

	// Seeded by direct SQL on purpose: ReplaceChannelAllocations is the very thing under
	// test and will refuse, so it cannot be used to build the precondition. This row is
	// load-bearing — delete it and the "the set is unchanged" assertion below has nothing
	// to be unchanged about, and the test would pass against an implementation that
	// refused AND wiped the table.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_allocations(pool_id, channel_code, cap)
		VALUES ($1, 'legacy', 4)`, slot); err != nil {
		t.Fatal(err)
	}

	before := currentRevision(t, ctx, st, org, slot)
	rowsBefore := allocationRows(t, ctx, st, slot)

	_, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "reseller", Cap: 5}}, &before)
	if !errors.Is(err, ErrPoolKindMismatch) {
		t.Fatalf("replace on a seated pool err = %v want ErrPoolKindMismatch", err)
	}
	// The refusal must land BEFORE the revision bump, not merely somewhere. A refused
	// call that still burned a revision would break TKT-250's stale-write protection for
	// every other operator holding a form on this slot: their view would go stale because
	// of a write that never happened.
	if after := currentRevision(t, ctx, st, org, slot); after != before {
		t.Errorf("revision after a refused replace = %d want %d (unchanged): the refusal "+
			"landed after the allocation_revision bump", after, before)
	}
	if got := allocationRows(t, ctx, st, slot); !allocationSetsEqual(got, rowsBefore) {
		t.Errorf("allocation set after a refused replace = %v want %v (unchanged)", got, rowsBefore)
	}
}

// The other half of the predicate, and it is not optional: a test that only proves a
// seated pool is refused cannot tell "refuses seated pools" from "refuses everything".
// This leg fails against an always-refuse implementation, which is the mutation the
// seated leg above is blind to.
//
// It lives HERE, beside the refusal, rather than being delegated to the pre-existing GA
// allocation tests. Those run in the same package and would go red first on an
// always-refuse mutation, so they would kill the mutant before this test ever spoke —
// and a mutation killed by a suite that runs first is evidence the mechanism is live,
// not evidence that this test caught it.
func TestChannelAllocationsStillSucceedOnAGaPool(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	before := currentRevision(t, ctx, st, org, slot)
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "reseller", Cap: 5}}, &before); err != nil {
		t.Fatalf("replace on a GA pool: %v", err)
	}
	// Derived from the rule "a successful replace advances the revision exactly once",
	// not read back from the implementation.
	if after := currentRevision(t, ctx, st, org, slot); after != before+1 {
		t.Errorf("revision after a successful GA replace = %d want %d", after, before+1)
	}
	if got := allocationRows(t, ctx, st, slot); !allocationSetsEqual(got, map[string]int32{"reseller": 5}) {
		t.Errorf("allocation set after a successful GA replace = %v want {reseller:5}", got)
	}
}

// A STALE seated set is refused for being STALE, not for being seated.
//
// This pins guard ORDERING rather than pool kind, and it is the only assertion that
// catches the kind check being hoisted above the staleness check — a tempting refactor,
// since the kind is the cheaper test. The reason the order matters is already written
// into ReplaceChannelAllocations: a stale set must be refused for staleness so the
// operator's remedy is "reload", and answering "this pool is seated" would send an
// operator whose view is out of date to reason about something that is not what went
// wrong. Separate from the kind case above so that an earlier refusal short-circuiting a
// later one stays visible instead of hiding inside one test that passes for two reasons.
func TestAStaleSeatedReplaceIsRefusedForStalenessNotForKind(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 10)

	stale := currentRevision(t, ctx, st, org, slot) - 1
	_, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "reseller", Cap: 5}}, &stale)
	if !errors.Is(err, ErrAllocationRevisionMismatch) {
		t.Fatalf("stale replace on a seated pool err = %v want ErrAllocationRevisionMismatch: "+
			"the kind check must not be hoisted above the staleness check", err)
	}
}

// allocationSetsEqual compares two allocation sets read from the table.
func allocationSetsEqual(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// The refusal must not strand what it refuses (ai-review, [high]).
//
// This endpoint ADMITTED seated allocations before TKT-176, and this store is the only
// writer of channel_allocations. So a pool that already holds legacy rows must still have
// a way out, or the guard converts a silent divergence into a permanent one: the public
// availability read keeps subtracting an allocation that the seated claim path ignores —
// the exact disagreement the refusal exists to end — and nothing short of hand-editing
// the database can clear it.
//
// An empty replace is that way out, and it does not weaken the invariant. The rule is
// that a seated pool must not CARRY allocations; an empty set satisfies it. Refusing to
// ADD while allowing to CLEAR moves towards the invariant from either starting state.
//
// The empty replace is an ordinary success, so it bumps the revision like any other:
// a reader holding the old value has read a set that a writer has since replaced.
func TestAnEmptyReplaceClearsLegacyAllocationsFromASeatedPool(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, _ := provisionedSeated(t, ctx, st, 10)

	// The legacy state, seeded the only way it can now arise: directly, or by a write
	// that predates the guard.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_allocations(pool_id, channel_code, cap)
		VALUES ($1, 'legacy', 4)`, slot); err != nil {
		t.Fatal(err)
	}
	before := currentRevision(t, ctx, st, org, slot)

	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{}, &before); err != nil {
		t.Fatalf("empty replace on a seated pool: %v; a seated pool holding legacy rows "+
			"must be repairable through the only writer of the table", err)
	}
	if got := allocationRows(t, ctx, st, slot); len(got) != 0 {
		t.Errorf("allocation set after an empty replace = %v want empty", got)
	}
	if after := currentRevision(t, ctx, st, org, slot); after != before+1 {
		t.Errorf("revision after a successful empty replace = %d want %d", after, before+1)
	}
}
