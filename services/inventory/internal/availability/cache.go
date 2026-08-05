// Package availability serves the public availability read from memory.
//
// This is the first implementation of ADR-004 rule 2 — "hot events are served
// from memory, refreshed/invalidated from the write path" — anywhere in the
// stack. See ADR-044 for the decision and, more importantly, for its limits.
//
// Three properties are load-bearing and each has a test that fails without it:
//
//   - The lifetime is cachetier.Seconds, the same value the response header
//     advertises. Not a literal (that is why TKT-204 shipped first).
//   - Concurrent misses for one key produce one load. An expiring hot slot must
//     not stampede Postgres.
//   - A load that started before a write committed can never reach a reader who
//     arrived after it: its result is neither cached NOR joinable. Discarding it
//     alone is not enough — a post-commit reader could otherwise join the
//     pre-commit load and be handed the old number directly, bypassing the cache
//     and the guard together.
//
// What this package is NOT (ADR-021's rule — name the adversary before claiming
// a guarantee):
//
//   - It is process-local. With a second inventory replica, a write invalidates
//     only the writer's process.
//   - It is honest-writer consistency, not tamper-evidence. Anyone who can write
//     to inventory's database bypasses every callback here.
//   - Bounded memory is not bounded load. Three ceilings — entries, entries per
//     slot, and concurrent loads — stop the cache growing on a public route; they
//     do not stop a hostile caller forcing real queries. This is not a rate
//     limiter.
package availability

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/cachetier"
)

// Default ceilings. Both are against a hostile caller rather than a busy one:
// the route is public and unauthenticated, so slot ids and channel strings are
// caller-supplied. maxPerSlot is 128 because the contract allows 100 channel
// allocations plus the default channel (openapi.yaml, `allocations.maxItems`),
// so a legitimate slot cannot approach it.
const (
	defaultMaxEntries = 10000
	defaultMaxPerSlot = 128
	// defaultMaxInFlight bounds concurrent loads, which the entry ceilings do not:
	// an entry is only counted once its load COMPLETES.
	defaultMaxInFlight = 1000
	// defaultLoadTimeout is the availability query budget. Naming it a backstop
	// would be dishonest: a detached load holds one of maxInFlight slots, so a
	// query slower than this fails for its waiters, and that is the intended
	// behaviour — without a deadline, enough hung queries take every slot and the
	// cache stops serving misses at all. Generous relative to a read that is
	// normally single-digit milliseconds.
	defaultLoadTimeout = 10 * time.Second
)

// Source is what the cache reads through. RegisterAvailabilityInvalidator is on
// this interface, not a separate wiring step, so New cannot produce a cache that
// receives no write notifications — a cache wired to nothing is worse than no
// cache, because it looks like it works.
type Source interface {
	Availability(ctx context.Context, org, slot uuid.UUID, channel string) (store.Availability, error)
	RegisterAvailabilityInvalidator(func(uuid.UUID))
}

// Read is a cached answer and how stale it already is. Age is what the handler
// turns into the Age header; without it a nearly-expired entry would grant a
// conformant client another full tier of freshness.
type Read struct {
	Value store.Availability
	Age   time.Duration
}

// Status is the operator-visible state. TKT-210 exposes it over HTTP.
type Status struct {
	Entries     int `json:"entries"`
	InFlight    int `json:"in_flight"`
	MaxEntries  int `json:"max_entries"`
	MaxPerSlot  int `json:"max_entries_per_slot"`
	MaxInFlight int `json:"max_in_flight"`
}

type key struct {
	org, slot uuid.UUID
	channel   string
}

type entry struct {
	key      key
	value    store.Availability
	loadedAt time.Time
	elem     *list.Element // position in the LRU list
}

// flight is one in-progress load. gen is the slot generation captured when the
// load started: if the slot has been invalidated since, the result is stale on
// arrival and must not be inserted.
type flight struct {
	done  chan struct{}
	gen   uint64
	value store.Availability
	err   error
}

type Service struct {
	src Source
	now func() time.Time
	ttl time.Duration

	maxEntries  int
	maxPerSlot  int
	maxInFlight int
	loadTimeout time.Duration

	// sem bounds concurrent source calls. Buffered to maxInFlight; a slot is held
	// for exactly the duration of one load.
	sem chan struct{}

	mu       sync.Mutex
	entries  map[key]*entry
	lru      *list.List // front = most recently used; values are key
	perSlot  map[uuid.UUID]int
	gen      map[uuid.UUID]uint64
	inflight map[key]*flight
}

type Option func(*Service)

// WithClock injects the clock so TTL decay is tested by advancing time rather
// than by sleeping — a sleeping test is slow and flaky, and neither proves more.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithBounds overrides the ceilings, for tests that need to reach them cheaply.
func WithBounds(maxEntries, maxPerSlot int) Option {
	return func(s *Service) { s.maxEntries, s.maxPerSlot = maxEntries, maxPerSlot }
}

// WithMaxInFlight overrides the concurrent-load ceiling, for tests that need to
// reach it cheaply.
func WithMaxInFlight(n int) Option { return func(s *Service) { s.maxInFlight = n } }

// WithLoadTimeout overrides the query budget, for tests that need to reach it
// without waiting the production ten seconds.
func WithLoadTimeout(d time.Duration) Option { return func(s *Service) { s.loadTimeout = d } }

func New(src Source, opts ...Option) *Service {
	s := &Service{
		src:         src,
		now:         time.Now,
		ttl:         cachetier.Seconds.Duration(),
		maxEntries:  defaultMaxEntries,
		maxPerSlot:  defaultMaxPerSlot,
		maxInFlight: defaultMaxInFlight,
		loadTimeout: defaultLoadTimeout,
		entries:     map[key]*entry{},
		lru:         list.New(),
		perSlot:     map[uuid.UUID]int{},
		gen:         map[uuid.UUID]uint64{},
		inflight:    map[key]*flight{},
	}
	for _, o := range opts {
		o(s)
	}
	s.sem = make(chan struct{}, s.maxInFlight)
	src.RegisterAvailabilityInvalidator(s.Invalidate)
	return s
}

// Invalidate drops every cached variant of one slot and bumps its generation.
//
// Indexed by slot alone, deliberately broader than the read key: the write paths
// that call this know the pool, and the consumer paths (ApplyArchive,
// ApplyClosure) do not always know its organizer. Dropping too much costs one
// reload; dropping too little serves a wrong number for a tier.
//
// The generation bump is what stops an in-flight load that started before the
// write from inserting its pre-commit answer afterwards.
func (s *Service) Invalidate(slot uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen[slot]++
	for k, e := range s.entries {
		if k.slot == slot {
			s.removeLocked(e)
		}
	}
}

// Read answers from memory when a fresh entry exists, and otherwise loads once —
// however many callers are waiting.
func (s *Service) Read(ctx context.Context, org, slot uuid.UUID, channel string) (Read, error) {
	k := key{org: org, slot: slot, channel: channel}

	s.mu.Lock()
	if e, ok := s.entries[k]; ok {
		if age := s.now().Sub(e.loadedAt); age < s.ttl {
			s.lru.MoveToFront(e.elem)
			v := e.value
			s.mu.Unlock()
			return Read{Value: v, Age: age}, nil
		}
		s.removeLocked(e)
	}
	gen := s.gen[slot]
	// Join an in-flight load for the same key rather than starting a second one —
	// but ONLY one from the current generation. A load that started before a write
	// committed holds the pre-commit answer, and handing that to a reader who
	// arrived after the commit is the same staleness the generation guard already
	// refuses to cache, delivered by a shorter route. Discarding the result is not
	// enough; the flight has to be unjoinable.
	if f, ok := s.inflight[k]; ok && f.gen == gen {
		s.mu.Unlock()
		return s.wait(ctx, f)
	}

	s.mu.Unlock()

	// Bound CONCURRENT SOURCE CALLS, not bookkeeping. Entries are counted only
	// once a load completes, so the LRU ceilings do not constrain a caller sending
	// unique slot ids — and both key components are caller-supplied on a public,
	// unauthenticated route. What has to be bounded is the number of queries
	// actually in the database at once, which shares a 25-connection pool with the
	// claim path.
	//
	// Taking the slot BEFORE registering the flight also bounds the map by the
	// same number, for free: a flight exists only while it holds a slot.
	//
	// It queues rather than sheds. A request waits here, cancellable by its own
	// context; shedding would turn a cache into an availability outage, which is
	// a worse failure than a slow read.
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return Read{}, ctx.Err()
	}

	// Re-check under the lock: while waiting for a slot, another goroutine may have
	// cached this key or started a joinable load for it.
	s.mu.Lock()
	if e, ok := s.entries[k]; ok {
		if age := s.now().Sub(e.loadedAt); age < s.ttl {
			s.lru.MoveToFront(e.elem)
			v := e.value
			s.mu.Unlock()
			<-s.sem
			return Read{Value: v, Age: age}, nil
		}
		s.removeLocked(e)
	}
	if f, ok := s.inflight[k]; ok && f.gen == s.gen[slot] {
		s.mu.Unlock()
		<-s.sem
		return s.wait(ctx, f)
	}

	f := &flight{done: make(chan struct{}), gen: s.gen[slot]}
	s.inflight[k] = f // replaces any superseded flight; see load's guarded delete
	s.mu.Unlock()

	go s.load(k, f) // releases the slot when it finishes
	return s.wait(ctx, f)
}

// load runs the single loader for one key and publishes the result to waiters.
//
// It deliberately does NOT take the caller's context: a follower cancelling must
// not abort the load every other waiter is depending on. The loader's own bound
// is the store's, and the entry it produces is discarded if the slot moved on.
func (s *Service) load(k key, f *flight) {
	defer func() { <-s.sem }()
	v, err := s.loadDirect(k)

	s.mu.Lock()
	f.value, f.err = v, err
	// Only a successful answer for a slot that has not moved on may be cached.
	// Errors are never cached: a transient store failure must not be pinned for a
	// tier, and ErrNotFound is not cached either — caching it would make a newly
	// published slot answer 404 until the entry expired.
	if err == nil && s.gen[k.slot] == f.gen {
		s.insertLocked(k, v)
	}
	// Only clear the map entry if it is still THIS flight. An invalidation while
	// this load was running makes a later reader install a replacement under the
	// same key; deleting unconditionally would evict that live flight and leave
	// its waiters attached to a record nothing will ever clean up.
	if cur, ok := s.inflight[k]; ok && cur == f {
		delete(s.inflight, k)
	}
	s.mu.Unlock()
	close(f.done)
}

// loadDirect runs the source query with its own deadline.
//
// The caller's context is deliberately not used: a follower cancelling must not
// abort the load every other waiter depends on. But a detached load with no
// deadline at all can pin a flight record forever behind a blocked query, so it
// gets one of its own. This bounds how long a load may occupy a slot in the
// in-flight map — it is not an attempt to bound query time, which is the
// database's business.
func (s *Service) loadDirect(k key) (store.Availability, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.loadTimeout)
	defer cancel()
	return s.src.Availability(ctx, k.org, k.slot, k.channel)
}

func (s *Service) wait(ctx context.Context, f *flight) (Read, error) {
	select {
	case <-f.done:
		if f.err != nil {
			return Read{}, f.err
		}
		return Read{Value: f.value}, nil
	case <-ctx.Done():
		return Read{}, ctx.Err()
	}
}

func (s *Service) insertLocked(k key, v store.Availability) {
	// Per-slot ceiling first: one slot must not be able to consume the whole
	// cache through arbitrary channel values.
	for s.perSlot[k.slot] >= s.maxPerSlot {
		if !s.evictOldestLocked(func(e *entry) bool { return e.key.slot == k.slot }) {
			return
		}
	}
	for len(s.entries) >= s.maxEntries {
		if !s.evictOldestLocked(nil) {
			return
		}
	}
	e := &entry{key: k, value: v, loadedAt: s.now()}
	e.elem = s.lru.PushFront(k)
	s.entries[k] = e
	s.perSlot[k.slot]++
}

// evictOldestLocked drops the least recently used entry matching want (any, when
// want is nil), reporting whether it found one.
func (s *Service) evictOldestLocked(want func(*entry) bool) bool {
	for el := s.lru.Back(); el != nil; el = el.Prev() {
		e, ok := s.entries[el.Value.(key)]
		if !ok {
			continue
		}
		if want == nil || want(e) {
			s.removeLocked(e)
			return true
		}
	}
	return false
}

func (s *Service) removeLocked(e *entry) {
	s.lru.Remove(e.elem)
	delete(s.entries, e.key)
	if s.perSlot[e.key.slot]--; s.perSlot[e.key.slot] <= 0 {
		delete(s.perSlot, e.key.slot)
	}
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Entries:     len(s.entries),
		InFlight:    len(s.inflight),
		MaxEntries:  s.maxEntries,
		MaxPerSlot:  s.maxPerSlot,
		MaxInFlight: s.maxInFlight,
	}
}
