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
// Response drift fails closed with 500 (ADR-028) and is logged through log
// (nil falls back to slog.Default()).
func RequestValidator(spec []byte, next http.Handler, log *slog.Logger) (http.Handler, error) {
	return requestValidator(spec, next, log, nil)
}

// RequestValidatorWithErrorHandler lets a service preserve an established
// error representation for requests rejected before its handler runs.
func RequestValidatorWithErrorHandler(spec []byte, next http.Handler, log *slog.Logger, errorHandler func(http.ResponseWriter, string, int)) (http.Handler, error) {
	return requestValidator(spec, next, log, errorHandler)
}

// ResponseValidator wraps next in response-drift enforcement only, for routers
// that already run their own request validation (catalog). Routes absent from
// the spec pass through untouched.
func ResponseValidator(spec []byte, next http.Handler, log *slog.Logger) (http.Handler, error) {
	_, router, err := load(spec)
	if err != nil {
		return nil, err
	}
	validated := responseValidated(router, next, log)
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

// responseValidated buffers next's response and fails closed on contract
// drift: the drifted payload never reaches the client, and the violation is
// logged with the operation coordinates so the 500 is diagnosable (ADR-028).
func responseValidated(router routers.Router, next http.Handler, log *slog.Logger) http.Handler {
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
			if validationErr := openapi3filter.ValidateResponse(context.Background(), input); validationErr != nil {
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

func requestValidator(spec []byte, next http.Handler, log *slog.Logger, errorHandler func(http.ResponseWriter, string, int)) (http.Handler, error) {
	doc, router, err := load(spec)
	if err != nil {
		return nil, err
	}
	validator := oapimiddleware.OapiRequestValidatorWithOptions(doc, &oapimiddleware.Options{
		ErrorHandler: func(w http.ResponseWriter, message string, status int) {
			if errorHandler != nil {
				errorHandler(w, message, status)
				return
			}
			writeValidationError(w, status, map[string]string{"error": message})
		},
	})
	validatedHandler := validator(responseValidated(router, next, log))
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
