package outbox

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// callLog is an OutboxDB that records claim attempts and fails them; the drainer
// under test needs no real database to prove when the backfill runs.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) record(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, what)
}

func (c *callLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *callLog) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	c.record("claim")
	return nil, errors.New("no db in this test")
}

func (c *callLog) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("no db in this test")
}

// The backfill must run exactly once per process, inside the drainer's lifecycle,
// and before the first drain pass — so its rows are published by the immediate
// initial drain instead of waiting a full interval. If a refactor drops the call,
// orders completed before the outbox existed stay paid-but-no-ticket forever;
// this is the test that fails.
func TestBackfillRunsOnceBeforeFirstDrain(t *testing.T) {
	log := &callLog{}
	backfilled := make(chan struct{})
	d := New(log, nil, time.Hour, 1, func(context.Context) (int, error) {
		log.record("backfill")
		// A second invocation panics the goroutine — the "exactly once" half.
		close(backfilled)
		return 0, nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()

	select {
	case <-backfilled:
	case <-time.After(5 * time.Second):
		t.Fatal("drainer never invoked the backfill")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drainer did not stop on context cancel")
	}

	calls := log.snapshot()
	if len(calls) < 2 || calls[0] != "backfill" || calls[1] != "claim" {
		t.Fatalf("call order = %v; want the backfill first, then the initial drain", calls)
	}
}

// A backfill error must not stop the drainer: the service stays up and draining,
// and the next boot retries the backfill. Failing here would reintroduce the
// fail-fast coupling TKT-71 removes.
func TestBackfillErrorDoesNotStopTheDrainer(t *testing.T) {
	log := &callLog{}
	d := New(log, nil, time.Hour, 1, func(context.Context) (int, error) {
		return 0, errors.New("relation completion_outbox does not exist")
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if calls := log.snapshot(); len(calls) > 0 && calls[0] == "claim" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("drainer never reached the initial drain after a backfill error")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	<-done
}

// A nil backfill means "nothing to repair" (tests, future callers): the drainer
// must simply drain.
func TestNilBackfillIsSkipped(t *testing.T) {
	log := &callLog{}
	d := New(log, nil, time.Hour, 1, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Run(ctx) // must not panic; the initial drain still runs
	if calls := log.snapshot(); len(calls) != 1 || calls[0] != "claim" {
		t.Fatalf("calls = %v; want exactly the initial drain", calls)
	}
}
