package api

import (
	"net/http"
	"testing"

	apispec "ticketing/services/commerce/api"
	"ticketing/shared/contract/contractlint"
)

var commerceContract = contractlint.ServiceConfig{
	Spec:        apispec.Spec,
	Directory:   ".",
	RouteSource: contractlint.ChiRoutes,
	StatusWrites: contractlint.Config{
		WriteFuncs: []string{"write"},
		StatusArg:  1,
		ReturnedStatuses: map[string][]int{
			"persistenceReadProblem": {http.StatusNotFound, http.StatusServiceUnavailable},
			"checkoutClaimProblem":   {http.StatusConflict, http.StatusInternalServerError},
			"paymentOutcomeProblem":  {http.StatusConflict},
			"refundProblem": {
				http.StatusNotFound,
				http.StatusConflict,
				http.StatusInternalServerError,
				http.StatusBadGateway,
				http.StatusServiceUnavailable,
			},
			"exchangeProblem":      {http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError},
			"voidProblem":          {http.StatusConflict, http.StatusInternalServerError},
			"terminalCheckoutCode": {http.StatusPaymentRequired, http.StatusRequestTimeout},
		},
	},
	// reserveWithScope only writes 403 when its scope argument is non-nil. The documented
	// POST /reservations route passes nil; the partner route is outside the document.
	ExcludedOnRoute: map[string][]int{
		"POST /reservations": {http.StatusForbidden},
	},
	Rules: []contractlint.Rule{
		{Name: "handler statuses", Kind: contractlint.HandlerStatuses},
	},
}

func TestCommerceOpenAPIContractPolicies(t *testing.T) {
	result, err := contractlint.Analyze(commerceContract)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range commerceContract.Rules {
		rule := rule
		t.Run(rule.Name, func(t *testing.T) {
			if report := result.Report(rule.Name); report != "" {
				t.Fatalf("OpenAPI contract policy failed:\n%s", report)
			}
		})
	}
}
