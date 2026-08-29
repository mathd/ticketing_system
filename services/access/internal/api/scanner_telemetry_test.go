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
	// devices is kept so a test can assert a request actually REACHED
	// authentication, rather than passing because it was refused earlier.
	devices *fakeDevices
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
		logs:   logs, reader: reader, spans: spans, device: device, devices: devices,
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

// ai-review F1. Authentication runs BEFORE parameter validation, and it both
// SELECTs the device and UPDATEs its last_seen_at. So the cheapest abusive
// request there is — one whose `limit` fails the contract's schema — used to
// cost two database operations and emit nothing at all, because the emit lived
// in the handler and the handler never ran. That is the exact inverse of this
// telemetry's purpose.
//
// The emit now sits in authenticateScannerDevice, scoped to the resolved feed
// operation. This test is the one that distinguishes the two placements: it is
// GREEN with the emit at the auth boundary and RED with it in the handler.
//
// The `limit` cases are the load-bearing ones — they never reach the handler.
// The cursor cases do reach it, and are kept to pin that the move did not
// introduce double-counting.
func TestFeedTelemetryCountsPollsRefusedByTheContract(t *testing.T) {
	for name, tc := range map[string]struct {
		target         string
		reachesHandler bool
	}{
		"schema-invalid limit": {"/scans/voided-tickets?limit=abc", false},
		"out-of-range limit":   {"/scans/voided-tickets?limit=999999", false},
		"garbage cursor":       {"/scans/voided-tickets?cursor=not-a-cursor", true},
		"tampered cursor":      {"/scans/voided-tickets?cursor=abc.def", true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTelemetryHarness(t)

			// Asserted rather than assumed: if these stopped being refusals the
			// test would quietly become a duplicate of the happy path.
			if code := h.poll(t, tc.target).Code; code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400 — this test is about REFUSED polls", name, code)
			}

			// Exactly one, whichever side of validation refused it. One
			// authenticated poll is one record: no misses, no double-count.
			records := h.abuseRecords(t)
			if len(records) != 1 {
				t.Fatalf("%s emitted %d records, want exactly 1 (reaches handler: %v)", name, len(records), tc.reachesHandler)
			}
			if got := records[0]["subject_id"]; got != h.device.String() {
				t.Fatalf("subject_id = %v, want the authenticated device %s", got, h.device)
			}
			if total, _ := h.counterValue(t); total != 1 {
				t.Fatalf("%s: counter = %d, want 1", name, total)
			}
		})
	}
}

// The emit is scoped to the FEED operation, and this is the test that proves the
// scoping does something.
//
// THREE operations share the ScannerDeviceToken scheme — scanTicket,
// reconcileScans and listVoidedTickets — and the emit lives in the shared
// authentication path. Without the operation check, every turnstile admission
// and every offline reconciliation batch would emit a record claiming the feed
// surface was polled, burying the signal this ticket exists to produce under
// ordinary door traffic.
//
// Every non-feed operation gets its own case, because one negative case does
// not prove exclusivity: a predicate as wrong as `OperationID != "scanTicket"`
// would satisfy a test whose only negative was scanTicket, while emitting on
// reconciliations (ai-review second pass). One case per operation, and each
// asserts it actually REACHED authentication — otherwise a case that 404s
// before the auth func would pass by never running the code under test.
//
// Found by mutation: replacing the scoping condition with `true` left the whole
// suite green, so the mechanism was live but unobserved.
func TestScannerTelemetryIsScopedToTheFeedOperation(t *testing.T) {
	for name, tc := range map[string]struct {
		method, target, body string
		wantCode             int
	}{
		// Refused on the credential, which is AFTER authentication.
		"scanTicket": {
			http.MethodPost, "/scans",
			`{"qr_payload":"not-a-ticket"}`,
			http.StatusUnprocessableEntity,
		},
		// A batch entry never fails the whole sync, so this answers 200 with a
		// per-item rejection — also well past authentication.
		"reconcileScans": {
			http.MethodPost, "/scans/reconciliations",
			`{"occurrences":[{"qr_payload":"not-a-ticket","occurrence_id":"` + uuid.NewString() + `","occurred_at":"2026-07-17T09:00:00Z"}]}`,
			http.StatusOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTelemetryHarness(t)

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(scannerDeviceHeader, testDeviceToken)
			h.router.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantCode {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.target, recorder.Code, tc.wantCode)
			}
			// The case reached authentication. Without this a case that was
			// refused earlier — a 404, a contract rejection — would pass while
			// never executing the emit path at all.
			if h.devices.touched == 0 {
				t.Fatalf("%s never reached authentication, so it cannot show the scoping", name)
			}
			if n := len(h.abuseRecords(t)); n != 0 {
				t.Fatalf("%s emitted %d feed-surface records, want 0 — feed telemetry must fire for the feed alone", name, n)
			}
			if total, _ := h.counterValue(t); total != 0 {
				t.Fatalf("%s incremented the feed counter to %d, want 0", name, total)
			}
		})
	}

	// Non-vacuity: the same harness DOES emit for the feed operation, so the
	// zeroes above are scoping rather than a fixture that emits nothing ever.
	t.Run("the feed itself still emits", func(t *testing.T) {
		h := newTelemetryHarness(t)
		h.poll(t, "/scans/voided-tickets?cursor=not-a-cursor")
		if n := len(h.abuseRecords(t)); n != 1 {
			t.Fatalf("the feed emitted %d records, want 1 — the zeroes above would prove nothing", n)
		}
	})
}

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
