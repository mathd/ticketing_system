package apispec

import (
	"encoding/json"
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

// TKT-355. Every ticket carries one presentation verdict, and the contract admits only
// its three meaningful forms. The nine refusals are one per malformation CLASS
// (absence, null, wrong type, unknown value, invalid pair, extra field), not examples:
// "valid with a reason" and "voided without one" are the pairs a flat schema would let
// through.
func TestStorefrontTicketValidityForms(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	ticket := doc.Components.Schemas["Ticket"]
	if ticket == nil || ticket.Value == nil {
		t.Fatal("schema Ticket is missing")
	}
	cases := []struct {
		name, validity string // "" means the key is absent
		accepted       bool
	}{
		{"valid", `{"status":"valid"}`, true},
		{"voided by refund", `{"status":"voided","reason":"refunded"}`, true},
		{"voided by exchange", `{"status":"voided","reason":"exchanged"}`, true},
		{"absent", ``, false},
		{"null", `null`, false},
		{"bare string", `"valid"`, false},
		{"empty object", `{}`, false},
		{"unknown status", `{"status":"suspended"}`, false},
		{"unknown reason", `{"status":"voided","reason":"stolen"}`, false},
		{"valid with a reason", `{"status":"valid","reason":"refunded"}`, false},
		{"voided without a reason", `{"status":"voided"}`, false},
		{"extra field", `{"status":"valid","note":"x"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value := map[string]any{
				"ticket_id":  "22222222-2222-4222-8222-222222222222",
				"qr_payload": "credential",
				"issued_at":  "2026-01-01T00:00:00Z",
				"history":    []any{},
				"qr_url":     "/qr.png",
				// TKT-365: required, so every case here carries it.
				"performance_timezone": "Europe/Paris",
			}
			if c.validity != "" {
				var validity any
				if err := json.Unmarshal([]byte(c.validity), &validity); err != nil {
					t.Fatal(err)
				}
				value["validity"] = validity
			}
			err := ticket.Value.VisitJSON(value)
			if c.accepted && err != nil {
				t.Errorf("contract refused %s: %v", c.validity, err)
			}
			if !c.accepted && err == nil {
				t.Errorf("contract accepted %q", c.validity)
			}
		})
	}
}

// TKT-365 (ADR-083). Each ticket carries its performance zone, required and nullable: null
// says Catalog did not answer, which a client shows as labelled UTC; an absent field is a
// different fact and a contract break. One case per class: a zone, null, absence, empty,
// wrong type.
func TestStorefrontTicketPerformanceZoneForms(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	ticket := doc.Components.Schemas["Ticket"]
	if ticket == nil || ticket.Value == nil {
		t.Fatal("schema Ticket is missing")
	}
	cases := []struct {
		name, zone string // "" means the key is absent
		accepted   bool
	}{
		{"a zone", `"Asia/Kolkata"`, true},
		{"null", `null`, true},
		{"absent", ``, false},
		{"empty", `""`, false},
		{"number", `5`, false},
		{"object", `{"name":"Asia/Kolkata"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value := map[string]any{
				"ticket_id":  "22222222-2222-4222-8222-222222222222",
				"qr_payload": "credential",
				"issued_at":  "2026-01-01T00:00:00Z",
				"history":    []any{},
				"qr_url":     "/qr.png",
				"validity":   map[string]any{"status": "valid"},
			}
			if c.zone != "" {
				var zone any
				if err := json.Unmarshal([]byte(c.zone), &zone); err != nil {
					t.Fatal(err)
				}
				value["performance_timezone"] = zone
			}
			err := ticket.Value.VisitJSON(value)
			if c.accepted && err != nil {
				t.Errorf("contract refused %s: %v", c.zone, err)
			}
			if !c.accepted && err == nil {
				t.Errorf("contract accepted %q", c.zone)
			}
		})
	}
}
