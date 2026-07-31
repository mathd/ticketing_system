// Package contract provides the shared OpenAPI enforcement seam used by the
// manually implemented service routers.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	oapimiddleware "github.com/oapi-codegen/nethttp-middleware"
)

// RequestValidator validates every documented service request. The served
// source document is deliberately bypassed because it is checked byte-for-byte
// by the smoke gate and is not part of the service's JSON operation surface.
// Requests are validated unconditionally — they are a trust boundary. When
// validateResponses is set, response drift additionally fails closed with 500
// (ADR-028) and is logged through log (nil falls back to slog.Default());
// where it is not set, the handler's response reaches the client untouched.
// Callers get the flag from runtimecfg.ResponseValidationFromEnv, which
// defaults it on (TKT-125).
func RequestValidator(spec []byte, next http.Handler, log *slog.Logger, validateResponses bool) (http.Handler, error) {
	return requestValidator(spec, next, log, validateResponses, nil)
}

// RequestValidatorWithErrorHandler lets a service preserve an established
// error representation for requests rejected before its handler runs.
//
// The handler receives the REQUEST, not just the message: a service's established
// representation is rarely the right answer for every route it serves, and a handler
// that cannot see which route it is answering for can only answer uniformly. Access
// learned this by emitting its gate-shaped 422 on an internal route that declares no
// 422 — an undeclared status, which is precisely the drift the response validator
// exists to catch, reached through the one path that runs before it (TKT-157 ai-review).
func RequestValidatorWithErrorHandler(spec []byte, next http.Handler, log *slog.Logger, validateResponses bool, errorHandler func(http.ResponseWriter, *http.Request, string, int)) (http.Handler, error) {
	return requestValidator(spec, next, log, validateResponses, errorHandler)
}

// ResponseValidator wraps next in response-drift enforcement only, for routers
// that already run their own request validation (catalog). Routes absent from
// the spec pass through untouched. The spec is loaded and validated either
// way: validateResponses governs enforcement, not whether the contract parses.
func ResponseValidator(spec []byte, next http.Handler, log *slog.Logger, validateResponses bool) (http.Handler, error) {
	_, router, err := load(spec)
	if err != nil {
		return nil, err
	}
	if !validateResponses {
		return next, nil
	}
	validated := responseValidated(router, next, log, openapi3filter.ValidateResponse)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.yaml" {
			next.ServeHTTP(w, r)
			return
		}
		validated.ServeHTTP(w, r)
	}), nil
}

func load(spec []byte) (*openapi3.T, routers.Router, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("load OpenAPI contract: %w", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return nil, nil, fmt.Errorf("validate OpenAPI contract: %w", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("build OpenAPI response router: %w", err)
	}
	return doc, router, nil
}

// validateResponse is openapi3filter.ValidateResponse in production. It is a
// parameter so a test can observe which context the validator actually runs
// on — the drift log already carried the request's trace, so asserting on the
// log could not have caught the background context (TKT-125). Note kin-openapi
// v0.142.0 accepts the ctx and never reads it, so passing the request context
// buys nothing observable today; it is correctness against a future version.
type validateResponse func(context.Context, *openapi3filter.ResponseValidationInput) error

// responseValidated buffers next's response and fails closed on contract
// drift: the drifted payload never reaches the client, and the violation is
// logged with the operation coordinates so the 500 is diagnosable (ADR-028).
// Buffering is why a handler behind this wrap can never stream: the response
// has to be withheld until the validator has accepted it.
func responseValidated(router routers.Router, next http.Handler, log *slog.Logger, validate validateResponse) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		route, params, routeErr := router.FindRoute(r)
		if routeErr == nil {
			input := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route},
				Status:                 recorder.Code,
				Header:                 recorder.Header(),
				Body:                   io.NopCloser(bytes.NewReader(recorder.Body.Bytes())),
				// An undocumented status is drift too (ADR-028); kin-openapi
				// allows it unless told otherwise.
				Options: &openapi3filter.Options{IncludeResponseStatus: true},
			}
			if validationErr := validate(r.Context(), input); validationErr != nil {
				logger := log
				if logger == nil {
					logger = slog.Default()
				}
				logger.ErrorContext(r.Context(), "response violates OpenAPI contract",
					"method", r.Method, "path", r.URL.Path, "status", recorder.Code, "error", validationErr.Error())
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "response violates OpenAPI contract"})
				return
			}
		}
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
}

func requestValidator(spec []byte, next http.Handler, log *slog.Logger, validateResponses bool, errorHandler func(http.ResponseWriter, *http.Request, string, int)) (http.Handler, error) {
	doc, router, err := load(spec)
	if err != nil {
		return nil, err
	}
	validator := oapimiddleware.OapiRequestValidatorWithOptions(doc, &oapimiddleware.Options{
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, r *http.Request, opts oapimiddleware.ErrorHandlerOpts) {
			if errorHandler != nil {
				errorHandler(w, r, err.Error(), opts.StatusCode)
				return
			}
			writeValidationError(w, opts.StatusCode, map[string]string{"error": err.Error()})
		},
	})
	inner := next
	if validateResponses {
		inner = responseValidated(router, next, log, openapi3filter.ValidateResponse)
	}
	validatedHandler := validator(inner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.yaml" {
			next.ServeHTTP(w, r)
			return
		}
		validatedHandler.ServeHTTP(w, r)
	}), nil
}

func writeValidationError(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
