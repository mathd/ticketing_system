package store

import "sync"

// The public-read invalidation seam (TKT-206, ADR-045).
//
// Catalog's four minute-tier public reads are served from memory. This is how a
// write tells that cache its answer changed.
//
// It is NOT a copy of inventory's availability seam, and the difference is the
// reason this file exists rather than reusing that one. Inventory's writes are
// all transactional, so a post-commit callback on `tx.Commit()` catches every
// one of them. Catalog's are not: PublishPerformance — the single most important
// write for these reads — is one atomic UPDATE with no transaction at all. A
// commit-only hook would have looked complete and silently missed publishing,
// which is the one thing a buyer notices.
//
// So the seam has two entry points: commitPublicRead for transactional writes,
// and notifyPublicRead for autocommit ones, called after the statement that
// changed the row succeeded.

// PublicReadScope says which cached representations a write invalidates. A
// bitmask because most writes affect both, some affect only detail, and
// classifying a write as "neither" is a decision the architecture test forces
// somebody to state.
type PublicReadScope uint8

const (
	// PublicReadList covers the public event list, whose MEMBERSHIP changes when
	// any event becomes or stops being publicly listable.
	PublicReadList PublicReadScope = 1 << iota
	// PublicReadDetail covers event, season and festival detail.
	PublicReadDetail
)

// PublicReadAll is the safe answer when a write's effect is uncertain:
// over-invalidating costs a reload, under-invalidating serves a wrong answer.
const PublicReadAll = PublicReadList | PublicReadDetail

func (s PublicReadScope) Has(other PublicReadScope) bool { return s&other != 0 }

type publicReadInvalidatorFields struct {
	publicReadMu sync.RWMutex
	publicReadFn func(PublicReadScope)
}

// RegisterPublicReadInvalidator wires the cache's callback. Called once at
// construction by newPublicReadCache, which is why the cache cannot be built
// unwired.
//
// Nil-receiver safe: handler tests construct a Server with a fake store and no
// *Postgres at all, and a store that cannot write has nothing to announce.
func (p *Postgres) RegisterPublicReadInvalidator(fn func(PublicReadScope)) {
	if p == nil {
		return
	}
	p.publicReadMu.Lock()
	defer p.publicReadMu.Unlock()
	p.publicReadFn = fn
}

// notifyPublicRead announces a completed write. Call it AFTER the row change is
// visible — never before. Invalidating first lets a concurrent read repopulate
// the entry from the pre-write row, so the cache would serve the old answer for
// a full five-minute tier: the exact defect this seam exists to remove,
// reintroduced by its own mechanism. ADR-018 sets the same rule for catalog's
// event emission, for the same reason.
func (p *Postgres) notifyPublicRead(scope PublicReadScope) {
	p.publicReadMu.RLock()
	fn := p.publicReadFn
	p.publicReadMu.RUnlock()
	if fn != nil {
		fn(scope)
	}
}

// committer is what commitPublicRead needs from a transaction — one method, so
// the ordering rule can be tested without a database. The ordering is the part
// that is easy to get wrong and it is pure logic.
type committer interface{ Commit() error }

// commitPublicRead commits a transactional write and announces it only on
// success. Returns the commit error unwrapped so callers keep the behaviour they
// had when they returned tx.Commit() directly.
func (p *Postgres) commitPublicRead(tx committer, scope PublicReadScope) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	p.notifyPublicRead(scope)
	return nil
}
