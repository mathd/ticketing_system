package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
