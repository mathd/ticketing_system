package api

import (
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	apispec "ticketing/services/commerce/api"
)

// The exchange 202 is DECLARED (TKT-167).
//
// `exchangeOrder` has answered 202 `confirmation_pending` since TKT-158, on the branch where
// the delta settled and the target claim did not confirm. The contract did not declare it.
// Commerce mounts the shared validator with response validation on by default
// (cmd/commerce/main.go reads runtimecfg.ResponseValidationFromEnv), and ADR-028 makes an
// undeclared status fail closed as a 500 — so the one exchange state a retry can recover
// from was reported to its caller as an outage.
//
// That matters more here than it would elsewhere: the 202 IS the resume's entry point. The
// caller told "500 persist exchange" has no reason to replay the key, and the whole point
// of the persisted basis is that replaying the key finishes the job.
//
// This test asserts the DECLARATION, not the handler. Deleting the '202' from openapi.yaml
// is the mutation it catches, and that mutation is invisible to every test that drives the
// handler directly — including exchange_resume_smoke_test.go, which mounts the raw chi
// router precisely so it tests the handler rather than the validator. Two different
// mechanisms, two tiers.
func TestTheExchangeOperationDeclaresItsConfirmationPending(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	item := doc.Paths.Find("/internal/orders/{id}/exchanges")
	if item == nil || item.Post == nil {
		t.Fatal("no POST /internal/orders/{id}/exchanges in the contract")
	}
	res := item.Post.Responses.Status(http.StatusAccepted)
	if res == nil || res.Value == nil {
		t.Fatal("exchangeOrder does not declare 202, but the handler writes one when the delta " +
			"settled and the target claim did not confirm. Undeclared, the response validator " +
			"turns it into a 500 (ADR-028) and the caller is told an outage happened instead of " +
			"being told to replay the key")
	}

	media := res.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("the 202 declares no application/json schema")
	}
	schema := media.Schema.Value

	// The body the handler actually writes must validate against what the contract promises.
	// A declared status whose schema does not describe the payload is drift wearing a
	// declaration — the validator would still fail the response closed.
	if err := schema.VisitJSON(map[string]any{
		"exchange_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"status":      "confirmation_pending",
	}); err != nil {
		t.Errorf("the body the handler writes does not satisfy the declared 202 schema: %v", err)
	}

	// And the status is a closed enum, so a future branch cannot answer 202 with a state
	// nobody declared — which is the same drift one level down.
	if err := schema.VisitJSON(map[string]any{
		"exchange_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"status":      "something_else",
	}); err == nil {
		t.Error("the 202 schema accepts any status string; it must name the one state it describes")
	}
}
