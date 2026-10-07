package api

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/commerce/api"
)

// TKT-287 COS4, at the tier that decides it: the CONTRACT. No request on the three
// staff operations can name an organizer. The router-tier 400 tests are not enough
// on their own — the handler's decoder also refuses unknown fields, so re-adding
// organizer_id to a schema as an optional property would leave them green while
// the contract invited clients to send it.
//
// Mutation that is evidence: add `organizer_id: {type: string}` back to RefundCreate
// or VoidCreate, drop their additionalProperties: false, or re-add OrganizerIdQuery
// to getStaffOrderDetail — each turns this red.
func TestStaffOperationsCannotNameAnOrganizerInTheContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"RefundCreate", "VoidCreate"} {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Fatalf("schema %s is missing", name)
		}
		schema := ref.Value
		if _, ok := schema.Properties["organizer_id"]; ok {
			t.Errorf("%s declares organizer_id: the organizer must come from the verified assertion only", name)
		}
		for _, required := range schema.Required {
			if required == "organizer_id" {
				t.Errorf("%s requires organizer_id", name)
			}
		}
		if schema.AdditionalProperties.Has == nil || *schema.AdditionalProperties.Has {
			t.Errorf("%s must set additionalProperties: false, or an undeclared organizer_id is accepted", name)
		}
	}
	// Every parameter the three operations accept, INCLUDING ones inherited from the
	// path item, which kin-openapi keeps separately from the operation's own.
	for _, op := range []struct{ path, method string }{
		{"/internal/orders/{id}", "GET"},
		{"/internal/orders/{id}/refunds", "POST"},
		{"/internal/orders/{id}/voids", "POST"},
	} {
		item := doc.Paths.Find(op.path)
		if item == nil || item.GetOperation(op.method) == nil {
			t.Fatalf("%s %s is missing", op.method, op.path)
		}
		params := append(openapi3.Parameters{}, item.Parameters...)
		params = append(params, item.GetOperation(op.method).Parameters...)
		for _, p := range params {
			if p.Value != nil && p.Value.Name == "organizer_id" {
				t.Errorf("%s %s still takes organizer_id in its %s", op.method, op.path, p.Value.In)
			}
		}
	}
}
