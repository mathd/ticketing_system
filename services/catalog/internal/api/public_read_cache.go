package api

import (
	"container/list"
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
	"ticketing/shared/cachetier"
)

// The catalog public-read cache (TKT-206, ADR-045) — ADR-004 rule 2 on the
// minutes tier, after TKT-205 proved the shape on inventory's seconds tier.
//
// Deliberately catalog-local rather than a shared package. The machinery (LRU,
// single flight, semaphore, generations) is the same; the SEMANTICS are not.
// Inventory invalidates every variant of one slot, which every write knows.
// Catalog's list is global and its detail dependencies are not derivable from a
// write — see Invalidate. Extracting a generic primitive would have to
// parameterise key type, value type, generation domain and eviction grouping,
// which is more abstraction than two consumers justify. ADR-045 records the
// shape to extract and the trigger: the third consumer.
//
// The bounds are against a hostile caller, not a busy one: the detail reads take
// a caller-supplied UUID on a public unauthenticated route.

const (
	defaultPublicReadEntries  = 2048
	defaultPublicReadInFlight = 16 // headroom inside the 25-connection pool
	defaultPublicReadTimeout  = 10 * time.Second
)

// publicReadSource is the narrow set of store reads this cache fronts, plus its
// invalidator registration. Registration lives on the interface so the cache
// cannot be constructed unwired — a cache nothing invalidates is worse than no
// cache, because it looks like it works.
//
// Writes are not on this interface. Server keeps separate handler dependencies
// for writes and seat-map reads, so only the four minute-tier handlers can reach
// a cached value. A structural test pins that separation.
type publicReadSource interface {
	ListPublishedEvents(ctx context.Context) ([]store.EventAggregate, error)
	GetPublishedEvent(ctx context.Context, id uuid.UUID) (store.EventAggregate, error)
	GetPublishedSeason(ctx context.Context, id uuid.UUID) (store.SeasonAggregate, error)
	GetPublishedFestival(ctx context.Context, id uuid.UUID) (store.FestivalAggregate, error)
	RegisterPublicReadInvalidator(func(store.PublicReadScope))
}

// cached is a value and how stale it already is. Age becomes the Age header,
// which is what stops catalog's five-minute tier and the storefront's stacking
// into ten.
type cached[T any] struct {
	Value T
	Age   time.Duration
}

type readKind uint8

const (
	kindList readKind = iota
	kindEvent
	kindSeason
	kindFestival
)

// scope says which generation a kind belongs to. The list is its own scope
// because a detail-only write must not dump it.
func (k readKind) scope() store.PublicReadScope {
	if k == kindList {
		return store.PublicReadList
	}
	return store.PublicReadDetail
}

// readKey is (kind, id). Locale is deliberately absent: the store aggregates
// carry every locale and the handler projects one, so keying by locale would
// double the entries for byte-identical data. If a future parameter ever changes
// what the STORE returns, it must be passed to the store method too — and that
// changes this interface, which makes the omission a compile error rather than a
// silent bug.
type readKey struct {
	kind readKind
	id   uuid.UUID
}

type readEntry struct {
	key      readKey
	value    any
	loadedAt time.Time
	elem     *list.Element
}

type readFlight struct {
	done chan struct{}
	gen  uint64
	// switchGen is the kill-switch generation when this load started (TKT-210).
	switchGen uint64
	value     any
	err       error
}

type publicReadCache struct {
	src publicReadSource
	now func() time.Time
	ttl time.Duration

	maxEntries  int
	maxInFlight int
	loadTimeout time.Duration

	sem chan struct{}

	mu      sync.Mutex
	enabled bool
	// switchGen advances on every real kill-switch transition, independently of
	// the list/detail generations. Without it, a disable followed by a re-enable
	// while a load is in flight would let that load insert on return: the cache
	// is on again and neither generation moved, so every other guard says yes —
	// but the value was read before an operator took the cache out of service.
	switchGen uint64
	entries   map[readKey]*readEntry
	lru       *list.List
	inflight  map[readKey]*readFlight
	// Two generations, both global. See Invalidate for why global.
	listGen   uint64
	detailGen uint64
}

type publicReadCacheConfig struct {
	now         func() time.Time
	ttl         time.Duration
	maxEntries  int
	maxInFlight int
	loadTimeout time.Duration
}

func defaultPublicReadCacheConfig() publicReadCacheConfig {
	return publicReadCacheConfig{
		now:         time.Now,
		ttl:         cachetier.Minutes.Duration(),
		maxEntries:  defaultPublicReadEntries,
		maxInFlight: defaultPublicReadInFlight,
		loadTimeout: defaultPublicReadTimeout,
	}
}

func newPublicReadCache(src publicReadSource, config publicReadCacheConfig) *publicReadCache {
	c := &publicReadCache{
		src:         src,
		now:         config.now,
		ttl:         config.ttl,
		maxEntries:  config.maxEntries,
		maxInFlight: config.maxInFlight,
		loadTimeout: config.loadTimeout,
		enabled:     true,
		entries:     map[readKey]*readEntry{},
		lru:         list.New(),
		inflight:    map[readKey]*readFlight{},
	}
	c.sem = make(chan struct{}, c.maxInFlight)
	src.RegisterPublicReadInvalidator(c.Invalidate)
	return c
}

// SetEnabled is the incident kill-switch ADR-004's Consequences required
// (TKT-210). Disabling purges everything and stops in-flight loads from
// inserting; re-enabling starts cold. Idempotent, so an operator repeating a
// command does not cost a reload wave across every cached event.
func (c *publicReadCache) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enabled == enabled {
		return
	}
	c.enabled = enabled
	c.switchGen++
	for _, e := range c.entries {
		c.removeLocked(e)
	}
}

// publicReadStatus is the operator-visible state. Only `enabled` and `entries`
// are exposed: entry count is CARDINALITY, not a byte-size claim — ADR-045 is
// explicit that the bounded entry count does not bound payload size.
type publicReadStatus struct {
	Enabled bool `json:"enabled"`
	Entries int  `json:"entries"`
}

func (c *publicReadCache) Status() publicReadStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return publicReadStatus{Enabled: c.enabled, Entries: len(c.entries)}
}

// bypass serves a read while the cache is disabled: its own source call, no
// entry consulted, no flight registered, no insertion, age zero. It still takes
// a concurrency slot, so disabling does not remove the bound protecting the
// database — a kill-switch is not a licence to stampede.
func (c *publicReadCache) bypass(ctx context.Context, load func(context.Context) (any, error)) (any, time.Duration, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	defer func() { <-c.sem }()
	lctx, cancel := context.WithTimeout(context.Background(), c.loadTimeout)
	defer cancel()
	v, err := load(lctx)
	return v, 0, err
}

// Invalidate drops every cached representation in the given scope.
//
// GLOBAL, not per id, and that is a correctness decision rather than a lazy one.
// The list endpoint has no organizer filter, so any newly listable event changes
// it. Detail keys look per-id-invalidatable and are not: catalog's writes carry
// no complete dependency graph, so a "precise" scheme misses exactly the case
// that matters — a member that was ABSENT from a cached response becoming
// present. Adding a ticket type can make an already-published slot appear;
// publishing can reach a season directly or through a series; ADR-018 lets a
// festival day participate in a series.
//
// Over-invalidating costs a reload. Under-invalidating serves a wrong answer.
// The cost is a colder cache during bulk authoring, bounded by the source-call
// ceiling — recorded in ADR-045 as an operating characteristic, not a surprise.
func (c *publicReadCache) Invalidate(scope store.PublicReadScope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if scope.Has(store.PublicReadList) {
		c.listGen++
	}
	if scope.Has(store.PublicReadDetail) {
		c.detailGen++
	}
	for k, e := range c.entries {
		if scope.Has(k.kind.scope()) {
			c.removeLocked(e)
		}
	}
}

func (c *publicReadCache) generationLocked(k readKind) uint64 {
	if k == kindList {
		return c.listGen
	}
	return c.detailGen
}

// read is the whole cache in one function, shared by the four typed methods.
func (c *publicReadCache) read(ctx context.Context, k readKey, load func(context.Context) (any, error)) (any, time.Duration, error) {
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return c.bypass(ctx, load)
	}
	if e, ok := c.entries[k]; ok {
		if age := c.now().Sub(e.loadedAt); age < c.ttl {
			c.lru.MoveToFront(e.elem)
			v := e.value
			c.mu.Unlock()
			return v, age, nil
		}
		c.removeLocked(e)
	}
	gen := c.generationLocked(k.kind)
	// Join only a flight from the CURRENT generation. A load that started before
	// a write holds the pre-write answer, and handing that to a reader who
	// arrived after the write is the same staleness the generation guard refuses
	// to cache, delivered by a shorter route. Discarding the result is not
	// enough; the flight has to be unjoinable. (TKT-205 shipped this bug and had
	// it caught in review.)
	if f, ok := c.inflight[k]; ok && f.gen == gen && f.switchGen == c.switchGen {
		c.mu.Unlock()
		return c.wait(ctx, f)
	}
	c.mu.Unlock()

	// Bound concurrent SOURCE CALLS, not bookkeeping — entries are counted only
	// once a load completes, so the entry ceiling constrains nothing about a
	// caller sending unique ids. Taking the slot before registering a flight
	// bounds the in-flight map by the same number for free. It queues rather than
	// sheds: shedding would turn a cache into an outage.
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	c.mu.Lock()
	// Deliberately no second `enabled` check: the insertion guard already requires
	// `enabled`, so a read that queued while the cache was on and found it off
	// cannot populate anything. See inventory's cache for the full reasoning.
	if e, ok := c.entries[k]; ok {
		if age := c.now().Sub(e.loadedAt); age < c.ttl {
			c.lru.MoveToFront(e.elem)
			v := e.value
			c.mu.Unlock()
			<-c.sem
			return v, age, nil
		}
		c.removeLocked(e)
	}
	gen = c.generationLocked(k.kind)
	if f, ok := c.inflight[k]; ok && f.gen == gen && f.switchGen == c.switchGen {
		c.mu.Unlock()
		<-c.sem
		return c.wait(ctx, f)
	}
	f := &readFlight{done: make(chan struct{}), gen: gen, switchGen: c.switchGen}
	c.inflight[k] = f
	c.mu.Unlock()

	go c.load(k, f, load)
	return c.wait(ctx, f)
}

func (c *publicReadCache) load(k readKey, f *readFlight, load func(context.Context) (any, error)) {
	defer func() { <-c.sem }()
	// The caller's context is deliberately not used: a follower cancelling must
	// not abort the load every other waiter depends on. A detached load holds a
	// concurrency slot for its whole life, so it gets its own budget — without
	// one, enough slow queries take every slot and the cache stops serving misses.
	ctx, cancel := context.WithTimeout(context.Background(), c.loadTimeout)
	defer cancel()
	v, err := load(ctx)

	c.mu.Lock()
	f.value, f.err = v, err
	// Only a successful load whose generation still stands may be cached. Errors
	// are never cached — a transient failure must not be pinned for five minutes,
	// and ErrNotFound is not cached either: a newly published season would answer
	// 404 until its entry expired.
	// Every guard must hold: the cache is on, no toggle since this load started,
	// the generation still matches, and the source succeeded.
	if err == nil && c.enabled && c.switchGen == f.switchGen && c.generationLocked(k.kind) == f.gen {
		c.insertLocked(k, v)
	}
	// Only clear the map entry if it is still THIS flight: an invalidation while
	// this load ran makes a later reader install a replacement under the same key,
	// and deleting unconditionally would evict that live flight.
	if cur, ok := c.inflight[k]; ok && cur == f {
		delete(c.inflight, k)
	}
	c.mu.Unlock()
	close(f.done)
}

func (c *publicReadCache) wait(ctx context.Context, f *readFlight) (any, time.Duration, error) {
	select {
	case <-f.done:
		return f.value, 0, f.err
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

func (c *publicReadCache) insertLocked(k readKey, v any) {
	for len(c.entries) >= c.maxEntries {
		el := c.lru.Back()
		if el == nil {
			return
		}
		e, ok := c.entries[el.Value.(readKey)]
		if !ok {
			c.lru.Remove(el)
			continue
		}
		c.removeLocked(e)
	}
	e := &readEntry{key: k, value: v, loadedAt: c.now()}
	e.elem = c.lru.PushFront(k)
	c.entries[k] = e
}

func (c *publicReadCache) removeLocked(e *readEntry) {
	c.lru.Remove(e.elem)
	delete(c.entries, e.key)
}

func (c *publicReadCache) ListPublishedEvents(ctx context.Context) (cached[[]store.EventAggregate], error) {
	v, age, err := c.read(ctx, readKey{kind: kindList}, func(ctx context.Context) (any, error) {
		return c.src.ListPublishedEvents(ctx)
	})
	if err != nil {
		return cached[[]store.EventAggregate]{}, err
	}
	return cached[[]store.EventAggregate]{Value: v.([]store.EventAggregate), Age: age}, nil
}

func (c *publicReadCache) GetPublishedEvent(ctx context.Context, id uuid.UUID) (cached[store.EventAggregate], error) {
	v, age, err := c.read(ctx, readKey{kind: kindEvent, id: id}, func(ctx context.Context) (any, error) {
		return c.src.GetPublishedEvent(ctx, id)
	})
	if err != nil {
		return cached[store.EventAggregate]{}, err
	}
	return cached[store.EventAggregate]{Value: v.(store.EventAggregate), Age: age}, nil
}

func (c *publicReadCache) GetPublishedSeason(ctx context.Context, id uuid.UUID) (cached[store.SeasonAggregate], error) {
	v, age, err := c.read(ctx, readKey{kind: kindSeason, id: id}, func(ctx context.Context) (any, error) {
		return c.src.GetPublishedSeason(ctx, id)
	})
	if err != nil {
		return cached[store.SeasonAggregate]{}, err
	}
	return cached[store.SeasonAggregate]{Value: v.(store.SeasonAggregate), Age: age}, nil
}

func (c *publicReadCache) GetPublishedFestival(ctx context.Context, id uuid.UUID) (cached[store.FestivalAggregate], error) {
	v, age, err := c.read(ctx, readKey{kind: kindFestival, id: id}, func(ctx context.Context) (any, error) {
		return c.src.GetPublishedFestival(ctx, id)
	})
	if err != nil {
		return cached[store.FestivalAggregate]{}, err
	}
	return cached[store.FestivalAggregate]{Value: v.(store.FestivalAggregate), Age: age}, nil
}

// setPublicReadAge emits RFC 9111's Age alongside the minutes tier.
//
// It is what stops two staleness budgets stacking. Cache-Control declares these
// responses publicly cacheable for five minutes; the storefront's SSR cache
// (web/storefront/src/lib/cache.ts) starts every entry it fetches at age zero.
// So without Age, a catalog entry already 299 seconds old would be granted
// another 300 by Astro — ten minutes of observable staleness against a tier that
// promises five. Rounded UP, so the number is never optimistic, and clamped to
// the tier because an entry older than max-age is never served.
//
// Varying Cache-Control by remaining freshness instead — which is what the
// storefront middleware does for pages — is not available here: ADR-028
// validates declared response headers, and the tier is committed as a fixed
// value.
func setPublicReadAge(w http.ResponseWriter, age time.Duration) {
	seconds := int(math.Ceil(age.Seconds()))
	if maxAge := int(cachetier.Minutes.Duration().Seconds()); seconds > maxAge {
		seconds = maxAge
	}
	if seconds < 0 {
		seconds = 0
	}
	w.Header().Set("Age", strconv.Itoa(seconds))
}
