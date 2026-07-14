// Package contract provides the shared OpenAPI enforcement seam used by the
// manually implemented service routers.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	oapimiddleware "github.com/oapi-codegen/nethttp-middleware"
)

// RequestValidator validates every documented service request. The served
// source document is deliberately bypassed because it is checked byte-for-byte
// by the smoke gate and is not part of the service's JSON operation surface.
func RequestValidator(spec []byte, next http.Handler) (http.Handler, error) {
	return requestValidator(spec, next, nil)
}

// RequestValidatorWithErrorStatus preserves an endpoint family's established
// validation status while still applying the shared OpenAPI validator.
func RequestValidatorWithErrorStatus(spec []byte, next http.Handler, errorStatus int) (http.Handler, error) {
	return RequestValidatorWithErrorHandler(spec, next, func(w http.ResponseWriter, message string, _ int) {
		writeValidationError(w, errorStatus, map[string]string{"error": message})
	})
}

// RequestValidatorWithErrorHandler lets a service preserve an established
// error representation for requests rejected before its handler runs.
func RequestValidatorWithErrorHandler(spec []byte, next http.Handler, errorHandler func(http.ResponseWriter, string, int)) (http.Handler, error) {
	return requestValidator(spec, next, errorHandler)
}

func requestValidator(spec []byte, next http.Handler, errorHandler func(http.ResponseWriter, string, int)) (http.Handler, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(spec)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI contract: %w", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return nil, fmt.Errorf("validate OpenAPI contract: %w", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("build OpenAPI response router: %w", err)
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
	responseValidated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		route, params, routeErr := router.FindRoute(r)
		if routeErr == nil {
			input := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route},
				Status:                 recorder.Code,
				Header:                 recorder.Header(),
				Body:                   io.NopCloser(bytes.NewReader(recorder.Body.Bytes())),
			}
			if validationErr := openapi3filter.ValidateResponse(context.Background(), input); validationErr != nil {
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
	validatedHandler := validator(responseValidated)
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
