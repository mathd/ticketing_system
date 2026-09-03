package api

// TKT-165 / ADR-071. Commerce's exchange switch callback must carry its ordering assumption
// in the SERVED contract. Same invariant and method as the inventory copy of this test,
// which states the reasoning in full: assert against the PARSED document, because the `#`
// comment this operation carried before this ticket cannot reach `openapi3.T`, the served
// spec, or the generated client.
//
// THIS ONE IS THE INTERESTING CASE OF THE SIX. `exchangeTicketsSwitched` exists precisely to
// make an ordering checkable — access calls it after its switch transaction commits, and
// commerce records `tickets_exchanged_at` before returning the old capacity — and it is
// itself ordering-dependent, because commerce takes access's report on trust. An operation
// whose whole purpose is to be evidence of an ordering, and which cannot verify the ordering
// it reports, is the clearest statement of why ADR-071 concludes what it does.

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/commerce/api"
)

const orderingMarker = "ORDERING ASSUMED, NOT VERIFIED"

func TestExchangeSwitchCallbackDeclaresItsOrderingAssumption(t *testing.T) {
	const id = "exchangeTicketsSwitched"
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	found := orderingOperationByID(doc, id)
	if found == nil {
		t.Fatalf("operation %q is not in the served spec — ADR-071 §4 enumerates it, so either "+
			"the enumeration is stale or the operation was renamed", id)
	}
	if strings.TrimSpace(found.Summary) == "" {
		t.Errorf("%s has no summary", id)
	}
	if !strings.Contains(found.Description, orderingMarker) {
		t.Errorf("%s does not declare its ordering assumption in the SERVED contract.\n"+
			"  want the marker %q in .description — access must call this only AFTER its switch "+
			"transaction commits, and commerce cannot verify that it did\n"+
			"  got description: %q\n"+
			"A `#` comment does NOT satisfy this: it never reaches the parsed document, the "+
			"served spec, or the generated client, which is the gap ADR-071 §6 closes.",
			id, orderingMarker, found.Description)
	}
}

func orderingOperationByID(doc *openapi3.T, id string) *openapi3.Operation {
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
