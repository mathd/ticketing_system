package api

// TKT-256: two payments operations could answer 500 and did not declare it —
// `getOperation` writes a literal `write(w, 500, ...)` when LookupOperation fails
// (server.go), and `appendFact` reaches one through `refused`, which answers
// http.StatusInternalServerError for any error that is not a store.IsRefusal
// sentinel — i.e. every wrapped pgx error. Under ADR-028 the payments router
// wraps contract.RequestValidator with response validation and
// `IncludeResponseStatus: true`, so an undeclared status is failed closed: the
// caller gets a generic "response violates OpenAPI contract" 500 plus an ERROR
// log, and the real cause is obscured exactly while someone debugs an outage.
//
// This test closes the CLASS rather than the two instances, following
// services/catalog/internal/api/lifecycle_badrequest_test.go.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/payments/api"
)

// The class: every documented payments operation must declare a 500-class
// response. The predicate is UNIVERSAL over the document, and that is a chosen
// property rather than an oversight — every one of the nine operations reaches
// the store, the provider, or both, so every one has a fallible dependency.
//
// A universal predicate is not an inert one. The dead-mechanism tell (AGENTS.md,
// TKT-162) is that you cannot name an input for which the mechanism changes the
// output; here there are two, and both are exercised below — an operation added
// without a 500-class response, and an existing declaration deleted. What makes
// it a closed allowlist rather than a no-op is precisely that it carries no
// exemption arm.
//
// `/openapi.yaml` drops out STRUCTURALLY, not by name: server.go mounts it on the
// chi router but the document does not list it under `paths`, so the scan never
// sees it — and it serves an embedded []byte with no fallible dependency. Do not
// "simplify" this into a name-based exclusion; the exclusion is a consequence of
// the operation being undocumented and should stay one. Should payments ever gain
// a documented operation that genuinely cannot answer 500, this goes red and the
// right response is an exemption arm with a stated reason, not deleting the test.
//
// ONE-DIRECTIONAL BY CHOICE. It proves every operation DECLARES 500; it does not
// prove each can EMIT one, which the spec cannot know — handler reachability
// crosses helpers (`refused`) and store methods. That asymmetry is deliberate:
// the direction ADR-028 punishes is a handler emitting an undeclared status. An
// operation declaring a 500 it never emits costs nothing.
func TestPaymentsOperationsDeclareInternalServerError(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	missing := operationsMissingInternalServerError(doc)
	if len(missing) > 0 {
		t.Fatalf("every payments operation reaches a fallible dependency and can therefore answer "+
			"500; ADR-028 fails closed on an undeclared status, rewriting it into a generic "+
			"contract 500, so each must declare '500' (or a '5XX' range, or a `default:` response). "+
			"Missing on %d:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// The ONE scan both the live-spec assertion above and the synthetic edge cases
// below run. Shared deliberately: when the two are separate, an edit to the live
// predicate can leave every edge case green while the real invariant regresses —
// the edge tests would be pinning their own copy of the logic rather than the
// contract (the lesson catalog's analogue learned at TKT-142 ai-review).
func operationsMissingInternalServerError(doc *openapi3.T) []string {
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			// Mirror the matcher the response validator itself uses:
			// openapi3filter.ValidateResponse resolves the declaration with
			// Responses.Status(status) and falls back to Default(). Status()
			// accepts the exact key AND the patterned `5XX` range, so all three
			// spellings — '500', '5XX', `default:` — satisfy the contract and
			// must satisfy this test.
			if op.Responses.Status(http.StatusInternalServerError) == nil && op.Responses.Default() == nil {
				missing = append(missing, method+" "+path+" ("+op.OperationID+")")
			}
		}
	}
	return missing
}

// The live document exercises only two of the scanner's arms — an exact '500'
// and a missing declaration. No payments operation declares `5XX` or `default:`,
// so those arms would ship as unfalsifiable claims if only the live spec tested
// them (catalog's TKT-142 ai-review found exactly that shape twice).
//
// Every fixture runs through the SAME operationsMissingInternalServerError the
// live assertion uses, so the two cannot drift, and every document is checked
// with doc.Validate first: payments' NewRouter builds a router from the document
// via contract.RequestValidator and panics if it is invalid, so an invariant
// "proved" on a document the router could never serve would be proved about
// nothing.
func TestInternalServerErrorDeclarationEdgeCases(t *testing.T) {
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
		return operationsMissingInternalServerError(d)
	}

	const head = "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /p:\n    get:\n      operationId: probe\n"

	// A `5XX` range declaration satisfies the contract: ValidateResponse resolves
	// it via Responses.Status(500). A scanner written against Responses.Value("500")
	// would report this contract-correct operation as missing.
	t.Run("5XX range satisfies the invariant", func(t *testing.T) {
		doc := head + "      responses:\n        '5XX': {description: boom}\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("an operation declaring '5XX' is contract-correct and must satisfy the "+
				"invariant, but it was reported missing: %v", got)
		}
	})

	// The `default:` arm. Nothing else reaches it — the live spec declares no
	// `default:`, and the 5XX case above only reaches Status(500). Deleting the
	// Default() fallback leaves the whole suite green until this case exists.
	t.Run("default response alone satisfies the invariant", func(t *testing.T) {
		doc := head + "      responses:\n        default: {description: whatever}\n"
		if got := missingIn(t, doc); len(got) != 0 {
			t.Fatalf("a `default:` response covers 500 under kin-openapi's status matching, so it "+
				"satisfies the invariant, but the operation was reported missing: %v", got)
		}
	})

	// The complement, and it is load-bearing rather than decorative: it is the
	// case that stops the two acceptance arms above from passing by admitting
	// everything. Without it, deleting the declaration check outright — making the
	// scanner report nothing — leaves the entire suite green.
	t.Run("no 500, 5XX or default is still caught", func(t *testing.T) {
		doc := head + "      responses:\n        '200': {description: ok}\n"
		if got := missingIn(t, doc); len(got) != 1 {
			t.Fatalf("an operation with no 500/5XX/default must be reported missing, got %v", got)
		}
	})
}
