package apispec

import (
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"ticketing/services/catalog/internal/store"
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

// TestInternalSeatMapPinContract (TKT-143):
// Asserts that the catalog contract declares the internal pin operations and schemas with exact bounds,
// required fields, maxItems matching store.MaxSeatMapPinPage, and additionalProperties: false.
// Mutation check: changing any bound, removing a required field, or toggling additionalProperties fails this test.
func TestInternalSeatMapPinContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}

	reqSchemaRef := doc.Components.Schemas["SeatPinRequest"]
	if reqSchemaRef == nil || reqSchemaRef.Value == nil {
		t.Fatal("schema SeatPinRequest is missing")
	}
	reqSchema := reqSchemaRef.Value

	pinSchemaRef := doc.Components.Schemas["SeatMapPin"]
	if pinSchemaRef == nil || pinSchemaRef.Value == nil {
		t.Fatal("schema SeatMapPin is missing")
	}
	pinSchema := pinSchemaRef.Value

	pageSchemaRef := doc.Components.Schemas["SeatMapPinPage"]
	if pageSchemaRef == nil || pageSchemaRef.Value == nil {
		t.Fatal("schema SeatMapPinPage is missing")
	}
	pageSchema := pageSchemaRef.Value

	// 1. additionalProperties: false
	for _, pair := range []struct {
		name   string
		schema *openapi3.Schema
	}{
		{"SeatPinRequest", reqSchema},
		{"SeatMapPin", pinSchema},
		{"SeatMapPinPage", pageSchema},
	} {
		if pair.schema.AdditionalProperties.Has == nil || *pair.schema.AdditionalProperties.Has {
			t.Errorf("schema %s must have additionalProperties: false", pair.name)
		}
	}

	// 2. Required lists
	assertRequired := func(s *openapi3.Schema, name string, expected []string) {
		t.Helper()
		if len(s.Required) != len(expected) {
			t.Fatalf("%s required = %v, want %v", name, s.Required, expected)
		}
		for _, req := range expected {
			found := false
			for _, r := range s.Required {
				if r == req {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s missing required field %q", name, req)
			}
		}
	}
	assertRequired(reqSchema, "SeatPinRequest", []string{"organizer_id", "seat_identities", "pinned_by"})
	assertRequired(pinSchema, "SeatMapPin", []string{"id", "organizer_id", "seat_map_id", "seat_identity", "pinned_by"})
	assertRequired(pageSchema, "SeatMapPinPage", []string{"pins"})

	// 3. Field bounds
	// SeatPinRequest.seat_identities
	seatIdentsProp := reqSchema.Properties["seat_identities"]
	if seatIdentsProp == nil || seatIdentsProp.Value == nil {
		t.Fatal("SeatPinRequest.seat_identities is missing")
	}
	if seatIdentsProp.Value.MinItems != 1 {
		t.Fatalf("SeatPinRequest.seat_identities minItems = %d, want 1", seatIdentsProp.Value.MinItems)
	}
	if seatIdentsProp.Value.Items == nil || seatIdentsProp.Value.Items.Value == nil {
		t.Fatal("SeatPinRequest.seat_identities.items is missing")
	}
	itemSchema := seatIdentsProp.Value.Items.Value
	if itemSchema.MinLength != 1 || itemSchema.MaxLength == nil || *itemSchema.MaxLength != store.MaxSeatIdentityCharacters {
		t.Fatalf("SeatPinRequest.seat_identities item bounds = [%d, %v], want [1, %d]",
			itemSchema.MinLength, itemSchema.MaxLength, store.MaxSeatIdentityCharacters)
	}

	// SeatPinRequest.pinned_by
	pinnedByProp := reqSchema.Properties["pinned_by"]
	if pinnedByProp == nil || pinnedByProp.Value == nil {
		t.Fatal("SeatPinRequest.pinned_by is missing")
	}
	if pinnedByProp.Value.MinLength != 1 || pinnedByProp.Value.MaxLength == nil || *pinnedByProp.Value.MaxLength != store.MaxPinnedByCharacters {
		t.Fatalf("SeatPinRequest.pinned_by bounds = [%d, %v], want [1, %d]",
			pinnedByProp.Value.MinLength, pinnedByProp.Value.MaxLength, store.MaxPinnedByCharacters)
	}

	// SeatMapPin.seat_identity
	pinIdentityProp := pinSchema.Properties["seat_identity"]
	if pinIdentityProp == nil || pinIdentityProp.Value == nil {
		t.Fatal("SeatMapPin.seat_identity is missing")
	}
	if pinIdentityProp.Value.MinLength != 1 || pinIdentityProp.Value.MaxLength == nil || *pinIdentityProp.Value.MaxLength != store.MaxSeatIdentityCharacters {
		t.Fatalf("SeatMapPin.seat_identity bounds = [%d, %v], want [1, %d]",
			pinIdentityProp.Value.MinLength, pinIdentityProp.Value.MaxLength, store.MaxSeatIdentityCharacters)
	}

	// SeatMapPin.pinned_by
	pinPinnedByProp := pinSchema.Properties["pinned_by"]
	if pinPinnedByProp == nil || pinPinnedByProp.Value == nil {
		t.Fatal("SeatMapPin.pinned_by is missing")
	}
	if pinPinnedByProp.Value.MinLength != 1 || pinPinnedByProp.Value.MaxLength == nil || *pinPinnedByProp.Value.MaxLength != store.MaxPinnedByCharacters {
		t.Fatalf("SeatMapPin.pinned_by bounds = [%d, %v], want [1, %d]",
			pinPinnedByProp.Value.MinLength, pinPinnedByProp.Value.MaxLength, store.MaxPinnedByCharacters)
	}

	// SeatMapPinPage.pins maxItems == store.MaxSeatMapPinPage
	pinsProp := pageSchema.Properties["pins"]
	if pinsProp == nil || pinsProp.Value == nil {
		t.Fatal("SeatMapPinPage.pins is missing")
	}
	if pinsProp.Value.MaxItems == nil || *pinsProp.Value.MaxItems != store.MaxSeatMapPinPage {
		t.Fatalf("SeatMapPinPage.pins maxItems = %v, want %d", pinsProp.Value.MaxItems, store.MaxSeatMapPinPage)
	}

	// 4. Query parameters on listSeatMapPins
	listOp := doc.Paths.Find("/internal/seat-map-pins")
	if listOp == nil || listOp.Get == nil {
		t.Fatal("GET /internal/seat-map-pins is missing")
	}
	limitParam := listOp.Get.Parameters.GetByInAndName("query", "limit")
	if limitParam == nil || limitParam.Schema == nil || limitParam.Schema.Value == nil {
		t.Fatal("query param limit on listSeatMapPins is missing")
	}
	limitSchema := limitParam.Schema.Value
	if limitSchema.Max == nil || *limitSchema.Max != float64(store.MaxSeatMapPinPage) {
		t.Fatalf("listSeatMapPins limit maximum = %v, want %d", limitSchema.Max, store.MaxSeatMapPinPage)
	}
	if limitSchema.Min == nil || *limitSchema.Min != 1 {
		t.Fatalf("listSeatMapPins limit minimum = %v, want 1", limitSchema.Min)
	}

	// 5. Check the operations exist with security: []
	pinPath := doc.Paths.Find("/internal/seat-maps/{id}/pins")
	if pinPath == nil || pinPath.Post == nil {
		t.Fatal("POST /internal/seat-maps/{id}/pins is missing")
	}
	if pinPath.Post.OperationID != "pinSeatMapSeats" {
		t.Fatalf("operationId = %q, want pinSeatMapSeats", pinPath.Post.OperationID)
	}
	if pinPath.Post.Security == nil || len(*pinPath.Post.Security) != 0 {
		t.Fatal("POST /internal/seat-maps/{id}/pins must declare security: []")
	}

	unpinPath := doc.Paths.Find("/internal/seat-maps/{id}/unpins")
	if unpinPath == nil || unpinPath.Post == nil {
		t.Fatal("POST /internal/seat-maps/{id}/unpins is missing")
	}
	if unpinPath.Post.OperationID != "unpinSeatMapSeats" {
		t.Fatalf("operationId = %q, want unpinSeatMapSeats", unpinPath.Post.OperationID)
	}
	if unpinPath.Post.Security == nil || len(*unpinPath.Post.Security) != 0 {
		t.Fatal("POST /internal/seat-maps/{id}/unpins must declare security: []")
	}

	if listOp.Get.OperationID != "listSeatMapPins" {
		t.Fatalf("operationId = %q, want listSeatMapPins", listOp.Get.OperationID)
	}
	if listOp.Get.Security == nil || len(*listOp.Get.Security) != 0 {
		t.Fatal("GET /internal/seat-map-pins must declare security: []")
	}
}
