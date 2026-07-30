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
type blockingResolver struct {
	entered chan struct{}
	once    sync.Once
}

func (r *blockingResolver) PoolOfferState(ctx context.Context, _ uuid.UUID) (PoolOfferState, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
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
