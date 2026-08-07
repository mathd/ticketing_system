package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// A controllable clock. Every test below uses it; none of them sleep.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// The burst is spendable, and the very next call is refused. Both halves matter:
// a limiter that refuses at burst-1 would pass "it refuses" while rejecting a
// caller who did nothing wrong.
func TestTheBurstIsSpendableAndTheNextCallIsRefused(t *testing.T) {
	c := newClock()
	l := New(5, time.Minute, 100, c.now)

	for i := range 5 {
		if !l.Allow("k") {
			t.Fatalf("call %d refused inside the burst of 5", i+1)
		}
	}
	if l.Allow("k") {
		t.Fatal("the 6th call was allowed; the limiter does not limit")
	}
}

// Refilling is gradual, not a window that resets. A fixed window would allow the
// full burst again the instant the window rolls, which is a 2x burst across the
// boundary — this pins that one fifth of the period buys exactly one token.
func TestTokensRefillGraduallyRatherThanResettingAtAWindowEdge(t *testing.T) {
	c := newClock()
	l := New(5, time.Minute, 100, c.now)
	for range 5 {
		l.Allow("k")
	}

	c.advance(12 * time.Second) // one fifth of a minute == one token
	if !l.Allow("k") {
		t.Fatal("one token's worth of time did not buy one call")
	}
	if l.Allow("k") {
		t.Fatal("one token's worth of time bought two calls")
	}

	c.advance(time.Hour) // far past a full refill
	for i := range 5 {
		if !l.Allow("k") {
			t.Fatalf("call %d refused after a long idle; the bucket did not refill to burst", i+1)
		}
	}
	if l.Allow("k") {
		t.Fatal("the bucket refilled beyond its burst")
	}
}

// Keys are independent. Without this, one busy caller would refuse everyone —
// which is a denial-of-service primitive rather than a limiter.
func TestBudgetsArePerKey(t *testing.T) {
	c := newClock()
	l := New(2, time.Minute, 100, c.now)

	l.Allow("a")
	l.Allow("a")
	if l.Allow("a") {
		t.Fatal("key a was not limited")
	}
	if !l.Allow("b") {
		t.Fatal("key b was refused because key a spent its own budget")
	}
}

// A clock that steps backwards must not drain a bucket. Computed elapsed time is
// negative across an NTP correction, and `tokens += elapsed*rate` would then
// SUBTRACT — refusing a caller who did nothing.
func TestABackwardsClockDoesNotDrainTheBucket(t *testing.T) {
	c := newClock()
	l := New(3, time.Minute, 100, c.now)

	if !l.Allow("k") {
		t.Fatal("first call refused")
	}
	c.advance(-30 * time.Second)
	if !l.Allow("k") {
		t.Fatal("a backwards clock step refused a caller with budget left")
	}
	if !l.Allow("k") {
		t.Fatal("a backwards clock step cost more than one token")
	}
}

// Idle buckets are reclaimed, so ordinary traffic churn does not walk the map up
// to the cap and start refusing new keys.
func TestIdleBucketsAreReclaimed(t *testing.T) {
	c := newClock()
	l := New(2, time.Minute, 100, c.now)

	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	if got := l.Tracked(); got != 3 {
		t.Fatalf("tracked %d keys, want 3", got)
	}

	c.advance(time.Minute) // every bucket has now refilled to full
	l.Allow("d")           // a miss, which is what triggers the sweep
	if got := l.Tracked(); got != 1 {
		t.Fatalf("tracked %d keys after the sweep, want 1 (only the new key)", got)
	}
}

// At the cap, an ALREADY-TRACKED key keeps working and only a new key is refused.
//
// The direction is the whole point. If reaching the cap evicted a live bucket, an
// attacker could rotate keys to flush the bucket holding them back — the limiter
// would become bypassable exactly when it was needed. This test fails if anyone
// "improves" it into an LRU.
func TestAtTheCapTrackedKeysStillWorkAndOnlyNewKeysAreRefused(t *testing.T) {
	c := newClock()
	l := New(5, time.Minute, 2, c.now)

	if !l.Allow("first") {
		t.Fatal("first key refused")
	}
	if !l.Allow("second") {
		t.Fatal("second key refused below the cap")
	}
	if l.Allow("third") {
		t.Fatal("a third key was tracked past a cap of 2")
	}
	// The bypass this guards: `first` must still be limited, not flushed.
	for range 4 {
		l.Allow("first")
	}
	if l.Allow("first") {
		t.Fatal("the tracked key's budget was reset by key-rotation pressure — this is the bypass")
	}
}

// Concurrent callers must not lose or double-spend tokens. Run with -race.
func TestConcurrentCallersSpendExactlyTheBurst(t *testing.T) {
	c := newClock()
	const burst = 50
	l := New(burst, time.Hour, 100, c.now)

	var wg sync.WaitGroup
	allowed := make(chan struct{}, 500)
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("k") {
				allowed <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(allowed)

	if got := len(allowed); got != burst {
		t.Fatalf("%d calls allowed under concurrency, want exactly %d", got, burst)
	}
}
