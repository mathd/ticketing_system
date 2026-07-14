//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

type loadedContract struct {
	doc    *openapi3.T
	router routers.Router
	err    error
}

var contractCache sync.Map

func loadContract(service string) loadedContract {
	if cached, ok := contractCache.Load(service); ok {
		return cached.(loadedContract)
	}
	data, err := os.ReadFile("../services/" + service + "/api/openapi.yaml")
	var loaded loadedContract
	if err == nil {
		loader := openapi3.NewLoader()
		loaded.doc, err = loader.LoadFromData(data)
		if err == nil {
			err = loaded.doc.Validate(loader.Context)
		}
		if err == nil {
			loaded.router, err = gorillamux.NewRouter(loaded.doc)
		}
	}
	loaded.err = err
	actual, _ := contractCache.LoadOrStore(service, loaded)
	return actual.(loadedContract)
}

func validateServiceResponse(t *testing.T, request *http.Request, status int, header http.Header, body []byte) {
	t.Helper()
	marker := "/api/"
	index := strings.Index(request.URL.Path, marker)
	if index < 0 {
		return
	}
	remainder := request.URL.Path[index+len(marker):]
	service, path, found := strings.Cut(remainder, "/")
	if !found || !strings.Contains(" inventory commerce payments access ", " "+service+" ") || path == "openapi.yaml" {
		return
	}
	// These 404s are the gateway security boundary, not service responses.
	if service == "inventory" && status == http.StatusNotFound {
		for _, suffix := range []string{"/confirm", "/finalize", "/release"} {
			if strings.HasSuffix(strings.TrimSuffix(request.URL.Path, "/"), suffix) {
				return
			}
		}
	}
	contract := loadContract(service)
	if contract.err != nil {
		t.Fatalf("load %s contract: %v", service, contract.err)
	}
	copyRequest := request.Clone(request.Context())
	copyURL := *request.URL
	copyURL.Path = "/" + path
	copyRequest.URL = &copyURL
	route, params, err := contract.router.FindRoute(copyRequest)
	if err != nil {
		t.Fatalf("%s %s is not committed in the %s contract: %v", request.Method, copyURL.Path, service, err)
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: copyRequest, PathParams: params, Route: route},
		Status:                 status,
		Header:                 header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Fatalf("%s %s response %d violates the %s contract: %v; body=%s", request.Method, copyURL.Path, status, service, err, body)
	}
}

func TestCommittedServiceContractsAreComplete(t *testing.T) {
	for _, service := range []string{"inventory", "commerce", "payments", "access"} {
		t.Run(service, func(t *testing.T) {
			contract := loadContract(service)
			if contract.err != nil {
				t.Fatal(contract.err)
			}
			for path, item := range contract.doc.Paths.Map() {
				for method, operation := range item.Operations() {
					if operation.OperationID == "" {
						t.Errorf("%s %s has no operationId", method, path)
					}
					for status, response := range operation.Responses.Map() {
						if response == nil || response.Value == nil || len(response.Value.Content) == 0 {
							t.Errorf("%s %s response %s has no committed body schema", method, path, status)
							continue
						}
						for mediaType, media := range response.Value.Content {
							if media == nil || media.Schema == nil || media.Schema.Value == nil {
								t.Errorf("%s %s response %s %s has no schema", method, path, status, mediaType)
							}
						}
					}
				}
			}
		})
	}
}

func TestGatewayDeniesGenericInternalRoutes(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/catalog/internal/ticket-types/00000000-0000-0000-0000-000000000001"},
		{http.MethodGet, "/api/commerce/internal/buyers/00000000-0000-0000-0000-000000000001/delivery-email"},
		{http.MethodPost, "/api/payments/internal/facts"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request, err := http.NewRequest(test.method, gatewayURL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := (&http.Client{}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", response.StatusCode)
			}
		})
	}
}
