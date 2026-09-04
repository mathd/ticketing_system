package api

import (
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func storefrontProperty(t *testing.T, doc *openapi3.T, schemaName, propertyName string) *openapi3.Schema {
	t.Helper()
	schema := doc.Components.Schemas[schemaName]
	if schema == nil || schema.Value == nil {
		t.Fatalf("schema %s is missing", schemaName)
	}
	property := schema.Value.Properties[propertyName]
	if property == nil || property.Value == nil {
		t.Fatalf("property %s.%s is missing", schemaName, propertyName)
	}
	return property.Value
}

func TestCommerceOutputMoneyAndCountBounds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	// Catalog bounds each unit amount at 2^53-1, but Commerce multiplies those
	// units by quantity and adds fees. The response contract must admit the same
	// large int64 totals that the producer deliberately permits.
	const maxUnitAmount int64 = 9007199254740991
	largeComposedAmount := maxUnitAmount * 2
	for _, field := range [][2]string{
		{"Reservation", "amount"},
		{"Reservation", "face_value"},
		{"Reservation", "passed_on_fees"},
		{"CustomerOrderSummary", "total_amount"},
		{"Refund", "amount"},
		{"Refund", "refunded_amount"},
	} {
		property := storefrontProperty(t, doc, field[0], field[1])
		if err := property.VisitJSON(0); err != nil {
			t.Errorf("%s.%s rejected zero: %v", field[0], field[1], err)
		}
		if err := property.VisitJSON(-1); err == nil {
			t.Errorf("%s.%s accepted a negative amount", field[0], field[1])
		}
		if err := property.VisitJSON(largeComposedAmount); err != nil {
			t.Errorf("%s.%s rejected a valid composed int64 amount: %v", field[0], field[1], err)
		}
	}

	quantity := storefrontProperty(t, doc, "CustomerOrderSummary", "quantity")
	if err := quantity.VisitJSON(50); err != nil {
		t.Errorf("CustomerOrderSummary.quantity rejected the per-order maximum: %v", err)
	}
	if err := quantity.VisitJSON(51); err == nil {
		t.Error("CustomerOrderSummary.quantity accepted more than one order can hold")
	}

	for _, field := range [][2]string{{"Reservation", "currency"}, {"CustomerOrderSummary", "currency"}} {
		property := storefrontProperty(t, doc, field[0], field[1])
		if err := property.VisitJSON("EUR"); err != nil {
			t.Errorf("%s.%s rejected an uppercase ISO code: %v", field[0], field[1], err)
		}
		if err := property.VisitJSON("eur"); err == nil {
			t.Errorf("%s.%s accepted a lowercase currency", field[0], field[1])
		}
		if err := property.VisitJSON("EU1"); err == nil {
			t.Errorf("%s.%s accepted a non-alphabetic currency", field[0], field[1])
		}
	}

	feeItems := storefrontProperty(t, doc, "Reservation", "fee_breakdown").Items.Value
	feeCode := feeItems.Properties["fee_code"].Value
	feeAmount := feeItems.Properties["amount"].Value
	if err := feeCode.VisitJSON(strings.Repeat("F", 64)); err != nil {
		t.Errorf("fee_code rejected 64 characters: %v", err)
	}
	if err := feeCode.VisitJSON(strings.Repeat("F", 65)); err == nil {
		t.Error("fee_code accepted 65 characters")
	}
	if err := feeAmount.VisitJSON(0); err != nil {
		t.Errorf("fee amount rejected zero: %v", err)
	}
	if err := feeAmount.VisitJSON(-1); err == nil {
		t.Error("fee amount accepted a negative value")
	}
	if err := feeAmount.VisitJSON(largeComposedAmount); err != nil {
		t.Errorf("fee amount rejected a valid quantity-composed int64 amount: %v", err)
	}
}

func TestStorefrontReservationSeatBounds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	seats := storefrontProperty(t, doc, "Reservation", "seats")
	fifty := make([]any, 50)
	for i := range fifty {
		fifty[i] = strings.Repeat("S", 200)
	}
	if err := seats.VisitJSON(fifty); err != nil {
		t.Errorf("Reservation.seats rejected its exact count and string limits: %v", err)
	}
	if err := seats.VisitJSON(append(fifty, "S")); err == nil {
		t.Error("Reservation.seats accepted 51 identities")
	}
	if err := seats.Items.Value.VisitJSON(strings.Repeat("S", 201)); err == nil {
		t.Error("Reservation.seats accepted a 201-character identity")
	}
}

func TestStorefrontCommerceDatesExcludeLeapSeconds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	for _, field := range [][2]string{
		{"Reservation", "expires_at"},
		{"Reservation", "server_time"},
		{"CustomerOrderSummary", "purchased_at"},
		{"CustomerOrderSummary", "starts_at"},
	} {
		property := storefrontProperty(t, doc, field[0], field[1])
		if property.Pattern == "" {
			t.Fatalf("%s.%s does not declare the non-leap-second wire pattern", field[0], field[1])
		}
		pattern := regexp.MustCompile(property.Pattern)
		if !pattern.MatchString("2016-12-31T23:59:59Z") {
			t.Errorf("%s.%s rejects second 59", field[0], field[1])
		}
		if pattern.MatchString("2016-12-31T23:59:60Z") {
			t.Errorf("%s.%s accepts a leap second", field[0], field[1])
		}
	}
}

func TestStorefrontCustomerAssertionWireFormat(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	assertion := storefrontProperty(t, doc, "CustomerPrincipal", "customer_assertion")
	if assertion.Pattern == "" {
		t.Fatal("CustomerPrincipal.customer_assertion has no wire pattern")
	}
	pattern := regexp.MustCompile(assertion.Pattern)
	valid := "v1.60000000-0000-4000-8000-000000000001.99999999999." + strings.Repeat("A", 43)
	if !pattern.MatchString(valid) {
		t.Error("customer assertion pattern rejects the documented v1 form")
	}
	for _, malformed := range []string{
		strings.Replace(valid, "v1.", "v2.", 1),
		strings.TrimSuffix(valid, "A"),
		"v1.customer.99999999999." + strings.Repeat("A", 43),
	} {
		if pattern.MatchString(malformed) {
			t.Errorf("customer assertion pattern accepts %q", malformed)
		}
	}
}
