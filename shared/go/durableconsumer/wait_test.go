package durableconsumer

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitAsyncTerminationLatchesUnreadyAndErrors is the shared half of the
// TKT-97 guarantee: when the consume context closes underneath a running
// consumer, Wait latches ready false AND returns a non-nil error.
func TestWaitAsyncTerminationLatchesUnreadyAndErrors(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})

	errc := make(chan error, 1)
	go func() { errc <- Wait(context.Background(), closed, &ready, "test-consumer") }()

	close(closed)

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected non-nil error on async termination, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after the consume context closed")
	}
	if ready.Load() {
		t.Fatal("expected ready to be latched false after async termination")
	}
}

// TestWaitCleanShutdownStaysLatchedAndReturnsNil pins the no-spurious-latch /
// no-self-heal half: a plain ctx cancellation is a clean shutdown, so Wait
// returns nil and does NOT touch readiness — the caller's own deferred
// Store(false) owns clean-shutdown latching.
func TestWaitCleanShutdownStaysLatchedAndReturnsNil(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{}) // never closed
	ctx, cancel := context.WithCancel(context.Background())

	errc := make(chan error, 1)
	go func() { errc <- Wait(ctx, closed, &ready, "test-consumer") }()

	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("expected nil error on clean shutdown, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after ctx cancellation")
	}
	if !ready.Load() {
		t.Fatal("Wait must not touch readiness on clean shutdown; the caller's defer owns it")
	}
}

// TestWaitTerminationWinsOverLiveContext pins that the termination arm is taken
// deterministically when only `closed` is ready and the context is still live —
// the real running-consumer scenario. A test that closes both channels cannot
// assert an arm (Go picks a ready case at random), so it would only prove "no
// deadlock"; this asserts the outcome instead.
func TestWaitTerminationWinsOverLiveContext(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})
	close(closed)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // ctx stays live; only `closed` is ready

	err := Wait(ctx, closed, &ready, "some-durable")
	if err == nil {
		t.Fatal("expected the termination arm to win when only closed is ready")
	}
	if ready.Load() {
		t.Fatal("termination arm must latch ready false")
	}
}

// TestWaitTerminationDiagnosticIsExact is the wire contract for the operator
// diagnostic, pinned as a HAND-WRITTEN literal.
//
// It is deliberately NOT built from Wait's own format string: a fixture derived
// from the code under test encodes the property it claims to prove and cannot
// fail (ADR-017's trap). The literal below is transcribed from what the code
// emitted before this package existed.
//
// Why it earns its place: smoke/access_consumer_test.go (TKT-99) asserts
//
//	access: access-slot-policy: consume context closed (durable deleted or subscription terminated)
//
// verbatim against a real broker, and its own comment calls that literal "the
// whole discriminator". That contract was previously protected only by a
// docker-based smoke run — the one moment it can silently drift is a move like
// this one. Here it fails in milliseconds instead of after a container restart.
func TestWaitTerminationDiagnosticIsExact(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})
	close(closed)

	err := Wait(context.Background(), closed, &ready, "access-slot-policy")
	if err == nil {
		t.Fatal("expected non-nil error on termination")
	}
	const want = "access-slot-policy: consume context closed (durable deleted or subscription terminated)"
	if err.Error() != want {
		t.Fatalf("diagnostic drifted.\n got: %q\nwant: %q\n\nsmoke/access_consumer_test.go asserts this string verbatim against a real broker (TKT-99); changing it there and here together is a deliberate contract change, not a test update.", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "access-slot-policy") {
		t.Fatal("the error must name the consumer so an operator can tell which one terminated")
	}
}

// TestWaitPrefersShutdownWhenBothAreReady pins the arbitration TKT-122's ai-review
// found under-specified — and then found had to point the OTHER way.
//
// Pass 2 was right that a random pick is wrong. Pass 3 showed which way it must be
// deterministic: after cancellation a closed subscription is ambiguous, because we
// close it ourselves. Both mains `defer nc.Close()` without joining their consumer
// goroutines, so an ordinary stop routinely closes the subscription under a
// goroutine that has not yet arbitrated — and preferring termination would emit a
// durable-deletion diagnostic on clean shutdowns, corrupting the operator evidence
// the producer-side logging exists to preserve.
//
// So shutdown wins. A durable that truly dies in this window is not distinguished,
// which is the same accepted residual as the drain snapshot: once SIGTERM lands,
// this process stops classifying late consumer events.
//
// Deterministic by construction: both channels are ready BEFORE Wait is called, so
// there is no interleaving to lose. Looped because a random select would pass a
// single run about half the time — which is exactly how the original behaviour
// escaped notice.
func TestWaitPrefersShutdownWhenBothAreReady(t *testing.T) {
	for i := 0; i < 64; i++ {
		var ready atomic.Bool
		ready.Store(true)
		closed := make(chan struct{})
		close(closed)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := Wait(ctx, closed, &ready, "test-consumer"); err != nil {
			t.Fatalf("iteration %d: an ordinary stop must not be reported as a durable termination: %v", i, err)
		}
		if !ready.Load() {
			t.Fatalf("iteration %d: a clean shutdown must leave readiness to the caller's own latch", i)
		}
	}
}

// The case that actually matters is unchanged: a durable deleted under a LIVE
// consumer, where nothing is ambiguous.
func TestWaitStillReportsTerminationWhenNotShuttingDown(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})
	close(closed)

	err := Wait(context.Background(), closed, &ready, "test-consumer")
	if err == nil || !strings.Contains(err.Error(), "consume context closed") {
		t.Fatalf("a durable deleted under a live consumer must still terminate the process, got %v", err)
	}
	if ready.Load() {
		t.Fatal("termination must latch ready false")
	}
}
