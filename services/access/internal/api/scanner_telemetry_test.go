package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"ticketing/services/access/internal/ticket"
	"ticketing/shared/obs"
)

// TKT-272. The feed is deliberately NOT rate limited (ADR-066): an enrolled
// device may poll without bound until an operator revokes it, and revocation
// takes a device id. So the telemetry that names the device IS the control's
// input, not decoration — an operator who cannot see which device is polling
// cannot run `access revoke-scanner` against it.
//
// These tests assert at the SINKS the values cross on their way out — the log
// buffer, the metric reader, the exported span — never on the telemetry
// object's own fields. TKT-202's F3/F7: a test that builds its own chain and
// checks the component stays green when the component is removed from the
// place that uses it.

// telemetryHarness drives the REAL router (the same New(...).Router(...) a
// service main builds) with a real slog sink, a real metric reader and a real
// span exporter behind it.
type telemetryHarness struct {
	router http.Handler
	logs   *bytes.Buffer
	reader *sdkmetric.ManualReader
	spans  *tracetest.InMemoryExporter
	device uuid.UUID
}

func newTelemetryHarness(t *testing.T) *telemetryHarness {
	t.Helper()

	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	telemetry := newScannerTelemetry(obs.NewLogger("access", logs))
	if err := telemetry.ObserveMetrics(meter); err != nil {
		t.Fatal(err)
	}

	device := uuid.New()
	devices := &fakeDevices{token: testDeviceToken, organizer: uuid.New(), id: device}

	spans := tracetest.NewInMemoryExporter()
	tp := obs.NewTracerProviderForTest(sdktrace.NewSimpleSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	inner := New(nil, verifier).
		WithScannerDevices(devices).
		WithScannerTelemetry(telemetry).
		Router(nil, true)

	return &telemetryHarness{
		router: obs.MiddlewareWithTracerProvider("access", tp, inner),
		logs:   logs, reader: reader, spans: spans, device: device,
	}
}

func (h *telemetryHarness) poll(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set(scannerDeviceHeader, testDeviceToken)
	h.router.ServeHTTP(recorder, request)
	return recorder
}

// abuseRecords returns the decoded abuse.request log lines.
func (h *telemetryHarness) abuseRecords(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, line)
		}
		if rec["msg"] == abuseRequestMessage {
			out = append(out, rec)
		}
	}
	return out
}

// counterValue reads the aggregate counter from the metric reader, and returns
// its attributes so a cardinality assertion can inspect them.
func (h *telemetryHarness) counterValue(t *testing.T) (int64, []attribute.KeyValue) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != abuseRequestsMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want Sum[int64]", m.Name, m.Data)
			}
			var total int64
			var attrs []attribute.KeyValue
			for _, dp := range sum.DataPoints {
				total += dp.Value
				attrs = append(attrs, dp.Attributes.ToSlice()...)
			}
			return total, attrs
		}
	}
	return 0, nil
}

// The record names the AUTHENTICATED device, which is the input
// `access revoke-scanner` takes. Mutation this must catch: emitting the
// organizer, a fresh uuid, or anything read off the request.
func TestFeedTelemetryNamesTheAuthenticatedDevice(t *testing.T) {
	h := newTelemetryHarness(t)

	// A handler-refused poll, deliberately: this Server has no store (the happy
	// path needs PostgreSQL and is covered in smoke), and the emit happens
	// before the handler's own validation precisely so it does not depend on
	// the outcome. What is under test is WHICH DEVICE the record names, which
	// this path exercises identically. A garbage cursor rather than a bad
	// `limit`: `limit` is schema-bounded and never reaches the handler.
	if code := h.poll(t, "/scans/voided-tickets?cursor=not-a-cursor").Code; code != http.StatusBadRequest {
		t.Fatalf("feed poll = %d, want 400", code)
	}

	records := h.abuseRecords(t)
	if len(records) != 1 {
		t.Fatalf("got %d %s records, want exactly 1 — one authenticated request is one record", len(records), abuseRequestMessage)
	}
	if got := records[0]["subject_id"]; got != h.device.String() {
		t.Fatalf("subject_id = %v, want the authenticated device %s", got, h.device)
	}
	if got := records[0]["surface"]; got != feedSurface {
		t.Fatalf("surface = %v, want %q", got, feedSurface)
	}
	if got := records[0]["subject_type"]; got != scannerDeviceSubject {
		t.Fatalf("subject_type = %v, want %q", got, scannerDeviceSubject)
	}
}

// A1 (plan-review), CORRECTED BY THE RED TEST. The amendment said "every
// authenticated request emits one record whatever the outcome". That is not
// achievable from the handler, and the failing test is what established it:
// `limit` is schema-bounded in the contract (openapi.yaml: integer, 1..100), so
// `limit=abc` and `limit=999999` are refused by the request validator BEFORE
// the handler runs. No handler, no emit — and moving the emit into the
// validator would mean emitting from a layer shared by every route in the
// service, which is far beyond this ticket.
//
// The honest boundary, which is what this test now pins: every request that
// REACHES THE HANDLER emits exactly one record, including the ones the handler
// itself refuses. `cursor` is both the case that matters and the one an abusive
// poller would actually use — it is a free-form string in the contract
// (minLength 1, maxLength 400), so a garbage cursor passes schema validation,
// reaches the handler, and is refused there.
//
// The residual is recorded in ADR-066 rather than hidden: a caller sending
// schema-invalid requests is refused at the contract layer and is NOT counted.
// That caller also never reaches the database, which is the cost this telemetry
// exists to make visible.
func TestFeedTelemetryEmitsOnRefusedRequestsToo(t *testing.T) {
	for name, target := range map[string]string{
		"bad cursor":      "/scans/voided-tickets?cursor=not-a-cursor",
		"unsigned cursor": "/scans/voided-tickets?cursor=eyJ2IjoxfQ",
		"tampered cursor": "/scans/voided-tickets?cursor=abc.def",
	} {
		t.Run(name, func(t *testing.T) {
			h := newTelemetryHarness(t)

			// The precondition, asserted rather than assumed: if these stopped
			// being refusals the test would silently become a duplicate of the
			// happy-path one above.
			if code := h.poll(t, target).Code; code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400 — this test is about the REFUSAL path", name, code)
			}
			if n := len(h.abuseRecords(t)); n != 1 {
				t.Fatalf("a refused poll emitted %d records, want 1 — an abusive poller sending junk is still abusive", n)
			}
			if total, _ := h.counterValue(t); total != 1 {
				t.Fatalf("counter = %d after one refused poll, want 1", total)
			}
		})
	}
}

// A2/A3 (plan-review) + the ticket's COS 3, asserted at all three sinks.
//
// The token is filled into EVERY position the harness can put it, because
// TKT-202's 576-arrangement sweep passed while the defect was live: it put a
// harmless placeholder in the position that leaked. The literal is distinctive
// so a substring search is exact.
func TestFeedTelemetryNeverEmitsTheDeviceToken(t *testing.T) {
	h := newTelemetryHarness(t)
	h.poll(t, "/scans/voided-tickets?cursor=not-a-cursor")

	// Non-vacuity: the sinks must have received something, or every absence
	// assertion below is satisfied by an empty pipeline.
	if len(h.abuseRecords(t)) == 0 {
		t.Fatal("no telemetry was emitted — the absence assertions would prove nothing")
	}

	if got := h.logs.String(); strings.Contains(got, testDeviceToken) {
		t.Fatalf("the device token reached the log sink: %s", got)
	}

	_, attrs := h.counterValue(t)
	for _, a := range attrs {
		if strings.Contains(a.Value.String(), testDeviceToken) {
			t.Fatalf("the device token reached a metric attribute: %s=%s", a.Key, a.Value.String())
		}
		// A2: the device id is deliberately NOT a metric label — unbounded
		// cardinality, one series per enrolled device.
		if strings.Contains(a.Value.String(), h.device.String()) {
			t.Fatalf("the device id reached metric attribute %s — unbounded cardinality", a.Key)
		}
	}

	for _, s := range h.spans.GetSpans() {
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.String(), testDeviceToken) {
				t.Fatalf("the device token reached span attribute %s", a.Key)
			}
		}
	}
}
