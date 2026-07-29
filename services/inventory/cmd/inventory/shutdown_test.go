package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// races is the iteration count for the both-branches-ready cases. The defect
// TKT-121 fixes is probabilistic — Go picks uniformly among ready select cases —
// so a single iteration passes half the time by luck. 200 makes an unfixed
// select passing the whole loop a 2^-200 event, which is the difference between
// a test that observes the bug and a test that flakes around it.
const races = 200

// terminationDiagnostic is the error durableconsumer.Wait returns when the
// consume context closes underneath a running consumer — inventory's name
// substituted into the contract string pinned by
// TestWaitTerminationDiagnosticIsExact. Written out by hand rather than built
// from Wait: this is the failure that most needs to survive awaitShutdown's
// filter, and a fixture derived from the code under test cannot fail (ADR-017's
// trap). It does NOT wrap context.Canceled — that is precisely why the narrow
// errors.Is predicate is safe.
const terminationDiagnostic = "inventory-catalog-offering: consume context closed (durable deleted or subscription terminated)"

// The state a SIGTERM actually produces: the signal context is canceled and the
// consumer's Run tail has already unwound into consumerErr with its
// context.Canceled-wrapped error, so two branches are ready at once. Before
// TKT-121 this exited non-zero on roughly half of clean shutdowns.
//
// Only consumerErr is pre-filled. srvErr is passed empty deliberately: "both
// channels pre-filled" in the COS means both *ready select branches* — a queued
// consumer error and a canceled context. Pre-fill srvErr too and its branch
// becomes ready as well, the test starts asserting the server-error path, and it
// passes against the unfixed code while no longer covering the race it exists
// for (TKT-121 plan-review PR-1).
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
// down (ADR-017 §236-241 — async termination goes loudly unready and exits), and
// so does a context.Canceled that did not come from our own signal.
func TestAwaitShutdownReturnsRealConsumerFailure(t *testing.T) {
	durableGone := errors.New(terminationDiagnostic)

	tests := []struct {
		name     string
		canceled bool
		err      error
	}{
		// Both branches ready. This is the case that pins the non-blocking
		// receive in the signal branch: without it, ctx.Done() wins the flip
		// about half the time and the process exits 0 on a deleted durable.
		//
		// It pins the ALREADY-ARRIVED error only — the value is queued before
		// awaitShutdown is called, so the inner receive is guaranteed to see
		// it. The late-arrival window (consumerErr empty at the receive, the
		// failure published while srv.Shutdown is still running) is NOT
		// covered here and is not closed by this ticket: it is the documented
		// snapshot residual, identical in access, and it belongs to TKT-122,
		// which must pick one shape for both services. Closing it needs a
		// delayed-send test this table cannot express by construction.
		// Spelled out because TKT-121's ai-review read this case as claiming
		// the wider guarantee.
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
// shown to be behavior-preserving rather than assumed to be. Observed GREEN at
// the extraction commit, in the same run that observed the cancellation-race
// test RED (TKT-121 plan-review PR-2).
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

// Inventory has ONE consumerErr producer (main.go, a single `go func()`), so
// access's TestAwaitShutdownPrefersRealFailureOverCancellation has no analogue
// here: two queued errors on a buffered-1 channel is a state that cannot occur,
// and a test of an unreachable state cannot fail for the right reason (TKT-121
// plan-review D3). The "real failure landing during shutdown" case above is the
// one-producer equivalent. If inventory ever gains a second producer, that test
// and access's multi-value drain loop come back together.
