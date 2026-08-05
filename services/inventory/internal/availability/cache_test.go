package availability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/cachetier"
)

// fakeSource counts raw loads. Counting is the assertion for every "served from
// memory" claim in this package: a timing assertion would be flaky under CI load
// and would not say where the answer came from.
type fakeSource struct {
	mu      sync.Mutex
	calls   int
	inside  int // source calls executing right now
	peak    int // the most that were ever executing at once
	perKey  map[string]int
	avail   int32
	err     error
	release chan struct{} // when non-nil, a load blocks until it is closed/fed
	entered chan struct{} // signalled once per load, after the call is counted
	inval   func(uuid.UUID)
}

func newFakeSource() *fakeSource { return &fakeSource{perKey: map[string]int{}} }

func (f *fakeSource) Availability(ctx context.Context, org, slot uuid.UUID, channel string) (store.Availability, error) {
	f.mu.Lock()
	f.calls++
	f.inside++
	f.peak = max(f.peak, f.inside)
	f.perKey[org.String()+"|"+slot.String()+"|"+channel]++
	release, entered, err, avail := f.release, f.entered, f.err, f.avail
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inside--; f.mu.Unlock() }()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return store.Availability{}, ctx.Err()
		}
	}
	if err != nil {
		return store.Availability{}, err
	}
	return store.Availability{SlotID: slot, Available: avail, OfferingStatus: "open"}, nil
}

func (f *fakeSource) RegisterAvailabilityInvalidator(fn func(uuid.UUID)) { f.inval = fn }

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

func (f *fakeSource) setAvailable(n int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.avail = n
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

func mustRead(t *testing.T, s *Service, org, slot uuid.UUID, channel string) Read {
	t.Helper()
	r, err := s.Read(context.Background(), org, slot, channel)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return r
}

// TestRepeatedReadsLoadOnce is COS 1.
func TestRepeatedReadsLoadOnce(t *testing.T) {
	src := newFakeSource()
	src.setAvailable(7)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	for i := range 5 {
		if got := mustRead(t, svc, org, slot, "").Value.Available; got != 7 {
			t.Fatalf("read %d: available = %d, want 7", i, got)
		}
	}
	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times, want exactly 1 — repeated reads must be served from memory", got)
	}
}

// TestKeyIncludesOrganizerAndChannel: ADR-024 makes channel a different question
// with different accounting queries, and the organizer is what stops one
// organizer's answer satisfying another's request. Both are caller-supplied.
func TestKeyIncludesOrganizerAndChannel(t *testing.T) {
	src := newFakeSource()
	svc, _ := newTestService(t, src)
	orgA, orgB, slot := uuid.New(), uuid.New(), uuid.New()

	mustRead(t, svc, orgA, slot, "")
	mustRead(t, svc, orgA, slot, "presale")
	mustRead(t, svc, orgB, slot, "")
	// Each of the three is a distinct question, so each must have loaded once.
	if got := src.count(); got != 3 {
		t.Fatalf("source called %d times, want 3 distinct loads", got)
	}
	// And each must now be cached independently.
	mustRead(t, svc, orgA, slot, "")
	mustRead(t, svc, orgA, slot, "presale")
	mustRead(t, svc, orgB, slot, "")
	if got := src.count(); got != 3 {
		t.Fatalf("source called %d times after re-reads, want still 3", got)
	}
}

// TestConcurrentMissesLoadOnce is COS 4: an expiring hot slot must not stampede
// the database.
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
			_, _ = svc.Read(context.Background(), org, slot, "")
		}()
	}
	// Wait until the single leader is inside the source, then let every follower
	// pile up behind it before releasing.
	<-src.entered
	waitFor(t, func() bool { return svc.Status().InFlight == 1 })
	close(src.release)
	wg.Wait()

	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times for %d concurrent misses, want 1", got, readers)
	}
}

// TestEntryExpiresAtTheSecondsTier is COS 3 — and the TTL must come from the
// registry, not a literal, which is the whole reason TKT-204 shipped first.
func TestEntryExpiresAtTheSecondsTier(t *testing.T) {
	src := newFakeSource()
	svc, clk := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	mustRead(t, svc, org, slot, "")
	clk.advance(cachetier.Seconds.Duration() - time.Millisecond)
	mustRead(t, svc, org, slot, "")
	if got := src.count(); got != 1 {
		t.Fatalf("source called %d times just inside the tier, want 1", got)
	}
	clk.advance(2 * time.Millisecond)
	mustRead(t, svc, org, slot, "")
	if got := src.count(); got != 2 {
		t.Fatalf("source called %d times just past the tier, want 2 — a stale entry must decay", got)
	}
}

// TestReadReportsAge underpins the Age header. Without it a response already
// nearly a full tier stale inside the service would hand a conformant client
// another full tier of freshness, doubling the staleness the epic's COS bounds.
func TestReadReportsAge(t *testing.T) {
	src := newFakeSource()
	svc, clk := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	if age := mustRead(t, svc, org, slot, "").Age; age != 0 {
		t.Fatalf("a freshly loaded entry has age %v, want 0", age)
	}
	clk.advance(3 * time.Second)
	if age := mustRead(t, svc, org, slot, "").Age; age != 3*time.Second {
		t.Fatalf("age = %v, want 3s", age)
	}
}

// TestInvalidationIsImmediateAndPerSlot is COS 2. It also pins that invalidation
// clears EVERY organizer/channel variant of the slot: the write paths that call
// it (ApplyArchive, ApplyClosure) know the pool, not always its organizer.
func TestInvalidationIsImmediateAndPerSlot(t *testing.T) {
	src := newFakeSource()
	src.setAvailable(10)
	svc, _ := newTestService(t, src)
	org, slot, other := uuid.New(), uuid.New(), uuid.New()

	mustRead(t, svc, org, slot, "")
	mustRead(t, svc, org, slot, "presale")
	mustRead(t, svc, org, other, "")
	src.setAvailable(4)

	svc.Invalidate(slot)

	if got := mustRead(t, svc, org, slot, "").Value.Available; got != 4 {
		t.Fatalf("default variant = %d, want 4 immediately after the write", got)
	}
	if got := mustRead(t, svc, org, slot, "presale").Value.Available; got != 4 {
		t.Fatalf("channel variant = %d, want 4 — every variant of the slot must drop", got)
	}
	if got := mustRead(t, svc, org, other, "").Value.Available; got != 10 {
		t.Fatalf("an untouched slot = %d, want its cached 10 — invalidation must be per slot", got)
	}
}

// TestInvalidationSupersedesAnOlderInFlightLoad is the subtle one, and it is the
// bug the ordering rule exists to prevent: a load that started BEFORE the write
// committed must not be allowed to populate the cache after it. If it could, the
// cache would serve the pre-commit answer for a full tier — the exact defect this
// ticket exists to fix, reintroduced by its own fix.
func TestInvalidationSupersedesAnOlderInFlightLoad(t *testing.T) {
	src := newFakeSource()
	src.setAvailable(10)
	src.release = make(chan struct{})
	src.entered = make(chan struct{}, 2)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Read(context.Background(), org, slot, "")
	}()
	<-src.entered // the stale load is now inside the source

	// The write commits and invalidates while that load is still in flight.
	src.setAvailable(4)
	svc.Invalidate(slot)

	close(src.release)
	<-done

	// The stale load has finished. Its result must not be in the cache.
	if got := mustRead(t, svc, org, slot, "").Value.Available; got != 4 {
		t.Fatalf("read after invalidation = %d, want 4 — a pre-commit load must not repopulate", got)
	}
}

// TestPostInvalidationReadDoesNotJoinAStaleFlight is the gap
// TestInvalidationSupersedesAnOlderInFlightLoad left open, and it is the more
// dangerous half.
//
// That test proves a pre-commit load cannot POPULATE the cache after the write.
// It says nothing about a reader that arrives after the write and finds the
// pre-commit load still running: joining it hands that reader the old number
// directly, without the cache being involved at all. COS 2 says the next read
// reflects the write, and "next read" includes this one.
func TestPostInvalidationReadDoesNotJoinAStaleFlight(t *testing.T) {
	src := newFakeSource()
	src.setAvailable(10)
	src.release = make(chan struct{})
	src.entered = make(chan struct{}, 4)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	stale := make(chan struct{})
	go func() {
		defer close(stale)
		_, _ = svc.Read(context.Background(), org, slot, "")
	}()
	<-src.entered // the pre-commit load is inside the source

	// The write commits and invalidates while that load is still running.
	src.setAvailable(4)
	svc.Invalidate(slot)

	// A buyer reads now. It must not be served the pre-commit answer.
	fresh := make(chan Read, 1)
	go func() {
		r, err := svc.Read(context.Background(), org, slot, "")
		if err != nil {
			t.Errorf("post-invalidation read: %v", err)
		}
		fresh <- r
	}()
	<-src.entered // it started its OWN load rather than joining the stale one

	close(src.release)
	<-stale
	got := <-fresh
	if got.Value.Available != 4 {
		t.Fatalf("post-invalidation read = %d, want 4 — it joined the pre-commit load", got.Value.Available)
	}
}

// TestASupersededLoadDoesNotEvictItsReplacement covers the bookkeeping half of
// the same race. When an invalidation makes a running load unjoinable, the next
// reader installs a replacement under the same key. If the superseded load then
// cleared the map entry unconditionally on its way out, it would remove the
// live replacement: later readers would start yet another load instead of
// joining, and the in-flight ceiling would be counting records that no longer
// match reality.
//
// Written because a mutation removing that guard left every other test green —
// defensive code with nothing pinning it is indistinguishable from dead code.
func TestASupersededLoadDoesNotEvictItsReplacement(t *testing.T) {
	src := newFakeSource()
	first := make(chan struct{})
	second := make(chan struct{})
	src.release = first
	src.entered = make(chan struct{}, 4)
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	stale := make(chan struct{})
	go func() { defer close(stale); _, _ = svc.Read(context.Background(), org, slot, "") }()
	<-src.entered

	svc.Invalidate(slot)

	// The replacement load blocks on a different channel, so it is still running
	// when the superseded one finishes.
	src.mu.Lock()
	src.release = second
	src.mu.Unlock()
	replacement := make(chan struct{})
	go func() { defer close(replacement); _, _ = svc.Read(context.Background(), org, slot, "") }()
	<-src.entered

	close(first)
	<-stale
	waitFor(t, func() bool { return src.count() == 2 })

	if got := svc.Status().InFlight; got != 1 {
		t.Fatalf("in-flight = %d after a superseded load finished, want 1 — it evicted its own replacement", got)
	}
	close(second)
	<-replacement
}

// TestConcurrentSourceLoadsAreBounded is the other half of COS 6, and the half
// the LRU does not cover: an entry is counted only once its load COMPLETES.
// Slot ids and channels are caller-supplied on a public unauthenticated route,
// so without a ceiling a caller sending unique keys drives unbounded concurrent
// queries into a 25-connection pool.
//
// It asserts PEAK CONCURRENT SOURCE CALLS, which is the actual invariant. The
// first version of this test asserted the size of the bookkeeping map instead,
// after deliberately waiting for ten concurrent loads to start — it proved the
// ceiling was not bounding anything and then passed. Counting the wrong thing is
// how a test certifies the opposite of what it claims.
func TestConcurrentSourceLoadsAreBounded(t *testing.T) {
	const ceiling = 3
	src := newFakeSource()
	src.release = make(chan struct{})
	src.entered = make(chan struct{}, 64)
	svc := New(src, WithBounds(16, 4), WithMaxInFlight(ceiling))

	org := uuid.New()
	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Read(context.Background(), org, uuid.New(), "")
		}()
	}
	// Let the ceiling fill and give any excess a chance to slip through.
	waitFor(t, func() bool { return src.concurrentPeak() >= ceiling })
	time.Sleep(50 * time.Millisecond)

	if peak := src.concurrentPeak(); peak > ceiling {
		t.Fatalf("peak concurrent source calls = %d, want at most %d — the ceiling must bound "+
			"real queries, not just the bookkeeping map", peak, ceiling)
	}
	close(src.release)
	wg.Wait()

	// Everything still completes: the ceiling queues work, it does not shed it.
	// Shedding would turn a cache into an availability outage.
	if got := src.count(); got != 25 {
		t.Fatalf("source called %d times, want all 25 reads served", got)
	}
}

// TestASlowLoadFailsRatherThanWedging pins the detached-load deadline.
//
// The load runs on its own context, because a follower cancelling must not abort
// the load other waiters depend on. That decision has a cost, and this is it: a
// query slower than the budget fails for every waiter. It has to fail, though —
// a detached load with no deadline holds its concurrency slot forever, and once
// enough of them do, the cache stops serving misses at all.
func TestASlowLoadFailsRatherThanWedging(t *testing.T) {
	src := newFakeSource()
	src.release = make(chan struct{}) // never closed: the query never returns
	svc := New(src, WithLoadTimeout(30*time.Millisecond))

	_, err := svc.Read(context.Background(), uuid.New(), uuid.New(), "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a load past its budget returned %v, want context.DeadlineExceeded", err)
	}
	// And the slot it held must be back, or one slow query would poison the cache.
	waitFor(t, func() bool { return svc.Status().InFlight == 0 })
}

// TestBoundedGloballyAndPerSlot is COS 6. The route is public and
// unauthenticated, so both ceilings are against a hostile caller, not a busy one:
// arbitrary slot UUIDs grow the global count, and arbitrary channel values grow
// one slot's variants.
func TestBoundedGloballyAndPerSlot(t *testing.T) {
	src := newFakeSource()
	svc := New(src, WithBounds(16, 4))
	org := uuid.New()

	slot := uuid.New()
	for i := range 50 {
		mustRead(t, svc, org, slot, "ch"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if got := svc.Status().Entries; got > 4 {
		t.Fatalf("one slot holds %d entries, want at most the per-slot bound of 4", got)
	}

	for range 100 {
		mustRead(t, svc, org, uuid.New(), "")
	}
	if got := svc.Status().Entries; got > 16 {
		t.Fatalf("cache holds %d entries, want at most the global bound of 16", got)
	}
}

// TestLoadErrorsAreNotCached: only successful answers are memoised. A transient
// store failure must not be pinned for a tier, and ErrNotFound is deliberately
// not cached either (plan-final decision 2) — caching it would make a
// newly-published slot answer 404 for five seconds.
func TestLoadErrorsAreNotCached(t *testing.T) {
	src := newFakeSource()
	src.err = errors.New("connection reset")
	svc, _ := newTestService(t, src)
	org, slot := uuid.New(), uuid.New()

	for range 3 {
		if _, err := svc.Read(context.Background(), org, slot, ""); err == nil {
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
		if _, err := svc.Read(context.Background(), org, slot, ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Read must surface ErrNotFound, got %v", err)
		}
	}
	if got := src.count(); got != 6 {
		t.Fatalf("source called %d times, want 6 — ErrNotFound is not cached either", got)
	}
}

// TestNewRegistersItsInvalidator: a cache wired to nothing is worse than no
// cache, because it looks like it works. New must not be able to produce one.
func TestNewRegistersItsInvalidator(t *testing.T) {
	src := newFakeSource()
	src.setAvailable(10)
	svc, _ := newTestService(t, src)
	if src.inval == nil {
		t.Fatal("New must register its invalidator with the source")
	}
	org, slot := uuid.New(), uuid.New()
	mustRead(t, svc, org, slot, "")
	src.setAvailable(4)
	src.inval(slot) // exactly what the store's post-commit callback does
	if got := mustRead(t, svc, org, slot, "").Value.Available; got != 4 {
		t.Fatalf("the registered invalidator did not reach the cache: %d", got)
	}
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
