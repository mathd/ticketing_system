// Package seatoccupancy serves the public seat-occupancy read from memory.
//
// Extends the in-process, single-flight, generation-fenced cache pattern from
// availability to seat occupancy (TKT-177, ADR-044).
package seatoccupancy

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/cachetier"
)

const (
	defaultMaxEntries  = 10000
	defaultMaxInFlight = 1000
	defaultLoadTimeout = 10 * time.Second
)

// Source is what the cache reads through. RegisterAvailabilityInvalidator is on
// this interface, not a separate wiring step, so New cannot produce a cache that
// receives no write notifications — a cache wired to nothing is worse than no
// cache, because it looks like it works.
//
// Reuses the availability-named registration method on the store: the post-commit
// seam fans out to all registered invalidators (TKT-177).
type Source interface {
	SeatOccupancy(ctx context.Context, org, slot uuid.UUID) (store.SeatOccupancy, error)
	RegisterAvailabilityInvalidator(func(uuid.UUID))
}

type Read struct {
	Value store.SeatOccupancy
	Age   time.Duration
}

type Status struct {
	Enabled     bool `json:"enabled"`
	Entries     int  `json:"entries"`
	InFlight    int  `json:"in_flight"`
	MaxEntries  int  `json:"max_entries"`
	MaxInFlight int  `json:"max_in_flight"`
}

type key struct {
	org, slot uuid.UUID
}

type entry struct {
	key      key
	value    store.SeatOccupancy
	loadedAt time.Time
	elem     *list.Element
}

type flight struct {
	done      chan struct{}
	gen       uint64
	switchGen uint64
	value     store.SeatOccupancy
	err       error
}

type Service struct {
	src         Source
	now         func() time.Time
	ttl         time.Duration
	maxEntries  int
	maxInFlight int
	loadTimeout time.Duration

	sem chan struct{}

	mu        sync.Mutex
	enabled   bool
	switchGen uint64
	entries   map[key]*entry
	lru       *list.List
	gen       map[uuid.UUID]uint64
	inflight  map[key]*flight
}

type Option func(*Service)

func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }
func WithMaxEntries(n int) Option           { return func(s *Service) { s.maxEntries = n } }
func WithMaxInFlight(n int) Option          { return func(s *Service) { s.maxInFlight = n } }
func WithLoadTimeout(d time.Duration) Option { return func(s *Service) { s.loadTimeout = d } }

func New(src Source, opts ...Option) *Service {
	s := &Service{
		src:         src,
		now:         time.Now,
		ttl:         cachetier.Seconds.Duration(),
		maxEntries:  defaultMaxEntries,
		maxInFlight: defaultMaxInFlight,
		loadTimeout: defaultLoadTimeout,
		enabled:     true,
		entries:     map[key]*entry{},
		lru:         list.New(),
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

func (s *Service) SetEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled == enabled {
		return
	}
	s.enabled = enabled
	s.switchGen++
	for _, e := range s.entries {
		s.removeLocked(e)
	}
}

func (s *Service) Read(ctx context.Context, org, slot uuid.UUID) (Read, error) {
	k := key{org: org, slot: slot}

	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return s.bypass(ctx, k)
	}
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
	if f, ok := s.inflight[k]; ok && f.gen == gen && f.switchGen == s.switchGen {
		s.mu.Unlock()
		return s.wait(ctx, f)
	}
	s.mu.Unlock()

	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return Read{}, ctx.Err()
	}

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
	if f, ok := s.inflight[k]; ok && f.gen == s.gen[slot] && f.switchGen == s.switchGen {
		s.mu.Unlock()
		<-s.sem
		return s.wait(ctx, f)
	}

	f := &flight{done: make(chan struct{}), gen: s.gen[slot], switchGen: s.switchGen}
	s.inflight[k] = f
	s.mu.Unlock()

	go s.load(k, f)
	return s.wait(ctx, f)
}

func (s *Service) load(k key, f *flight) {
	defer func() { <-s.sem }()
	v, err := s.loadDirect(k)

	s.mu.Lock()
	f.value, f.err = v, err
	if err == nil && s.enabled && s.switchGen == f.switchGen && s.gen[k.slot] == f.gen {
		s.insertLocked(k, v)
	}
	if cur, ok := s.inflight[k]; ok && cur == f {
		delete(s.inflight, k)
	}
	s.mu.Unlock()
	close(f.done)
}

func (s *Service) loadDirect(k key) (store.SeatOccupancy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.loadTimeout)
	defer cancel()
	return s.src.SeatOccupancy(ctx, k.org, k.slot)
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

func (s *Service) insertLocked(k key, v store.SeatOccupancy) {
	for len(s.entries) >= s.maxEntries {
		if !s.evictOldestLocked() {
			return
		}
	}
	e := &entry{key: k, value: v, loadedAt: s.now()}
	e.elem = s.lru.PushFront(k)
	s.entries[k] = e
}

func (s *Service) evictOldestLocked() bool {
	for el := s.lru.Back(); el != nil; el = el.Prev() {
		e, ok := s.entries[el.Value.(key)]
		if !ok {
			continue
		}
		s.removeLocked(e)
		return true
	}
	return false
}

func (s *Service) removeLocked(e *entry) {
	s.lru.Remove(e.elem)
	delete(s.entries, e.key)
}

func (s *Service) bypass(ctx context.Context, k key) (Read, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return Read{}, ctx.Err()
	}
	defer func() { <-s.sem }()
	v, err := s.loadDirect(k)
	if err != nil {
		return Read{}, err
	}
	return Read{Value: v}, nil
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Enabled:     s.enabled,
		Entries:     len(s.entries),
		InFlight:    len(s.inflight),
		MaxEntries:  s.maxEntries,
		MaxInFlight: s.maxInFlight,
	}
}
