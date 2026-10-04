package api

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/payments/api"
)

// TKT-285 (D9): the contract states what the handler enforces. A charge moves money, so an
// amount below 1 is not a charge; commerce skips the provider for a zero total rather than
// sending one.
func TestChargeAmountDeclaresItsMinimumAndTheRefusal(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	schema, ok := doc.Components.Schemas["Charge"]
	if !ok {
		t.Fatal("the spec declares no Charge schema")
	}
	amount, ok := schema.Value.Properties["amount"]
	if !ok {
		t.Fatal("Charge declares no amount property")
	}
	if amount.Value.Min == nil {
		t.Fatal("Charge.amount declares no minimum; a charge of zero is not a charge")
	}
	if got := *amount.Value.Min; got != 1 {
		t.Errorf("Charge.amount minimum = %v, want 1", got)
	}
	op := doc.Paths.Find("/internal/charges").Post
	if op == nil {
		t.Fatal("the spec declares no POST /internal/charges")
	}
	if op.Responses.Status(400) == nil {
		t.Error("POST /internal/charges does not declare a 400 response")
	}
}
