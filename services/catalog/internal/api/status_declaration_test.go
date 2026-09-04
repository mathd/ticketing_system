package api

import (
	"net/http"
	"testing"

	apispec "ticketing/services/catalog/api"
	"ticketing/shared/contract/contractlint"
)

var catalogContract = contractlint.ServiceConfig{
	Spec:        apispec.Spec,
	Directory:   ".",
	RouteSource: contractlint.GeneratedRoutes,
	StatusWrites: contractlint.Config{
		WriteFuncs: []string{"writeJSON"},
		StatusArg:  1,
		Floors: map[string][]int{
			"writeStoreError": {http.StatusInternalServerError},
		},
	},
	Rules: []contractlint.Rule{
		{Name: "handler statuses", Kind: contractlint.HandlerStatuses},
		{Name: "request-layer bad request", Kind: contractlint.RequestRejections, Status: http.StatusBadRequest},
		{
			Name: "store-error not found", Kind: contractlint.ReachesBoundary,
			Status: http.StatusNotFound, Receiver: "Server", Boundary: "writeStoreError",
		},
	},
}

func TestCatalogOpenAPIContractPolicies(t *testing.T) {
	result, err := contractlint.Analyze(catalogContract)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range catalogContract.Rules {
		rule := rule
		t.Run(rule.Name, func(t *testing.T) {
			if report := result.Report(rule.Name); report != "" {
				t.Fatalf("OpenAPI contract policy failed:\n%s", report)
			}
		})
	}
}
