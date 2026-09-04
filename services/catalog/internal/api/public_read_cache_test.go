package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
	"ticketing/shared/cachetier"
)

// countingSource is the seam for every "served from memory" claim here. Claims
// are counted, never timed: a timing assertion is flaky under CI load and says
// nothing about where the answer came from.
type countingSource struct {
	mu      sync.Mutex
	calls   map[string]int
	inside  int
	peak    int
	name    string // distinguishes the value a source returns, so hits are visible
	err     error
	release chan struct{}
	entered chan struct{}
	inval   func(store.PublicReadScope)
}

func newCountingSource() *countingSource {
	return &countingSource{calls: map[string]int{}, name: "first"}
}

func (c *countingSource) enter(op string) (chan struct{}, error) {
	c.mu.Lock()
	c.calls[op]++
	c.inside++
	c.peak = max(c.peak, c.inside)
	release, entered, err := c.release, c.entered, c.err
	c.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	return release, err
}

func (c *countingSource) leave() {
	c.mu.Lock()
	c.inside--
	c.mu.Unlock()
}

func (c *countingSource) block(ctx context.Context, release chan struct{}) error {
	if release == nil {
		return nil
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *countingSource) ListPublishedEvents(ctx context.Context) ([]store.EventAggregate, error) {
	release, err := c.enter("list")
	defer c.leave()
	if err != nil {
		return nil, err
	}
	if berr := c.block(ctx, release); berr != nil {
		return nil, berr
	}
	return []store.EventAggregate{{Event: store.Event{Name: store.LocalizedText{"en": c.label()}}}}, nil
}

func (c *countingSource) GetPublishedEvent(ctx context.Context, id uuid.UUID) (store.EventAggregate, error) {
	release, err := c.enter("event")
	defer c.leave()
	if err != nil {
		return store.EventAggregate{}, err
	}
	if berr := c.block(ctx, release); berr != nil {
		return store.EventAggregate{}, berr
	}
	return store.EventAggregate{Event: store.Event{ID: id, Name: store.LocalizedText{"en": c.label()}}}, nil
}

func (c *countingSource) GetPublishedSeason(ctx context.Context, id uuid.UUID) (store.SeasonAggregate, error) {
	release, err := c.enter("season")
	defer c.leave()
	if err != nil {
		return store.SeasonAggregate{}, err
	}
	if berr := c.block(ctx, release); berr != nil {
		return store.SeasonAggregate{}, berr
	}
	return store.SeasonAggregate{Season: store.Season{ID: id, Name: store.LocalizedText{"en": c.label()}}}, nil
}

func (c *countingSource) GetPublishedFestival(ctx context.Context, id uuid.UUID) (store.FestivalAggregate, error) {
	release, err := c.enter("festival")
	defer c.leave()
	if err != nil {
		return store.FestivalAggregate{}, err
	}
	if berr := c.block(ctx, release); berr != nil {
		return store.FestivalAggregate{}, berr
	}
	return store.FestivalAggregate{Festival: store.Festival{ID: id, Name: store.LocalizedText{"en": c.label()}}}, nil
}

func (c *countingSource) RegisterPublicReadInvalidator(fn func(store.PublicReadScope)) { c.inval = fn }

func (c *countingSource) label() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name
}

func (c *countingSource) setLabel(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name = s
}

func (c *countingSource) count(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[op]
}

func (c *countingSource) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *countingSource) concurrentPeak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestCache(t *testing.T, src publicReadSource) (*publicReadCache, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	config := defaultPublicReadCacheConfig()
	config.now = clk.now
	return newPublicReadCache(src, config), clk
}

func configuredTestCache(src publicReadSource, configure func(*publicReadCacheConfig)) *publicReadCache {
	config := defaultPublicReadCacheConfig()
	configure(&config)
	return newPublicReadCache(src, config)
}

// TestPublicReadCacheServesRepeatedReadsFromMemory is COS 1, across all four
// reads — season included, which the original COS omitted.
func TestPublicReadCacheServesRepeatedReadsFromMemory(t *testing.T) {
	src := newCountingSource()
	c, _ := newTestCache(t, src)
	ctx := context.Background()
	id := uuid.New()

	for range 4 {
		if _, err := c.ListPublishedEvents(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetPublishedEvent(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetPublishedSeason(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetPublishedFestival(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range []string{"list", "event", "season", "festival"} {
		if got := src.count(op); got != 1 {
			t.Errorf("%s source called %d times for 4 identical reads, want 1", op, got)
		}
	}
}

// TestPublicReadCacheKeyIsKindAndID: an id must not be shared across kinds, and
// two ids must not share an entry. Locale is deliberately NOT in the key — the
// store aggregates carry every locale and the handler projects one, so keying by
// locale would double the entries for identical data.
func TestPublicReadCacheKeyIsKindAndID(t *testing.T) {
	src := newCountingSource()
	c, _ := newTestCache(t, src)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()

	for _, id := range []uuid.UUID{a, b} {
		if _, err := c.GetPublishedEvent(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetPublishedSeason(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := src.count("event"); got != 2 {
		t.Errorf("event source called %d times for 2 ids, want 2", got)
	}
	if got := src.count("season"); got != 2 {
		t.Errorf("season source called %d times for 2 ids, want 2 — the same id under a different kind is a different entry", got)
	}
	// All four are now cached independently.
	for _, id := range []uuid.UUID{a, b} {
		_, _ = c.GetPublishedEvent(ctx, id)
		_, _ = c.GetPublishedSeason(ctx, id)
	}
	if got := src.total(); got != 4 {
		t.Errorf("source called %d times after re-reads, want still 4", got)
	}
}

// TestPublicReadCacheExpiresAtTheMinutesTier — and the TTL comes from the
// registry, which is why TKT-204 shipped first.
func TestPublicReadCacheExpiresAtTheMinutesTier(t *testing.T) {
	src := newCountingSource()
	c, clk := newTestCache(t, src)
	ctx := context.Background()

	_, _ = c.ListPublishedEvents(ctx)
	clk.advance(cachetier.Minutes.Duration() - time.Second)
	_, _ = c.ListPublishedEvents(ctx)
	if got := src.count("list"); got != 1 {
		t.Fatalf("source called %d times just inside the tier, want 1", got)
	}
	clk.advance(2 * time.Second)
	_, _ = c.ListPublishedEvents(ctx)
	if got := src.count("list"); got != 2 {
		t.Fatalf("source called %d times just past the tier, want 2 — a stale entry must decay", got)
	}
}

// TestPublicReadCacheReportsAge underpins the Age header, which is what stops
// catalog's five-minute tier and the storefront's stacking into ten.
func TestPublicReadCacheReportsAge(t *testing.T) {
	src := newCountingSource()
	c, clk := newTestCache(t, src)
	ctx := context.Background()

	r, err := c.ListPublishedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Age != 0 {
		t.Fatalf("a freshly loaded entry has age %v, want 0", r.Age)
	}
	clk.advance(90 * time.Second)
	r, _ = c.ListPublishedEvents(ctx)
	if r.Age != 90*time.Second {
		t.Fatalf("age = %v, want 90s", r.Age)
	}
}

// TestPublicReadInvalidationScopesListAndDetail is COS 2 and the reason the
// cache carries two generations rather than one: a season attachment must not
// dump the event list, and publishing must dump both.
func TestPublicReadInvalidationScopesListAndDetail(t *testing.T) {
	src := newCountingSource()
	c, _ := newTestCache(t, src)
	ctx := context.Background()
	id := uuid.New()

	warm := func() {
		_, _ = c.ListPublishedEvents(ctx)
		_, _ = c.GetPublishedEvent(ctx, id)
		_, _ = c.GetPublishedSeason(ctx, id)
		_, _ = c.GetPublishedFestival(ctx, id)
	}
	warm()
	src.setLabel("second")

	// Detail-only: details reload, the list stays warm.
	c.Invalidate(store.PublicReadDetail)
	if r, _ := c.GetPublishedEvent(ctx, id); r.Value.Event.Name["en"] != "second" {
		t.Errorf("event detail = %q, want the post-write value", r.Value.Event.Name["en"])
	}
	if r, _ := c.GetPublishedSeason(ctx, id); r.Value.Season.Name["en"] != "second" {
		t.Errorf("season detail = %q, want the post-write value", r.Value.Season.Name["en"])
	}
	if r, _ := c.GetPublishedFestival(ctx, id); r.Value.Festival.Name["en"] != "second" {
		t.Errorf("festival detail = %q, want the post-write value", r.Value.Festival.Name["en"])
	}
	if r, _ := c.ListPublishedEvents(ctx); r.Value[0].Event.Name["en"] != "first" {
		t.Errorf("list = %q, want its warm value — a detail-only write must not dump the list", r.Value[0].Event.Name["en"])
	}

	// List scope: the list reloads.
	src.setLabel("third")
	c.Invalidate(store.PublicReadList)
	if r, _ := c.ListPublishedEvents(ctx); r.Value[0].Event.Name["en"] != "third" {
		t.Errorf("list = %q, want the post-write value", r.Value[0].Event.Name["en"])
	}
}

// TestDetailInvalidationDoesNotDiscardAnInFlightListLoad is the other half of
// the two-generation split, and the half a warm-entry test cannot reach.
//
// Generations gate FLIGHTS, not cached entries — entries are dropped by the
// scope loop in Invalidate. So collapsing list and detail into one generation
// leaves every warm-entry assertion passing, and only shows up here: a detail
// write would discard a list load already in progress and force it to run again.
// Correctness-neutral, wasteful, and invisible without this test.
//
// Written because a mutation merging the two generations left every other test
// green. A property nothing pins is indistinguishable from an accident.
func TestDetailInvalidationDoesNotDiscardAnInFlightListLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newCountingSource()
		src.release = make(chan struct{})
		c, _ := newTestCache(t, src)

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = c.ListPublishedEvents(context.Background())
		}()
		synctest.Wait() // the list load is inside the source

		// A detail-scoped write commits while that list load is running.
		c.Invalidate(store.PublicReadDetail)

		close(src.release)
		<-done

		// The list load's generation was untouched, so its result was cached.
		// A shared generation would have discarded it and the next read reloads.
		if _, err := c.ListPublishedEvents(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := src.count("list"); got != 1 {
			t.Fatalf("list source called %d times, want 1 — a DETAIL write discarded an in-flight LIST load", got)
		}
	})
}

// TestPublicReadCacheDoesNotCacheErrors: a transient failure must not be pinned
// for five minutes, and ErrNotFound is not cached either — caching it would make
// a newly published season answer 404 until its entry expired.
func TestPublicReadCacheDoesNotCacheErrors(t *testing.T) {
	src := newCountingSource()
	src.err = store.ErrNotFound
	c, _ := newTestCache(t, src)
	ctx := context.Background()
	id := uuid.New()

	for range 3 {
		if _, err := c.GetPublishedSeason(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if got := src.count("season"); got != 3 {
		t.Fatalf("source called %d times, want 3 — errors are never cached", got)
	}
}

// TestPublicReadCacheConcurrentMissesLoadOnce — a hot event page expiring under
// an on-sale burst must not stampede catalog.
func TestPublicReadCacheConcurrentMissesLoadOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newCountingSource()
		src.release = make(chan struct{})
		c, _ := newTestCache(t, src)

		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = c.ListPublishedEvents(context.Background())
			}()
		}
		synctest.Wait()
		close(src.release)
		wg.Wait()

		if got := src.count("list"); got != 1 {
			t.Fatalf("source called %d times for 20 concurrent misses, want 1", got)
		}
	})
}

// TestPublicReadCacheBoundsConcurrentSourceLoads asserts PEAK CONCURRENT SOURCE
// CALLS, not the size of any bookkeeping map. TKT-205 shipped a ceiling test
// that asserted the map instead; it waited for ten concurrent loads and then
// passed because the map held three. Counting the wrong thing is how a test
// certifies the opposite of its claim.
func TestPublicReadCacheBoundsConcurrentSourceLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ceiling = 3
		src := newCountingSource()
		src.release = make(chan struct{})
		c := configuredTestCache(src, func(config *publicReadCacheConfig) {
			config.maxEntries = 64
			config.maxInFlight = ceiling
		})

		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = c.GetPublishedEvent(context.Background(), uuid.New())
			}()
		}
		synctest.Wait()
		if peak := src.concurrentPeak(); peak > ceiling {
			t.Fatalf("peak concurrent source calls = %d, want at most %d", peak, ceiling)
		}
		close(src.release)
		wg.Wait()
		if got := src.count("event"); got != 20 {
			t.Fatalf("source called %d times, want all 20 served — the ceiling queues, it does not shed", got)
		}
	})
}

// TestPublicReadCacheIsBounded: the detail reads take a caller-supplied UUID on
// a public unauthenticated route, so entries cannot be allowed to grow with
// distinct ids.
func TestPublicReadCacheIsBounded(t *testing.T) {
	src := newCountingSource()
	c := configuredTestCache(src, func(config *publicReadCacheConfig) {
		config.maxEntries = 8
		config.maxInFlight = 16
	})
	for range 100 {
		_, _ = c.GetPublishedEvent(context.Background(), uuid.New())
	}
	if got := c.Status().Entries; got > 8 {
		t.Fatalf("cache holds %d entries, want at most the bound of 8", got)
	}
}

// TestPublicReadCacheSlowLoadFailsRatherThanWedging pins the query budget.
//
// A load runs on its own context, because a follower cancelling must not abort
// the load other waiters depend on. The cost of that decision is this: a query
// slower than the budget fails for every waiter. It has to fail — a detached
// load holds one of the concurrency slots for its whole life, and once enough
// of them do, the cache stops serving misses at all.
func TestPublicReadCacheSlowLoadFailsRatherThanWedging(t *testing.T) {
	src := newCountingSource()
	src.release = make(chan struct{}) // never closed: the query never returns
	c := configuredTestCache(src, func(config *publicReadCacheConfig) {
		config.loadTimeout = 30 * time.Millisecond
	})

	_, err := c.ListPublishedEvents(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a load past its budget returned %v, want context.DeadlineExceeded", err)
	}
	// And the concurrency slot it held must come back, or one slow query
	// permanently narrows the cache.
	deadline := time.Now().Add(2 * time.Second)
	for len(c.sem) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(c.sem) != 0 {
		t.Fatal("the timed-out load did not release its concurrency slot")
	}
}

// TestPostInvalidationReadDoesNotJoinAStaleFlight is the subtle half of COS 2,
// and the bug TKT-205 shipped and had caught in review. Discarding a pre-write
// load's result is not enough: a reader arriving after the write could still
// JOIN that load and be handed the old value directly, bypassing the cache and
// the generation guard together.
func TestPostInvalidationReadDoesNotJoinAStaleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newCountingSource()
		src.release = make(chan struct{})
		src.entered = make(chan struct{}, 4)
		c, _ := newTestCache(t, src)

		stale := make(chan struct{})
		go func() {
			defer close(stale)
			_, _ = c.ListPublishedEvents(context.Background())
		}()
		<-src.entered

		src.setLabel("second")
		c.Invalidate(store.PublicReadList)

		fresh := make(chan string, 1)
		go func() {
			r, _ := c.ListPublishedEvents(context.Background())
			fresh <- r.Value[0].Event.Name["en"]
		}()
		<-src.entered // it started its OWN load rather than joining the stale one

		close(src.release)
		<-stale
		if got := <-fresh; got != "second" {
			t.Fatalf("post-invalidation read = %q, want the post-write value — it joined the pre-write load", got)
		}
	})
}

// --- TKT-210: the incident kill-switch ---

// TestDisablePurgesAndBypassesEveryPublicRead is COS 1 and 2 at the cache level,
// across all four reads. Counted, never timed.
func TestDisablePurgesAndBypassesEveryPublicRead(t *testing.T) {
	src := newCountingSource()
	c, _ := newTestCache(t, src)
	ctx := context.Background()
	id := uuid.New()

	warm := func() {
		_, _ = c.ListPublishedEvents(ctx)
		_, _ = c.GetPublishedEvent(ctx, id)
		_, _ = c.GetPublishedSeason(ctx, id)
		_, _ = c.GetPublishedFestival(ctx, id)
	}
	warm()
	warm()
	if got := src.total(); got != 4 {
		t.Fatalf("warm-up: source called %d times, want 4", got)
	}

	c.SetEnabled(false)
	if st := c.Status(); st.Enabled || st.Entries != 0 {
		t.Fatalf("after disable: %+v, want enabled=false entries=0", st)
	}
	warm()
	warm()
	if got := src.total(); got != 12 {
		t.Fatalf("source called %d times across two disabled rounds, want 12 — every read must reach the store", got)
	}
	for _, op := range []string{"list", "event", "season", "festival"} {
		if got := src.count(op); got != 3 {
			t.Errorf("%s called %d times, want 3 (1 warm + 2 bypassed) — every read kind must bypass", op, got)
		}
	}

	// Re-enabling starts all four cold.
	c.SetEnabled(true)
	if st := c.Status(); !st.Enabled || st.Entries != 0 {
		t.Fatalf("after re-enable: %+v, want enabled=true entries=0", st)
	}
	warm()
	warm()
	if got := src.total(); got != 16 {
		t.Fatalf("source called %d times after re-enable, want 16 — one cold load each, then hits", got)
	}
}

// TestAToggleCycleRejectsPreToggleLoads: the case a plain `enabled` check misses.
// Disable and re-enable while loads are in flight — on return the cache is on
// again and neither the list nor the detail generation moved, so every other
// guard says insert. But those values were read before an operator took the
// cache out of service. Covers BOTH generation domains, since implementing the
// switch for only one would leave the other silently repopulating.
func TestAToggleCycleRejectsPreToggleLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newCountingSource()
		src.release = make(chan struct{})
		c, _ := newTestCache(t, src)
		id := uuid.New()

		done := make(chan struct{}, 2)
		go func() { _, _ = c.ListPublishedEvents(context.Background()); done <- struct{}{} }()
		go func() { _, _ = c.GetPublishedEvent(context.Background(), id); done <- struct{}{} }()
		synctest.Wait()

		c.SetEnabled(false)
		c.SetEnabled(true)

		src.setLabel("second")
		close(src.release)
		<-done
		<-done

		if st := c.Status(); st.Entries != 0 {
			t.Fatalf("loads that started before a toggle cycle were cached: %+v", st)
		}
		if r, _ := c.ListPublishedEvents(context.Background()); r.Value[0].Event.Name["en"] != "second" {
			t.Errorf("list = %q, want the post-toggle value", r.Value[0].Event.Name["en"])
		}
		if r, _ := c.GetPublishedEvent(context.Background(), id); r.Value.Event.Name["en"] != "second" {
			t.Errorf("detail = %q, want the post-toggle value", r.Value.Event.Name["en"])
		}
	})
}

// TestCatalogSetEnabledIsIdempotent: re-asserting the current state must not
// purge a warm cache — an operator repeating a command should not cost a reload
// wave across every cached event.
func TestCatalogSetEnabledIsIdempotent(t *testing.T) {
	src := newCountingSource()
	c, _ := newTestCache(t, src)
	ctx := context.Background()

	_, _ = c.ListPublishedEvents(ctx)
	c.SetEnabled(true)
	_, _ = c.ListPublishedEvents(ctx)
	if got := src.count("list"); got != 1 {
		t.Fatalf("source called %d times, want 1 — enabling an enabled cache must not purge it", got)
	}
}

// TestADisabledPublicReadDoesNotJoinAnInFlightLoad — same property as inventory's:
// SetEnabled purges entries but does NOT clear the in-flight map, and the
// list/detail generations do not move on a toggle. Without the read path's first
// enabled check, a disabled reader would join a load that started before the
// operator disabled the cache and be served the very value being withdrawn.
func TestADisabledPublicReadDoesNotJoinAnInFlightLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newCountingSource()
		src.release = make(chan struct{})
		c, _ := newTestCache(t, src)

		done := make(chan struct{})
		go func() { defer close(done); _, _ = c.ListPublishedEvents(context.Background()) }()
		synctest.Wait()

		c.SetEnabled(false)
		src.setLabel("second")

		fresh := make(chan string, 1)
		go func() {
			r, _ := c.ListPublishedEvents(context.Background())
			fresh <- r.Value[0].Event.Name["en"]
		}()
		synctest.Wait()

		close(src.release)
		<-done
		if got := <-fresh; got != "second" {
			t.Fatalf("a disabled read returned %q — it joined a load started before the cache was disabled", got)
		}
		if got := src.count("list"); got != 2 {
			t.Fatalf("source called %d times, want 2 — the disabled read must make its own call", got)
		}
	})
}

// TestAPostReEnableReadDoesNotJoinAPreDisableFlight — same transition window as
// inventory's. TestAToggleCycleRejectsPreToggleLoads waits for the old loads to
// finish, so it only exercised the insertion guard; a reader arriving while a
// pre-disable load is still running would join it if the predicate checks only
// the list/detail generation, which a toggle never moves.
func TestAPostReEnableReadDoesNotJoinAPreDisableFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newCountingSource()
		src.release = make(chan struct{})
		c, _ := newTestCache(t, src)

		stale := make(chan struct{})
		go func() { defer close(stale); _, _ = c.ListPublishedEvents(context.Background()) }()
		synctest.Wait()

		c.SetEnabled(false)
		c.SetEnabled(true)
		src.setLabel("second")

		fresh := make(chan string, 1)
		go func() {
			r, _ := c.ListPublishedEvents(context.Background())
			fresh <- r.Value[0].Event.Name["en"]
		}()
		synctest.Wait()

		close(src.release)
		<-stale
		if got := <-fresh; got != "second" {
			t.Fatalf("post-re-enable read = %q, want the post-toggle value — it joined a pre-disable load", got)
		}
		if got := src.count("list"); got != 2 {
			t.Fatalf("source called %d times, want 2", got)
		}
	})
}
