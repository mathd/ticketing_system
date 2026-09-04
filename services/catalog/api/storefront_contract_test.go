package apispec

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

func TestStorefrontSeatMapBounds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	for _, schemaName := range []string{"SeatSection", "SeatRow", "Seat"} {
		position := storefrontProperty(t, doc, schemaName, "position")
		if err := position.VisitJSON(1); err != nil {
			t.Errorf("%s.position rejected the lower bound: %v", schemaName, err)
		}
		if err := position.VisitJSON(0); err == nil {
			t.Errorf("%s.position accepted zero", schemaName)
		}
	}
	for _, field := range [][2]string{
		{"SeatMapSectionCreate", "name"},
		{"SeatMapRowCreate", "label"},
		{"SeatMapSeatCreate", "label"},
		{"SeatMapEditSection", "name"},
		{"SeatMapEditRow", "label"},
		{"SeatMapEditSeat", "label"},
	} {
		component := storefrontProperty(t, doc, field[0], field[1])
		if err := component.VisitJSON(strings.Repeat("S", 196)); err != nil {
			t.Errorf("%s.%s rejected 196 characters: %v", field[0], field[1], err)
		}
		if err := component.VisitJSON(strings.Repeat("S", 197)); err == nil {
			t.Errorf("%s.%s accepted 197 characters", field[0], field[1])
		}
	}

	identity := storefrontProperty(t, doc, "Seat", "seat_identity")
	if err := identity.VisitJSON(""); err == nil {
		t.Error("Seat.seat_identity accepted an empty identity")
	}
	if err := identity.VisitJSON(strings.Repeat("S", 200)); err != nil {
		t.Errorf("Seat.seat_identity rejected the 200-character boundary: %v", err)
	}
	if err := identity.VisitJSON(strings.Repeat("S", 201)); err == nil {
		t.Error("Seat.seat_identity accepted 201 characters")
	}
}

func TestSeatIdentityBoundaryMatchesSalesRequestContracts(t *testing.T) {
	catalog, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	commerce, err := openapi3.NewLoader().LoadFromFile("../../commerce/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := openapi3.NewLoader().LoadFromFile("../../inventory/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}

	properties := map[string]*openapi3.Schema{
		"catalog response":  storefrontProperty(t, catalog, "Seat", "seat_identity"),
		"commerce request":  storefrontProperty(t, commerce, "ReservationCreate", "seat_identities").Items.Value,
		"inventory request": storefrontProperty(t, inventory, "SeatHoldCreate", "seat_identities").Items.Value,
	}
	for name, property := range properties {
		t.Run(name, func(t *testing.T) {
			if err := property.VisitJSON(strings.Repeat("S", 200)); err != nil {
				t.Fatalf("rejected 200 characters: %v", err)
			}
			if err := property.VisitJSON(strings.Repeat("S", 201)); err == nil {
				t.Fatal("accepted 201 characters")
			}
		})
	}
}

func TestStorefrontCatalogDatesExcludeLeapSeconds(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	for _, field := range [][2]string{
		{"SeatMap", "published_at"},
		{"SeatMap", "created_at"},
		{"PublicPerformanceSummary", "starts_at"},
		{"PublicPerformanceDetail", "starts_at"},
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

func TestBackofficeOrganizerAssertionWireFormat(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	assertion := storefrontProperty(t, doc, "StaffPrincipal", "organizer_assertion")
	if assertion.Pattern == "" {
		t.Fatal("StaffPrincipal.organizer_assertion has no wire pattern")
	}
	pattern := regexp.MustCompile(assertion.Pattern)
	valid := "v1.60000000-0000-4000-8000-000000000001.00000000-0000-4000-8000-000000000001.99999999999." + strings.Repeat("A", 43)
	if !pattern.MatchString(valid) {
		t.Error("organizer assertion pattern rejects the documented v1 form")
	}
	for _, malformed := range []string{
		strings.Replace(valid, "v1.", "v2.", 1),
		strings.TrimSuffix(valid, "A"),
		"v1.staff.organizer.99999999999." + strings.Repeat("A", 43),
	} {
		if pattern.MatchString(malformed) {
			t.Errorf("organizer assertion pattern accepts %q", malformed)
		}
	}
}
