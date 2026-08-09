//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Undoing a claim through the live stack (TKT-225 / ADR-052).
//
// What only the live stack proves, and no handler test can: the operation is
// reachable on commerce directly with the service credential, it is NOT reachable
// through the gateway (which edge-denies /internal/ by construction), and the
// order it detaches becomes claimable again by a different customer — across the
// real gateway, real database and real contract validator.
//
// The last part is the one that matters most. The claim and the detach are two
// halves of one story and the ticket's argument is that the buyer gets their
// recourse back; a test that only asserted "customer_id became NULL" would pass
// against an implementation that left the order unclaimable by anyone.
func TestAClaimedOrderCanBeDetachedAndClaimedAgain(t *testing.T) {
	type principal struct {
		CustomerID string `json:"customer_id"`
		Assertion  string `json:"customer_assertion"`
	}
	register := func(t *testing.T, prefix string) principal {
		t.Helper()
		status, body := customerPost(t, "/api/commerce/customers", map[string]string{
			"email": prefix + "-" + uuid.NewString() + "@example.test", "password": "correct horse battery",
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
		req.Header.Set("X-Customer-Assertion", assertion)
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST claim: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)
		return resp.StatusCode, body
	}

	// A guest buys, then the WRONG account claims it — the situation this
	// operation exists to undo. Anyone holding the reference can do this, which is
	// the exposure ADR-049 accepted and TKT-202 is the cause of.
	_, ticketType := setupCheckoutOffer(t, "unclaim")
	reservation := reserveCheckout(t, ticketType, "reserve-unclaim")
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-unclaim", map[string]any{
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

	wrong := register(t, "unclaim-wrong")
	if status, taken := claim(t, purchase.GuestOrderRef, wrong.Assertion); status != http.StatusOK {
		t.Fatalf("the wrong account claiming: %d %s", status, taken)
	}

	// The rightful buyer cannot claim it now — this is the state the operator is
	// called about, and asserting it here is what makes the recovery below mean
	// something.
	rightful := register(t, "unclaim-rightful")
	if status, refused := claim(t, purchase.GuestOrderRef, rightful.Assertion); status != http.StatusNotFound {
		t.Fatalf("the rightful buyer claiming a taken order: %d, want 404: %s", status, refused)
	}

	// The gateway must NOT expose this. /internal/ is edge-denied by construction,
	// and a support action on someone else's purchase is exactly what that denial
	// is for.
	unclaimPath := fmt.Sprintf("/api/commerce/internal/orders/%s/unclaim", purchase.OrderID)
	if status, viaGateway := customerPost(t, unclaimPath, map[string]string{
		"actor": "staff:amy", "reason": "reachable through the gateway?",
	}); status != http.StatusNotFound {
		t.Fatalf("the un-claim is reachable through the gateway: %d %s", status, viaGateway)
	}

	// Directly on commerce, with the service credential.
	unclaimURL := fmt.Sprintf("%s/internal/orders/%s/unclaim", commerceURL, purchase.OrderID)
	status, detached := internalJSON(t, http.MethodPost, unclaimURL, "", map[string]string{
		"actor": "staff:amy", "reason": "claimed by the wrong account",
	})
	if status != http.StatusOK {
		t.Fatalf("un-claim: %d %s", status, detached)
	}
	var result struct {
		OrderID            string `json:"order_id"`
		DetachedCustomerID string `json:"detached_customer_id"`
	}
	if err := json.Unmarshal(detached, &result); err != nil {
		t.Fatal(err)
	}
	// It reports WHO lost the order. That value is gone from `orders` the moment
	// this succeeds, so an operator who detached the wrong one has no other way to
	// find out who to hand it back to.
	if result.OrderID != purchase.OrderID || result.DetachedCustomerID != wrong.CustomerID {
		t.Fatalf("un-claim result = %+v, want order %s detached from %s",
			result, purchase.OrderID, wrong.CustomerID)
	}

	// The recourse the ticket exists to restore.
	if status, reclaimed := claim(t, purchase.GuestOrderRef, rightful.Assertion); status != http.StatusOK {
		t.Fatalf("the rightful buyer claiming after a detach: %d %s", status, reclaimed)
	}

	// And the same detach does not apply twice: the order now belongs to the
	// rightful buyer, and a replayed request must not silently take it away again.
	// (It IS detachable again — from its new owner — which is correct; what would
	// be wrong is the FIRST request's effect being repeatable without a new
	// decision. So this asserts the second one detaches the NEW owner, naming them.)
	status, second := internalJSON(t, http.MethodPost, unclaimURL, "", map[string]string{
		"actor": "staff:bo", "reason": "second detach, now from the rightful buyer",
	})
	if status != http.StatusOK {
		t.Fatalf("second un-claim: %d %s", status, second)
	}
	var secondResult struct {
		DetachedCustomerID string `json:"detached_customer_id"`
	}
	if err := json.Unmarshal(second, &secondResult); err != nil {
		t.Fatal(err)
	}
	if secondResult.DetachedCustomerID != rightful.CustomerID {
		t.Fatalf("the second detach reported %s, want the CURRENT owner %s — "+
			"an un-claim must act on the attribution it finds, not the one it remembers",
			secondResult.DetachedCustomerID, rightful.CustomerID)
	}

	// A third finds nothing to detach.
	if status, third := internalJSON(t, http.MethodPost, unclaimURL, "", map[string]string{
		"actor": "staff:bo", "reason": "nothing left",
	}); status != http.StatusNotFound {
		t.Fatalf("detaching an unattributed order: %d, want 404: %s", status, third)
	}
}
