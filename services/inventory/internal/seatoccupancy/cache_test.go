package seatoccupancy

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/cachetier"
)

type fakeSource struct {
	mu      sync.Mutex
	calls   int
	inside  int
	peak    int
	perKey  map[string]int
	occ     store.SeatOccupancy
	err     error
	release chan struct{}
	entered chan struct{}
	inval   func(uuid.UUID)
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		perKey: map[string]int{},
		occ: store.SeatOccupancy{
			OfferingStatus:    "open",
			RemainingCapacity: 10,
			Unavailable:       []string{},
		},
	}
}

func (f *fakeSource) SeatOccupancy(ctx context.Context, org, slot uuid.UUID) (store.SeatOccupancy, error) {
	f.mu.Lock()
	f.calls++
	f.inside++
	f.peak = max(f.peak, f.inside)
	f.perKey[org.String()+"|"+slot.String()]++
	release, entered, err, occ := f.release, f.entered, f.err, f.occ
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inside--
		f.mu.Unlock()
	}()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return store.SeatOccupancy{}, ctx.Err()
		}
	}
	if err != nil {
		return store.SeatOccupancy{}, err
	}
	occ.SlotID = slot
	return occ, nil
}

func (f *fakeSource) RegisterAvailabilityInvalidator(fn func(uuid.UUID)) {
	f.inval = fn
}

func (f *fakeSource) concurrentPeak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSource) setUnavailable(unavail ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.occ.Unavailable = slices.Clone(unavail)
	f.occ.RemainingCapacity = int32(10 - len(unavail))
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestService(t *testing.T, src Source) (*Service, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	return New(src, WithClock(clk.now)), clk
}

func mustRead(t *testing.T, s *Service, org, slot uuid.UUID) Read {
	t.Helper()
	r, err := s.Read(context.Background(), org, slot)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return r
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached within 2s")
}

// 1. Repeated reads load once.
func TestRepeatedReadsLoadOnce(t *testing.T) {
	src := newFakeSource()
	src.setUnavailable("A1")
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	for i := range 5 {
		got := mustRead(t, svc, org, slot).Value
		if !slices.Equal(got.Unavailable, []string{"A1"}) {
			t.Fatalf("read %d: unavailable = %v, want [A1]", i, got.Unavailable)
		}
	}
	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times, want exactly 1 — repeated reads must be served from memory", got)
	}
}

// 2. Concurrent misses load once (barriers, not sleeps).
func TestConcurrentMissesLoadOnce(t *testing.T) {
	src := newFakeSource()
	src.release = make(chan struct{})
	src.entered = make(chan struct{}, 1)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	const readers = 20
	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			_, _ = svc.Read(context.Background(), org, slot)
		}()
	}
	<-src.entered
	waitFor(t, func() bool { return svc.Status().InFlight == 1 })
	close(src.release)
	wg.Wait()

	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times for %d concurrent misses, want 1", got, readers)
	}
}

// 3. Entry expires at the cachetier.Seconds tier (fake clock, not sleeping).
func TestEntryExpiresAtTheSecondsTier(t *testing.T) {
	src := newFakeSource()
	svc, clk := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	mustRead(t, svc, org, slot)
	clk.advance(cachetier.Seconds.Duration() - time.Millisecond)
	mustRead(t, svc, org, slot)
	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times just inside the tier, want 1", got)
	}
	clk.advance(2 * time.Millisecond)
	mustRead(t, svc, org, slot)
	if got := src.count(); got != 2 {
		t.Fatalf("source called %d times just past the tier, want 2 — a stale entry must decay", got)
	}
}

// 4. Read reports age.
func TestReadReportsAge(t *testing.T) {
	src := newFakeSource()
	svc, clk := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	if age := mustRead(t, svc, org, slot).Age; age != 0 {
		t.Fatalf("a freshly loaded entry has age %v, want 0", age)
	}
	clk.advance(3 * time.Second)
	if age := mustRead(t, svc, org, slot).Age; age != 3*time.Second {
		t.Fatalf("age = %v, want 3s", age)
	}
}

// 5. Invalidation is immediate and per-slot.
func TestInvalidationIsImmediateAndPerSlot(t *testing.T) {
	src := newFakeSource()
	src.setUnavailable("A1")
	svc, _ := newTestService(t, src)
	org, slot, other := uuid.New(), uuid.New(), uuid.New()

	mustRead(t, svc, org, slot)
	mustRead(t, svc, org, other)
	src.setUnavailable("A1", "A2")

	svc.Invalidate(slot)

	if got := mustRead(t, svc, org, slot).Value.Unavailable; !slices.Equal(got, []string{"A1", "A2"}) {
		t.Fatalf("invalidated slot = %v, want [A1 A2]", got)
	}
	if got := mustRead(t, svc, org, other).Value.Unavailable; !slices.Equal(got, []string{"A1"}) {
		t.Fatalf("untouched slot = %v, want [A1]", got)
	}
}

// 6. COS-2a — a post-commit reader does not JOIN a pre-commit flight.
// Fixture: start read 1, wait on the source's entered barrier, change the fake's value,
// call the registered invalidator, start read 2, require a SECOND source-entered notification
// before releasing either load, then release both and assert read 2 got the new value.
func TestPostInvalidationReadDoesNotJoinAStaleFlight(t *testing.T) {
	src := newFakeSource()
	src.setUnavailable("A1")
	src.release = make(chan struct{})
	src.entered = make(chan struct{}, 4)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	stale := make(chan struct{})
	go func() {
		defer close(stale)
		_, _ = svc.Read(context.Background(), org, slot)
	}()
	<-src.entered // read 1 is inside the source

	// Value changes and commit invalidates while load 1 is running
	src.setUnavailable("A1", "A2")
	svc.Invalidate(slot)

	fresh := make(chan Read, 1)
	go func() {
		r, err := svc.Read(context.Background(), org, slot)
		if err != nil {
			t.Errorf("post-invalidation read: %v", err)
		}
		fresh <- r
	}()
	// Require a SECOND source-entered notification before releasing either load
	<-src.entered

	close(src.release)
	<-stale
	got := <-fresh
	if want := []string{"A1", "A2"}; !slices.Equal(got.Value.Unavailable, want) {
		t.Fatalf("post-invalidation read = %v, want %v — it joined the pre-commit load", got.Value.Unavailable, want)
	}
}

// 7. COS-2b — a pre-commit load does not POPULATE the cache.
// Start read 1, wait for entered, invalidate, change fake value, release read 1.
// Read again; assert new value, proving the stale load did not insert.
func TestInvalidationSupersedesAnOlderInFlightLoad(t *testing.T) {
	src := newFakeSource()
	src.setUnavailable("A1")
	src.release = make(chan struct{})
	src.entered = make(chan struct{}, 2)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Read(context.Background(), org, slot)
	}()
	<-src.entered // the stale load is now inside the source

	// The write commits and invalidates while that load is still in flight.
	src.setUnavailable("A1", "A2")
	svc.Invalidate(slot)

	close(src.release)
	<-done

	// The stale load has finished. Its result must not be in the cache.
	if got := mustRead(t, svc, org, slot).Value.Unavailable; !slices.Equal(got, []string{"A1", "A2"}) {
		t.Fatalf("read after invalidation = %v, want [A1 A2] — a pre-commit load must not repopulate", got)
	}
}

// 8. Concurrent source loads are bounded (semaphore).
func TestConcurrentSourceLoadsAreBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ceiling, readers = 3, 25
		src := newFakeSource()
		src.release = make(chan struct{})
		svc := New(src, WithMaxEntries(16), WithMaxInFlight(ceiling))

		org := uuid.New()
		var wg sync.WaitGroup
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = svc.Read(context.Background(), org, uuid.New())
			}()
		}

		synctest.Wait()

		if peak := src.concurrentPeak(); peak > ceiling {
			t.Fatalf("peak concurrent source calls = %d, want at most %d", peak, ceiling)
		}
		close(src.release)
		wg.Wait()

		if got := src.count(); got != readers {
			t.Fatalf("source called %d times, want all %d reads served", got, readers)
		}
	})
}

// 9. A slow load fails rather than wedging (load timeout).
func TestASlowLoadFailsRatherThanWedging(t *testing.T) {
	src := newFakeSource()
	src.release = make(chan struct{}) // never closed
	svc := New(src, WithLoadTimeout(30*time.Millisecond))

	_, err := svc.Read(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a load past its budget returned %v, want context.DeadlineExceeded", err)
	}
	waitFor(t, func() bool { return svc.Status().InFlight == 0 })
}

// 10. Bounded globally.
func TestBoundedGlobally(t *testing.T) {
	src := newFakeSource()
	svc := New(src, WithMaxEntries(16))
	org := uuid.New()

	for range 100 {
		mustRead(t, svc, org, uuid.New())
	}
	if got := svc.Status().Entries; got > 16 {
		t.Fatalf("cache holds %d entries, want at most the global bound of 16", got)
	}
}

// 11. Load errors are not cached.
func TestLoadErrorsAreNotCached(t *testing.T) {
	src := newFakeSource()
	src.err = errors.New("connection reset")
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	for range 3 {
		if _, err := svc.Read(context.Background(), org, slot); err == nil {
			t.Fatal("Read must surface the source error")
		}
	}
	if got := src.count(); got != 3 {
		t.Fatalf("source called %d times, want 3 — errors must not be cached", got)
	}

	src.mu.Lock()
	src.err = store.ErrNotFound
	src.mu.Unlock()
	for range 3 {
		if _, err := svc.Read(context.Background(), org, slot); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Read must surface ErrNotFound, got %v", err)
		}
	}
	if got := src.count(); got != 6 {
		t.Fatalf("source called %d times, want 6 — ErrNotFound is not cached either", got)
	}
}

// 12. New registers its invalidator.
func TestNewRegistersItsInvalidator(t *testing.T) {
	src := newFakeSource()
	src.setUnavailable("A1")
	svc, _ := newTestService(t, src)
	if src.inval == nil {
		t.Fatal("New must register its invalidator with the source")
	}
	org, slot := uuid.New(), uuid.New()
	mustRead(t, svc, org, slot)
	src.setUnavailable("A1", "A2")
	src.inval(slot)
	if got := mustRead(t, svc, org, slot).Value.Unavailable; !slices.Equal(got, []string{"A1", "A2"}) {
		t.Fatalf("the registered invalidator did not reach the cache: %v", got)
	}
}

// 13. Disable purges and bypasses; a bypassed read reports Age 0.
func TestDisablePurgesAndBypassesTheCache(t *testing.T) {
	src := newFakeSource()
	src.setUnavailable("A1")
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	mustRead(t, svc, org, slot)
	mustRead(t, svc, org, slot)
	if got := src.count(); got != 1 {
		t.Fatalf("warm-up: source called %d times, want 1", got)
	}

	svc.SetEnabled(false)
	if st := svc.Status(); st.Enabled || st.Entries != 0 {
		t.Fatalf("after disable: %+v, want enabled=false entries=0", st)
	}
	for range 3 {
		r := mustRead(t, svc, org, slot)
		if r.Age != 0 {
			t.Fatalf("bypassed read reports age %v, want 0", r.Age)
		}
	}
	if got := src.count(); got != 4 {
		t.Fatalf("source called %d times across 3 disabled reads, want 4", got)
	}

	svc.SetEnabled(true)
	if st := svc.Status(); !st.Enabled || st.Entries != 0 {
		t.Fatalf("after re-enable: %+v, want enabled=true entries=0", st)
	}
	mustRead(t, svc, org, slot)
	mustRead(t, svc, org, slot)
	if got := src.count(); got != 5 {
		t.Fatalf("source called %d times after re-enable, want 5 — one cold load then a hit", got)
	}
}

// TestBypassDoesNotConsultEntriesMap pins that bypass never consults the entries map.
func TestBypassDoesNotConsultEntriesMap(t *testing.T) {
	src := newFakeSource()
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	svc.mu.Lock()
	svc.enabled = false
	svc.entries[key{org: org, slot: slot}] = &entry{
		key:      key{org: org, slot: slot},
		value:    store.SeatOccupancy{Unavailable: []string{"STALE"}},
		loadedAt: time.Now(),
	}
	svc.mu.Unlock()

	src.setUnavailable("FRESH")
	r := mustRead(t, svc, org, slot)
	if !slices.Equal(r.Value.Unavailable, []string{"FRESH"}) {
		t.Fatalf("bypassed read returned stale entry %v, want [FRESH]", r.Value.Unavailable)
	}
	if r.Age != 0 {
		t.Fatalf("bypassed read returned age %v, want 0", r.Age)
	}
}

// 14. A toggle cycle rejects a load that started before it.
func TestAToggleCycleRejectsALoadThatStartedBeforeIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource()
		src.setUnavailable("A1")
		src.release = make(chan struct{})
		svc, _ := newTestService(t, src)
		org, slot := uuid.New(), uuid.New()

		done := make(chan struct{})
		go func() { defer close(done); _, _ = svc.Read(context.Background(), org, slot) }()
		synctest.Wait()

		svc.SetEnabled(false)
		svc.SetEnabled(true)

		src.setUnavailable("A1", "A2")
		close(src.release)
		<-done

		if st := svc.Status(); st.Entries != 0 {
			t.Fatalf("a load that started before a toggle cycle was cached: %+v", st)
		}
		if got := mustRead(t, svc, org, slot).Value.Unavailable; !slices.Equal(got, []string{"A1", "A2"}) {
			t.Fatalf("read after toggle cycle = %v, want [A1 A2]", got)
		}
	})
}

// 15. SetEnabled is idempotent.
func TestSetEnabledIsIdempotent(t *testing.T) {
	src := newFakeSource()
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	mustRead(t, svc, org, slot)
	svc.SetEnabled(true)
	mustRead(t, svc, org, slot)
	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times, want 1 — enabling an enabled cache must not purge it", got)
	}
}

// 16. A disabled read does not join an in-flight load.
func TestADisabledReadDoesNotJoinAnInFlightLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource()
		src.setUnavailable("A1")
		src.release = make(chan struct{})
		svc, _ := newTestService(t, src)
		org, slot := uuid.New(), uuid.New()

		done := make(chan struct{})
		go func() { defer close(done); _, _ = svc.Read(context.Background(), org, slot) }()
		synctest.Wait()

		svc.SetEnabled(false)
		src.setUnavailable("A1", "A2")

		fresh := make(chan store.SeatOccupancy, 1)
		go func() {
			r, _ := svc.Read(context.Background(), org, slot)
			fresh <- r.Value
		}()
		synctest.Wait()

		close(src.release)
		<-done
		if got := <-fresh; !slices.Equal(got.Unavailable, []string{"A1", "A2"}) {
			t.Fatalf("a disabled read returned %v — it joined a load started before disable", got.Unavailable)
		}
		if got := src.count(); got != 2 {
			t.Fatalf("source called %d times, want 2", got)
		}
	})
}

// 17. A post-re-enable reader does not join a pre-disable flight.
func TestAPostReEnableReaderDoesNotJoinAPreDisableFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource()
		src.setUnavailable("A1")
		src.release = make(chan struct{})
		svc, _ := newTestService(t, src)
		org, slot := uuid.New(), uuid.New()

		stale := make(chan struct{})
		go func() { defer close(stale); _, _ = svc.Read(context.Background(), org, slot) }()
		synctest.Wait()

		svc.SetEnabled(false)
		svc.SetEnabled(true)
		src.setUnavailable("A1", "A2")

		fresh := make(chan store.SeatOccupancy, 1)
		go func() {
			r, _ := svc.Read(context.Background(), org, slot)
			fresh <- r.Value
		}()
		synctest.Wait()

		close(src.release)
		<-stale
		if got := <-fresh; !slices.Equal(got.Unavailable, []string{"A1", "A2"}) {
			t.Fatalf("post-re-enable read = %v, want [A1 A2]", got.Unavailable)
		}
		if got := src.count(); got != 2 {
			t.Fatalf("source called %d times, want 2", got)
		}
	})
}

// 18. COS-6 — the reduction test, with BOTH arms measured.
// Several generations; each generation runs N concurrent readers for one key behind a barrier,
// then invalidates and changes the value. Run the wave twice against the same fake:
// once with the cache enabled, asserting exactly generations source calls;
// once with SetEnabled(false), asserting exactly readers × generations source calls.
// Both numbers must be observed from a real run — do not assert the disabled baseline arithmetically.
// Also assert every reader got its generation's value.
func TestReductionBothArmsMeasured(t *testing.T) {
	const generations = 4
	const readers = 10
	genValues := [][]string{
		{"A1"},
		{"A1", "A2"},
		{"A1", "A2", "A3"},
		{"A1", "A2", "A3", "A4"},
	}

	// Arm 1: Cache ENABLED
	src := newFakeSource()
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	for g := range generations {
		src.setUnavailable(genValues[g]...)
		if g > 0 {
			svc.Invalidate(slot)
		}

		barrier := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]store.SeatOccupancy, readers)
		for i := range readers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-barrier
				r := mustRead(t, svc, org, slot)
				results[idx] = r.Value
			}(i)
		}
		close(barrier)
		wg.Wait()

		for i, res := range results {
			if !slices.Equal(res.Unavailable, genValues[g]) {
				t.Fatalf("gen %d reader %d: got %v, want %v", g, i, res.Unavailable, genValues[g])
			}
		}
	}

	enabledCalls := src.count()
	if enabledCalls != generations {
		t.Fatalf("enabled run: source called %d times, want exactly %d (1 per generation)", enabledCalls, generations)
	}

	// Arm 2: Cache DISABLED
	svc.SetEnabled(false)
	baselineCalls := src.count()

	for g := range generations {
		src.setUnavailable(genValues[g]...)

		barrier := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]store.SeatOccupancy, readers)
		for i := range readers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-barrier
				r := mustRead(t, svc, org, slot)
				results[idx] = r.Value
			}(i)
		}
		close(barrier)
		wg.Wait()

		for i, res := range results {
			if !slices.Equal(res.Unavailable, genValues[g]) {
				t.Fatalf("disabled gen %d reader %d: got %v, want %v", g, i, res.Unavailable, genValues[g])
			}
		}
	}

	disabledCalls := src.count() - baselineCalls
	if disabledCalls != readers*generations {
		t.Fatalf("disabled run: source called %d times, want exactly %d (%d readers × %d generations)",
			disabledCalls, readers*generations, readers, generations)
	}
}

// TestASupersededLoadDoesNotEvictItsReplacement
func TestASupersededLoadDoesNotEvictItsReplacement(t *testing.T) {
	src := newFakeSource()
	first := make(chan struct{})
	second := make(chan struct{})
	src.release = first
	src.entered = make(chan struct{}, 4)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	stale := make(chan struct{})
	go func() { defer close(stale); _, _ = svc.Read(context.Background(), org, slot) }()
	<-src.entered

	svc.Invalidate(slot)

	src.mu.Lock()
	src.release = second
	src.mu.Unlock()
	replacement := make(chan struct{})
	go func() { defer close(replacement); _, _ = svc.Read(context.Background(), org, slot) }()
	<-src.entered

	close(first)
	<-stale
	waitFor(t, func() bool { return src.count() == 2 })

	if got := svc.Status().InFlight; got != 1 {
		t.Fatalf("in-flight = %d after a superseded load finished, want 1", got)
	}
	close(second)
	<-replacement
}
