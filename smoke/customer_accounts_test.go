//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// The wallet through the live gateway (TKT-222 / US-A3).
//
// Only what a unit test structurally cannot reach: that the read is served
// through the gateway with **no service credential** — only the assertion the
// buyer earned — and that one customer's assertion cannot open another's wallet
// across the real stack.
func TestWalletIsReachableWithOnlyTheCustomersOwnAssertion(t *testing.T) {
	type principal struct {
		CustomerID string `json:"customer_id"`
		Assertion  string `json:"customer_assertion"`
	}
	register := func(t *testing.T) principal {
		t.Helper()
		status, body := customerPost(t, "/api/commerce/customers", map[string]string{
			"email": "wallet-" + uuid.NewString() + "@example.test", "password": "correct horse battery",
		})
		if status != http.StatusCreated {
			t.Fatalf("register: %d %s", status, body)
		}
		var p principal
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		if p.Assertion == "" {
			t.Fatal("registration returned no assertion; the wallet has no way to identify anyone")
		}
		return p
	}

	wallet := func(t *testing.T, customerID, assertion string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet,
			gatewayURL+"/api/commerce/customers/"+customerID+"/orders?locale=en", nil)
		if err != nil {
			t.Fatal(err)
		}
		// No X-Internal-Token, no staff-write token. That is the assertion.
		if assertion != "" {
			req.Header.Set("X-Customer-Assertion", assertion)
		}
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET wallet: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store — a wallet is per-customer",
				resp.Header.Get("Cache-Control"))
		}
		return resp.StatusCode, body
	}

	alice, bob := register(t), register(t)

	status, body := wallet(t, alice.CustomerID, alice.Assertion)
	if status != http.StatusOK {
		t.Fatalf("alice reading her own wallet: %d %s", status, body)
	}
	// A brand-new account: the empty page, which is an acceptance criterion and
	// the case every other fixture skips.
	if !strings.Contains(string(body), `"orders":[]`) {
		t.Fatalf("a new customer's wallet should be an empty ARRAY, got %s", body)
	}

	// The whole point. Bob holds a perfectly valid assertion — his own.
	if status, body = wallet(t, alice.CustomerID, bob.Assertion); status != http.StatusNotFound {
		t.Fatalf("bob reading alice's wallet: status %d, want 404 (and NOT 403, which would confirm "+
			"she exists): %s", status, body)
	}
	if status, body = wallet(t, alice.CustomerID, ""); status != http.StatusUnauthorized {
		t.Fatalf("no assertion at all: status %d, want 401: %s", status, body)
	}
}

// Claiming a guest order through the live gateway (TKT-223 / US-A4).
//
// What only the live stack proves: the claim is reachable with the customer's own
// assertion and NO service credential, and one customer's assertion cannot take
// another's order — across the real gateway, not a handler under test.
func TestClaimIsReachableWithOnlyTheCustomersOwnAssertion(t *testing.T) {
	type principal struct {
		CustomerID string `json:"customer_id"`
		Assertion  string `json:"customer_assertion"`
	}
	register := func(t *testing.T) principal {
		t.Helper()
		status, body := customerPost(t, "/api/commerce/customers", map[string]string{
			"email": "claim-" + uuid.NewString() + "@example.test", "password": "correct horse battery",
		})
		if status != http.StatusCreated {
			t.Fatalf("register: %d %s", status, body)
		}
		var p principal
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	claim := func(t *testing.T, ref, assertion string) (int, []byte) {
		t.Helper()
		encoded, err := json.Marshal(map[string]string{"guest_order_ref": ref})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, gatewayURL+"/api/commerce/orders/claim", bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		// No X-Internal-Token and no staff-write token. That is the assertion.
		if assertion != "" {
			req.Header.Set("X-Customer-Assertion", assertion)
		}
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST claim: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)
		return resp.StatusCode, body
	}

	buyer := register(t)

	// The happy path, end to end through the live stack: buy as a GUEST, sign up
	// afterwards with a different address, and claim.
	//
	// The different address is the point. Reference-only proof exists precisely so
	// a buyer who signed up later, with whatever address they felt like, is not
	// locked out of their own history — a fixture that reused the checkout email
	// would pass against an implementation that required them to match.
	_, ticketType := setupCheckoutOffer(t, "claim")
	reservation := reserveCheckout(t, ticketType, "reserve-claim")
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-claim", map[string]any{
		"reservation_id": reservation["reservation_id"],
		"name":           "Guest Buyer",
		"email":          "guest-" + uuid.NewString() + "@example.test",
		"payment_token":  "fake-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("guest checkout: %d %s", code, body)
	}
	var purchase struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &purchase); err != nil {
		t.Fatal(err)
	}

	status, claimed := claim(t, purchase.GuestOrderRef, buyer.Assertion)
	if status != http.StatusOK {
		t.Fatalf("claiming a completed guest order: %d %s", status, claimed)
	}
	var result struct {
		OrderID    string `json:"order_id"`
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(claimed, &result); err != nil {
		t.Fatal(err)
	}
	if result.OrderID != purchase.OrderID || result.CustomerID != buyer.CustomerID {
		t.Fatalf("claim result = %+v, want order %s for customer %s", result, purchase.OrderID, buyer.CustomerID)
	}

	// Idempotent: the same customer claiming again succeeds rather than erroring,
	// so a browser retry is safe.
	if status, again := claim(t, purchase.GuestOrderRef, buyer.Assertion); status != http.StatusOK {
		t.Fatalf("claiming twice as the same customer: %d %s", status, again)
	}

	// And somebody ELSE gets the ordinary refusal — the same 404 an unknown
	// reference gets, so the two are indistinguishable across the real stack.
	stranger := register(t)
	if status, refused := claim(t, purchase.GuestOrderRef, stranger.Assertion); status != http.StatusNotFound {
		t.Fatalf("a stranger claiming a taken order: %d, want 404: %s", status, refused)
	}

	// An order reference nobody holds: the generic refusal, and it must be the
	// SAME answer a real-but-taken order would give.
	if status, body := claim(t, uuid.NewString(), buyer.Assertion); status != http.StatusNotFound {
		t.Fatalf("claiming an unknown reference: status %d, want 404: %s", status, body)
	}
	// No assertion at all.
	if status, body := claim(t, uuid.NewString(), ""); status != http.StatusUnauthorized {
		t.Fatalf("claiming with no assertion: status %d, want 401: %s", status, body)
	}
	// A malformed reference is request validation, NOT an answer about the order
	// book — the one place the two are deliberately distinguishable.
	if status, body := claim(t, "not-a-uuid", buyer.Assertion); status != http.StatusBadRequest {
		t.Fatalf("claiming a malformed reference: status %d, want 400: %s", status, body)
	}
}
