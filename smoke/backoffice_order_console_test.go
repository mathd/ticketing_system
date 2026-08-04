//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// consoleFixture buys a real order through the public checkout and waits for
// access to issue its tickets, returning the two identifiers the console takes
// and the QR credential that must never appear in its output.
//
// The fixture is a REAL purchase rather than seeded rows because the identifier
// split this ticket exists to handle only shows up on the real path: checkout is
// the one place both the order id and the guest reference are produced, and it
// produces them as deliberately different values (ADR-012).
func consoleFixture(t *testing.T, suffix string) (orderID, guestRef string, ticketIDs, qrSecrets []string) {
	t.Helper()
	_, ticketType := setupCheckoutOffer(t, suffix)
	reservation := reserveCheckout(t, ticketType, "reserve-console-"+suffix)
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-console-"+suffix, map[string]any{
		"reservation_id": reservation["reservation_id"],
		"name":           "Console Buyer",
		"email":          "console@example.test",
		"payment_token":  "fake-ok",
	})
	if code != 200 {
		t.Fatalf("checkout %d %s", code, body)
	}
	var order map[string]any
	if err := json.Unmarshal(body, &order); err != nil {
		t.Fatal(err)
	}
	orderID, guestRef = fmt.Sprint(order["order_id"]), fmt.Sprint(order["guest_order_ref"])
	if orderID == guestRef || guestRef == "" || guestRef == "<nil>" {
		t.Fatalf("checkout must produce a guest ref distinct from the order id: %v", order)
	}

	// Issuance is asynchronous — the same wait the checkout smoke test performs.
	var bundle struct {
		Tickets []struct {
			TicketID  string `json:"ticket_id"`
			QRPayload string `json:"qr_payload"`
			QRURL     string `json:"qr_url"`
		} `json:"tickets"`
	}
	retry(t, 30*time.Second, func() error {
		code, raw, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestRef+"/tickets")
		if code != 200 {
			return fmt.Errorf("tickets %d %s", code, raw)
		}
		if err := json.Unmarshal(raw, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) == 0 {
			return fmt.Errorf("no tickets yet")
		}
		return nil
	})
	for _, tk := range bundle.Tickets {
		// Without this the credential assertions below would pass against an
		// empty string — the fixture must carry the secret to prove its absence.
		if tk.QRPayload == "" || tk.QRURL == "" {
			t.Fatalf("fixture cannot prove credential absence: access returned an empty qr field for %s", tk.TicketID)
		}
		qrSecrets = append(qrSecrets, tk.QRPayload, tk.QRURL)
		ticketIDs = append(ticketIDs, tk.TicketID)
	}
	return orderID, guestRef, ticketIDs, qrSecrets
}

func lookUpOrder(t *testing.T, c *http.Client, orderID, ref string) *http.Response {
	t.Helper()
	return postForm(t, c, gatewayURL+"/admin/orders", url.Values{
		"order_id": {orderID}, "ticket_ref": {ref}})
}

// TestBackofficeOrderConsoleShowsStatusAndLifecycle is the COS-1 proof, driven
// as a real form submission through the SSR layer — the only thing that
// exercises Astro's origin handling, the base-path rewrite and the response
// headers together (AGENTS.md, TKT-105).
func TestBackofficeOrderConsoleShowsStatusAndLifecycle(t *testing.T) {
	orderID, guestRef, ticketIDs, qrSecrets := consoleFixture(t, "console")
	client := signInAs(t, "box_office")

	resp := lookUpOrder(t, client, orderID, guestRef)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup status %d", resp.StatusCode)
	}
	page := readBody(t, resp)

	// COS-1: commerce's status and, per ticket, its lifecycle history.
	if !strings.Contains(page, "completed") {
		t.Errorf("page does not report the order's status; body=%.600s", page)
	}
	// NOT `strings.Contains(page, "issued")`: the ticket-not-found copy says "may
	// not have been issued yet", so that assertion passes on a page whose ticket
	// lookup returned nothing — it survives mutating the access read to receive
	// the order id instead of the reference (ai-review pass 1). What
	// distinguishes is the fixture's own ticket id, the event rendered as
	// markup, and the ABSENCE of the not-found copy.
	for _, id := range ticketIDs {
		if !strings.Contains(page, id) {
			t.Errorf("page does not render ticket %s; body=%.600s", id, page)
		}
	}
	if !strings.Contains(page, "<strong>issued</strong>") {
		t.Errorf("page does not render an issued lifecycle event; body=%.600s", page)
	}
	if strings.Contains(page, "No tickets matched that reference") {
		t.Errorf("page reports no tickets for an order that has them; body=%.600s", page)
	}

	// COS-3: ADR-004's "never" tier.
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	// COS-7, the finding that matters most. qr_payload is what a scanner admits
	// on and qr_url serves it as an image from an endpoint the gateway does NOT
	// authenticate — either one rendered here is a working ticket for someone
	// else's order, in every screenshot and support transcript thereafter.
	//
	// Asserted on the VALUES access actually issued for this order, not on
	// placeholder strings: a projection that renamed the field would still leak,
	// and only the real value catches that.
	for _, secret := range qrSecrets {
		if strings.Contains(page, secret) {
			t.Errorf("the console rendered a QR credential: %.40s...", secret)
		}
	}
	for _, key := range []string{"qr_payload", "qr_url", ".png"} {
		if strings.Contains(page, key) {
			t.Errorf("the console rendered %q; body=%.600s", key, page)
		}
	}

	// COS-6: it says what it cannot show, rather than rendering empty fields.
	if !strings.Contains(page, "does not show buyer contact") {
		t.Errorf("page does not declare its scope; body=%.600s", page)
	}
	for _, absent := range []string{"Total", "Line items"} {
		if strings.Contains(page, absent) {
			t.Errorf("page renders a %q field it has no read for", absent)
		}
	}

	// admin reaches it too, so the finance refusal in the RBAC test is about the
	// role and not about a broken URL.
	if r := lookUpOrder(t, signInAs(t, "admin"), orderID, guestRef); r.StatusCode != http.StatusOK {
		t.Errorf("admin lookup status %d", r.StatusCode)
	}
}

// TestBackofficeOrderConsoleHalvesFailIndependently is COS-2. The two reads are
// keyed by different identifiers, so each of these is a real state a support
// agent hits — not a contrived one.
func TestBackofficeOrderConsoleHalvesFailIndependently(t *testing.T) {
	orderID, guestRef, _, _ := consoleFixture(t, "halves")
	client := signInAs(t, "box_office")
	unknown := uuid.NewString()

	for _, tc := range []struct {
		name       string
		order, ref string
		status     int
		want       string
		absent     string
	}{
		{"order alone", orderID, unknown, 200, "No tickets matched that reference", ""},
		{"tickets alone", unknown, guestRef, 200, "No order matched that order id", ""},
		// Both absent is the only 404: nothing the console asked about exists.
		{"neither", unknown, uuid.NewString(), 404, "No order matched that order id", "completed"},
		{"nothing typed", "", "", 200, "Enter an order id, a ticket reference, or both", ""},
		// Refused here, so no request leaves for either service.
		{"malformed", "not-a-uuid", "", 200, "not a valid identifier", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := lookUpOrder(t, client, tc.order, tc.ref)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			page := readBody(t, resp)
			if !strings.Contains(page, tc.want) {
				t.Errorf("page missing %q; body=%.600s", tc.want, page)
			}
			if tc.absent != "" && strings.Contains(page, tc.absent) {
				t.Errorf("page contains %q, which nothing here should have produced", tc.absent)
			}
			// Whatever happened, the form comes back so the agent can correct it.
			if !strings.Contains(page, `name="order_id"`) {
				t.Errorf("the form did not survive; body=%.600s", page)
			}
		})
	}
}

// TestBackofficeOrderConsoleDoesNotCorrelateTwoOrders is COS-7, found by
// ai-review pass 1.
//
// Nothing links an order id to a ticket reference — that is why the page takes
// both — so submitting one order's id beside ANOTHER order's reference returns
// two unrelated successful reads. Rendering them under one heading would let a
// support agent read order A's status above order B's ticket history and act on
// it. Two real orders, deliberately crossed: the page must name the identifier
// each half came from and say it cannot vouch for the pairing.
func TestBackofficeOrderConsoleDoesNotCorrelateTwoOrders(t *testing.T) {
	orderA, _, _, _ := consoleFixture(t, "crossed-a")
	_, refB, ticketsB, _ := consoleFixture(t, "crossed-b")

	page := readBody(t, lookUpOrder(t, signInAs(t, "box_office"), orderA, refB))

	// Both reads succeeded — without this the assertions below would pass on a
	// page that simply found nothing, which is not the state being tested.
	if !strings.Contains(page, "completed") || !strings.Contains(page, ticketsB[0]) {
		t.Fatalf("the crossed lookup did not return two successful reads; body=%.800s", page)
	}
	if !strings.Contains(page, "cannot confirm that the two below belong to the same order") {
		t.Errorf("the page presented two unrelated lookups without saying so; body=%.800s", page)
	}
	if !strings.Contains(page, orderA) || !strings.Contains(page, refB) {
		t.Errorf("each half must name the identifier it came from; body=%.800s", page)
	}
}

// TestBackofficeOrderConsoleIsRefusedToFinance is COS-4 and COS-5: refusal is
// proven by DRIVING the url, because the absent nav link proves only that the
// link is absent.
func TestBackofficeOrderConsoleIsRefusedToFinance(t *testing.T) {
	console := gatewayURL + "/admin/orders"

	finance := signInAs(t, "finance")
	if body := readBody(t, doRequest(t, finance, http.MethodGet, gatewayURL+"/admin/", nil, nil)); strings.Contains(body, "Look up an order") {
		t.Error("finance is offered the order console in the nav")
	}
	for _, r := range []*http.Response{
		doRequest(t, finance, http.MethodGet, console, nil, nil),
		lookUpOrder(t, finance, uuid.NewString(), ""),
	} {
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("finance reached the console: status %d", r.StatusCode)
		}
	}

	// box_office IS offered it — without this the assertion above passes on a
	// page that offers the link to nobody.
	//
	// `/admin/`, with the slash: `/admin` is Astro's own 307 to the canonical
	// path, and these clients do not follow redirects, so the bare path returns
	// an empty body that contains no link for anybody (TKT-190 hit this too).
	if body := readBody(t, doRequest(t, signInAs(t, "box_office"), http.MethodGet, gatewayURL+"/admin/", nil, nil)); !strings.Contains(body, "Look up an order") {
		t.Error("box_office is not offered the order console")
	}

	// COS-4: signed out, the console is unreachable. Authentication is checked
	// before authorization, so this is a redirect to login and never the 403 the
	// wrong role gets — the difference is deliberate and observable (TKT-190).
	anon := doRequest(t, jarClient(t), http.MethodGet, console, nil, nil)
	if anon.StatusCode == http.StatusOK {
		t.Errorf("an anonymous caller reached the console; body=%.300s", readBody(t, anon))
	}
	if loc := anon.Header.Get("Location"); !strings.Contains(loc, "/admin/login") {
		t.Errorf("an anonymous caller was sent to %q, want the login page (status %d)", loc, anon.StatusCode)
	}
}
