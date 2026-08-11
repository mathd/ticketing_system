package api

import (
	"reflect"
	"strings"
	"testing"

	apispec "ticketing/services/commerce/api"
)

// GET /orders/{id} is public by contract and answers for ANY order id, and order
// ids are derived (uuid.NewSHA1 over organizer + checkout idempotency key) rather
// than random — so every field on this response is disclosed to anyone who can
// recompute an id. customer_id was one of them until ai-review S3: it turned a
// value the caller already had into an account-existence oracle.
//
// Two assertions, because the field can come back two ways and each is enforced
// somewhere different. The generated struct is what a handler would populate; the
// spec is what ADR-028's response validator holds the handler to, and
// `additionalProperties: false` is what makes an undeclared field a 500 rather
// than a leak. Deleting either mechanism has to fail here.
//
// If TKT-222 makes this read authenticated and ownership-scoped, the customer is
// no longer a disclosure and this test is the thing to revisit — not to delete
// quietly.
func TestPublicOrderReadDiscloseNoCustomer(t *testing.T) {
	if _, found := reflect.TypeOf(OrderState{}).FieldByName("CustomerId"); found {
		t.Error("OrderState declares CustomerId: the public order read must not disclose the account behind an order")
	}

	spec := string(apispec.Spec)
	start := strings.Index(spec, "\n    OrderState:")
	if start < 0 {
		t.Fatal("OrderState is not in the spec — rename this test with the schema")
	}
	// To the next sibling schema at the same indentation.
	rest := spec[start+1:]
	end := strings.Index(rest, "\n    RefundCreate:")
	if end < 0 {
		t.Fatal("could not bound the OrderState schema — adjust the sibling this test reads to")
	}
	if schema := rest[:end]; strings.Contains(schema, "customer_id:") {
		t.Errorf("OrderState schema declares customer_id:\n%s", schema)
	}
}
