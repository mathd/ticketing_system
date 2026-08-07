// Package ratelimit bounds how often one key may do something.
//
// It exists for the public, credential-free surfaces two services expose:
// commerce's customer account operations (TKT-224, ADR-049 §2/§3) and catalog's
// back-office login (TKT-195, ADR-042). Those live in different processes, so
// they cannot share a deployment — the mechanism is shared here instead, and each
// service wires it to its own routes with its own thresholds.
//
// # The adversary, named (ADR-021)
//
// A Limiter is IN-PROCESS. It bounds a single scripted client against one replica.
// It does NOT bound:
//
//   - a distributed caller — every source gets its own budget by definition;
//   - a caller who waits out the window — this slows enumeration, it does not
//     close it;
//   - anything at all after a restart, which empties every bucket.
//
// Say that sentence before writing "rate limited" in a doc or an acceptance
// criterion. The counters are deliberately not in Postgres: every REFUSED request
// would then cost a write, which hands an attacker write amplification against the
// money-path database and makes the limiter the vector it was added to remove.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a keyed token bucket. The zero value is not usable; call New.
type Limiter struct {
	refillPerSec float64
	burst        float64
	idle         time.Duration
	maxKeys      int
	now          func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter allowing `burst` requests immediately and refilling to that
// ceiling over `per`.
//
// maxKeys caps how many distinct keys are tracked at once, and it is not
// optional. The map is fed by unauthenticated input, so without a cap the limiter
// is itself the memory-exhaustion vector it was added to prevent — the same trap
// the storefront's session map had to grow a bound for (ADR-049 §4). See Allow for
// what happens at the cap.
//
// now is the clock seam. Tests inject one; every caller in production passes nil
// for time.Now. Without it, every expiry test is a sleep and the suite gets slower
// and flakier for no coverage.
func New(burst int, per time.Duration, maxKeys int, now func() time.Time) *Limiter {
	if burst < 1 {
		panic("ratelimit: burst must be at least 1")
	}
	if per <= 0 {
		panic("ratelimit: refill period must be positive")
	}
	if maxKeys < 1 {
		panic("ratelimit: maxKeys must be at least 1")
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		refillPerSec: float64(burst) / per.Seconds(),
		burst:        float64(burst),
		// A bucket that has refilled to full is indistinguishable from one that
		// does not exist, so it may be dropped without losing anything. `per` is
		// exactly how long refilling takes.
		idle:    per,
		maxKeys: maxKeys,
		now:     now,
		buckets: make(map[string]*bucket),
	}
}

// Allow reports whether key may proceed, and spends a token if so.
//
// At the key cap, a key that is ALREADY tracked is still served normally — only a
// key never seen before is refused. That direction is deliberate. Evicting a live
// bucket to make room would let an attacker rotate keys to flush the bucket that
// is holding them back, which is a bypass; refusing new keys instead degrades
// service while under a key-rotation flood but never opens the door. Reaching the
// cap is a symptom to escalate, not a state to tune around.
func (l *Limiter) Allow(key string) bool {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, seen := l.buckets[key]
	if !seen {
		l.sweep(now)
		if len(l.buckets) >= l.maxKeys {
			return false
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Refill for elapsed time, capped at burst. A clock that goes backwards yields
	// a negative elapsed and would DRAIN the bucket, refusing a legitimate caller
	// on a leap-second correction or an NTP step; clamp at zero instead.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.refillPerSec)
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have sat idle long enough to have refilled completely.
// Called only on the miss path, so the common case takes no extra work.
func (l *Limiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.last) >= l.idle {
			delete(l.buckets, key)
		}
	}
}

// Tracked reports how many keys are held. For tests and for a gauge; not part of
// the enforcement path.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
