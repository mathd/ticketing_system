package store

import (
	"sync"

	"github.com/google/uuid"
)

// The availability-invalidation seam (TKT-205, ADR-044).
//
// The cache that serves GET /slots/{id}/availability has to be told when an
// answer changed. Putting that notification in the HTTP handlers would have been
// the obvious design and it would have been wrong twice over: it is a list
// somebody has to remember to extend, and it would miss the NATS consumer
// entirely — publication, archival and closure all change offering status, which
// forces availability to zero, and none of them go through a handler.
//
// Both problems disappear at this layer. main.go builds ONE *Postgres and hands
// it to both the API and the consumer, so a callback here covers every writer in
// the process, and the consumer never learns a cache exists.
//
// The rule is post-commit, and it is not a detail: invalidating before the
// commit lets a concurrent read repopulate the entry from the pre-commit row, so
// the cache would serve the old number for a full tier. Catalog holds the same
// rule for its own reason (ADR-018 — state-deriving transitions emit after
// commit). TestAvailabilityMutationsUseInvalidatingCommit is what stops a new
// write path forgetting.

// committer is what commitAvailability needs from a transaction. Narrowed to one
// method so the ordering rule can be tested without a database — the ordering is
// the part that is easy to get wrong, and it is pure logic.
type committer interface{ Commit() error }

// RegisterAvailabilityInvalidator wires the cache's invalidation callback.
// Called once at construction by availability.New, which is why that package
// cannot build a cache that receives no notifications.
// A nil *Postgres is a no-op rather than a panic. Handler tests that exercise
// request validation and guard refusals construct a Server with no store at all,
// and have since long before this seam existed; a nil store has no writes to
// notify anyone about. This does not weaken the wiring guarantee where it
// matters — production always has a real store, and availability.New still has
// no way to skip registration.
func (p *Postgres) RegisterAvailabilityInvalidator(fn func(uuid.UUID)) {
	if p == nil {
		return
	}
	p.invalidateMu.Lock()
	defer p.invalidateMu.Unlock()
	p.invalidateAvailability = fn
}

// commitAvailability commits a transaction that may have changed a slot's
// availability, then — only on success — notifies the cache.
//
// Returns the commit error unwrapped so callers keep the behaviour they had when
// they returned tx.Commit() directly.
func (p *Postgres) commitAvailability(tx committer, slot uuid.UUID) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	p.invalidateMu.RLock()
	fn := p.invalidateAvailability
	p.invalidateMu.RUnlock()
	if fn != nil {
		fn(slot)
	}
	return nil
}

// invalidatorFields is embedded in Postgres. Kept here rather than in store.go so
// the whole seam reads as one unit.
type invalidatorFields struct {
	invalidateMu           sync.RWMutex
	invalidateAvailability func(uuid.UUID)
}
