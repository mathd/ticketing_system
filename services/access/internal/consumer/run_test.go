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
	go func() { errc <- waitConsume(context.Background(), closed, &ready, "test-consumer") }()

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
	go func() { errc <- waitConsume(ctx, closed, &ready, "test-consumer") }()

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

	err := waitConsume(context.Background(), closed, &ready, "access-slot-policy")
	if err == nil {
		t.Fatal("expected non-nil error on termination")
	}
	if !strings.Contains(err.Error(), "access-slot-policy") {
		t.Fatalf("error should name the consumer, got %q", err.Error())
	}
}

// TestWaitConsumeReturnsPromptlyWhenBothSignalsReady guards against a future
// refactor that orders the select arms into a deadlock: with both ctx cancelled
// and closed already closed, either arm is a defensible outcome, but the helper
// must return in bounded time and never block.
func TestWaitConsumeReturnsPromptlyWhenBothSignalsReady(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})
	close(closed)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_ = waitConsume(ctx, closed, &ready, "test-consumer")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitConsume blocked when both signals were ready")
	}
}
