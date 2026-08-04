//go:build smoke

package smoke_test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The refund form's server-minted idempotency key. Extracted from the rendered
// page rather than generated here, because the whole point is that ONE rendered
// form carries ONE key: two submits of the same page must replay, and a test
// that invented its own key would be proving something else.
var refundKeyRE = regexp.MustCompile(`name="idempotency_key"[^>]*value="([0-9a-fA-F-]{36})"`)

func refundFormKey(t *testing.T, page string) string {
	t.Helper()
	m := refundKeyRE.FindStringSubmatch(page)
	if len(m) != 2 {
		t.Fatalf("no refund form on the page; body=%.700s", page)
	}
	return m[1]
}

func lookupOrder(t *testing.T, c *http.Client, orderID, ref string) string {
	t.Helper()
	return readBody(t, postForm(t, c, gatewayURL+"/admin/orders", url.Values{
		"_action": {"lookup"}, "order_id": {orderID}, "ticket_ref": {ref}}))
}

func submitRefund(t *testing.T, c *http.Client, orderID, key, qty, reason string) *http.Response {
	t.Helper()
	return postForm(t, c, gatewayURL+"/admin/orders", url.Values{
		"_action": {"refund"}, "order_id": {orderID}, "ticket_ref": {""},
		"idempotency_key": {key}, "quantity": {qty}, "reason": {reason}})
}

// TestBackofficeRefundMovesMoneyAndReportsCommercesResult is COS-1 and COS-8.
func TestBackofficeRefundMovesMoneyAndReportsCommercesResult(t *testing.T) {
	orderID, guestRef, _, _ := consoleFixture(t, "refund")
	// Box office, not admin: the owner decided box office refunds, so the
	// capability is proven for the role that actually uses it.
	client := signInAs(t, "box_office")

	page := lookupOrder(t, client, orderID, guestRef)
	key := refundFormKey(t, page)

	resp := submitRefund(t, client, orderID, key, "1", "customer called")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refund status %d", resp.StatusCode)
	}
	page = readBody(t, resp)

	// The fixture buys 2 tickets at 1250 EUR each, so refunding 1 is a PARTIAL
	// refund of 1250 minor units — values that differ from the order total, so
	// a page echoing the wrong field is visible.
	for _, want := range []string{"Commerce refunded", "1250 EUR", "partial"} {
		if !strings.Contains(page, want) {
			t.Errorf("refund result missing %q; body=%.800s", want, page)
		}
	}
	// COS-8: minor units, never a decimal rendering of them.
	for _, forbidden := range []string{"12.50", "12,50", "€12"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("money rendered as a decimal (%q); body=%.800s", forbidden, page)
		}
	}
	// ADR-037: the order stays completed. A refund is a separate dimension, and
	// a console reporting "status: refunded" would be contradicting commerce.
	if !strings.Contains(page, "completed") {
		t.Errorf("the order's own status stopped being reported; body=%.800s", page)
	}
	if strings.Contains(page, "Commerce reports this order as <strong>refunded") {
		t.Error("the console reported the refund as an order status change (ADR-037)")
	}
}

// TestBackofficeRefundIsIdempotentUnderAConcurrentDoubleSubmit is COS-6.
//
// Concurrent, from a barrier — not sequential. A sequential pair passes with a
// per-request key, because the second request simply refunds again and both
// "succeed"; only overlapping requests sharing one key show that commerce
// replayed rather than refunded twice.
func TestBackofficeRefundIsIdempotentUnderAConcurrentDoubleSubmit(t *testing.T) {
	orderID, guestRef, _, _ := consoleFixture(t, "double")
	client := signInAs(t, "box_office")
	key := refundFormKey(t, lookupOrder(t, client, orderID, guestRef))

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	bodies := make([]string, 2)
	codes := make([]int, 2)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			resp := submitRefund(t, client, orderID, key, "1", "double submit")
			codes[i], bodies[i] = resp.StatusCode, readBody(t, resp)
		}(i)
	}
	start.Done()
	wg.Wait()

	refundIDs := map[string]bool{}
	for i, body := range bodies {
		if codes[i] != http.StatusOK {
			t.Fatalf("submit %d: status %d; body=%.400s", i, codes[i], body)
		}
		m := regexp.MustCompile(`Refund ([0-9a-fA-F-]{36})`).FindStringSubmatch(body)
		if len(m) != 2 {
			t.Fatalf("submit %d rendered no refund id; body=%.700s", i, body)
		}
		refundIDs[m[1]] = true
	}
	if len(refundIDs) != 1 {
		t.Errorf("two concurrent submits of one form produced %d refunds: %v", len(refundIDs), refundIDs)
	}

	// The durable check, and the one that matters: commerce's own arithmetic.
	//
	// The fixture buys TWO tickets. If the concurrent pair replayed, exactly one
	// is refunded and one remains — so refunding one more must succeed and take
	// the order to `full`. If it had refunded twice, nothing remains and this
	// would be refused. Asserting on the page's rendered refund id alone would
	// not distinguish those: it could render one id while two refunds landed.
	page := lookupOrder(t, client, orderID, guestRef)
	body := readBody(t, submitRefund(t, client, orderID, refundFormKey(t, page), "1", "refund the remainder"))
	if !strings.Contains(body, "full") {
		t.Errorf("one ticket did not remain refundable, so the concurrent pair did not replay; body=%.900s", body)
	}
}

// TestRefundIsRefusedToFinanceAndAnonymously is COS-2.
//
// It proves the ROUTE rule covers the write — box office and admin may refund
// (owner decision), finance may not reach the console at all, and there is no
// second action-level mechanism. Saying that precisely matters: this is not a
// per-action guarantee, it is the page's guarantee.
func TestRefundIsRefusedToFinanceAndAnonymously(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "refund-rbac")
	key := "abcdef01-2345-4678-89ab-cdef01234567"

	if r := submitRefund(t, signInAs(t, "finance"), orderID, key, "1", "should not happen"); r.StatusCode != http.StatusForbidden {
		t.Errorf("finance refunded: status %d", r.StatusCode)
	}
	anon := submitRefund(t, jarClient(t), orderID, key, "1", "should not happen")
	if anon.StatusCode == http.StatusOK {
		t.Errorf("an anonymous caller refunded; body=%.300s", readBody(t, anon))
	}

	// And the refusals refunded nothing: box office can still refund all 2.
	client := signInAs(t, "box_office")
	page := lookupOrder(t, client, orderID, "")
	body := readBody(t, submitRefund(t, client, orderID, refundFormKey(t, page), "2", "post-refusal check"))
	if !strings.Contains(body, "full") {
		t.Errorf("a refused caller appears to have consumed refundable quantity; body=%.800s", body)
	}
}

// TestGatewayStillEdgeDeniesCommercesRefund is the negative half of COS-4, and
// the assertion that outlives this ticket.
//
// The back office reaches the refund DIRECTLY, in-network. The gateway's
// /internal/ deny is therefore untouched, and it must stay that way: adding a
// route here would publish a money-moving endpoint to the public internet while
// granting the back office nothing it does not already have.
func TestGatewayStillEdgeDeniesCommercesRefund(t *testing.T) {
	for _, path := range []string{
		"/api/commerce/internal/orders/00000000-0000-0000-0000-000000000001/refunds",
		"/api/commerce/internal/orders/00000000-0000-0000-0000-000000000001/exchanges",
		"/api/commerce/internal/buyers/00000000-0000-0000-0000-000000000001/delivery-email",
	} {
		code, body, _ := postRaw(t, gatewayURL+path, os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"))
		if code != http.StatusNotFound {
			t.Errorf("%s is reachable from the public edge: %d %s", path, code, body)
		}
	}
}

func postRaw(t *testing.T, target, staffToken string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "gateway-deny-probe")
	// WITH the real staff credential: the point is that the edge refuses it
	// regardless of what the caller holds, not that an unauthenticated probe
	// bounces.
	req.Header.Set("X-Commerce-Staff-Write-Token", staffToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, fmt.Sprintf("%.200s", readBody(t, resp)), resp.Header
}
