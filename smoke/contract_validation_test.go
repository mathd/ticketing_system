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
	if !found || !strings.Contains(" catalog inventory commerce payments access ", " "+service+" ") || path == "openapi.yaml" {
		return
	}
	// These 404s are the gateway security boundary, not service responses:
	// the gateway NotFounds every /api/<svc>/internal/* by construction.
	if status == http.StatusNotFound && strings.HasPrefix(path, "internal/") {
		return
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
	recordSmokeCoverage(service, route.Operation.OperationID, status)
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

// validateDirectServiceResponse validates a response obtained by calling a service
// directly (not through the gateway): internal routes are deliberately 404 at the edge,
// so validateServiceResponse's gateway-path parsing never sees them.
func validateDirectServiceResponse(t *testing.T, service string, request *http.Request, status int, header http.Header, body []byte) {
	t.Helper()
	contract := loadContract(service)
	if contract.err != nil {
		t.Fatalf("load %s contract: %v", service, contract.err)
	}
	route, params, err := contract.router.FindRoute(request)
	if err != nil {
		t.Fatalf("%s %s is not committed in the %s contract: %v", request.Method, request.URL.Path, service, err)
	}
	recordSmokeCoverage(service, route.Operation.OperationID, status)
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: request, PathParams: params, Route: route},
		Status:                 status,
		Header:                 header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
	}
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Fatalf("%s %s response %d violates the %s contract: %v; body=%s", request.Method, request.URL.Path, status, service, err, body)
	}
}

func TestCommittedServiceContractsAreComplete(t *testing.T) {
	for _, service := range []string{"catalog", "inventory", "commerce", "payments", "access"} {
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

// TestGatewayDeniesGenericInternalRoutes drives raw http.Client on purpose: these
// responses are refusals, not service responses, so they must not go through
// validateServiceResponse (a path the gateway refuses has no operation to validate
// against, and the helper would fail on the contract lookup rather than on the status).
//
// Two different controls are asserted here and they prove different things (TKT-124):
//
//   - /api/<svc>/internal/... — refused AT THE EDGE by the gateway's explicit
//     prefix+"internal/" NotFoundHandler registration, before any proxying. This is the
//     boundary. It holds for routes that exist and are simply not public.
//   - the retired /api/inventory/holds/{id}/<transition> paths — refused because the
//     route no longer exists: the gateway proxies them, and inventory's OpenAPI request
//     validator 404s a path absent from its spec. This proves the old public surface is
//     gone and no compatibility alias came back — it is NOT an edge control.
//
// Both are 404. Only the first is the security boundary; conflating them is exactly the
// overclaim docs/learnings/2026-07-15-name-what-a-control-reaches.md warns about — so
// each case asserts the 404's BODY, which says which control produced it. Status alone
// is a vacuous proof here: an unguarded legacy alias handed a hold id that does not
// exist would answer 404 from the store (ErrNotFound) and pass a status-only assertion
// while the public mutation surface was back (ai-review F5).
func TestGatewayDeniesGenericInternalRoutes(t *testing.T) {
	const holdID = "00000000-0000-0000-0000-000000000001"
	// The gateway emits a refusal body only it emits; inventory's request validator
	// emits the contract error. Distinguishing them is the whole point, and it only
	// works because the gateway's body is NOT http.NotFound's generic "404 page not
	// found" — which chi and every service can also produce, so asserting it would have
	// looked like provenance and proved nothing (ai-review pass 2, F1).
	// Kept in sync by gateway/cmd/gateway/main.go's edgeDeniedBody.
	const byGateway = `{"error":"refused at the gateway edge"}`
	const byServiceContract = `{"error":"no matching operation was found"}`
	tests := []struct {
		method   string
		path     string
		wantBody string
	}{
		{http.MethodGet, "/api/catalog/internal/ticket-types/00000000-0000-0000-0000-000000000001", byGateway},
		{http.MethodGet, "/api/commerce/internal/buyers/00000000-0000-0000-0000-000000000001/delivery-email", byGateway},
		{http.MethodPost, "/api/payments/internal/facts", byGateway},
		{http.MethodPost, "/api/inventory/internal/slots/00000000-0000-0000-0000-000000000001/capacity-adjustments", byGateway},
		// The boundary, on the transitions themselves.
		{http.MethodPost, "/api/inventory/internal/holds/" + holdID + "/confirm", byGateway},
		{http.MethodPost, "/api/inventory/internal/holds/" + holdID + "/finalize", byGateway},
		{http.MethodPost, "/api/inventory/internal/holds/" + holdID + "/release", byGateway},
		// An encoded separator must not walk past the boundary either: ServeMux reads
		// "internal%2Fholds" as one segment and misses the literal "internal" child,
		// while the proxy forwards the decoded path. Refused by denyEncodedSeparators,
		// so this is the gateway's own 404 (ai-review F1).
		{http.MethodPost, "/api/inventory/internal%2Fholds/" + holdID + "/confirm", byGateway},
		{http.MethodPost, "/api/inventory/internal%2fslots/" + holdID + "/capacity-adjustments", byGateway},
		// The retired public paths. organizer_id is valid so a surviving route could not
		// hide behind a validation 400, and the body assertion is what makes this fail if
		// an alias came back: a restored route answers from the store, not the contract.
		{http.MethodPost, "/api/inventory/holds/" + holdID + "/confirm?organizer_id=" + organizerID, byServiceContract},
		{http.MethodPost, "/api/inventory/holds/" + holdID + "/finalize?organizer_id=" + organizerID, byServiceContract},
		{http.MethodPost, "/api/inventory/holds/" + holdID + "/release?organizer_id=" + organizerID, byServiceContract},
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
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", response.StatusCode, body)
			}
			// Exact, not substring: the point is which layer answered, and a
			// substring match would accept a body that merely embeds the marker.
			if got := strings.TrimSpace(string(body)); got != test.wantBody {
				t.Fatalf("404 came from the wrong layer: body=%q, want exactly %q", got, test.wantBody)
			}
			if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("content-type=%q, want application/json — a refusal from an unexpected layer", got)
			}
		})
	}
}
