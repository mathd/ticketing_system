package api

// TKT-165 / ADR-070. Payments' two refund legs must carry their ordering assumption in the
// SERVED contract. Same invariant and same reasoning as inventory's copy of this test
// (services/inventory/internal/api/internal_ordering_contract_test.go), which states the
// method in full: assert against the PARSED document, because a `#` comment — which is what
// both of these operations carried before this ticket — cannot survive into `openapi3.T`,
// so a check reading the YAML as text would pass on exactly the input being rejected.
//
// WHY PAYMENTS GETS A GO TEST AND NO VITEST COMPANION. `generate:api` (package.json) runs
// `openapi-typescript` for catalog, commerce, inventory and access. Payments has no
// generated TypeScript target at all — commerce is its only caller, and it is a Go service.
// So the served Go document is not merely the best tier here, it is the only one.
//
// AND THE ADVERSARY IS DIFFERENT HERE, which is the reason these two operations are worth a
// separate file rather than a row in inventory's table. Payments authenticates on
// PAYMENTS_INTERNAL_TOKEN, which runtimecfg refuses to let equal INTERNAL_SERVICE_TOKEN, so
// a holder of the shared credential cannot reach these routes at all (ADR-070 §5). Their
// prose says so; this test only checks it is said.

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/payments/api"
)

const orderingMarker = "ORDERING ASSUMED, NOT VERIFIED"

func TestRefundLegsDeclareTheirOrderingAssumption(t *testing.T) {
	for _, op := range []struct {
		id   string
		what string
	}{
		{"pspRefund", "one leg of a caller-owned reversal sequence; payments knows nothing about the seat"},
		{"pspPartialRefund", "same position as pspRefund, per leg"},
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
			if strings.TrimSpace(found.Summary) == "" {
				t.Errorf("%s has no summary (%s)", op.id, op.what)
			}
			if !strings.Contains(found.Description, orderingMarker) {
				t.Errorf("%s does not declare its ordering assumption in the SERVED contract.\n"+
					"  want the marker %q in .description — %s\n"+
					"  got description: %q\n"+
					"A `#` comment does NOT satisfy this: it never reaches the parsed document or "+
					"the served spec, which is the gap ADR-070 §6 closes.",
					op.id, orderingMarker, op.what, found.Description)
			}
		})
	}
}

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
