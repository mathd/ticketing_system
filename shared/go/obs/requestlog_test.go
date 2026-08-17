package obs_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ticketing/shared/obs"
)

func TestRequestLoggerLogsMethodPathStatus(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger("svc", &buf)

	h := obs.RequestLogger(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(startTestSpan(t))
	h.ServeHTTP(rec, req)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if line["msg"] != "request" || line["method"] != "GET" || line["path"] != "/healthz" {
		t.Errorf("line = %v", line)
	}
	if line["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", line["status"])
	}
	if id, _ := line["trace_id"].(string); len(id) != 32 {
		t.Errorf("trace_id = %q, want correlation id", id)
	}
}

// A guest retrieval logs enough to debug — method, status, duration, the route
// SHAPE — and not the capability itself (TKT-202, ADR-012 COS #1/#3).
//
// Asserting the exact sanitized path, not merely "the ref is absent": the latter
// also passes if the logger stops writing a path at all, or blanks the whole
// line, which would meet the letter of the ticket and destroy the debuggability
// the same COS requires.
//
// Mutation this must catch: drop the SanitizedPath call in RequestLogger.
func TestRequestLoggerSanitizesCapabilityPaths(t *testing.T) {
	const ref = "2f1e3d4c-5b6a-4978-8899-aabbccddeeff"

	var buf bytes.Buffer
	log := obs.NewLogger("svc", &buf)
	h := obs.RequestLogger(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/access/orders/"+ref+"/tickets", nil).
		WithContext(startTestSpan(t))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(buf.String(), ref) {
		t.Errorf("log line leaks the guest reference: %s", buf.String())
	}

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if line["path"] != "/api/access/orders/:capability/tickets" {
		t.Errorf("path = %v, want the route shape with the capability replaced", line["path"])
	}
	// The rest of the line must survive: a redaction that costs the debugging
	// fields is not the fix this ticket asked for.
	if line["method"] != "GET" {
		t.Errorf("method = %v", line["method"])
	}
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v", line["status"])
	}
	if _, ok := line["duration_ms"]; !ok {
		t.Error("duration_ms missing")
	}
	if id, _ := line["trace_id"].(string); len(id) != 32 {
		t.Errorf("trace_id = %q — correlation must still work without the capability", id)
	}
}
