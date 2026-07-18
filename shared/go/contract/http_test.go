package contract

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testSpec = []byte(`openapi: 3.0.3
info:
  title: contract test
  version: 1.0.0
paths:
  /things:
    post:
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              required: [name]
              properties:
                name: {type: string}
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [ok]
                properties:
                  ok: {type: boolean}
`)

func TestCustomRequestValidationError(t *testing.T) {
	handler, err := RequestValidatorWithErrorHandler(testSpec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached handler")
	}), nil, func(w http.ResponseWriter, _ string, _ int) {
		writeValidationError(w, http.StatusUnprocessableEntity, map[string]string{"decision": "rejected"})
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), `"decision":"rejected"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestInvalidHandlerResponseIsRejected(t *testing.T) {
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wrong":true}`))
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"name":"valid"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

// The fail-closed 500 must be diagnosable: one structured log line naming the
// operation and the validation error (TKT-47, ADR-028).
func TestResponseDriftEmitsStructuredLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wrong":true}`))
	}), log)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"name":"valid"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("no structured drift log emitted: %q", buf.String())
	}
	for field, want := range map[string]any{
		"msg":    "response violates OpenAPI contract",
		"method": http.MethodPost,
		"path":   "/things",
		"status": float64(http.StatusOK),
	} {
		if entry[field] != want {
			t.Errorf("log %s = %v, want %v", field, entry[field], want)
		}
	}
	if entry["error"] == nil || entry["error"] == "" {
		t.Error("log line carries no validation error detail")
	}
}

func TestValidResponseEmitsNoLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}), log)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"name":"valid"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %q", buf.String())
	}
}

// ResponseValidator is the response-only wrap used by routers that already run
// their own request validation (catalog). Undocumented routes pass through.
func TestResponseValidatorRejectsDriftAndLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, err := ResponseValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wrong":true}`))
	}), log)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"name":"valid"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(buf.String(), "response violates OpenAPI contract") {
		t.Fatalf("no drift log emitted: %q", buf.String())
	}
}

func TestResponseValidatorPassesValidAndUndocumented(t *testing.T) {
	handler, err := ResponseValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/things" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"free":"form"}`))
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"name":"valid"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("documented route = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/undocumented", nil))
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("undocumented route = %d, want passthrough %d", recorder.Code, http.StatusTeapot)
	}
}
