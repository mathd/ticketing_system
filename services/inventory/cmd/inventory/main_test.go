package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

func TestSubcommandsRegisterMigrateAndReprocessQuarantine(t *testing.T) {
	subs := subcommands()
	for _, name := range []string{"migrate", "reprocess-quarantine", "reconcile-pins"} {
		if _, ok := subs[name]; !ok {
			t.Fatalf("subcommands() lacks %q", name)
		}
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

type fakeQuarantine struct {
	rows      []store.QuarantinedCatalogEvent
	published []struct{ subject, msgID, envelope string }
	marked    []uuid.UUID
	pubErr    error
	markErr   error
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
	// Rows marked reinjected leave the pending set, like the real store.
	var out []store.QuarantinedCatalogEvent
	for _, r := range f.rows[start:] {
		resolved := false
		for _, m := range f.marked {
			if m == r.EventID {
				resolved = true
			}
		}
		if !resolved {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeQuarantine) publish(_ context.Context, subject, msgID string, envelope []byte) error {
	if f.pubErr != nil {
		return f.pubErr
	}
	f.published = append(f.published, struct{ subject, msgID, envelope string }{subject, msgID, string(envelope)})
	return nil
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
	return quarantineReprocessor{list: f.list, publish: f.publish, mark: f.mark, supports: supports}
}

// The core protocol: supported rows are republished byte-identically to their original subject
// with a deterministic message id, and marked only after the broker accepted the publish.
// Unsupported rows stay unresolved without blocking supported rows behind them.
func TestReprocessRepublishesSupportedRowsPastUnsupportedOnes(t *testing.T) {
	t0 := time.Now()
	unsupported := row("platform.catalog.performance.published", 9, t0)
	supported := row("platform.catalog.performance.published", 4, t0.Add(time.Minute))
	f := &fakeQuarantine{rows: []store.QuarantinedCatalogEvent{unsupported, supported}}

	reinjected, skipped, err := reprocessorFor(f, func(_ string, schema int) bool { return schema == 4 }).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reinjected != 1 || skipped != 1 {
		t.Fatalf("reinjected=%d skipped=%d, want 1 and 1", reinjected, skipped)
	}
	if len(f.published) != 1 || f.published[0].subject != supported.Subject || f.published[0].envelope != string(supported.Envelope) {
		t.Fatalf("published %+v, want exactly the supported row's original subject and byte-identical envelope", f.published)
	}
	wantID := quarantineMsgID(supported.Subject, supported.EventID, supported.Schema)
	if f.published[0].msgID != wantID {
		t.Fatalf("msgID = %q, want the deterministic %q", f.published[0].msgID, wantID)
	}
	if len(f.marked) != 1 || f.marked[0] != supported.EventID {
		t.Fatalf("marked %v, want exactly the supported row", f.marked)
	}
}

// Publish failure leaves the row unresolved and surfaces the error; nothing may be marked on
// the strength of a publish the broker never accepted.
func TestReprocessPublishFailureLeavesRowUnresolved(t *testing.T) {
	f := &fakeQuarantine{
		rows:   []store.QuarantinedCatalogEvent{row("platform.catalog.performance.published", 4, time.Now())},
		pubErr: errors.New("broker down"),
	}
	_, _, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
	if err == nil {
		t.Fatal("a failed publish must surface as an error")
	}
	if len(f.marked) != 0 {
		t.Fatalf("marked %v — a row must never be marked without a broker-accepted publish", f.marked)
	}
}

// A mark failure after a successful publish is an error, not a success claim: the re-run is
// safe (deterministic msg id inside the duplicate window, consumed_events beyond it).
func TestReprocessMarkFailureIsReturned(t *testing.T) {
	f := &fakeQuarantine{
		rows:    []store.QuarantinedCatalogEvent{row("platform.catalog.performance.published", 4, time.Now())},
		markErr: errors.New("db down"),
	}
	_, _, err := reprocessorFor(f, func(string, int) bool { return true }).run(context.Background())
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
		rows = append(rows, row("platform.catalog.performance.published", 4, t0.Add(time.Duration(i)*time.Second)))
	}
	f := &fakeQuarantine{rows: rows}
	sup := func(string, int) bool { return true }

	reinjected, _, err := reprocessorFor(f, sup).run(context.Background())
	if err != nil || reinjected != len(rows) {
		t.Fatalf("reinjected=%d err=%v, want the full backlog %d", reinjected, err, len(rows))
	}
	again, _, err := reprocessorFor(f, sup).run(context.Background())
	if err != nil || again != 0 {
		t.Fatalf("rerun reinjected=%d err=%v, want a clean no-op", again, err)
	}
}
