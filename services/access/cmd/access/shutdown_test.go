package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// races is the iteration count for the both-branches-ready cases. The defect
// TKT-98 fixes is probabilistic — Go picks uniformly among ready select cases —
// so a single iteration passes half the time by luck. 200 makes an unfixed
// select passing the whole loop a 2^-200 event, which is the difference between
// a test that observes the bug and a test that flakes around it.
const races = 200

// The state a SIGTERM actually produces: the signal context is canceled and a
// consumer's Run tail has already unwound into consumerErr with its
// context.Canceled-wrapped error, so two branches are ready at once. Before
// TKT-98 this exited non-zero on roughly half of clean shutdowns.
func TestAwaitShutdownIgnoresCancellationRace(t *testing.T) {
	for i := 0; i < races; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		consumerErr := make(chan error, 1)
		consumerErr <- fmt.Errorf("consumer stopped: %w", context.Canceled)
		var shutdownRan bool
		shutdown := func(context.Context) error { shutdownRan = true; return nil }

		if err := awaitShutdown(ctx, make(chan error, 1), consumerErr, shutdown); err != nil {
			t.Fatalf("awaitShutdown returned %q, want nil (iteration %d)", err, i)
		}
		if !shutdownRan {
			t.Fatalf("clean shutdown never reached srv.Shutdown (iteration %d)", i)
		}
	}
}

// The other half of the contract: filtering cancellation must not become
// swallowing. A consumer that died of something real still takes the process
// down, and so does a context.Canceled that did not come from our own signal.
func TestAwaitShutdownReturnsRealConsumerFailure(t *testing.T) {
	// The shape consumer.waitConsume returns when a durable disappears — the
	// failure that most needs to survive this filter.
	durableGone := errors.New("access-ticket-issuer: consume context closed (durable deleted or subscription terminated)")

	tests := []struct {
		name     string
		canceled bool
		err      error
	}{
		// Both branches ready, so this one is the reason the signal branch
		// drains consumerErr instead of shutting down on whichever won.
		{"real failure landing during shutdown", true, durableGone},
		{"real failure while running", false, durableGone},
		{"cancellation not caused by our shutdown", false, fmt.Errorf("consumer stopped: %w", context.Canceled)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < races; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.canceled {
					cancel()
				}

				consumerErr := make(chan error, 1)
				consumerErr <- tc.err
				shutdown := func(context.Context) error {
					t.Errorf("shut down cleanly on a consumer failure (iteration %d)", i)
					return nil
				}

				if err := awaitShutdown(ctx, make(chan error, 1), consumerErr, shutdown); !errors.Is(err, tc.err) {
					t.Fatalf("awaitShutdown returned %v, want %v (iteration %d)", err, tc.err, i)
				}
			}
		})
	}
}

// Pins the branch the fix does not touch, so the extraction out of run() is
// shown to be behavior-preserving rather than assumed to be.
func TestAwaitShutdownReturnsServerError(t *testing.T) {
	bindFailed := errors.New("listen tcp :8080: bind: address already in use")

	srvErr := make(chan error, 1)
	srvErr <- bindFailed
	shutdown := func(context.Context) error {
		t.Error("shut down cleanly on a server failure")
		return nil
	}

	err := awaitShutdown(context.Background(), srvErr, make(chan error, 1), shutdown)
	if !errors.Is(err, bindFailed) {
		t.Fatalf("awaitShutdown returned %v, want %v", err, bindFailed)
	}
}
