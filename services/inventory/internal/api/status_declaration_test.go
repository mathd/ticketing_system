package api

import (
	"net/http"
	"testing"

	apispec "ticketing/services/inventory/api"
	"ticketing/shared/contract/contractlint"
)

// These floors are unconditional guarantees of helpers used by inventory handlers. The
// conditional 400/404/409 branches of problem are not attributed to every caller.
var inventoryContract = contractlint.ServiceConfig{
	Spec:        apispec.Spec,
	Directory:   ".",
	RouteSource: contractlint.ChiRoutes,
	StatusWrites: contractlint.Config{
		WriteFuncs: []string{"write"},
		StatusArg:  1,
		Floors: map[string][]int{
			"problem":         {http.StatusInternalServerError},
			"internalOnly":    {http.StatusUnauthorized},
			"staffOrInternal": {http.StatusUnauthorized},
			"idempotencyKey":  {http.StatusBadRequest},
		},
		ReturnedStatuses: map[string][]int{
			"refundCapacityProblem": {http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
		},
	},
	Rules: []contractlint.Rule{
		{Name: "handler statuses", Kind: contractlint.HandlerStatuses},
	},
}

func TestInventoryOpenAPIContractPolicies(t *testing.T) {
	result, err := contractlint.Analyze(inventoryContract)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range inventoryContract.Rules {
		rule := rule
		t.Run(rule.Name, func(t *testing.T) {
			if report := result.Report(rule.Name); report != "" {
				t.Fatalf("OpenAPI contract policy failed:\n%s", report)
			}
		})
	}
}
