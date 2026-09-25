package api

import (
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/access/api"
)

func TestOrderAdmissionDeclaresItsResponsesAndBody(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	item := doc.Paths.Find("/internal/orders/{id}/admission")
	if item == nil || item.Get == nil || item.Get.OperationID != "getOrderAdmission" {
		t.Fatal("GET /internal/orders/{id}/admission is not declared as getOrderAdmission")
	}
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError} {
		if item.Get.Responses.Status(status) == nil {
			t.Errorf("getOrderAdmission does not declare %d", status)
		}
	}
	response := item.Get.Responses.Status(http.StatusOK).Value
	media := response.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("the 200 response has no JSON schema")
	}
	if err := media.Schema.Value.VisitJSON(map[string]any{"admitted": true, "issued_count": 2}); err != nil {
		t.Fatalf("the response body does not satisfy the declared schema: %v", err)
	}
	if err := media.Schema.Value.VisitJSON(map[string]any{"admitted": false, "issued_count": 0}); err != nil {
		t.Errorf("the response schema rejects an authenticated empty ticket set: %v", err)
	}
}
