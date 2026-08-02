package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"ticketing/services/inventory/internal/store"
)

// Fakes for the JetStream chain Run walks. Each embeds its interface so only the
// methods Run actually calls need bodies — anything else panics loudly rather than
// silently returning a zero value, which is what we want from a fake standing in
// for a large third-party interface.

type fakeJS struct {
	jetstream.JetStream
	stream jetstream.Stream
}

func (f fakeJS) Stream(context.Context, string) (jetstream.Stream, error) { return f.stream, nil }

type fakeStream struct {
	jetstream.Stream
	cons jetstream.Consumer
}

func (f fakeStream) DeleteConsumer(context.Context, string) error { return nil }
func (f fakeStream) CreateOrUpdateConsumer(context.Context, jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return f.cons, nil
}

type fakeConsumer struct {
	jetstream.Consumer
	cc *fakeConsumeContext
}

func (f fakeConsumer) Consume(jetstream.MessageHandler, ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	return f.cc, nil
}

type fakeConsumeContext struct {
	closed  chan struct{}
	stopped atomic.Bool
}

func (f *fakeConsumeContext) Stop()                   { f.stopped.Store(true) }
func (f *fakeConsumeContext) Drain()                  {}
func (f *fakeConsumeContext) Closed() <-chan struct{} { return f.closed }

// blockingResolver signals that reconciliation has started, then blocks until its
// context is cancelled. It is what makes the startup window observable: without a
// resolver that actually waits, startupConverge finishes before the test can close
// the consume context and the race under test never happens.
// SeatMapAdjacency is never exercised here: this double blocks the offer-state path,
// and adjacency is fetched only at seated provisioning (ADR-041).
func (r *blockingResolver) SeatMapAdjacency(context.Context, uuid.UUID) ([]SeatAdjacency, error) {
	return nil, nil
}

type blockingResolver struct {
	entered chan struct{}
	once    sync.Once
	// cancelled/release let a test hold the pass INSIDE its unwind, after its
	// context died but before it returns. That window is the only place a
	// simultaneous parent cancellation can be injected deterministically; a sleep
	// after closing the durable lands far too late, because the pass has already
	// returned by then.
	cancelled     chan struct{}
	release       chan struct{}
	cancelledOnce sync.Once
}

func (r *blockingResolver) PoolOfferState(ctx context.Context, _ uuid.UUID) (PoolOfferState, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	if r.cancelled != nil {
		// Once: reconcile calls this per pool per attempt, and a second close
		// would panic — turning a test-fixture detail into a failure that looks
		// like a product bug.
		r.cancelledOnce.Do(func() { close(r.cancelled) })
		<-r.release
	}
	return PoolOfferState{}, ctx.Err()
}

func (r *blockingResolver) PublishedPerformance(context.Context, uuid.UUID) (PublishedPerformance, error) {
	return PublishedPerformance{}, nil
}

// TKT-122 half B. Run began observing ConsumeContext.Closed() only AFTER
// startupConverge returned, and that pass retries reconcileAttempts times with
// retryBackoff between them plus a serial catalog call per published pool. A
// durable deleted inside that window went unnoticed until the pass ended.
//
// Not a false-ready lie — refreshStartupReadiness stores true as its last act, so
// the service is honestly unready throughout — but a detection-latency gap: the
// process stays alive, unready, consuming nothing, and compose does not restart an
// unhealthy container (ADR-017 §236-241), so the late process exit is the signal
// that matters.
//
// This enters through Consumer.Run deliberately. TKT-127's tests call waitConsume
// directly and by construction cannot express this: the defect is not in
// waitConsume but in WHEN Run starts calling it.
func TestRunObservesTerminationDuringStartupConverge(t *testing.T) {
	cc := &fakeConsumeContext{closed: make(chan struct{})}
	resolver := &blockingResolver{entered: make(chan struct{})}
	st := &fakeCatalogStore{pools: []store.PoolOffering{{SlotID: uuid.New(), ClosureStatus: "open"}}}

	c := &Consumer{
		js:           fakeJS{stream: fakeStream{cons: fakeConsumer{cc: cc}}},
		st:           st,
		resolver:     resolver,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff: time.Hour, // a retry must never be what unblocks this test
	}

	errc := make(chan error, 1)
	go func() { errc <- c.Run(context.Background()) }()

	// Wait until reconciliation is genuinely in flight, then kill the durable.
	select {
	case <-resolver.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciliation never started; the test is not exercising the startup window")
	}
	close(cc.closed) // durable deleted underneath a running startupConverge

	var err error
	select {
	case err = <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after the consume context closed during startupConverge")
	}

	if err == nil {
		t.Fatal("termination during startup must return an error so main tears the process down")
	}
	if !strings.Contains(err.Error(), "inventory-catalog-offering") {
		t.Fatalf("error must carry the durable diagnostic so an operator knows which consumer died, got %q", err)
	}
	// The discriminator main relies on: a termination is NOT a shutdown
	// cancellation, or isShutdownConsumerError would filter it and the process
	// would exit 0 with nothing consuming.
	if errors.Is(err, context.Canceled) {
		t.Fatalf("termination error must not wrap context.Canceled; main would filter it as a clean shutdown: %v", err)
	}

	if !cc.stopped.Load() {
		t.Fatal("Run returned without stopping the consume context; the observer goroutine would leak")
	}
}

// Sub-item 3, and it needs its own test: the cancellation path above returns from
// startupConverge's retry select and never reaches refreshStartupReadiness, so it
// cannot prove anything about the true latch.
//
// This is the case where reconciliation finishes (or gives up) and the readiness
// pass then runs with a context already cancelled by termination. Storing true
// there would put the service back to READY with nothing consuming — the exact
// silent stall ADR-017 §236-241 forbids, and one that self-heals a latch
// durableconsumer.Wait had just set false.
func TestRefreshStartupReadinessNeverStoresTrueAfterCancellation(t *testing.T) {
	c := &Consumer{
		st:  &fakeCatalogStore{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // termination cancelled the startup context

	if err := c.refreshStartupReadiness(ctx); err == nil {
		t.Fatal("a cancelled readiness pass must report why it did not complete")
	}
	if c.Ready() {
		t.Fatal("readiness stored true after its context was cancelled; a terminated consumer would report healthy")
	}
}

// ai-review [high]. The ctx.Err() guard alone was a TOCTOU: durableconsumer.Wait
// latches ready false through a plain atomic WITHOUT readinessMu, so a startup
// pass could check "not cancelled", have Wait latch false underneath it, and then
// store true over that latch. /readyz would report healthy for a terminated
// consumer until Run's deferred store ran.
//
// Serializing the re-assert under the mutex would only SHRINK that window to a
// mutex handoff. The terminal flag closes it: termination never self-heals
// (ADR-017 §240-241), so a readiness latch that outlives the flag can never be
// correct, and Ready() consults both.
//
// Deterministic on purpose — it asserts the invariant, not an interleaving, so it
// cannot flake and cannot pass by winning a race.
func TestReadyIsFalseOnceTerminatedRegardlessOfTheLatch(t *testing.T) {
	c := &Consumer{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	c.ready.Store(true)
	if !c.Ready() {
		t.Fatal("a consumer that has not terminated must report its latch")
	}

	// Exactly the state the race produces: the latch says true, termination has
	// been observed. The latch must not win.
	c.terminated.Store(true)
	if c.Ready() {
		t.Fatal("readiness reported true after termination; /readyz would call a dead consumer healthy")
	}

	// And the startup pass must refuse to publish readiness at all in that state,
	// so the flag is not the only thing standing between a terminated consumer and
	// a true latch.
	c.ready.Store(false)
	c.st = &fakeCatalogStore{}
	if err := c.refreshStartupReadiness(context.Background()); err == nil {
		t.Fatal("startup readiness must refuse to complete after termination")
	}
	if c.ready.Load() {
		t.Fatal("startup readiness stored true after termination was observed")
	}
}

// ai-review [medium]. A durable that dies and a SIGTERM that lands a moment later
// both leave the parent context cancelled. Gating the observer's verdict on
// ctx.Err()==nil made Run return startupConverge's context.Canceled in that case —
// which both mains classify as a clean stop and neither logs, so a real
// termination vanished entirely, exit 0 and silent.
func TestTerminationWinsOverASimultaneousParentCancellation(t *testing.T) {
	cc := &fakeConsumeContext{closed: make(chan struct{})}
	resolver := &blockingResolver{
		entered:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
	st := &fakeCatalogStore{pools: []store.PoolOffering{{SlotID: uuid.New(), ClosureStatus: "open"}}}

	c := &Consumer{
		js:           fakeJS{stream: fakeStream{cons: fakeConsumer{cc: cc}}},
		st:           st,
		resolver:     resolver,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff: time.Hour,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx) }()

	select {
	case <-resolver.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciliation never started")
	}
	close(cc.closed) // the durable dies...

	// ...and the operator stops the service while the pass is still unwinding, so
	// that by the time Run picks an error BOTH contexts are cancelled. Injected
	// here rather than after a sleep: the pass returns the instant its context
	// dies, so anything later misses the window entirely and the test passes
	// against the defect (confirmed by mutation).
	select {
	case <-resolver.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the pass never observed its cancellation")
	}
	cancel()
	close(resolver.release)

	var err error
	select {
	case err = <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	if err == nil || !strings.Contains(err.Error(), "inventory-catalog-offering") {
		t.Fatalf("the durable diagnostic must survive a simultaneous shutdown, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("a termination reported as a cancellation is filtered by main and exits 0: %v", err)
	}
}
