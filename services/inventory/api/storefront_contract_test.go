package apispec

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestStorefrontRemainingCapacityBounds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	schema := doc.Components.Schemas["SeatOccupancy"]
	if schema == nil || schema.Value == nil {
		t.Fatal("SeatOccupancy schema is missing")
	}
	property := schema.Value.Properties["remaining_capacity"]
	if property == nil || property.Value == nil {
		t.Fatal("SeatOccupancy.remaining_capacity is missing")
	}

	const safeMaximum int64 = 9007199254740991
	if err := property.Value.VisitJSON(safeMaximum); err != nil {
		t.Errorf("remaining_capacity rejected the safe maximum: %v", err)
	}
	if err := property.Value.VisitJSON(safeMaximum + 1); err == nil {
		t.Error("remaining_capacity accepted an unsafe integer")
	}
}
