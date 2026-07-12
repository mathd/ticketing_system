package httpx_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	if body.Checks["nats"] != "connection refused" {
		t.Errorf("nats check = %q, want the error message", body.Checks["nats"])
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
