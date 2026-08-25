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
	"errors"
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
	missing := operationsMissingBadRequest(doc)
	if len(missing) > 0 {
		t.Fatalf("these operations carry a request-layer rejection source (a format:uuid path "+
			"parameter, a request body, a query parameter or a header parameter), so the binder "+
			"or the kin-openapi request validator can answer 400; ADR-028 turns an undeclared "+
			"400 into a 500, so each must declare '400' (or a '4XX' range, or a `default:` response). "+
			"Missing on %d:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// The ONE scan both the live-spec assertion above and the synthetic edge cases
// below run. It is shared deliberately: when the two were separate, an edit to
// the live predicate could leave every edge case green while the real invariant
// regressed — the edge tests would have been pinning their own copy of the logic
// rather than the contract (TKT-142 ai-review, second pass).
func operationsMissingBadRequest(doc *openapi3.T) []string {
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !hasRejectionSource(item.Parameters, op.Parameters, op.RequestBody) {
				continue
			}
			// Mirror the matcher the response validator itself uses:
			// openapi3filter.ValidateResponse resolves the declaration with
			// Responses.Status(status) and falls back to Default(). Status()
			// accepts the exact key AND the patterned `4XX` range, so all three
			// spellings — '400', '4XX', `default:` — satisfy the contract and
			// must satisfy this test.
			if op.Responses.Status(http.StatusBadRequest) == nil && op.Responses.Default() == nil {
				missing = append(missing, method+" "+path+" ("+op.OperationID+")")
			}
		}
	}
	return missing
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
// No header parameter exists in the catalog document, so the LIVE spec cannot
// exercise the header arm — but a synthetic one can, and does: see
// TestRejectionSourceAndDeclarationEdgeCases. An earlier revision of this file
// called the arm "unexercised by construction" and left it untested, which was a
// claim about the live spec masquerading as a claim about the test.
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
				// A parameter with neither schema nor content is unrejectable:
				// openapi3filter.ValidateParameter returns nil immediately for
				// it ("assume that everything passes a schema-less check"), so
				// demanding a 400 for it would be an obligation no request can
				// trigger. Requiredness is NOT the line — an optional parameter
				// with a schema is rejectable when supplied invalid, measured on
				// this spec: `?channel_code=` (optional, minLength 1) answers
				// 400 "minimum string length is 1".
				if p.Schema != nil || p.Content != nil {
					return true
				}
			case openapi3.ParameterInPath:
				if p.Schema != nil && p.Schema.Value != nil && p.Schema.Value.Format == "uuid" {
					return true
				}
			}
		}
	}
	return false
}

// The predicate above has arms the catalog document cannot exercise — no
// operation declares `4XX`, and no query or header parameter is schemaless — so
// they would ship as unfalsifiable claims if only the live spec tested them.
// TKT-142's ai-review found both, and a fix with no test that can fail is how
// the same defect returns.
//
// These build synthetic documents and run them through the SAME
// operationsMissingBadRequest the live assertion uses, so the two cannot drift.
// Every document here is checked with doc.Validate first — catalog's NewRouter
// validates the document before serving it, so a fixture that Validate rejects
// could never reach the router, and an invariant "proved" on one would be proved
// about nothing. The one shape OpenAPI cannot express is split out below.
func TestRejectionSourceAndDeclarationEdgeCases(t *testing.T) {
	// Loads the document, REFUSES it if it is not a valid OpenAPI contract, and
	// returns what the live scanner reports missing.
	missingIn := func(t *testing.T, doc string) []string {
		t.Helper()
		loader := openapi3.NewLoader()
		d, err := loader.LoadFromData([]byte(doc))
		if err != nil {
			t.Fatalf("load synthetic spec: %v", err)
		}
		if err := d.Validate(loader.Context); err != nil {
			t.Fatalf("synthetic spec is not a valid OpenAPI document, so proving anything on it "+
				"would prove nothing about a contract the router can serve: %v", err)
		}
		return operationsMissingBadRequest(d)
	}

	const head = "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /p:\n    get:\n      operationId: probe\n"

	// A `4XX` range declaration satisfies the contract: ValidateResponse resolves
	// it via Responses.Status(400). Before the fix this read Value("400") and
	// reported this contract-correct operation as missing.
	t.Run("4XX range satisfies the invariant", func(t *testing.T) {
		doc := head + "      parameters: [{name: q, in: query, schema: {type: string}}]\n" +
			"      responses:\n        '4XX': {description: bad}\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("an operation declaring '4XX' is contract-correct and must satisfy the "+
				"invariant, but it was reported missing: %v", got)
		}
	})

	// The complement, so the arm above cannot pass by admitting everything: an
	// operation with a rejectable parameter and NO 4xx spelling at all is still
	// caught. Without this the test would stay green if the declaration check
	// were deleted outright.
	t.Run("no 400, 4XX or default is still caught", func(t *testing.T) {
		doc := head + "      parameters: [{name: q, in: query, schema: {type: string}}]\n" +
			"      responses:\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 1 {
			t.Fatalf("an operation with a rejectable query parameter and no 400/4XX/default must "+
				"be reported missing, got %v", got)
		}
	})

	// The `default:` arm. The failure message promises this spelling is accepted
	// and nothing exercised it: the live spec declares no `default:`, and the
	// 4XX case above only reaches Status(400). Deleting the Default() fallback
	// left the whole suite green until this case existed.
	t.Run("default response alone satisfies the invariant", func(t *testing.T) {
		doc := head + "      parameters: [{name: q, in: query, schema: {type: string}}]\n" +
			"      responses:\n        default: {description: whatever}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("a `default:` response covers 400 under kin-openapi's status matching, so it "+
				"satisfies the invariant, but the operation was reported missing: %v", got)
		}
	})

	// The header arm. The file used to call this "unexercised by construction"
	// because the catalog document declares no header parameter — true of the
	// live spec, and wrong as a claim about the test: a synthetic document
	// reaches it fine. A regression dropping headers from hasRejectionSource
	// passed every other case here (TKT-142 ai-review, third pass).
	t.Run("schema-backed header parameter creates an obligation", func(t *testing.T) {
		doc := head + "      parameters: [{name: X-Thing, in: header, schema: {type: string, minLength: 1}}]\n" +
			"      responses:\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 1 {
			t.Fatalf("a header parameter with a schema is rejectable by the request validator, so "+
				"it must demand a 400, got %v", got)
		}
	})

	// The `content:` arm of the parameter check. Every other case here declares
	// `schema:`, and the predicate is `p.Schema != nil || p.Content != nil`, so
	// short-circuiting meant nothing ever evaluated the second operand: deleting
	// it left the suite green while content-backed parameters silently became
	// exempt (TKT-142 ai-review, fourth pass). A parameter carries exactly one of
	// the two, so this fixture declares `content` and no `schema`.
	t.Run("content-backed parameter creates an obligation", func(t *testing.T) {
		doc := head + "      parameters: [{name: q, in: query, content: {application/json: {schema: {type: object}}}}]\n" +
			"      responses:\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 1 {
			t.Fatalf("a content-backed parameter is rejectable — the validator decodes and "+
				"schema-checks it — so it must demand a 400, got %v", got)
		}
	})

	// Requiredness is NOT the line — this is the case the ai-review's proposed
	// "count only required parameters" fix would have wrongly exempted. Measured
	// against the real spec: `?channel_code=` (optional, minLength 1) answers 400
	// "minimum string length is 1".
	t.Run("optional but constrained parameter does create an obligation", func(t *testing.T) {
		doc := head + "      parameters: [{name: q, in: query, required: false, schema: {type: string, minLength: 1}}]\n" +
			"      responses:\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 1 {
			t.Fatalf("an optional parameter with a schema is rejectable when supplied invalid, so "+
				"it must demand a 400, got %v", got)
		}
	})
}

// The schemaless exemption in hasRejectionSource, tested apart from the cases
// above because OpenAPI CANNOT EXPRESS the shape: a parameter must contain
// exactly one of `schema` and `content`, so `doc.Validate` rejects this document
// and catalog's NewRouter — which validates before serving — would refuse to
// start on it. The exemption is therefore about a state only kin-openapi's
// loader can hold, not about a contract any router can serve, and saying so is
// the point of keeping it separate (TKT-142 ai-review, second pass).
//
// It still earns its place. `Parameter.Schema == nil && Content == nil` is
// reachable in the loader, hasRejectionSource is written against the loader's
// types, and if the exemption were wrong the failure would be a FALSE NEGATIVE —
// the defect class this whole test exists to close.
//
// Deliberately INVALID fixture. Do not "fix" it by adding a schema; that deletes
// the case.
func TestSchemalessParameterExemptionIsAboutTheLoaderNotTheContract(t *testing.T) {
	const head = "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /p:\n    get:\n      operationId: probe\n"

	for _, tc := range []struct {
		name  string
		param string
	}{
		{"optional", "{name: q, in: query}"},
		// The sharpest case, and the one a reader will doubt. In
		// openapi3filter.ValidateParameter the schema-less early return sits at
		// the TOP of the function, before the `parameter.Required && !found`
		// presence check, so a missing required-but-schemaless parameter never
		// reaches that check and no 400 is produced. If that ordering ever
		// reverses, the exemption becomes a false negative and this goes red.
		{"required", "{name: q, in: query, required: true}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loader := openapi3.NewLoader()
			doc, err := loader.LoadFromData([]byte(head +
				"      parameters: [" + tc.param + "]\n" +
				"      responses:\n        '200': {description: ok}\n"))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			// Pin the reason this case is quarantined, and pin it by CAUSE: a
			// bare "Validate returned an error" would also be satisfied by a
			// typo in the fixture, leaving this test green while covering
			// nothing. Assert the schema/content rule specifically, so the day
			// OpenAPI admits the shape — or the day the fixture breaks for some
			// other reason — this says which happened.
			err = doc.Validate(loader.Context)
			if err == nil {
				t.Fatal("this fixture is kept separate BECAUSE doc.Validate rejects it; it now " +
					"validates, so fold it into TestRejectionSourceAndDeclarationEdgeCases")
			}
			// Matched on the TYPED error, not the message. kin-openapi exports
			// *openapi3.ParameterContentSchemaExactlyOne for exactly this rule,
			// so a wording change upstream must not redden a test whose subject
			// is the rule rather than its phrasing.
			var exactlyOne *openapi3.ParameterContentSchemaExactlyOne
			if !errors.As(err, &exactlyOne) {
				t.Fatalf("the fixture must be rejected for having neither schema nor content — "+
					"that is the state under test. It was rejected for something else, so this "+
					"test no longer covers the schemaless case: %v", err)
			}
			if got := operationsMissingBadRequest(doc); len(got) != 0 {
				t.Fatalf("a schemaless parameter is exempt from validation before its requiredness "+
					"is ever checked, so it demands no 400, got %v", got)
			}
		})
	}
}
