package api

import (
	"testing"

	apispec "ticketing/services/access/api"
	"ticketing/shared/contract/contractlint"
)

var accessContract = contractlint.ServiceConfig{
	Spec:        apispec.Spec,
	Directory:   ".",
	RouteSource: contractlint.ChiRoutes,
	StatusWrites: contractlint.Config{
		WriteFuncs: []string{"write"},
		StatusArg:  1,
	},
	Rules: []contractlint.Rule{
		{Name: "handler statuses", Kind: contractlint.HandlerStatuses},
	},
}

func TestAccessOpenAPIContractPolicies(t *testing.T) {
	result, err := contractlint.Analyze(accessContract)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range accessContract.Rules {
		rule := rule
		t.Run(rule.Name, func(t *testing.T) {
			if report := result.Report(rule.Name); report != "" {
				t.Fatalf("OpenAPI contract policy failed:\n%s", report)
			}
		})
	}
}
