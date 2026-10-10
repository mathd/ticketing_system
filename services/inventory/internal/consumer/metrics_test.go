package consumer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// parkedCounts reads the parked counter back from a local SDK reader, keyed by subject. A real
// provider is used, not a fake counter, so the test sees what the SDK actually collects (TKT-317).
func parkedCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "inventory.catalog.events.parked" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				subject, _ := dp.Attributes.Value(attribute.Key("subject"))
				counts[subject.AsString()] += dp.Value
			}
		}
	}
	return counts
}

// TKT-317 D2: each parked event is counted once and logged at ERROR. A redelivery of the same
// bytes, after a crash between the park and the Term, is not counted again.
func TestTKT317ParkingIsCountedOnceAndLoggedAtError(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st := &fakeCatalogStore{}
	c := New(nil, st, fakeResolver{err: errCatalogUnusable}, logger, WithMeter(mp.Meter("test")))
	c.ready.Store(true)
	ctx := context.Background()
	body := `{"id":"` + uuid.NewString() + `","schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","closure_version":1}}`

	first := &fakeMsg{subject: subjectClosed, data: []byte(withSubjectType(subjectClosed, body)), delivered: parkAfterDeliveries}
	c.handle(ctx, first)
	if !slices.Contains(first.actions, "term") {
		t.Fatalf("actions = %v, want term", first.actions)
	}
	if got := parkedCounts(t, reader)[subjectClosed]; got != 1 {
		t.Fatalf("parked count = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), `level=ERROR msg="catalog event parked`) {
		t.Fatalf("the park was not logged at ERROR:\n%s", logs.String())
	}

	again := &fakeMsg{subject: subjectClosed, data: first.data, delivered: parkAfterDeliveries + 1}
	c.handle(ctx, again)
	if got := parkedCounts(t, reader)[subjectClosed]; got != 1 {
		t.Fatalf("parked count after a duplicate delivery = %d, want 1", got)
	}
}

// TKT-317 D2: a transport failure is never parked, so it is never counted.
func TestTKT317TransportFailureIsNeverCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	st := &fakeCatalogStore{}
	c := New(nil, st, fakeResolver{err: errResolveUnavailable}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMeter(mp.Meter("test")))
	c.ready.Store(true)
	body := `{"id":"` + uuid.NewString() + `","schema":1,"data":{"performance_id":"` + perfID + `","organizer_id":"` + orgID + `","closure_version":1}}`

	msg := &fakeMsg{subject: subjectClosed, data: []byte(withSubjectType(subjectClosed, body)), delivered: parkAfterDeliveries + 3}
	c.handle(context.Background(), msg)

	if !slices.Contains(msg.actions, "nak-delay") {
		t.Fatalf("actions = %v, want a delayed retry", msg.actions)
	}
	if got := parkedCounts(t, reader)[subjectClosed]; got != 0 {
		t.Fatalf("parked count = %d for a transport failure, want 0", got)
	}
}
