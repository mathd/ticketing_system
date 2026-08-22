package recovery

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"ticketing/services/commerce/internal/store"
)

// backlogStore is a Store that answers only Backlog. Every other method panics: these
// tests are about the gauge callback, and a recovery transition reaching this fake would
// be a test that had quietly become about something else.
type backlogStore struct {
	b   store.RecoveryBacklog
	err error
	// calls counts Backlog reads so the one-snapshot property is assertable.
	calls int
}

func (s *backlogStore) Backlog(context.Context) (store.RecoveryBacklog, error) {
	s.calls++
	return s.b, s.err
}

func (s *backlogStore) ClaimStuckOrders(context.Context, int, time.Duration) ([]store.StuckOrder, error) {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) RecordTerminalOutcome(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) ParkForReconciliation(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) QueueForCompensation(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) MarkRefunded(context.Context, store.StuckOrder) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) ClearRecoveryClaim(context.Context, uuid.UUID, uuid.UUID) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) AbandonRecoveryClaim(context.Context, uuid.UUID, uuid.UUID) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) MarkReleased(context.Context, store.StuckOrder) error {
	panic("a recovery transition reached the metrics fake")
}
func (s *backlogStore) ReleaseStuckOrder(context.Context, uuid.UUID, uuid.UUID, error) error {
	panic("a recovery transition reached the metrics fake")
}

// collect registers the gauges against a real SDK MeterProvider and reads one collection
// cycle back. A real provider and not a hand-rolled meter fake on purpose: a fake would
// let the test pass while asserting facts about itself, and the thing under test is
// precisely whether the callback feeds the instruments the SDK actually produces.
func collect(t *testing.T, st Store) (map[string]int64, error) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	r := New(st, nil, nil, nil, nil, time.Minute, 1, time.Second, slog.Default())
	if err := r.ObserveMetrics(mp.Meter("test")); err != nil {
		return nil, err
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		return nil, err
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is not an int64 gauge", m.Name)
			}
			for _, dp := range g.DataPoints {
				got[m.Name] = dp.Value
			}
		}
	}
	return got, nil
}

// Every registered series carries the value the store reported for it.
//
// The four values are deliberately DISTINCT. With 1/1/1/1 a callback that observed the
// wrong field into the wrong instrument — the likeliest real mistake in a four-way
// ObserveInt64 block — would produce an identical collection and the test could not see
// it. Distinct values make a transposition fail.
func TestObserveMetricsReportsEveryBacklogSeries(t *testing.T) {
	st := &backlogStore{b: store.RecoveryBacklog{
		Parked: 3, ReconciliationRequired: 5, Total: 8, OldestAgeSeconds: 7200,
	}}
	got, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}
	// Derived from what the store reported, which is the contract: the callback's job is
	// to pass the store's answer through unchanged.
	want := map[string]int64{
		"commerce.recovery.parked":                         3,
		"commerce.recovery.parked.reconciliation_required": 5,
		"commerce.recovery.parked.total":                   8,
		"commerce.recovery.parked.oldest_age_seconds":      7200,
	}
	for name, w := range want {
		v, ok := got[name]
		if !ok {
			t.Fatalf("%s was not produced: an instrument was never registered, or was left out of RegisterCallback", name)
		}
		if v != w {
			t.Fatalf("%s = %d, want %d", name, v, w)
		}
	}
	// One snapshot for all four series: separate reads could report a total that
	// disagrees with its own splits.
	if st.calls != 1 {
		t.Fatalf("the store was read %d times, want 1: all four series must come from one snapshot", st.calls)
	}
}

// A store failure surfaces as a collection error rather than as silently zeroed gauges.
// Zeros would be indistinguishable from an empty backlog, which is the one reading an
// operator must never get wrong here: "nothing is parked" and "I cannot tell you" are
// opposite answers.
func TestObserveMetricsPropagatesAStoreFailure(t *testing.T) {
	sentinel := errors.New("backlog read failed")
	_, err := collect(t, &backlogStore{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("collect error = %v, want it to wrap %v", err, sentinel)
	}
}
