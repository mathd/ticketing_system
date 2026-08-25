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

// The class, not the nine instances: any operation the request layer can reject
// owes the contract a '400'. Four sources produce one — the generated binder
// rejecting a `format: uuid` path parameter, and the kin-openapi request
// validator rejecting a request body, a query parameter or a header parameter
// (measured on TKT-110: invalid JSON, a wrong property type, a minLength
// violation and a wrong Content-Type all reach the client as 400). ADR-028
// wraps response validation around the router from the OUTSIDE, so each of those
// 400s is written inside it and an undeclared one is rewritten into a 500.
//
// Spec-only — it drives no requests, so it covers operation number ten without a
// fixture. Two notes on its reach, both deliberate:
//
//   - `getOpenAPISpec` drops out STRUCTURALLY, not by name: it has no parameters
//     and no request body, so it has no rejection source. Do not "simplify" this
//     into a name check — the exclusion is a consequence of the predicate and
//     should stay one.
//   - At HEAD the predicate selects 41 of the document's 42 operations, so it
//     discriminates little TODAY. Its value is prospective: it fires on the
//     future operation that carries a body, query or header and no uuid path
//     parameter — which is exactly how the nine TKT-110 fixed slipped through.
//
// The hand-mounted /internal/* chi routes (server.go NewRouter) are absent from
// the document and mounted outside the validator, so they are out of reach here.
// The two /internal/* paths that ARE documented are in scope like any other.
func TestCatalogRequestRejectionSourcesDeclareBadRequest(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !hasRejectionSource(item.Parameters, op.Parameters, op.RequestBody) {
				continue
			}
			// A `default:` response covers 400 under kin-openapi's status
			// matching, so it satisfies the invariant. None exist in this spec
			// today (ADR-028 notes the same for its coverage gate); without this
			// arm the test would fail a future default-documented operation that
			// is in fact contract-correct.
			if op.Responses.Value("400") == nil && op.Responses.Default() == nil {
				missing = append(missing, method+" "+path+" ("+op.OperationID+")")
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("these operations carry a request-layer rejection source (a format:uuid path "+
			"parameter, a request body, a query parameter or a header parameter), so the binder "+
			"or the kin-openapi request validator can answer 400; ADR-028 turns an undeclared "+
			"400 into a 500, so each must declare '400': BadRequest (or a `default:` response). "+
			"Missing on %d:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// A source the request layer can reject on, before any handler runs.
//
// The request body counts whether or not it is `required`: closeSlot's is
// `required: false` and TestLifecycleRejectionsAreDeclaredBadRequests above
// proves an invalid-but-optional body still returns 400. Query and header
// parameters count whether or not they are required, for the same reason — a
// supplied value can violate its schema.
//
// Path parameters count ONLY when the format is uuid. That is the binder
// guarantee TKT-110 measured; a plain string path parameter binds anything, so
// widening this arm would demand 400s no source can produce.
//
// The header arm is UNEXERCISED BY CONSTRUCTION: this document declares no
// header parameter, so no mutation of it can prove the branch. It is kept
// because it is correct for a future operation, and labelled because a branch a
// fixture cannot reach must not be mistaken for coverage.
func hasRejectionSource(pathParams, opParams openapi3.Parameters, body *openapi3.RequestBodyRef) bool {
	if body != nil && body.Value != nil {
		return true
	}
	for _, params := range []openapi3.Parameters{pathParams, opParams} {
		for _, ref := range params {
			p := ref.Value
			if p == nil {
				continue
			}
			switch p.In {
			case openapi3.ParameterInQuery, openapi3.ParameterInHeader:
				return true
			case openapi3.ParameterInPath:
				if p.Schema != nil && p.Schema.Value != nil && p.Schema.Value.Format == "uuid" {
					return true
				}
			}
		}
	}
	return false
}
