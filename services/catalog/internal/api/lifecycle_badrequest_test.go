package api

// TKT-110: the nine lifecycle operations declared 200/404/409/500 and no '400',
// but every codegen'd route can emit one — the generated wrapper unmarshals a
// `format: uuid` path parameter into a uuid.UUID and calls ChiServerOptions'
// ErrorHandlerFunc (400) when that fails. That 400 is written *inside*
// contract.ResponseValidator, which NewRouter wraps around HandlerWithOptions
// from the outside, so ADR-028 fail-closed turned an undeclared 400 into a
// production 500 plus a false-alarm ERROR log. Declaring '400' is the fix; these
// two tests pin the runtime status and the contract, respectively.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	apispec "ticketing/services/catalog/api"
)

// A malformed path UUID must reach the client as the 400 the binder wrote, not
// as ADR-028's generic 500. Before the spec declared '400', env.validateResponse
// caught the mask first (it Fatals on a "response violates OpenAPI contract"
// body), so this failed on the mask rather than on the status assertion.
func TestLifecycleRejectionsAreDeclaredBadRequests(t *testing.T) {
	for _, path := range []string{
		"/seat-maps/not-a-uuid/publish",
		"/performances/not-a-uuid/publish",
		"/performances/not-a-uuid/archive",
		"/performances/not-a-uuid/close",
		"/performances/not-a-uuid/reopen",
		"/series/not-a-uuid/publish",
		"/series/not-a-uuid/archive",
		"/festivals/not-a-uuid/publish",
		"/festivals/not-a-uuid/archive",
	} {
		t.Run(path, func(t *testing.T) {
			rec := newEnv(t).do("POST", path, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed path UUID must be 400, got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	// closeSlot is the only one of the nine with a requestBody, so it has a
	// second, independent 400 source: the kin-openapi request validator.
	t.Run("closeSlot invalid body", func(t *testing.T) {
		rec := newEnv(t).do("POST", "/performances/"+uuid.NewString()+"/close",
			map[string]any{"reason": 123})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid SlotCloseRequest must be 400, got %d %s", rec.Code, rec.Body.String())
		}
	})
}

// The class, not the nine instances: any operation whose path carries a
// format:uuid parameter can be rejected by the generated binder, so it owes the
// contract a '400'. Spec-only — it drives no requests, so it covers operation
// number ten without a fixture. The hand-mounted /internal/* chi routes are
// deliberately absent from the document and so out of its reach.
func TestUUIDPathOperationsDeclareBadRequest(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !hasUUIDPathParam(item.Parameters, op.Parameters) {
				continue
			}
			if op.Responses.Value("400") == nil {
				missing = append(missing, method+" "+path+" ("+op.OperationID+")")
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("operations with a format:uuid path parameter can be rejected by the generated "+
			"binder with 400; ADR-028 turns an undeclared 400 into a 500, so each must declare "+
			"'400': BadRequest. Missing on %d:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

func hasUUIDPathParam(sets ...openapi3.Parameters) bool {
	for _, params := range sets {
		for _, ref := range params {
			p := ref.Value
			if p == nil || p.In != openapi3.ParameterInPath || p.Schema == nil || p.Schema.Value == nil {
				continue
			}
			if p.Schema.Value.Format == "uuid" {
				return true
			}
		}
	}
	return false
}
