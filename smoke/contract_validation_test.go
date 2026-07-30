//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// The chokepoints come in three layers (TKT-95):
//
//	checkServiceResponse / checkDirectServiceResponse  — testing.T-free cores; record
//	  coverage and RETURN the violation. Callable from any goroutine.
//	validateServiceResponse / validateDirectServiceResponse — test-goroutine wrappers:
//	  t.Fatal, which is what every sequential helper wants.
//	validateServiceResponseAsync / validateDirectServiceResponseAsync — goroutine-safe
//	  wrappers: t.Error. T.FailNow (and so t.Fatal) must run on the test goroutine;
//	  T.Error must not, which is why the concurrent helpers report through these.
//
// The split exists so concurrent helpers can validate at all: before TKT-95 the
// hold/scan helpers skipped validation entirely — the only way to stay legal in a
// goroutine was to not call a t.Fatal-ing validator — and that raw traffic was
// therefore invisible to the coverage gate.
//
// Coverage is recorded BEFORE response validation, deliberately: a drifted response
// counts as covered and is then reported as a violation by the caller, so it can
// never produce a green run. Flipping the order would report a violating response as
// *uncovered*, pointing the reader at a missing driver instead of at the drift.
func checkServiceResponse(request *http.Request, status int, header http.Header, body []byte) error {
	marker := "/api/"
	index := strings.Index(request.URL.Path, marker)
	if index < 0 {
		return nil
	}
	remainder := request.URL.Path[index+len(marker):]
	service, path, found := strings.Cut(remainder, "/")
	if !found || !strings.Contains(" catalog inventory commerce payments access ", " "+service+" ") || path == "openapi.yaml" {
		return nil
	}
	// These 404s are the gateway security boundary, not service responses:
	// the gateway NotFounds every /api/<svc>/internal/* by construction.
	if status == http.StatusNotFound && strings.HasPrefix(path, "internal/") {
		return nil
	}
	contract := loadContract(service)
	if contract.err != nil {
		return fmt.Errorf("load %s contract: %w", service, contract.err)
	}
	copyRequest := request.Clone(request.Context())
	copyURL := *request.URL
	copyURL.Path = "/" + path
	copyRequest.URL = &copyURL
	route, params, err := contract.router.FindRoute(copyRequest)
	if err != nil {
		return fmt.Errorf("%s %s is not committed in the %s contract: %w", request.Method, copyURL.Path, service, err)
	}
	recordSmokeCoverage(service, route.Operation.OperationID, status)
	if err := validateAgainstRoute(copyRequest, route, params, status, header, body); err != nil {
		return fmt.Errorf("%s %s response %d violates the %s contract: %w; body=%s", request.Method, copyURL.Path, status, service, err, body)
	}
	return nil
}

// checkDirectServiceResponse validates a response obtained by calling a service
// directly (not through the gateway): internal routes are deliberately 404 at the edge,
// so checkServiceResponse's gateway-path parsing never sees them.
func checkDirectServiceResponse(service string, request *http.Request, status int, header http.Header, body []byte) error {
	contract := loadContract(service)
	if contract.err != nil {
		return fmt.Errorf("load %s contract: %w", service, contract.err)
	}
	route, params, err := contract.router.FindRoute(request)
	if err != nil {
		return fmt.Errorf("%s %s is not committed in the %s contract: %w", request.Method, request.URL.Path, service, err)
	}
	recordSmokeCoverage(service, route.Operation.OperationID, status)
	if err := validateAgainstRoute(request, route, params, status, header, body); err != nil {
		return fmt.Errorf("%s %s response %d violates the %s contract: %w; body=%s", request.Method, request.URL.Path, status, service, err, body)
	}
	return nil
}

func validateAgainstRoute(request *http.Request, route *routers.Route, params map[string]string, status int, header http.Header, body []byte) error {
	return openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: request, PathParams: params, Route: route},
		Status:                 status,
		Header:                 header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
	})
}

func validateServiceResponse(t *testing.T, request *http.Request, status int, header http.Header, body []byte) {
	t.Helper()
	if err := checkServiceResponse(request, status, header, body); err != nil {
		t.Fatal(err)
	}
}

// validateServiceResponseAsync is for helpers called from inside a `go func`. No
// t.Helper(): the caller is not the test goroutine, so the helper-frame skip would
// attribute the failure to the wrong line. t.Error marks the test failed and returns,
// which is what a contention test needs — its remaining goroutines still have to be
// joined. Every such goroutine MUST be joined (WaitGroup or channel receive) before its
// test function returns: t.Error after the test completes panics. The contention,
// group-reservation and operational-hold races each carry a deferred join for exactly
// that reason.
//
// The direct path has no wrappers, Fatal or Async: both of its callers want the error
// itself rather than a report. internalRequest takes its reporter (t.Fatalf or t.Errorf)
// as a parameter so one implementation serves internalJSON and internalJSONAsync, and
// timedPost returns the violation to the load harness so an invalid response can be
// classified as a server error instead of entering the OK samples. Wrappers there would
// be dead code.
func validateServiceResponseAsync(t *testing.T, request *http.Request, status int, header http.Header, body []byte) {
	if err := checkServiceResponse(request, status, header, body); err != nil {
		t.Error(err)
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

// TestConcurrentPostValidatesAndRecordsCoverage pins the TKT-95 half that is not
// observable from any feature test: traffic sent by a helper called from INSIDE a
// goroutine reaches the contract chokepoint, so it is contract-validated and counted by
// the coverage gate.
//
// Before TKT-95 it could not: the chokepoints only came in a t.Fatal flavour, T.FailNow
// is illegal off the test goroutine, and so the concurrent hold/scan helpers sent raw
// unvalidated traffic. coverage_test.go's scope comment had to disclaim exactly that.
//
// Stack-free on purpose (httptest.Server, no compose stack): the property under test is
// "the async path reaches the chokepoint", which needs a response, not a service. The
// arrangement mirrors TestValidateServiceResponseCoversCatalog — snapshot, clear, act,
// assert *this call* re-recorded, restore — for the same reason: another test may already
// have recorded the operation, which would make a plain "is it present" check pass
// vacuously.
//
// What this does NOT prove: that a *violating* concurrent response fails the suite.
// Asserting that would need a test that fails itself or a child test binary. The guard
// there is that validateServiceResponseAsync is four lines wrapping the same
// checkServiceResponse this test exercises, differing only in t.Error vs t.Fatal.
func TestConcurrentPostValidatesAndRecordsCoverage(t *testing.T) {
	const op = "inventory createHold"

	// A contract-valid inventory createHold 201: every required Hold field present
	// (hold_id, organizer_id, slot_id, quantity, status, expires_at, server_time), and
	// the schema is additionalProperties:false, so nothing extra.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"hold_id":"11111111-1111-1111-1111-111111111111",` +
			`"organizer_id":"00000000-0000-0000-0000-000000000001",` +
			`"slot_id":"22222222-2222-2222-2222-222222222222",` +
			`"quantity":1,"status":"held",` +
			`"expires_at":"2026-07-29T20:00:00Z","server_time":"2026-07-29T19:00:00Z"}`))
	}))
	defer server.Close()

	// Restore the EXACT prior state, both ways. Setting it back only when prior was true
	// would leave this stub's bit in the map when it was false — and inventory IS in
	// smokeCoverageGatedServices, so TestMain would then count "inventory createHold" as
	// covered on the strength of an httptest response. The gate's whole claim is
	// real-handler/real-store coverage, and a stub-satisfied bit is indistinguishable
	// from a genuine one.
	smokeCoverageMu.Lock()
	prior := smokeCoverage[op]
	delete(smokeCoverage, op)
	smokeCoverageMu.Unlock()
	defer func() {
		smokeCoverageMu.Lock()
		if prior {
			smokeCoverage[op] = true
		} else {
			delete(smokeCoverage, op)
		}
		smokeCoverageMu.Unlock()
	}()

	// The call under test runs in a goroutine and is joined before this test returns —
	// the invariant every async helper depends on (t.Error after the test completes
	// panics).
	var wg sync.WaitGroup
	wg.Add(1)
	var code int
	go func() {
		defer wg.Done()
		code, _ = postWithKeyAsync(t, server.URL+"/api/inventory/holds", "tkt95-async", map[string]any{
			"organizer_id": "00000000-0000-0000-0000-000000000001",
			"slot_id":      "22222222-2222-2222-2222-222222222222",
			"quantity":     1,
		})
	}()
	wg.Wait()

	if code != http.StatusCreated {
		t.Fatalf("fixture served %d, want 201", code)
	}
	smokeCoverageMu.Lock()
	recorded := smokeCoverage[op]
	smokeCoverageMu.Unlock()
	if !recorded {
		t.Fatalf("a goroutine-issued request did not reach the contract chokepoint: %q not recorded by this call. "+
			"postWithKeyAsync must validate through validateServiceResponseAsync (TKT-95).", op)
	}
}
