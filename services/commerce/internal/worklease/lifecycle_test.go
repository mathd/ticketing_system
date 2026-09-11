package worklease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type testRow struct {
	id  string
	key string
}

type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordHandler) countLevel(lvl slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int
	for _, r := range h.records {
		if r.Level == lvl {
			n++
		}
	}
	return n
}

func (h *recordHandler) hasMessage(lvl slog.Level, substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == lvl && strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}

// Test 1: A duplicate key across two batches is handed back and driven only once.
// Mutation that must make it RED: Delete the driven map insertion, or move it after Process.
func TestDuplicateKeyAcrossBatchesHandedBackAndDrivenOnce(t *testing.T) {
	r1 := testRow{id: "1", key: "A"}
	r2 := testRow{id: "2", key: "B"}
	r3 := testRow{id: "3", key: "B"} // duplicate of key "B" across batches
	r4 := testRow{id: "4", key: "C"}

	batches := [][]testRow{
		{r1, r2},
		{r3, r4},
	}

	var (
		mu        sync.Mutex
		processed []testRow
		abandoned []testRow
		batchIdx  int
	)

	h := &recordHandler{}
	logger := slog.New(h)

	a := Adapter[testRow, string]{
		Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
			mu.Lock()
			defer mu.Unlock()
			if batchIdx >= len(batches) {
				return nil, nil
			}
			b := batches[batchIdx]
			batchIdx++
			return b, nil
		},
		Key: func(r testRow) string {
			return r.key
		},
		Process: func(ctx context.Context, row testRow) bool {
			mu.Lock()
			defer mu.Unlock()
			processed = append(processed, row)
			return true
		},
		Abandon: func(ctx context.Context, row testRow) error {
			mu.Lock()
			defer mu.Unlock()
			abandoned = append(abandoned, row)
			return nil
		},
		Batch:             2,
		Lease:             time.Minute,
		MaxBatchesPerPass: 5,
		Log:               logger,
		Name:              "test",
	}

	resolved := Drain(context.Background(), a)
	if resolved != 3 {
		t.Fatalf("resolved = %d, want 3", resolved)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(processed) != 3 {
		t.Fatalf("processed %d rows, want 3: %+v", len(processed), processed)
	}
	for _, p := range processed {
		if p.id == "3" {
			t.Fatalf("duplicate row r3 was processed; want only r2 for key B")
		}
	}
	if len(abandoned) != 1 || abandoned[0].id != "3" {
		t.Fatalf("abandoned %+v, want [r3]", abandoned)
	}
}

// Test 2: Cancellation after row 1 of a 3-row batch hands back rows 2 and 3, exactly, and does not Process them.
// Mutation that must make it RED: Move the cancellation check outside the row loop.
func TestCancellationAfterRowOneHandsBackSuffix(t *testing.T) {
	r1 := testRow{id: "1", key: "A"}
	r2 := testRow{id: "2", key: "B"}
	r3 := testRow{id: "3", key: "C"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu        sync.Mutex
		processed []testRow
		abandoned []testRow
	)

	h := &recordHandler{}
	logger := slog.New(h)

	a := Adapter[testRow, string]{
		Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
			return []testRow{r1, r2, r3}, nil
		},
		Key: func(r testRow) string {
			return r.key
		},
		Process: func(pCtx context.Context, row testRow) bool {
			mu.Lock()
			defer mu.Unlock()
			processed = append(processed, row)
			if row.id == "1" {
				cancel() // cancel context immediately after row 1 is processed
			}
			return true
		},
		Abandon: func(aCtx context.Context, row testRow) error {
			mu.Lock()
			defer mu.Unlock()
			abandoned = append(abandoned, row)
			return nil
		},
		Batch:             3,
		Lease:             time.Minute,
		MaxBatchesPerPass: 5,
		Log:               logger,
		Name:              "test",
	}

	resolved := Drain(ctx, a)
	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(processed) != 1 || processed[0].id != "1" {
		t.Fatalf("processed = %+v, want only [r1]", processed)
	}
	if len(abandoned) != 2 {
		t.Fatalf("abandoned %d rows, want 2: %+v", len(abandoned), abandoned)
	}
	if abandoned[0].id != "2" || abandoned[1].id != "3" {
		t.Fatalf("abandoned = %+v, want [r2, r3] exactly", abandoned)
	}
}

// Test 3: The undriven suffix is handed back on a context that is ALREADY cancelled
// (assert Abandon observed a live ctx, i.e. ctx.Err() == nil inside the callback).
// Mutation that must make it RED: Pass the caller's cancelled ctx to the suffix hand-back instead of a detached one.
func TestUndrivenSuffixHandsBackOnLiveDetachedContext(t *testing.T) {
	r1 := testRow{id: "1", key: "A"}
	r2 := testRow{id: "2", key: "B"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Pre-cancelled

	var (
		mu               sync.Mutex
		abandoned        []testRow
		abandonCtxErrs   []error
		abandonDeadlines []bool
	)

	h := &recordHandler{}
	logger := slog.New(h)

	a := Adapter[testRow, string]{
		Claim: func(claimCtx context.Context, limit int, lease time.Duration) ([]testRow, error) {
			return []testRow{r1, r2}, nil
		},
		Key: func(r testRow) string {
			return r.key
		},
		Process: func(pCtx context.Context, row testRow) bool {
			t.Fatal("Process should not be called when context is cancelled")
			return false
		},
		Abandon: func(aCtx context.Context, row testRow) error {
			mu.Lock()
			defer mu.Unlock()
			abandoned = append(abandoned, row)
			abandonCtxErrs = append(abandonCtxErrs, aCtx.Err())
			_, hasDeadline := aCtx.Deadline()
			abandonDeadlines = append(abandonDeadlines, hasDeadline)
			return nil
		},
		Batch:             2,
		Lease:             time.Minute,
		MaxBatchesPerPass: 1,
		Log:               logger,
		Name:              "test",
	}

	Drain(ctx, a)

	mu.Lock()
	defer mu.Unlock()

	if len(abandoned) != 2 {
		t.Fatalf("abandoned %d rows, want 2: %+v", len(abandoned), abandoned)
	}
	for i, err := range abandonCtxErrs {
		if err != nil {
			t.Fatalf("Abandon call %d observed cancelled/errored context: %v; want live ctx (ctx.Err() == nil)", i, err)
		}
	}
	for i, hasDeadline := range abandonDeadlines {
		if !hasDeadline {
			t.Fatalf("Abandon call %d context had no deadline; want 5s timeout", i)
		}
	}
}

// Test 4: A claim error is distinguished from an empty claim: error returns without calling Process, empty returns without logging an error.
// Mutation that must make it RED: Treat a claim error as an empty batch.
func TestClaimErrorDistinguishedFromEmptyClaim(t *testing.T) {
	t.Run("claim error returns without calling Process and logs error", func(t *testing.T) {
		var processed bool
		h := &recordHandler{}
		logger := slog.New(h)

		a := Adapter[testRow, string]{
			Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
				return nil, errors.New("database connection refused")
			},
			Key: func(r testRow) string { return r.key },
			Process: func(ctx context.Context, row testRow) bool {
				processed = true
				return true
			},
			Abandon:           func(ctx context.Context, row testRow) error { return nil },
			Batch:             2,
			Lease:             time.Minute,
			MaxBatchesPerPass: 1,
			Log:               logger,
			Name:              "test",
		}

		resolved := Drain(context.Background(), a)
		if resolved != 0 {
			t.Fatalf("resolved = %d, want 0", resolved)
		}
		if processed {
			t.Fatal("Process was called after claim error")
		}
		if n := h.countLevel(slog.LevelError); n != 1 {
			t.Fatalf("logged %d errors, want 1", n)
		}
	})

	t.Run("empty claim returns without calling Process and does not log error", func(t *testing.T) {
		var processed bool
		h := &recordHandler{}
		logger := slog.New(h)

		a := Adapter[testRow, string]{
			Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
				return nil, nil
			},
			Key: func(r testRow) string { return r.key },
			Process: func(ctx context.Context, row testRow) bool {
				processed = true
				return true
			},
			Abandon:           func(ctx context.Context, row testRow) error { return nil },
			Batch:             2,
			Lease:             time.Minute,
			MaxBatchesPerPass: 1,
			Log:               logger,
			Name:              "test",
		}

		resolved := Drain(context.Background(), a)
		if resolved != 0 {
			t.Fatalf("resolved = %d, want 0", resolved)
		}
		if processed {
			t.Fatal("Process was called on empty claim")
		}
		if n := h.countLevel(slog.LevelError); n != 0 {
			t.Fatalf("logged %d errors on empty claim, want 0", n)
		}
	})

	t.Run("context canceled claim error does not log at Error", func(t *testing.T) {
		h := &recordHandler{}
		logger := slog.New(h)

		a := Adapter[testRow, string]{
			Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
				return nil, context.Canceled
			},
			Key:               func(r testRow) string { return r.key },
			Process:           func(ctx context.Context, row testRow) bool { return true },
			Abandon:           func(ctx context.Context, row testRow) error { return nil },
			Batch:             2,
			Lease:             time.Minute,
			MaxBatchesPerPass: 1,
			Log:               logger,
			Name:              "test",
		}

		Drain(context.Background(), a)
		if n := h.countLevel(slog.LevelError); n != 0 {
			t.Fatalf("logged %d errors on context.Canceled claim, want 0", n)
		}
	})
}

// Test 5: A full batch of all-duplicates does not spin: Claim is called a bounded number of times.
// Mutation that must make it RED: Remove the fresh == 0 exit.
func TestFullBatchOfAllDuplicatesDoesNotSpin(t *testing.T) {
	r1 := testRow{id: "1", key: "A"}
	r2 := testRow{id: "2", key: "B"}

	var claimCalls int

	h := &recordHandler{}
	logger := slog.New(h)

	a := Adapter[testRow, string]{
		Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
			claimCalls++
			// Returns [r1, r2] on every call.
			// Batch 1: fresh=2, len=2.
			// Batch 2: both are duplicates, fresh=0, len=2 == Batch. Must exit, not spin to 100!
			return []testRow{r1, r2}, nil
		},
		Key: func(r testRow) string {
			return r.key
		},
		Process: func(ctx context.Context, row testRow) bool {
			return true
		},
		Abandon: func(ctx context.Context, row testRow) error {
			return nil
		},
		Batch:             2,
		Lease:             time.Minute,
		MaxBatchesPerPass: 100,
		Log:               logger,
		Name:              "test",
	}

	resolved := Drain(context.Background(), a)
	if resolved != 2 {
		t.Fatalf("resolved = %d, want 2", resolved)
	}
	if claimCalls != 2 {
		t.Fatalf("Claim was called %d times; want exactly 2 (must not spin to MaxBatchesPerPass=100)", claimCalls)
	}
}

// Test 6: An endless full-batch fake stops at MaxBatchesPerPass claims.
// Mutation that must make it RED: Remove the pass bound.
func TestEndlessFullBatchStopsAtMaxBatchesPerPass(t *testing.T) {
	const maxBatches = 4
	const batchSize = 3

	var claimCalls int
	var processedCount int

	h := &recordHandler{}
	logger := slog.New(h)

	a := Adapter[testRow, string]{
		Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
			claimCalls++
			if claimCalls > maxBatches+5 {
				return nil, fmt.Errorf("safety limit exceeded: drain loop did not stop at MaxBatchesPerPass")
			}
			rows := make([]testRow, batchSize)
			for i := 0; i < batchSize; i++ {
				id := fmt.Sprintf("%d-%d", claimCalls, i)
				rows[i] = testRow{id: id, key: id}
			}
			return rows, nil
		},
		Key: func(r testRow) string {
			return r.key
		},
		Process: func(ctx context.Context, row testRow) bool {
			processedCount++
			return true
		},
		Abandon: func(ctx context.Context, row testRow) error {
			return nil
		},
		Batch:             batchSize,
		Lease:             time.Minute,
		MaxBatchesPerPass: maxBatches,
		Log:               logger,
		Name:              "test",
	}

	resolved := Drain(context.Background(), a)
	if resolved != maxBatches*batchSize {
		t.Fatalf("resolved = %d, want %d", resolved, maxBatches*batchSize)
	}
	if claimCalls != maxBatches {
		t.Fatalf("Claim called %d times, want exactly %d", claimCalls, maxBatches)
	}
	if !h.hasMessage(slog.LevelInfo, "per-pass bound") {
		t.Fatal("expected Info log message when drain hits per-pass bound")
	}
}

// Test 7: Every claimed row is either Processed or Abandoned — never silently dropped. Assert over all rows in all scenarios.
// Mutation that must make it RED: Return early from the suffix loop after the first row.
func TestEveryClaimedRowProcessedOrAbandonedNeverDropped(t *testing.T) {
	t.Run("cancellation suffix with multiple rows does not drop rows", func(t *testing.T) {
		r1 := testRow{id: "1", key: "A"}
		r2 := testRow{id: "2", key: "B"}
		r3 := testRow{id: "3", key: "C"}
		r4 := testRow{id: "4", key: "D"}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		claimedMap := make(map[string]testRow)
		processedMap := make(map[string]testRow)
		abandonedMap := make(map[string]testRow)
		var mu sync.Mutex

		h := &recordHandler{}
		logger := slog.New(h)

		a := Adapter[testRow, string]{
			Claim: func(cCtx context.Context, limit int, lease time.Duration) ([]testRow, error) {
				mu.Lock()
				defer mu.Unlock()
				rows := []testRow{r1, r2, r3, r4}
				for _, r := range rows {
					claimedMap[r.id] = r
				}
				return rows, nil
			},
			Key: func(r testRow) string {
				return r.key
			},
			Process: func(pCtx context.Context, row testRow) bool {
				mu.Lock()
				defer mu.Unlock()
				processedMap[row.id] = row
				if row.id == "1" {
					cancel() // cancel during row 1
				}
				return true
			},
			Abandon: func(aCtx context.Context, row testRow) error {
				mu.Lock()
				defer mu.Unlock()
				abandonedMap[row.id] = row
				return nil
			},
			Batch:             4,
			Lease:             time.Minute,
			MaxBatchesPerPass: 1,
			Log:               logger,
			Name:              "test",
		}

		Drain(ctx, a)

		mu.Lock()
		defer mu.Unlock()

		if len(claimedMap) != 4 {
			t.Fatalf("expected 4 claimed rows, got %d", len(claimedMap))
		}

		// Invariant: every claimed row MUST be accounted for — either processed or abandoned, exactly one of the two.
		for id := range claimedMap {
			_, inProc := processedMap[id]
			_, inAban := abandonedMap[id]

			if inProc && inAban {
				t.Fatalf("row %s was BOTH processed and abandoned", id)
			}
			if !inProc && !inAban {
				t.Fatalf("row %s was claimed but dropped: neither processed nor abandoned", id)
			}
		}

		if len(processedMap) != 1 {
			t.Fatalf("processed count = %d, want 1", len(processedMap))
		}
		if len(abandonedMap) != 3 {
			t.Fatalf("abandoned count = %d, want 3", len(abandonedMap))
		}
	})

	t.Run("duplicate rows across batches are accounted for", func(t *testing.T) {
		r1 := testRow{id: "1", key: "A"}
		r2 := testRow{id: "2", key: "B"}
		r3 := testRow{id: "3", key: "B"} // duplicate
		r4 := testRow{id: "4", key: "C"}

		batches := [][]testRow{
			{r1, r2},
			{r3, r4},
		}

		var (
			mu           sync.Mutex
			batchIdx     int
			claimedList  []testRow
			processedMap = make(map[string]testRow)
			abandonedMap = make(map[string]testRow)
		)

		h := &recordHandler{}
		logger := slog.New(h)

		a := Adapter[testRow, string]{
			Claim: func(ctx context.Context, limit int, lease time.Duration) ([]testRow, error) {
				mu.Lock()
				defer mu.Unlock()
				if batchIdx >= len(batches) {
					return nil, nil
				}
				b := batches[batchIdx]
				batchIdx++
				claimedList = append(claimedList, b...)
				return b, nil
			},
			Key: func(r testRow) string {
				return r.key
			},
			Process: func(ctx context.Context, row testRow) bool {
				mu.Lock()
				defer mu.Unlock()
				processedMap[row.id] = row
				return true
			},
			Abandon: func(ctx context.Context, row testRow) error {
				mu.Lock()
				defer mu.Unlock()
				abandonedMap[row.id] = row
				return nil
			},
			Batch:             2,
			Lease:             time.Minute,
			MaxBatchesPerPass: 5,
			Log:               logger,
			Name:              "test",
		}

		Drain(context.Background(), a)

		mu.Lock()
		defer mu.Unlock()

		if len(claimedList) != 4 {
			t.Fatalf("claimedList length = %d, want 4", len(claimedList))
		}

		for _, row := range claimedList {
			_, inProc := processedMap[row.id]
			_, inAban := abandonedMap[row.id]

			if inProc && inAban {
				t.Fatalf("row %s was BOTH processed and abandoned", row.id)
			}
			if !inProc && !inAban {
				t.Fatalf("row %s was claimed but dropped: neither processed nor abandoned", row.id)
			}
		}
	})
}

// Test 8: a duplicate hand-back that RACES a shutdown falls back to the detached path.
//
// This is the narrowest branch in Drain and the easiest to lose in a refactor. The
// per-row ctx.Err() check and the duplicate's Abandon are NOT atomic: a shutdown can
// land between them, or while that write is in flight. The live-context Abandon then
// fails on the cancelled context, and if the loop merely logged and moved on, the row
// would stay leased for the FULL lease with its obligation outstanding -- minutes of
// nothing happening for work that is already overdue. So the failure falls through to
// the detached path, which is the case that path exists for.
//
// FORCING THE SCHEDULE IS THE WHOLE DIFFICULTY. Cancelling from Process does NOT reach
// this branch: the per-row check at the top of the next batch sees the cancellation
// first and sends the entire suffix down the detached path, so the duplicate branch is
// never entered. The race has to happen INSIDE the duplicate's own Abandon call, which
// is precisely the window the production comment describes. So the fixture cancels from
// within Abandon itself, on the duplicate, and then fails that same call the way a real
// driver refuses a write on a context that has just been cancelled.
//
// Mutation that must make it RED: replace the `if ctx.Err() != nil` fallback arm in the
// duplicate branch with `if false`, i.e. remove the detached retry.
func TestDuplicateHandBackRacingShutdownFallsBackToDetachedContext(t *testing.T) {
	dup := testRow{id: "1", key: "A"}
	filler := testRow{id: "2", key: "B"}

	// Both rows are driven in batch 1. In batch 2 the FILLER is reached first and is a
	// duplicate, so the duplicate branch is entered while the caller's context is still
	// live -- that is the branch under test.
	batches := [][]testRow{
		{dup, filler},
		{filler, dup},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var claims int
	type handBack struct {
		id   string
		live bool
	}
	var handBacks []handBack
	var cancelled bool

	h := &recordHandler{}
	a := Adapter[testRow, string]{
		Claim: func(context.Context, int, time.Duration) ([]testRow, error) {
			if claims >= len(batches) {
				return nil, nil
			}
			b := batches[claims]
			claims++
			return b, nil
		},
		Key:     func(r testRow) string { return r.key },
		Process: func(context.Context, testRow) bool { return true },
		Abandon: func(c context.Context, r testRow) error {
			// The shutdown lands DURING the duplicate's live-context hand-back: cancel,
			// then refuse this very call, exactly as a driver would once its context is
			// gone. Only the detached retry can still hand this row back.
			if !cancelled {
				cancelled = true
				cancel()
				handBacks = append(handBacks, handBack{id: r.id, live: false})
				return c.Err()
			}
			handBacks = append(handBacks, handBack{id: r.id, live: c.Err() == nil})
			return nil
		},
		Batch:             2,
		Lease:             time.Minute,
		MaxBatchesPerPass: 8,
		Log:               slog.New(h),
		Name:              "test",
	}

	Drain(ctx, a)

	// The row whose hand-back was refused mid-shutdown must have been handed back again
	// on a live context. Without the detached retry it is handed back once, on a dying
	// context, and stays leased.
	var refused string
	for _, hb := range handBacks {
		if !hb.live {
			refused = hb.id
			break
		}
	}
	if refused == "" {
		t.Fatal("the fixture never produced a refused hand-back; it cannot observe the fallback")
	}
	var recovered bool
	for _, hb := range handBacks {
		if hb.id == refused && hb.live {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("row %s was refused on a cancelled context and never retried on a live one: "+
			"it stays leased for the full lease with its obligation outstanding (hand-backs: %+v)",
			refused, handBacks)
	}
}
