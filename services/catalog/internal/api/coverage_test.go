package api

// TKT-47: every documented 2xx operation must be exercised by a happy-path
// test whose real handler response passes through runtime response validation.
// Coverage is recorded in env.validateResponse (the chokepoint every test
// response flows through) and enforced after the run in TestMain — a new
// spec operation without a driving test fails the suite. Scope: requests must
// go through env.do to be seen; coverage is per-operation (any 2xx counts),
// and an operation documented only via `default:` is exempt.

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/catalog/api"
)

var (
	coveredMu  sync.Mutex
	coveredOps = map[string]bool{}
)

func recordCoverage(operationID string, status int) {
	if operationID == "" || status < 200 || status > 299 {
		return
	}
	coveredMu.Lock()
	coveredOps[operationID] = true
	coveredMu.Unlock()
}

func uncovered2xxOps() []string {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		return []string{fmt.Sprintf("load spec: %v", err)}
	}
	coveredMu.Lock()
	defer coveredMu.Unlock()
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			has2xx := false
			for status := range op.Responses.Map() {
				if strings.HasPrefix(status, "2") {
					has2xx = true
					break
				}
			}
			if has2xx && !coveredOps[op.OperationID] {
				missing = append(missing, fmt.Sprintf("%s %s (%s)", method, path, op.OperationID))
			}
		}
	}
	sort.Strings(missing)
	return missing
}

func TestMain(m *testing.M) {
	code := m.Run()
	// Enforce only on unfiltered runs: a focused `go test -run X` exercises
	// one op by design and must not fail the coverage gate.
	filtered := flag.Lookup("test.run") != nil && flag.Lookup("test.run").Value.String() != ""
	if code == 0 && !filtered {
		if missing := uncovered2xxOps(); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "documented 2xx operations with no happy-path test:\n  %s\n",
				strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

// TestRuntimeResponseDriftFailsClosed proves the runtime wiring, not just the
// shared middleware: committed store data is rigged out of schema (negative
// minor units violate Money.amount minimum 0), and the drifted payload must
// never reach the client (ADR-028 fail-closed).
func TestRuntimeResponseDriftFailsClosed(t *testing.T) {
	e := newEnv(t)
	eventID, _ := e.createFixture(true)
	for id, tt := range e.store.ticketTypes {
		tt.PriceAmount = -1
		e.store.ticketTypes[id] = tt
	}
	req := httptest.NewRequest(http.MethodGet, "http://catalog.local/public/events/"+eventID.String()+"?locale=fr", nil)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("drifted response reached the client: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response violates OpenAPI contract") {
		t.Fatalf("unexpected drift body: %s", rec.Body.String())
	}
}

// The sibling of TestRuntimeResponseDriftFailsClosed, on the other side of the
// TKT-125 knob: the same rigged out-of-schema payload, a router built with
// response validation off, and the drifted body reaching the client untouched.
// It is here rather than in shared/go/contract because what is under test is
// catalog's *wiring* — that NewRouter actually threads the policy — not the
// middleware, which has its own tests. ADR-030's coverage gate keeps running
// against the enabled router (newEnv passes true); this test builds its own.
func TestRuntimeResponseDriftPassesThroughWhenValidationDisabled(t *testing.T) {
	e := newEnv(t)
	handler, err := NewRouter(NewServer(e.store, e.pub, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-internal-token"), false)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	eventID, _ := e.createFixture(true)
	for id, tt := range e.store.ticketTypes {
		tt.PriceAmount = -1
		e.store.ticketTypes[id] = tt
	}
	req := httptest.NewRequest(http.MethodGet, "http://catalog.local/public/events/"+eventID.String()+"?locale=fr", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled validation must not substitute a status: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "response violates OpenAPI contract") {
		t.Fatalf("disabled validation still masked the response: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"amount":-1`) {
		t.Fatalf("the drifted payload did not reach the client: %s", rec.Body.String())
	}
}
