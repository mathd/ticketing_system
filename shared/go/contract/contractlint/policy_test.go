package contractlint

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestPoliciesReportIndependentSources(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "openapi_gen.go"), `package api
type ServerInterface interface {
	// Status
	// (GET /status)
	Status()
	// Request
	// (GET /request)
	Request()
	// Store
	// (GET /store)
	Store()
	// All
	// (GET /all)
	All()
}
`)
	writeFixture(t, filepath.Join(dir, "handlers.go"), `package api
import "net/http"
type Server struct{}
func (s *Server) Status() { writeJSON(nil, http.StatusUnauthorized, nil) }
func (s *Server) Request() { writeJSON(nil, http.StatusOK, nil) }
func (s *Server) Store() { writeJSON(nil, http.StatusOK, nil); s.storeHelper() }
func (s *Server) All() { writeJSON(nil, http.StatusOK, nil) }
func (s *Server) storeHelper() { s.writeStoreError() }
func (s *Server) writeStoreError() { writeJSON(nil, http.StatusInternalServerError, nil) }
func writeJSON(any, int, any) {}
`)

	const spec = `openapi: 3.0.3
info: {title: policy fixture, version: "1"}
paths:
  /status:
    get:
      operationId: status
      responses:
        "200": {description: ok}
        "418": {description: all-operation policy}
  /request:
    get:
      operationId: request
      parameters:
        - {name: q, in: query, schema: {type: string}}
      responses:
        "200": {description: ok}
        "418": {description: all-operation policy}
  /store:
    get:
      operationId: store
      responses:
        "200": {description: ok}
        "500": {description: failed}
        "418": {description: all-operation policy}
  /all:
    get:
      operationId: all
      responses:
        "200": {description: ok}
`
	config := ServiceConfig{
		Spec:        []byte(spec),
		Directory:   dir,
		RouteSource: GeneratedRoutes,
		StatusWrites: Config{
			WriteFuncs: []string{"writeJSON"},
			StatusArg:  1,
			Floors: map[string][]int{
				"writeStoreError": {http.StatusInternalServerError},
			},
		},
		Rules: []Rule{
			{Name: "handler", Kind: HandlerStatuses},
			{Name: "request", Kind: RequestRejections, Status: http.StatusBadRequest},
			{Name: "boundary", Kind: ReachesBoundary, Status: http.StatusNotFound, Receiver: "Server", Boundary: "writeStoreError"},
			{Name: "all", Kind: AllOperations, Status: http.StatusTeapot},
		},
	}

	result, err := Analyze(config)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyRoute(t, result.Report("handler"), "GET /status", "GET /request", "GET /store", "GET /all")
	assertOnlyRoute(t, result.Report("request"), "GET /request", "GET /status", "GET /store", "GET /all")
	assertOnlyRoute(t, result.Report("boundary"), "GET /store", "GET /status", "GET /request", "GET /all")
	assertOnlyRoute(t, result.Report("all"), "GET /all", "GET /status", "GET /request", "GET /store")
}

func TestAllOperationsChecksEveryDocumentedOperation(t *testing.T) {
	const spec = `openapi: 3.0.3
info: {title: all-operations fixture, version: "1"}
paths:
  /alpha:
    get:
      operationId: alpha
      responses:
        "200": {description: ok}
  /beta:
    post:
      operationId: beta
      responses:
        "201": {description: created}
  /gamma:
    delete:
      operationId: gamma
      responses:
        "204": {description: deleted}
`
	result, err := Analyze(ServiceConfig{
		Spec:        []byte(spec),
		Directory:   ".",
		RouteSource: DocumentRoutes,
		Rules: []Rule{
			{Name: "all", Kind: AllOperations, Status: http.StatusInternalServerError},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	report := result.Report("all")
	for _, route := range []string{"DELETE /gamma", "GET /alpha", "POST /beta"} {
		if !strings.Contains(report, route) {
			t.Fatalf("all-operations report does not contain %q:\n%s", route, report)
		}
	}
	if got := strings.Count(report, "missing [500]"); got != 3 {
		t.Fatalf("all-operations report contains %d missing-status findings, want 3:\n%s", got, report)
	}
}

func TestHandlerPolicyResolvesTerminalExpressions(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "routes.go"), `package api
import (
	"net/http"
	"github.com/go-chi/chi/v5"
)
type Server struct{}
type alternateHandlers struct{}
func (s *Server) registerRoutes(r chi.Router) {
	other := alternateHandlers{}
	selected := other.created
	r.Get("/inline", s.internalOnly(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, nil)
	}))
	r.Get("/variable", s.internalOnly(selected))
	r.Get("/selector", other.noContent)
}
func (s *Server) internalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") == "" {
			writeJSON(w, http.StatusUnauthorized, nil)
			return
		}
		next(w, r)
	}
}
func (alternateHandlers) created(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusCreated, nil)
}
func (alternateHandlers) noContent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNoContent, nil)
}
func writeJSON(http.ResponseWriter, int, any) {}
`)
	const spec = `openapi: 3.0.3
info: {title: handler expression fixture, version: "1"}
paths:
  /inline:
    get:
      operationId: inline
      responses:
        "401": {description: unauthorized}
  /variable:
    get:
      operationId: variable
      responses:
        "401": {description: unauthorized}
  /selector:
    get:
      operationId: selector
      responses:
        "200": {description: placeholder}
`

	result, err := Analyze(ServiceConfig{
		Spec:        []byte(spec),
		Directory:   dir,
		RouteSource: ChiRoutes,
		StatusWrites: Config{
			WriteFuncs: []string{"writeJSON"},
			StatusArg:  1,
			Floors: map[string][]int{
				"internalOnly": {http.StatusUnauthorized},
			},
		},
		Rules: []Rule{{Name: "handler", Kind: HandlerStatuses}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report := result.Report("handler")
	for _, want := range []string{
		"GET /inline (inline): derived [202 401]; missing [202]",
		"GET /selector (selector): derived [204]; missing [204]",
		"GET /variable (variable): derived [201 401]; missing [201]",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("handler report does not contain %q:\n%s", want, report)
		}
	}
}

func TestHandlerPolicyRejectsUnresolvedWrappedVariable(t *testing.T) {
	for _, test := range []struct {
		name    string
		setup   string
		handler string
	}{
		{name: "variable", handler: "nextHandler"},
		{name: "selector", setup: "other := alternateHandlers{}", handler: "other.missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, filepath.Join(dir, "routes.go"), fmt.Sprintf(`package api
import (
	"net/http"
	"github.com/go-chi/chi/v5"
)
type Server struct{}
type alternateHandlers struct{}
func (s *Server) registerRoutes(r chi.Router) {
	%s
	r.Get("/wrapped", s.internalOnly(%s))
}
func (s *Server) internalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") == "" {
			writeJSON(w, http.StatusUnauthorized, nil)
			return
		}
		next(w, r)
	}
}
func writeJSON(http.ResponseWriter, int, any) {}
`, test.setup, test.handler))
			const spec = `openapi: 3.0.3
info: {title: unresolved fixture, version: "1"}
paths:
  /wrapped:
    get:
      operationId: wrapped
      responses:
        "401": {description: unauthorized}
`
			_, err := Analyze(ServiceConfig{
				Spec:        []byte(spec),
				Directory:   dir,
				RouteSource: ChiRoutes,
				StatusWrites: Config{
					WriteFuncs: []string{"writeJSON"},
					StatusArg:  1,
					Floors: map[string][]int{
						"internalOnly": {http.StatusUnauthorized},
					},
				},
				Rules: []Rule{{Name: "handler", Kind: HandlerStatuses}},
			})
			if err == nil || !strings.Contains(err.Error(), "GET /wrapped terminal handler could not be resolved") {
				t.Fatalf("Analyze() error = %v, want unresolved terminal handler", err)
			}
		})
	}
}

func TestRequestRejectionSources(t *testing.T) {
	const head = `openapi: 3.0.3
info: {title: request fixture, version: "1"}
paths:
  /p:
    get:
      operationId: probe
`
	tests := []struct {
		name string
		tail string
		want bool
	}{
		{name: "request body", tail: "      requestBody:\n        content:\n          application/json:\n            schema: {type: object}\n", want: true},
		{name: "query schema", tail: "      parameters: [{name: q, in: query, schema: {type: string}}]\n", want: true},
		{name: "header schema", tail: "      parameters: [{name: X-Probe, in: header, schema: {type: string}}]\n", want: true},
		{name: "query content", tail: "      parameters: [{name: q, in: query, content: {application/json: {schema: {type: object}}}}]\n", want: true},
		{name: "UUID path", tail: "      parameters: [{name: id, in: path, required: true, schema: {type: string, format: uuid}}]\n", want: true},
		{name: "plain path", tail: "      parameters: [{name: id, in: path, required: true, schema: {type: string}}]\n", want: false},
		{name: "schemaless query", tail: "      parameters: [{name: q, in: query}]\n", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc, err := openapi3.NewLoader().LoadFromData([]byte(head + test.tail +
				"      responses:\n        \"200\": {description: ok}\n"))
			if err != nil {
				t.Fatal(err)
			}
			item := doc.Paths.Value("/p")
			op := item.GetOperation(http.MethodGet)
			if got := requestCanReject(item.Parameters, op.Parameters, op.RequestBody); got != test.want {
				t.Fatalf("requestCanReject() = %t, want %t", got, test.want)
			}
		})
	}
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertOnlyRoute(t *testing.T, report, want string, unwanted ...string) {
	t.Helper()
	if !strings.Contains(report, want) {
		t.Fatalf("report does not contain %q:\n%s", want, report)
	}
	for _, route := range unwanted {
		if strings.Contains(report, route) {
			t.Fatalf("report for %q also contains unrelated route %q:\n%s", want, route, report)
		}
	}
}
