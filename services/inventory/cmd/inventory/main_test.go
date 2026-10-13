package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
	"ticketing/shared/runtimecfg"
)

func TestCommandRegistryInvokesEveryInventoryCallback(t *testing.T) {
	var invoked string
	withoutArgs := func(name string) func() error {
		return func() error { invoked = name; return nil }
	}
	withArgs := func(name string) func([]string) error {
		return func([]string) error { invoked = name; return nil }
	}
	callbacks := commandCallbacks{
		migrate: withoutArgs("migrate"), healthcheck: func() int { invoked = "healthcheck"; return 7 },
		reprocessQuarantine: withArgs("reprocess-quarantine"), reconcilePins: withArgs("reconcile-pins"),
		checkClaimHistorySequence: withArgs("check-claim-history-sequence"),
	}
	registry := commandRegistry(callbacks)
	names := []string{"migrate", "healthcheck", "reprocess-quarantine", "reconcile-pins", "check-claim-history-sequence"}
	if len(registry) != len(names) {
		t.Fatalf("registry has %d commands, test names %d", len(registry), len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			invoked = ""
			got := execute([]string{name, "tail"}, callbacks, func() error {
				t.Fatal("server ran after a command was selected")
				return nil
			})
			wantExit := 0
			if name == "healthcheck" {
				wantExit = 7
			}
			if got.Name != name || got.Err != nil || got.ExitCode != wantExit {
				t.Fatalf("dispatch result = %+v, want %s with exit %d", got, name, wantExit)
			}
			if invoked != name {
				t.Fatalf("invoked %q, want %q", invoked, name)
			}
		})
	}
}

func TestReprocessQuarantineRejectsArguments(t *testing.T) {
	if err := reprocessQuarantine([]string{"--force"}); err == nil {
		t.Fatal("unexpected arguments must be a usage error, not silently ignored")
	}
}

func TestReconcilePinsRejectsArguments(t *testing.T) {
	if err := reconcilePins([]string{"--all"}); err == nil {
		t.Fatal("unexpected arguments must be a usage error, not silently ignored")
	}
}

// A3 (TKT-326 D6): without catalog access, schema-1, schema-4 and schema-5 publications and every
// closure cannot be judged. The command refuses before it lists a single row. The database URL
// points nowhere, so a command that listed first fails with a connection error instead, and the
// assertion on the variable name tells the two apart.
func TestReprocessQuarantineRefusesWithoutCatalogAccess(t *testing.T) {
	token := strings.Repeat("a1", runtimecfg.CredentialMinBytes/2)
	for _, tc := range []struct {
		missing    string
		catalogURL string
		token      string
	}{
		{"CATALOG_URL", "", token},
		{"INTERNAL_SERVICE_TOKEN", "http://catalog.invalid", ""},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			t.Setenv("CATALOG_URL", tc.catalogURL)
			t.Setenv("INTERNAL_SERVICE_TOKEN", tc.token)
			t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
			t.Setenv("NATS_URL", "nats://127.0.0.1:1")
			err := reprocessQuarantine(nil)
			if err == nil || !strings.Contains(err.Error(), tc.missing) {
				t.Fatalf("err = %v, want a refusal that names %s", err, tc.missing)
			}
		})
	}
}

type fakeQuarantine struct {
	rows      []store.QuarantinedCatalogEvent
	processed []struct{ subject, envelope string }
	// outcomes is the processor's answer per event id. A row with no entry is applied.
	outcomes   map[uuid.UUID]consumer.Outcome
	marked     []uuid.UUID
	processErr error
	markErr    error
	// onProcess runs at the start of each process call, with the 1-based call number.
	onProcess func(n int)
	pageSizes []int
}

func (f *fakeQuarantine) list(_ context.Context, after *store.QuarantinedCatalogEvent, limit int) ([]store.QuarantinedCatalogEvent, error) {
	f.pageSizes = append(f.pageSizes, limit)
	start := 0
	if after != nil {
		for i, r := range f.rows {
			if r.EventID == after.EventID {
				start = i + 1
			}
		}
	}
	// Marked rows leave the pending set, like the real store.
	var out []store.QuarantinedCatalogEvent
	for _, r := range f.rows[start:] {
		if !slices.Contains(f.marked, r.EventID) {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeQuarantine) process(_ context.Context, subject string, envelope []byte) (consumer.Outcome, error) {
	f.processed = append(f.processed, struct{ subject, envelope string }{subject, string(envelope)})
	if f.onProcess != nil {
		f.onProcess(len(f.processed))
	}
	if f.processErr != nil {
		return 0, f.processErr
	}
	for _, r := range f.rows {
		if string(r.Envelope) == string(envelope) {
			if o, ok := f.outcomes[r.EventID]; ok {
				return o, nil
			}
		}
	}
	return consumer.OutcomeApplied, nil
}

func (f *fakeQuarantine) mark(_ context.Context, _ string, eventID uuid.UUID) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.marked = append(f.marked, eventID)
	return nil
}

func row(subject string, schema int, seen time.Time) store.QuarantinedCatalogEvent {
	id := uuid.New()
	return store.QuarantinedCatalogEvent{
		Subject: subject, EventID: id, Schema: schema,
		Envelope:    []byte(`{"id":"` + id.String() + `","schema":4,"data":{"weird":[1,2]}}`),
		FirstSeenAt: seen,
	}
}

func reprocessorFor(f *fakeQuarantine, supports func(string, int) bool) quarantineReprocessor {
	return quarantineReprocessor{list: f.list, process: f.process, mark: f.mark, supports: supports}
}

const pubSubject = "platform.catalog.performance.published"

// The core protocol: a supported row is processed in-process with its original subject and
// byte-identical envelope, and marked once the processor reports it applied. An unsupported row
// is never handed to the processor and stays unresolved without blocking rows behind it.
func TestReprocessAppliesSupportedRowsPastUnsupportedOnes(t *testing.T) {
	t0 := time.Now()
	unsupported := row(pubSubject, 9, t0)
	supported := row(pubSubject, 4, t0.Add(time.Minute))
	f := &fakeQuarantine{rows: []store.QuarantinedCatalogEvent{unsupported, supported}}

	got, err := reprocessorFor(f, func(_ string, schema int) bool { return schema == 4 }).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != (reprocessCounts{applied: 1, unsupported: 1}) {
		t.Fatalf("counts = %+v, want applied 1 and unsupported 1", got)
	}
	if len(f.processed) != 1 || f.processed[0].subject != supported.Subject || f.processed[0].envelope != string(supported.Envelope) {
		t.Fatalf("processed %+v, want exactly the supported row's subject and byte-identical envelope", f.processed)
	}
	if len(f.marked) != 1 || f.marked[0] != supported.EventID {
		t.Fatalf("marked %v, want exactly the supported row", f.marked)
	}
}

// A terminal row is resolved (no binary will ever apply it) but counted apart from applied ones,
// so an operator can tell a recovered event from a discarded one.
func TestReprocessMarksTerminalRowsAndCountsThemApart(t *testing.T) {
	r := row(pubSubject, 4, time.Now())
	f := &fakeQuarantine{rows: []store.QuarantinedCatalogEvent{r}, outcomes: map[uuid.UUID]consumer.Outcome{r.EventID: consumer.OutcomeTerminal}}

	got, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != (reprocessCounts{terminal: 1}) {
		t.Fatalf("counts = %+v, want terminal 1 and nothing applied", got)
	}
	if len(f.marked) != 1 || f.marked[0] != r.EventID {
		t.Fatalf("marked %v, want the terminal row resolved", f.marked)
	}
}

// A deferred row (an offering event before its pool, a catalog outage) stays pending, so
// readiness stays down after a restart. The run still succeeds and moves on to later rows.
func TestReprocessLeavesDeferredRowsPending(t *testing.T) {
	t0 := time.Now()
	deferred := row(pubSubject, 4, t0)
	applied := row(pubSubject, 4, t0.Add(time.Minute))
	f := &fakeQuarantine{
		rows:     []store.QuarantinedCatalogEvent{deferred, applied},
		outcomes: map[uuid.UUID]consumer.Outcome{deferred.EventID: consumer.OutcomeDeferred},
	}

	got, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
	if err != nil {
		t.Fatalf("a deferral failed the run: %v", err)
	}
	if got != (reprocessCounts{applied: 1, deferred: 1}) {
		t.Fatalf("counts = %+v, want applied 1 and deferred 1", got)
	}
	if len(f.marked) != 1 || f.marked[0] != applied.EventID {
		t.Fatalf("marked %v, want only the applied row; a deferred row must stay pending", f.marked)
	}
}

// A processing failure (a store failure in the live handler) leaves the row unresolved and
// surfaces the error.
func TestReprocessProcessFailureLeavesRowUnresolved(t *testing.T) {
	f := &fakeQuarantine{
		rows:       []store.QuarantinedCatalogEvent{row(pubSubject, 4, time.Now())},
		processErr: errors.New("db down"),
	}
	_, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
	if err == nil {
		t.Fatal("a failed process must surface as an error")
	}
	if len(f.marked) != 0 {
		t.Fatalf("marked %v; a row must never be marked when processing failed", f.marked)
	}
}

// An outcome the command does not know is an error, not a silent skip or a mark.
func TestReprocessRefusesAnUnknownOutcome(t *testing.T) {
	r := row(pubSubject, 4, time.Now())
	f := &fakeQuarantine{rows: []store.QuarantinedCatalogEvent{r}, outcomes: map[uuid.UUID]consumer.Outcome{r.EventID: 0}}
	_, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
	if err == nil || len(f.marked) != 0 {
		t.Fatalf("err = %v, marked = %v; want an error and no mark", err, f.marked)
	}
}

// A mark failure after a committed projection is an error, not a success claim. The re-run is
// safe: consumed_events dedups the projection.
func TestReprocessMarkFailureIsReturned(t *testing.T) {
	f := &fakeQuarantine{
		rows:    []store.QuarantinedCatalogEvent{row(pubSubject, 4, time.Now())},
		markErr: errors.New("db down"),
	}
	_, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
	if err == nil {
		t.Fatal("a failed mark must surface as an error — the run may not claim success")
	}
}

// The pending scan is keyset-paginated: a backlog larger than one page is fully drained, and
// re-running after a crash mid-way is idempotent against already-marked rows.
func TestReprocessDrainsAcrossPagesAndReruns(t *testing.T) {
	t0 := time.Now()
	var rows []store.QuarantinedCatalogEvent
	for i := range reprocessBatchSize + 3 {
		rows = append(rows, row(pubSubject, 4, t0.Add(time.Duration(i)*time.Second)))
	}
	f := &fakeQuarantine{rows: rows}
	sup := func(string, int) bool { return true }

	got, err := reprocessorFor(f, sup).run(context.Background())
	if err != nil || got.applied != len(rows) {
		t.Fatalf("applied=%d err=%v, want the full backlog %d", got.applied, err, len(rows))
	}
	again, err := reprocessorFor(f, sup).run(context.Background())
	if err != nil || again != (reprocessCounts{}) {
		t.Fatalf("rerun = %+v err=%v, want a clean no-op", again, err)
	}
}

// A4 (TKT-326 D6): a SIGTERM cancels the context, and every resolver call after it fails like an
// outage, which the handler defers. Without a cancellation check, each remaining row would defer
// and the run would report success. The run must stop and fail instead.
func TestReprocessStopsWhenCancelled(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rows          int
		wantProcessed int
	}{
		{"cancelled before the next row", 2, 1},
		{"cancelled during the last row", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			t0 := time.Now()
			f := &fakeQuarantine{outcomes: map[uuid.UUID]consumer.Outcome{}}
			for i := range tc.rows {
				r := row(pubSubject, 4, t0.Add(time.Duration(i)*time.Second))
				f.rows = append(f.rows, r)
				f.outcomes[r.EventID] = consumer.OutcomeDeferred
			}
			f.onProcess = func(n int) {
				if n == 1 {
					cancel()
				}
			}

			_, err := reprocessorFor(f, func(string, int) bool { return true }).run(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the run to fail with context.Canceled", err)
			}
			if len(f.processed) != tc.wantProcessed {
				t.Fatalf("processed %d rows, want %d; no row may be processed after cancellation", len(f.processed), tc.wantProcessed)
			}
			if len(f.marked) != 0 {
				t.Fatalf("marked %v after cancellation", f.marked)
			}
		})
	}
}
