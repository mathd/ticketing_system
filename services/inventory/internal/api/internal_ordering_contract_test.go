package api

// TKT-165 / ADR-070. Inventory's three ordering-dependent internal mutations must carry
// their assumption in the SERVED contract, not in a YAML comment.
//
// WHY THIS TIER, AND WHY NOT A GREP. The thing being fixed is that the assumption was
// invisible: two of these three carried no operation prose at all, and the third stated its
// ordering in a `#` comment. A `#` comment is not part of the document — it never reaches
// the router's validator, `GET /openapi.yaml`, or the generated TypeScript client. So a
// check that reads openapi.yaml as TEXT would pass on exactly the input this test exists to
// reject, which is the ticket's own COS-4 warning in as many words.
//
// This loads `apispec.Spec` through the OpenAPI loader instead. What comes back is the
// document the router validates against and the server serves, and a `#` comment cannot
// survive into `openapi3.T` by construction. That is the whole reason the assertion is
// written against a parsed document rather than a file.
//
// THE DISCRIMINATING MUTATION, which is exact and was run: move an ordering sentence from
// `description` back into a `#` comment. Every behaviour test stays green, `check-generate`
// stays green — the generated client changes, and regenerating it makes the diff clean
// again — and this test goes red. That is the defect being fixed, reproduced.
//
// WHAT IT DOES NOT ASSERT, stated so a green run is not read as more than it is. It does
// not check that the prose is TRUE, that the ordering is enforced anywhere, or that any
// caller honours it. Enforcement lives in commerce and is pinned by
// services/commerce/internal/refunds/reversal_order_test.go; ADR-070 §2 says so and this
// test would stay green if that guard were deleted. It asserts visibility, which is the one
// thing this ticket changed.

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/inventory/api"
)

// orderingMarker is the phrase every ordering-dependent internal operation carries. A fixed
// string rather than a fuzzy match: the point is that a reader scanning the served contract
// meets the same words at every such endpoint, so drift in the phrase is itself a finding.
const orderingMarker = "ORDERING ASSUMED, NOT VERIFIED"

func TestOrderingDependentOperationsDeclareTheirAssumption(t *testing.T) {
	// The three inventory operations ADR-070 §4 enumerates. Named by operationId rather than
	// by path so a route rename cannot silently empty this list — a path typo would make the
	// lookup fail loudly below, where a `paths[...]` miss would just skip.
	for _, op := range []struct {
		id   string
		what string
	}{
		{"confirmHold", "the caller must have captured the payment"},
		{"releaseHold", "the caller must have ensured the entitlement can no longer admit"},
		{"returnRefundedCapacity", "the caller must have voided the refunded tickets"},
	} {
		t.Run(op.id, func(t *testing.T) {
			doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
			if err != nil {
				t.Fatalf("load spec: %v", err)
			}
			found := operationByID(doc, op.id)
			if found == nil {
				t.Fatalf("operation %q is not in the served spec — ADR-070 §4 enumerates it, so "+
					"either the enumeration is stale or the operation was renamed", op.id)
			}
			// Summary AND description, for different reasons. The summary is what the
			// generated TypeScript client shows as the operation's one-line doc comment;
			// the description is where the assumption itself lives. Two of these three had
			// neither before this ticket.
			if strings.TrimSpace(found.Summary) == "" {
				t.Errorf("%s has no summary: the generated client shows nothing at all for it "+
					"(%s)", op.id, op.what)
			}
			if !strings.Contains(found.Description, orderingMarker) {
				t.Errorf("%s does not declare its ordering assumption in the SERVED contract.\n"+
					"  want the marker %q in .description — %s\n"+
					"  got description: %q\n"+
					"A `#` comment in openapi.yaml does NOT satisfy this: it never reaches the "+
					"parsed document, the served spec, or the generated client, which is the "+
					"exact gap ADR-070 §6 closes.",
					op.id, orderingMarker, op.what, found.Description)
			}
		})
	}
}

// operationByID walks the document rather than indexing a path, so the test does not encode
// the routes' spelling a second time. Returns nil when no operation carries the id.
func operationByID(doc *openapi3.T, id string) *openapi3.Operation {
	if doc.Paths == nil {
		return nil
	}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op != nil && op.OperationID == id {
				return op
			}
		}
	}
	return nil
}
