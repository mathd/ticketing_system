package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
)

type countingReadCloser struct {
	reader io.Reader
	read   int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

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
  /api/access/orders/{ref}/tickets:
    get:
      parameters:
        - name: ref
          in: path
          required: true
          schema: {type: string}
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

var cardinalitySpec = []byte(`openapi: 3.0.3
info: {title: cardinality test, version: 1.0.0}
paths:
  /headers:
    get:
      parameters:
        - in: header
          name: X-Request
          required: false
          schema: {type: string, enum: [allowed]}
      responses:
        '200':
          description: ok
          headers:
            X-String: {$ref: '#/components/headers/StringHeader'}
            x-MiXeD: {schema: {type: string}}
            X-Integer: {schema: {type: integer, minimum: 0}}
            X-Number: {schema: {type: number}}
            X-Boolean: {schema: {type: boolean}}
            X-Optional: {schema: {type: string}}
            X-Canonical: {schema: {type: string, enum: [allowed]}}
            X-Enum: {schema: {type: string, enum: [alpha, beta]}}
            X-Composed: {schema: {type: string, allOf: [{type: string, enum: [allowed]}]}}
            X-Array: {schema: {type: array, items: {type: string, enum: [alpha, beta]}}}
            X-Integer-Array: {schema: {type: array, items: {type: integer, minimum: 0}}}
            X-Untyped: {schema: {}}
          content: {application/json: {schema: {type: string}}}
        '304':
          description: not modified
          headers:
            X-String: {schema: {type: string}}
            X-Untyped: {schema: {}}
  /fallback:
    get:
      responses:
        '200':
          description: exact
          headers: {X-Mode: {schema: {type: string}}}
        '2XX':
          description: range
          headers: {X-Mode: {schema: {type: array, items: {type: string}}}}
        default:
          description: fallback
          headers: {X-Mode: {schema: {type: string}}}
  /head:
    head:
      responses:
        '200':
          description: head
          headers: {X-Mode: {schema: {type: string}}}
components:
  headers:
    StringHeader:
      required: false
      schema: {$ref: '#/components/schemas/StringValue'}
  schemas:
    StringValue: {type: string, enum: [allowed]}
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

func TestRequestValidationBoundsBodyBeforeReadingIt(t *testing.T) {
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("oversized request reached handler")
	}), nil, false)
	if err != nil {
		t.Fatal(err)
	}

	payload := `{"name":"` + strings.Repeat("a", int(maxValidatedRequestBodyBytes)) + `"}`
	body := &countingReadCloser{reader: strings.NewReader(payload)}
	request := httptest.NewRequest(http.MethodPost, "/things", nil)
	request.Body = body
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if body.read != maxValidatedRequestBodyBytes+1 {
		t.Fatalf("validator read %d body bytes, want exactly %d", body.read, maxValidatedRequestBodyBytes+1)
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
	}), nil, true, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
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

// The drift log is a SECOND emitter of the raw request path, independent of
// obs.RequestLogger — a fix applied only to the request logger leaves this one
// writing the capability at ERROR (TKT-202, ADR-012).
//
// Driven through responseValidated directly rather than the test spec's routes:
// the mechanism under test is the sanitiser call on the log field, not routing,
// and the shared spec declares no capability-shaped path.
//
// Asserting the exact sanitized value, not merely the reference's absence — a
// logger that dropped the path entirely would satisfy "absent" while destroying
// the diagnosability ADR-028 requires of this line.
//
// Mutation this must catch: drop the obs.SanitizedPath call in responseValidated.
func TestResponseDriftLogSanitizesCapabilityPaths(t *testing.T) {
	const ref = "2f1e3d4c-5b6a-4978-8899-aabbccddeeff"

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	_, router, err := load(testSpec)
	if err != nil {
		t.Fatal(err)
	}
	handler := responseValidated(router, driftingHandler(), log,
		func(context.Context, *openapi3filter.ResponseValidationInput) error {
			return errors.New("drift")
		})

	request := httptest.NewRequest(http.MethodGet, "/api/access/orders/"+ref+"/tickets", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if buf.Len() == 0 {
		t.Fatal("no drift log emitted — this test would prove nothing")
	}
	if strings.Contains(buf.String(), ref) {
		t.Errorf("contract drift log leaks the guest reference: %s", buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("not JSON: %q", buf.String())
	}
	if entry["path"] != "/api/access/orders/:capability/tickets" {
		t.Errorf("path = %v, want the route shape with the capability replaced", entry["path"])
	}
	if entry["method"] != http.MethodGet {
		t.Errorf("method = %v — the line must stay diagnosable", entry["method"])
	}
}

func TestResponseHeaderCardinality(t *testing.T) {
	tests := []struct {
		name, header  string
		status, count int
		set           func(http.Header)
	}{
		{name: "string reference refuses two values", header: "X-String", count: 2, set: func(h http.Header) { h.Add("X-String", "allowed"); h.Add("X-String", "forbidden") }},
		{name: "integer refuses two values", header: "X-Integer", count: 2, set: func(h http.Header) { h.Add("X-Integer", "0"); h.Add("X-Integer", "1") }},
		{name: "number refuses two values", header: "X-Number", count: 2, set: func(h http.Header) { h.Add("X-Number", "1"); h.Add("X-Number", "2") }},
		{name: "boolean refuses two values", header: "X-Boolean", count: 2, set: func(h http.Header) { h.Add("X-Boolean", "true"); h.Add("X-Boolean", "false") }},
		{name: "optional primitive refuses two values", header: "X-Optional", count: 2, set: func(h http.Header) { h.Add("X-Optional", "one"); h.Add("X-Optional", "two") }},
		{name: "enum refuses two allowed values", header: "X-Enum", count: 2, set: func(h http.Header) { h.Add("X-Enum", "alpha"); h.Add("X-Enum", "beta") }},
		{name: "enum refuses identical values", header: "X-Enum", count: 2, set: func(h http.Header) { h.Add("X-Enum", "alpha"); h.Add("X-Enum", "alpha") }},
		{name: "composed string refuses two valid values", header: "X-Composed", count: 2, set: func(h http.Header) { h.Add("X-Composed", "allowed"); h.Add("X-Composed", "allowed") }},
		{name: "three values are refused", header: "X-Integer", count: 3, set: func(h http.Header) { h.Add("X-Integer", "0"); h.Add("X-Integer", "1"); h.Add("X-Integer", "2") }},
		{name: "split map key casing is counted", header: "X-String", count: 2, set: func(h http.Header) { h["X-String"] = []string{"allowed"}; h["x-string"] = []string{"allowed"} }},
		{name: "mixed-case declaration is counted", header: "x-MiXeD", count: 2, set: func(h http.Header) { h.Add("x-mixed", "one"); h.Add("x-mixed", "two") }},
		{name: "head response is checked", header: "X-Mode", count: 2, status: http.StatusOK, set: func(h http.Header) { h.Add("X-Mode", "one"); h.Add("X-Mode", "two") }},
		{name: "not modified response is checked", header: "X-String", count: 2, status: http.StatusNotModified, set: func(h http.Header) { h.Add("X-String", "allowed"); h.Add("X-String", "allowed") }},
		{name: "exact status wins over range and default", header: "X-Mode", count: 2, status: http.StatusOK, set: func(h http.Header) { h.Add("X-Mode", "one"); h.Add("X-Mode", "two") }},
		{name: "default response selected when no exact or range", header: "X-Mode", count: 2, status: http.StatusNotFound, set: func(h http.Header) { h.Add("X-Mode", "one"); h.Add("X-Mode", "two") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			handler, err := ResponseValidator(cardinalitySpec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.set(w.Header())
				w.Header().Set("X-Handler-Only", "secret")
				if r.URL.Path == "/headers" {
					w.Header().Set("Content-Type", "application/json")
				}
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `"original body"`)
			}), log, true)
			if err != nil {
				t.Fatal(err)
			}
			path := "/headers"
			method := http.MethodGet
			if strings.Contains(tc.name, "head response") {
				path, method = "/head", http.MethodHead
			} else if strings.Contains(tc.name, "exact status") || strings.Contains(tc.name, "default response") {
				path = "/fallback"
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
			}
			if got, want := rec.Body.String(), "{\"error\":\"response violates OpenAPI contract\"}\n"; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
				t.Fatalf("Cache-Control = %v", got)
			}
			if rec.Header().Get("X-Handler-Only") != "" || strings.Contains(rec.Body.String(), "original body") {
				t.Fatalf("handler response leaked: headers=%v body=%q", rec.Header(), rec.Body.String())
			}
			if strings.Count(logs.String(), "\n") != 1 {
				t.Fatalf("want exactly one drift log, got %q", logs.String())
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("decode drift log: %v", err)
			}
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			if entry["level"] != "ERROR" || entry["method"] != method || entry["path"] != path || entry["status"] != float64(status) {
				t.Fatalf("drift log = %v", entry)
			}
			if value, _ := entry["error"].(string); !strings.Contains(value, tc.header) || !strings.Contains(value, fmt.Sprintf("%d field values", tc.count)) {
				t.Fatalf("drift log does not name the header and count: %v", entry)
			}
		})
	}

	for _, tc := range []struct {
		name, path, header string
		status, wantStatus int
		headers            func(http.Header)
		want               []string
	}{
		{name: "one comma value is one field value", path: "/headers", header: "X-Optional", status: 200, headers: func(h http.Header) { h.Set("X-Optional", "one, two") }, want: []string{"one, two"}},
		{name: "array accepts multiple field values", path: "/headers", header: "X-Array", status: 200, headers: func(h http.Header) { h.Add("X-Array", "alpha"); h.Add("X-Array", "beta") }, want: []string{"alpha", "beta"}},
		{name: "array accepts one comma-separated field value", path: "/headers", header: "X-Array", status: 200, headers: func(h http.Header) { h.Set("X-Array", "alpha,beta") }, want: []string{"alpha,beta"}},
		{name: "array validates first field and leaves later field alone", path: "/headers", header: "X-Array", status: 200, headers: func(h http.Header) { h.Add("X-Array", "alpha"); h.Add("X-Array", "forbidden") }, want: []string{"alpha", "forbidden"}},
		{name: "integer array ignores invalid later field as before", path: "/headers", header: "X-Integer-Array", status: 200, headers: func(h http.Header) { h.Add("X-Integer-Array", "1"); h.Add("X-Integer-Array", "invalid") }, want: []string{"1", "invalid"}},
		{name: "untyped optional declaration with no value remains unchanged", path: "/headers", status: 200, headers: func(http.Header) {}},
		{name: "untyped header preserves duplicate values on not modified response", path: "/headers", header: "X-Untyped", status: http.StatusNotModified, headers: func(h http.Header) { h.Add("X-Untyped", "one"); h.Add("X-Untyped", "two") }, want: []string{"one", "two"}},
		{name: "single noncanonical key retains value validation limitation", path: "/headers", header: "X-Canonical", status: 200, headers: func(h http.Header) { h["x-canonical"] = []string{"invalid"} }, want: []string{"invalid"}},
		{name: "composed string accepts one value", path: "/headers", header: "X-Composed", status: 200, headers: func(h http.Header) { h.Set("X-Composed", "allowed") }, want: []string{"allowed"}},
		{name: "different declared headers each accept one value", path: "/headers", status: 200, headers: func(h http.Header) { h.Set("X-String", "allowed"); h.Set("X-Integer", "0") }},
		{name: "range response selected before default", path: "/fallback", header: "X-Mode", status: 201, headers: func(h http.Header) { h.Add("X-Mode", "one"); h.Add("X-Mode", "two") }, want: []string{"one", "two"}},
		{name: "invalid first array field still fails ordinary validation", path: "/headers", header: "X-Array", status: 200, wantStatus: http.StatusInternalServerError, headers: func(h http.Header) { h.Add("X-Array", "forbidden"); h.Add("X-Array", "alpha") }},
	} {
		t.Run("control/"+tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler, err := ResponseValidator(cardinalitySpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.headers(w.Header())
				if tc.path == "/headers" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				if tc.path == "/headers" {
					_, _ = io.WriteString(w, `"original body"`)
				} else {
					_, _ = io.WriteString(w, "original body")
				}
			}), slog.New(slog.NewJSONHandler(&logs, nil)), true)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			wantStatus := tc.wantStatus
			if wantStatus == 0 {
				wantStatus = tc.status
			}
			if rec.Code != wantStatus {
				t.Fatalf("status = %d, want %d; body=%q logs=%q", rec.Code, wantStatus, rec.Body.String(), logs.String())
			}
			if wantStatus == http.StatusInternalServerError {
				return
			}
			got := rec.Header().Values(tc.header)
			if tc.want != nil && !slices.Equal(got, tc.want) {
				t.Fatalf("%s values = %v, want %v", tc.header, got, tc.want)
			}
			wantBody := "original body"
			if tc.path == "/headers" {
				wantBody = `"original body"`
			}
			if rec.Body.String() != wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), wantBody)
			}
			if logs.Len() != 0 {
				t.Fatalf("successful response logged drift: %q", logs.String())
			}
		})
	}
}

func TestRequestHeaderCardinalityUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name       string
		values     []string
		wantStatus int
	}{
		{name: "valid first value reaches Header Get despite invalid second", values: []string{"allowed", "forbidden"}, wantStatus: http.StatusOK},
		{name: "invalid first value still fails", values: []string{"forbidden", "allowed"}, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			handler, err := RequestValidator(cardinalitySpec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("X-Request")
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-String", "allowed")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `"ok"`)
			}), nil, true)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/headers", nil)
			for _, value := range tc.values {
				req.Header.Add("X-Request", value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK && seen != "allowed" {
				t.Fatalf("handler saw %q", seen)
			}
		})
	}
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("response validation enabled=%v", enabled), func(t *testing.T) {
			var logs bytes.Buffer
			handler, err := RequestValidator(cardinalitySpec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Add("X-String", "allowed")
				w.Header().Add("X-String", "allowed")
				_, _ = io.WriteString(w, `"ok"`)
			}), slog.New(slog.NewJSONHandler(&logs, nil)), enabled)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/headers", nil))
			if enabled {
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("enabled response validation status = %d", rec.Code)
				}
			} else {
				if rec.Code != http.StatusOK || len(rec.Header().Values("X-String")) != 2 {
					t.Fatalf("disabled response changed duplicate values: %d %v", rec.Code, rec.Header().Values("X-String"))
				}
				if logs.Len() != 0 {
					t.Fatalf("disabled response validation logged: %q", logs.String())
				}
			}
		})
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

// The handler receives the request, so a service whose established error representation
// suits one route family can answer differently for another (TKT-157 ai-review F4).
// Without it, access emitted its gate-shaped 422 on an internal route declaring no 422 —
// an undeclared status reached through the one path that runs BEFORE the response
// validator, so nothing downstream could catch it.
func TestCustomRequestValidationErrorSeesTheRequest(t *testing.T) {
	handler, err := RequestValidatorWithErrorHandler(testSpec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached handler")
	}), nil, true, func(w http.ResponseWriter, r *http.Request, _ string, status int) {
		if r == nil {
			t.Fatal("error handler received no request")
		}
		writeValidationError(w, status, map[string]string{"path": r.URL.Path})
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(`{"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), `"path":"/things"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

// Services that pass no custom handler must be unaffected by the request-aware hook
// existing at all (TKT-157 ai-review pass 2). nethttp-middleware v1.1.2 routes the two
// hooks through different code: the newer one hard-codes 404 for every route-lookup
// failure, while the legacy one distinguishes ErrMethodNotAllowed as 405. Switching
// everyone to the newer hook to serve one service would have changed wrong-method
// responses platform-wide, silently — nothing else in the gate looks at them.
func TestNilErrorHandlerKeepsLegacyValidationStatuses(t *testing.T) {
	handler, err := RequestValidator(testSpec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached handler")
	}), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"wrong method on a known path", http.MethodDelete, "/things", http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/nothing-here", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `"error"`) {
				t.Fatalf("body lost its Error shape: %s", recorder.Body.String())
			}
		})
	}
}
