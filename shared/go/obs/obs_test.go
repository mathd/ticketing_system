package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace"

	"ticketing/shared/obs"
)

// startTestSpan returns a context carrying a recording span.
func startTestSpan(t *testing.T) context.Context {
	t.Helper()
	tp := trace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	t.Cleanup(func() { span.End() })
	return ctx
}

func TestLoggerEmitsJSONWithTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger("catalog", &buf)

	ctx := startTestSpan(t)
	log.InfoContext(ctx, "hello", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["k"] != "v" || rec["service"] != "catalog" {
		t.Errorf("record = %v, want msg/k/service fields", rec)
	}
	traceID, _ := rec["trace_id"].(string)
	if len(traceID) != 32 || traceID == strings.Repeat("0", 32) {
		t.Errorf("trace_id = %q, want a valid 32-hex trace id", traceID)
	}
	if spanID, _ := rec["span_id"].(string); len(spanID) != 16 {
		t.Errorf("span_id = %q, want a valid 16-hex span id", spanID)
	}
}

func TestLoggerWithoutSpanOmitsTraceID(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger("catalog", &buf)
	log.Info("no span")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if _, present := rec["trace_id"]; present {
		t.Errorf("trace_id present without an active span: %v", rec)
	}
}

func TestClientInjectsTraceparent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
	}))
	defer srv.Close()

	ctx := startTestSpan(t)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := obs.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if !strings.HasPrefix(got, "00-") || len(strings.Split(got, "-")) != 4 {
		t.Errorf("traceparent = %q, want W3C traceparent header", got)
	}
}

// Every cross-service call goes through Client(), so its deadline is the platform's only
// backstop against a hung downstream. Unbounded, a stuck charge call could hold a checkout
// handler open past commerce's 2-minute recovery grace period — the window that lets a
// live checkout and the recovery runner both act on one order (TKT-116).
func TestClientHasBoundedTimeout(t *testing.T) {
	c := obs.Client()
	if c.Timeout <= 0 {
		t.Fatal("obs.Client() has no timeout; a hung downstream can hold a handler open forever")
	}
	if c.Timeout >= 2*time.Minute {
		t.Fatalf("timeout %s does not bound below the 2-minute recovery grace period", c.Timeout)
	}
}

// TKT-308: the SHARED transport still traces, even though it is now built at package
// init rather than per call.
//
// This is the risk that came with making the transport package-level, and it is the
// kind that fails silently: if otelhttp captured a tracer provider at construction,
// a transport built before Setup() installs the real one would hold a no-op provider
// for the process lifetime and every client span would vanish. Nothing would fail —
// there would simply be no traces, which is exactly the observability gap you notice
// during an incident.
//
// It is safe because otelhttp resolves the tracer PER REQUEST (transport.go: `tracer
// := t.tracer`, nil unless WithTracerProvider was passed, then falling back to
// otel.GetTracerProvider() at RoundTrip time). We pass only WithPropagators, so
// construction order does not matter.
//
// That is a property of a dependency, so it is pinned here rather than trusted: this
// test fails if someone adds otelhttp.WithTracerProvider to the package-level
// construction, or if the library starts resolving at construction time. Both would
// reintroduce the ordering hazard invisibly.
func TestSharedTransportStillPropagatesAfterPackageInit(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A span established AFTER the package-level transport was constructed — which is
	// the ordering the hazard is about.
	ctx := startTestSpan(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := obs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if !strings.HasPrefix(got, "00-") || len(strings.Split(got, "-")) != 4 {
		t.Errorf("traceparent = %q, want a W3C traceparent. The shared transport is built at "+
			"package init; if it captured a tracer provider then rather than resolving one per "+
			"request, every client span made after Setup() would be dropped silently", got)
	}
}
