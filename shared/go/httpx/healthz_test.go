package httpx_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ticketing/shared/httpx"
)

func TestHealthzAllChecksPass(t *testing.T) {
	h := httpx.Healthz("catalog",
		httpx.Check("db", func() error { return nil }),
		httpx.Check("nats", func() error { return nil }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status  string            `json:"status"`
		Service string            `json:"service"`
		Checks  map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body.Status != "ok" || body.Service != "catalog" {
		t.Errorf("body = %+v, want status ok / service catalog", body)
	}
	if body.Checks["db"] != "ok" || body.Checks["nats"] != "ok" {
		t.Errorf("checks = %v, want all ok", body.Checks)
	}
}

func TestHealthzFailingCheckReturns503(t *testing.T) {
	h := httpx.Healthz("inventory",
		httpx.Check("db", func() error { return nil }),
		httpx.Check("nats", func() error { return errors.New("connection refused") }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	// The check is NAMED but the reason is not: /healthz answers unauthenticated
	// callers, and a driver error carries DSN and schema detail. Asserting on the
	// substring rather than only on the replacement is what makes this fail if the
	// error text is ever appended to the static word.
	if body.Checks["nats"] != "unhealthy" {
		t.Errorf("nats check = %q, want the static reason", body.Checks["nats"])
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("body leaks the probe error: %s", rec.Body.String())
	}
}

func TestHealthzNoChecks(t *testing.T) {
	h := httpx.Healthz("gateway")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
