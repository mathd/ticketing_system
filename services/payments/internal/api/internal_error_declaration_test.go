package api

import (
	"net/http"
	"testing"

	apispec "ticketing/services/payments/api"
	"ticketing/shared/contract/contractlint"
)

// Every documented payments operation reaches a fallible store or provider dependency.
var paymentsContract = contractlint.ServiceConfig{
	Spec:        apispec.Spec,
	Directory:   ".",
	RouteSource: contractlint.DocumentRoutes,
	Rules: []contractlint.Rule{
		{Name: "dependency failures", Kind: contractlint.AllOperations, Status: http.StatusInternalServerError},
	},
}

func TestPaymentsOpenAPIContractPolicies(t *testing.T) {
	result, err := contractlint.Analyze(paymentsContract)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range paymentsContract.Rules {
		rule := rule
		t.Run(rule.Name, func(t *testing.T) {
			if report := result.Report(rule.Name); report != "" {
				t.Fatalf("OpenAPI contract policy failed:\n%s", report)
			}
		})
	}
}
