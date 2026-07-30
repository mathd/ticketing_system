package consumer

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitConsumeAsyncTerminationLatchesUnreadyAndErrors is the COS #1 proof:
// when the consume context closes underneath a running consumer (durable
// deleted / subscription dropped), waitConsume latches ready false and returns
// a non-nil error, so /readyz fails and main tears the process down instead of
// the consumer stalling silently while ready stays true.
func TestWaitConsumeAsyncTerminationLatchesUnreadyAndErrors(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})

	errc := make(chan error, 1)
	go func() { errc <- waitConsume(context.Background(), closed, &ready, "test-consumer", nil) }()

	close(closed) // durable deleted → library Stop() → Closed() fires

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected non-nil error on async termination, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("waitConsume did not return after the consume context closed")
	}
	if ready.Load() {
		t.Fatal("expected ready to be latched false after async termination")
	}
}

// TestWaitConsumeCleanShutdownStaysLatchedAndReturnsNil pins the no-spurious-latch
// / no-self-heal guarantee: a plain ctx cancellation is a clean shutdown, so
// waitConsume returns nil and does NOT touch readiness — Run's own deferred
// c.ready.Store(false) owns clean-shutdown latching. If the helper flipped
// readiness here it would flap on every ordinary restart.
func TestWaitConsumeCleanShutdownStaysLatchedAndReturnsNil(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{}) // never closed
	ctx, cancel := context.WithCancel(context.Background())

	errc := make(chan error, 1)
	go func() { errc <- waitConsume(ctx, closed, &ready, "test-consumer", nil) }()

	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("expected nil error on clean shutdown, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitConsume did not return after ctx cancellation")
	}
	if !ready.Load() {
		t.Fatal("waitConsume must not touch readiness on clean shutdown; Run's defer owns it")
	}
}

// TestWaitConsumeTerminationErrorNamesConsumer locks the contract that the two
// call sites (access-ticket-issuer, access-slot-policy) produce distinguishable
// errors, so an operator can tell which consumer terminated from the log.
func TestWaitConsumeTerminationErrorNamesConsumer(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})
	close(closed)

	err := waitConsume(context.Background(), closed, &ready, "access-slot-policy", nil)
	if err == nil {
		t.Fatal("expected non-nil error on termination")
	}
	if !strings.Contains(err.Error(), "access-slot-policy") {
		t.Fatalf("error should name the consumer, got %q", err.Error())
	}
}

// TestWaitConsumeTerminationWinsOverLiveContext pins that the termination arm is
// taken deterministically when only `closed` is ready and the context is still
// live — the real running-consumer scenario, where ctx.Done() is NOT ready. This
// is the deterministic counterpart to the async-termination test: it proves the
// select does not require ctx to be cancelled to observe termination. (A test
// that closes both channels cannot assert an arm — Go picks a ready case at
// random — so it would only prove "no deadlock", which the tests above already
// prove; this one asserts the outcome instead.)
func TestWaitConsumeTerminationWinsOverLiveContext(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})
	close(closed)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // ctx stays live; only `closed` is ready

	err := waitConsume(ctx, closed, &ready, "access-ticket-issuer", nil)
	if err == nil {
		t.Fatal("expected the termination arm to win when only closed is ready")
	}
	if ready.Load() {
		t.Fatal("termination arm must latch ready false")
	}
}
