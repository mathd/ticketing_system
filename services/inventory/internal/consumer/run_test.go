package consumer

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Inventory did not observe ConsumeContext.Closed() before TKT-127: Run blocked
// on <-ctx.Done() alone, so a durable deleted underneath a live consumer left
// the process running and READY with nothing consuming — the silent-stall shape
// ADR-017 §236-241 forbids ("loudly unready, never self-heals"). Access had the
// guarantee (TKT-97) and inventory did not.
//
// This is therefore a deliberate BEHAVIOUR ADDITION, not a refactor, and it gets
// its own test rather than riding on access's. It exercises the package-local
// delegate — the same symbol Run calls — so the production path is what is
// under test.
func TestWaitConsumeAsyncTerminationLatchesInventoryUnready(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{})

	errc := make(chan error, 1)
	go func() { errc <- waitConsume(context.Background(), closed, &ready, "inventory-catalog-offering") }()

	close(closed) // durable deleted → library Stop() → Closed() fires

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected non-nil error on async termination — main must tear the process down")
		}
		if !strings.Contains(err.Error(), "inventory-catalog-offering") {
			t.Fatalf("error must name inventory's durable so an operator can tell which consumer died, got %q", err.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("waitConsume did not return after the consume context closed")
	}
	if ready.Load() {
		t.Fatal("async termination must latch ready false; a stalled consumer must not report healthy")
	}
}

// The clean-shutdown half: inventory's Run tail must keep returning its
// context-cancellation error unchanged, so adopting the termination arm does
// not alter ordinary shutdown (and does not disturb TKT-121, which is about
// main's arbitration of that error and stays open).
func TestWaitConsumeCleanShutdownLeavesInventoryReadinessAlone(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	closed := make(chan struct{}) // never closed
	ctx, cancel := context.WithCancel(context.Background())

	errc := make(chan error, 1)
	go func() { errc <- waitConsume(ctx, closed, &ready, "inventory-catalog-offering") }()

	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("clean shutdown must return nil, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitConsume did not return after ctx cancellation")
	}
	if !ready.Load() {
		t.Fatal("clean shutdown must not touch readiness here; Run's own defer owns it")
	}
}
