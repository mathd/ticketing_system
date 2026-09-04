package apispec

import (
	"regexp"
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

func TestStorefrontLifecycleSequenceBounds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	sequence := storefrontProperty(t, doc, "LifecycleEvent", "sequence")

	const safeMaximum int64 = 9007199254740991
	if err := sequence.VisitJSON(safeMaximum); err != nil {
		t.Errorf("sequence rejected the safe maximum: %v", err)
	}
	if err := sequence.VisitJSON(safeMaximum + 1); err == nil {
		t.Error("sequence accepted an unsafe integer")
	}
}

func TestStorefrontAccessDatesExcludeLeapSeconds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	for _, field := range [][2]string{{"Ticket", "issued_at"}, {"LifecycleEvent", "occurred_at"}} {
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
