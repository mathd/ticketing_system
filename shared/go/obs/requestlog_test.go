package obs_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
