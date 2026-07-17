//go:build smoke

package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Pool offering state (TKT-75 / US-012): archived/closed pools reject NEW holds only;
// live and confirmed claims keep their lifecycle. Event application is deduped and
// closure is ordered by the catalog's monotonic version.

func TestArchivedPoolRejectsNewHolds(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	if err := st.ApplyArchive(ctx, uuid.New(), slot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "k-buyer"); !errors.Is(err, ErrSlotArchived) {
		t.Fatalf("CreateHold err = %v, want ErrSlotArchived", err)
	}
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 1, "house", "foh", "staff:amy", "r", "k-op"); !errors.Is(err, ErrSlotArchived) {
		t.Fatalf("PlaceOperationalHold err = %v, want ErrSlotArchived", err)
	}
	// Archival is terminal: a later reopen event must not restore claimability.
	if err := st.ApplyClosure(ctx, uuid.New(), slot, slot, false, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "k-buyer-2"); !errors.Is(err, ErrSlotArchived) {
		t.Fatalf("CreateHold after reopen err = %v, want ErrSlotArchived", err)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.OfferingStatus != "archived" || a.Available != 0 {
		t.Fatalf("availability = %+v, want archived with zero available", a)
	}
}

func TestClosedPoolRejectsNewHoldsAndReopenRestoresThem(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	if err := st.ApplyClosure(ctx, uuid.New(), slot, slot, true, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "k1"); !errors.Is(err, ErrSlotClosed) {
		t.Fatalf("CreateHold err = %v, want ErrSlotClosed", err)
	}
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 1, "house", "foh", "staff:amy", "r", "k-op"); !errors.Is(err, ErrSlotClosed) {
		t.Fatalf("PlaceOperationalHold err = %v, want ErrSlotClosed", err)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.OfferingStatus != "closed" || a.Available != 0 || a.Capacity != 10 {
		t.Fatalf("closed availability = %+v, want closed/zero with factual capacity", a)
	}
	if err := st.ApplyClosure(ctx, uuid.New(), slot, slot, false, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "k2"); err != nil {
		t.Fatalf("CreateHold after reopen: %v", err)
	}
	if a, err = st.Availability(ctx, org, slot, ""); err != nil || a.OfferingStatus != "open" {
		t.Fatalf("reopened availability = %+v err=%v, want open", a, err)
	}
}

func TestOfferingEventDedupeAndClosureOrdering(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	closeEvent := uuid.New()
	if err := st.ApplyClosure(ctx, closeEvent, slot, slot, true, 1); err != nil {
		t.Fatal(err)
	}
	// Replay of the same event id: consumed once, applied once.
	if err := st.ApplyClosure(ctx, closeEvent, slot, slot, true, 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, closeEvent).Scan(&n); err != nil || n != 1 {
		t.Fatalf("consumed_events rows = %d err=%v, want exactly 1", n, err)
	}
	if err := st.ApplyClosure(ctx, uuid.New(), slot, slot, false, 2); err != nil {
		t.Fatal(err)
	}
	// A delayed closed(v1) after reopened(v2) is stale: consumed, but must not re-close.
	if err := st.ApplyClosure(ctx, uuid.New(), slot, slot, true, 1); err != nil {
		t.Fatal(err)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.OfferingStatus != "open" {
		t.Fatalf("offering status = %q — a stale closed(v1) re-closed a reopened(v2) pool", a.OfferingStatus)
	}
}

func TestMissingPoolOfferingEventIsNotConsumed(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	ghost, archiveEvent, closeEvent := uuid.New(), uuid.New(), uuid.New()

	if err := st.ApplyArchive(ctx, archiveEvent, ghost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplyArchive err = %v, want ErrNotFound", err)
	}
	if err := st.ApplyClosure(ctx, closeEvent, ghost, ghost, true, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplyClosure err = %v, want ErrNotFound", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM consumed_events WHERE event_id IN ($1,$2)`, archiveEvent, closeEvent).Scan(&n); err != nil || n != 0 {
		t.Fatalf("consumed_events rows = %d err=%v — a missing-pool event must stay unconsumed for redelivery", n, err)
	}
}

func TestLiveClaimsSurviveClosureAndArchive(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	held, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "", "k-live")
	if err != nil {
		t.Fatal(err)
	}
	confirmed, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 3, 0, "", "", "k-confirm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, confirmed.ID, "finalizing"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Transition(ctx, org, confirmed.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}

	if err = st.ApplyClosure(ctx, uuid.New(), slot, slot, true, 1); err != nil {
		t.Fatal(err)
	}
	// The live hold keeps its TTL and may still finalize and confirm mid-closure —
	// the buyer already in checkout is commerce's concern, not an inventory revocation.
	c, err := st.Transition(ctx, org, held.ID, "finalizing")
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpiresAt != nil && held.ExpiresAt != nil && !c.ExpiresAt.Equal(*held.ExpiresAt) {
		t.Fatalf("closure rewrote the hold TTL: %v -> %v", held.ExpiresAt, c.ExpiresAt)
	}
	if _, err = st.Transition(ctx, org, held.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}

	if err = st.ApplyArchive(ctx, uuid.New(), slot); err != nil {
		t.Fatal(err)
	}
	a, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if a.Confirmed != 5 {
		t.Fatalf("confirmed = %d after closure+archive, want 5 untouched", a.Confirmed)
	}
	if a.OfferingStatus != "archived" || a.Available != 0 || a.PublicAvailable != 0 {
		t.Fatalf("staff availability = %+v, want archived with zero claimable", a)
	}
}

func TestIdempotencyReplaySurvivesOfferingStop(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)

	before, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "", "k-replay")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApplyClosure(ctx, uuid.New(), slot, slot, true, 1); err != nil {
		t.Fatal(err)
	}
	// Same key, same request: the replay returns the original hold, not ErrSlotClosed.
	replayed, replay, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "", "k-replay")
	if err != nil || !replay || replayed.ID != before.ID {
		t.Fatalf("replay = (%v, %v, %v), want the original hold back", replayed.ID, replay, err)
	}
	// Same key, different request: still the idempotency error, not the offering error.
	if _, _, err = st.CreateHold(ctx, org, slot, uuid.Nil, 5, 0, "", "", "k-replay"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("key reuse err = %v, want ErrIdempotency", err)
	}
}

// Closure and hold creation serialize on the pool row lock: no hold that starts
// after a closure commits may succeed, whatever the interleaving.
func TestClosureSerializesAgainstConcurrentHolds(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 1000)

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, 40)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", uuid.NewString())
			results[i] = err
		}(i)
	}
	wg.Add(1)
	var closeErr error
	go func() {
		defer wg.Done()
		<-start
		closeErr = st.ApplyClosure(ctx, uuid.New(), slot, slot, true, 1)
	}()
	close(start)
	wg.Wait()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	for i, err := range results {
		if err != nil && !errors.Is(err, ErrSlotClosed) {
			t.Fatalf("hold %d: %v — only ErrSlotClosed is an acceptable failure", i, err)
		}
	}
	// After the dust settles the pool is closed: the next hold must fail.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", "k-after"); !errors.Is(err, ErrSlotClosed) {
		t.Fatalf("post-closure hold err = %v, want ErrSlotClosed", err)
	}
}

// The ai-review finding 1 regression: catalog closure versions are monotonic PER
// PERFORMANCE, and grouped festival days share one pool. Ordering must therefore be
// per (pool, performance); a single pool-level counter discards day B's first closure
// as "stale" after day A reached a higher version, leaving a closed day on sale.
func TestGroupedPoolOrdersClosuresPerSlot(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, pool := provisioned(t, ctx, st, 100) // the shared festival pool
	dayA, dayB := uuid.New(), uuid.New()

	// Day A: closed(v1) then reopened(v2) — the pool is open, A's counter is at 2.
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayA, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayA, false, 2); err != nil {
		t.Fatal(err)
	}
	// Day B's FIRST closure arrives at v1. Under a pool-level counter this is "stale".
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayB, true, 1); err != nil {
		t.Fatal(err)
	}
	a, err := st.Availability(ctx, org, pool, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.OfferingStatus != "closed" {
		t.Fatalf("offering status = %q — day B's closure was discarded against day A's version counter", a.OfferingStatus)
	}
	// Any closed member keeps the pool closed: reopening A changes nothing while B is closed.
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayA, true, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayA, false, 4); err != nil {
		t.Fatal(err)
	}
	if a, err = st.Availability(ctx, org, pool, ""); err != nil || a.OfferingStatus != "closed" {
		t.Fatalf("offering status = %q err=%v, want closed while day B stays closed", a.OfferingStatus, err)
	}
	// Reopening B — the last closed member — reopens the pool.
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayB, false, 2); err != nil {
		t.Fatal(err)
	}
	if a, err = st.Availability(ctx, org, pool, ""); err != nil || a.OfferingStatus != "open" {
		t.Fatalf("offering status = %q err=%v, want open once every member reopened", a.OfferingStatus, err)
	}
	// And a stale closed(v1) for B — already at v2 — must not re-close the pool.
	if err := st.ApplyClosure(ctx, uuid.New(), pool, dayB, true, 1); err != nil {
		t.Fatal(err)
	}
	if a, err = st.Availability(ctx, org, pool, ""); err != nil || a.OfferingStatus != "open" {
		t.Fatalf("offering status = %q err=%v — a stale per-slot closed re-closed the pool", a.OfferingStatus, err)
	}
}
