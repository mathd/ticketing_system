//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Customer accounts through the live gateway (TKT-220 / US-A1, ADR-049).
//
// Deliberately only TWO assertions. Everything else about these operations —
// the 409 on a duplicate, the identical unknown/wrong-password answers, the
// timing shape, the contract being served byte-identically — is already proven by
// the store tests, the handler tests running against the real contract validator,
// and the existing contract-validation smoke tests. Re-asserting them here would
// buy a slower gate, not more coverage.
//
// What ONLY a live stack can prove is the pair below, and both are properties of
// the deployment rather than of the code:
//
//  1. The operations are reachable THROUGH the gateway with no credential at all.
//     That is what lets the storefront container keep holding exactly one
//     environment variable and no service token — an assumption in compose.yaml,
//     not in any Go file.
//  2. They are NOT under the edge-denied /internal/ namespace. The gateway
//     registers /api/<svc>/internal/ to a deny handler; a path that drifted under
//     it would 404 for every real buyer while every unit test stayed green.

func customerPost(t *testing.T, path string, body map[string]string) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, gatewayURL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// No X-Internal-Token, no X-Commerce-Staff-Write-Token, no credential of any
	// kind. That is the assertion, not an omission.
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, responseBody)
	return resp.StatusCode, responseBody
}

func TestCustomerAccountsAreReachableThroughTheGatewayWithoutACredential(t *testing.T) {
	email := "smoke-" + uuid.NewString() + "@example.test"
	const password = "correct horse battery"

	status, body := customerPost(t, "/api/commerce/customers", map[string]string{
		"email": email, "password": password,
	})
	if status != http.StatusCreated {
		t.Fatalf("register: status = %d, want 201: %s", status, body)
	}
	var registered struct {
		CustomerID string `json:"customer_id"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatal(err)
	}
	if registered.CustomerID == "" || registered.Email != email {
		t.Fatalf("principal = %+v", registered)
	}

	status, body = customerPost(t, "/api/commerce/customers/authenticate", map[string]string{
		"email": email, "password": password,
	})
	if status != http.StatusOK {
		t.Fatalf("authenticate: status = %d, want 200: %s", status, body)
	}
	var signedIn struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(body, &signedIn); err != nil {
		t.Fatal(err)
	}
	if signedIn.CustomerID != registered.CustomerID {
		t.Fatalf("authenticated a different account: %s != %s", signedIn.CustomerID, registered.CustomerID)
	}
}

// The gateway answers 404 with its own distinctive body for anything under
// /api/<svc>/internal/. Asserting the customer paths are NOT refused that way is
// how a future move under /internal/ — which would 404 for every buyer while the
// unit tests stayed green — becomes visible.
func TestCustomerAccountPathsAreNotEdgeDenied(t *testing.T) {
	for _, path := range []string{"/api/commerce/customers", "/api/commerce/customers/authenticate"} {
		// A deliberately invalid body: the point is WHICH layer refuses, not that
		// the request succeeds. The gateway's edge-deny answers 404 before
		// commerce sees anything; commerce's own validator answers 400.
		status, body := customerPost(t, path, map[string]string{"email": ""})
		if status == http.StatusNotFound {
			t.Fatalf("%s was refused at the gateway edge — it has drifted under /internal/: %s", path, body)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 from commerce's own validator: %s", path, status, body)
		}
	}
}
