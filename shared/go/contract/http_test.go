package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
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

// driftingHandler answers /things with a body the spec forbids (no `ok`).
func driftingHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Handler-Wrote", "1")
		_, _ = w.Write([]byte(`{"wrong":true}`))
	})
}

func validRequest() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"name":"valid"}`))
	request.Header.Set("Content-Type", "application/json")
	return request
}

// TKT-125: with the knob off, a drifting response is the handler's own bytes —
// no buffering, no substitution, no drift log. This is the whole point of the
// switch, and the property ADR-028 gives up wherever it is set.
func TestResponseValidationDisabledPassesDriftUnmodified(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, err := RequestValidator(testSpec, driftingHandler(), log, false)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, validRequest())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want the handler's own %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != `{"wrong":true}` {
		t.Fatalf("body = %q, want the handler's own bytes", got)
	}
	if recorder.Header().Get("X-Handler-Wrote") != "1" {
		t.Fatalf("handler headers lost: %v", recorder.Header())
	}
	if buf.Len() != 0 {
		t.Fatalf("disabled validation must not log drift: %q", buf.String())
	}
}

// The knob moves the response half only. Requests are a trust boundary and
// stay validated unconditionally (TKT-125 scope).
func TestRequestValidationRemainsEnabledWhenResponseValidationIsDisabled(t *testing.T) {
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached handler with response validation off")
	}), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

// Where validation IS enabled, TKT-125 changes nothing: this pins the exact
// ADR-028 artifact — status, both headers, the byte-for-byte body — so a
// future placement change cannot quietly alter the semantics too.
func TestResponseValidationEnabledPreservesFailClosedResponse(t *testing.T) {
	handler, err := RequestValidator(testSpec, driftingHandler(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, validRequest())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if got := recorder.Body.String(); got != "{\"error\":\"response violates OpenAPI contract\"}\n" {
		t.Fatalf("fail-closed body drifted: %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

// A malformed spec must fail at construction whether or not responses are
// validated: the knob controls enforcement, not whether the contract loads.
func TestBrokenSpecFailsConstructionWithValidationDisabled(t *testing.T) {
	if _, err := RequestValidator([]byte("not: [an, openapi, document"), http.NotFoundHandler(), nil, false); err == nil {
		t.Fatal("a broken spec must fail construction even with response validation off")
	}
	if _, err := ResponseValidator([]byte("not: [an, openapi, document"), http.NotFoundHandler(), nil, false); err == nil {
		t.Fatal("a broken spec must fail construction even with response validation off")
	}
}

// TKT-125: the validator ran on context.Background(). Passing r.Context()
// changes no behaviour at kin-openapi v0.142.0 — ValidateResponse accepts a
// ctx and never reads it (verified in the module source; ai-review finding) —
// so this is hygiene against a future version that does honour it, not a fix
// for observable cancellation today. Claim it as nothing more.
// The assertion is on what the validator received, not on the log: the log
// line already used r.Context(), so a log-based test would pass against the
// pre-fix code.
func TestResponseValidationUsesRequestContext(t *testing.T) {
	type ctxKey struct{}
	_, router, err := load(testSpec)
	if err != nil {
		t.Fatal(err)
	}
	var seen context.Context
	handler := responseValidated(router, driftingHandler(), nil,
		func(ctx context.Context, _ *openapi3filter.ResponseValidationInput) error {
			seen = ctx
			return nil
		})
	request := validRequest()
	handler.ServeHTTP(httptest.NewRecorder(), request.WithContext(context.WithValue(request.Context(), ctxKey{}, "sentinel")))
	if seen == nil {
		t.Fatal("validator was never called")
	}
	if seen.Value(ctxKey{}) != "sentinel" {
		t.Fatal("validator ran on a context detached from the request")
	}
}

func TestCustomRequestValidationError(t *testing.T) {
	handler, err := RequestValidatorWithErrorHandler(testSpec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached handler")
	}), nil, true, func(w http.ResponseWriter, _ string, _ int) {
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
	}), nil, true)
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
	}), log, true)
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
	}), log, true)
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

// An undocumented status is drift too (ADR-028): kin-openapi allows it by
// default, so the middleware must opt in to rejecting it.
func TestUndocumentedResponseStatusIsRejected(t *testing.T) {
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted) // spec documents only 200
		_, _ = w.Write([]byte(`{"ok":true}`))
	}), nil, true)
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

// ResponseValidator is the response-only wrap used by routers that already run
// their own request validation (catalog). Undocumented routes pass through.
func TestResponseValidatorRejectsDriftAndLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, err := ResponseValidator(testSpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wrong":true}`))
	}), log, true)
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
	}), nil, true)
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

// The response-only wrap (catalog's seam) honours the knob too — catalog runs
// its own request validation, so this is the only half it contributes.
func TestResponseValidatorDisabledPassesDriftUnmodified(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, err := ResponseValidator(testSpec, driftingHandler(), log, false)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, validRequest())
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"wrong":true}` {
		t.Fatalf("drift did not pass through: %d %s", recorder.Code, recorder.Body.String())
	}
	if buf.Len() != 0 {
		t.Fatalf("disabled validation must not log drift: %q", buf.String())
	}
}
