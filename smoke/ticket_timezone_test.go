//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-365 (ADR-083). The ticket bundle carries each ticket's performance time zone, which
// Access reads from Catalog. This is the only tier that runs the real wiring: Access's own
// tests use a fake zone reader, so they stay green if cmd/access never passes the Catalog
// client to the server, or if compose never gives Access a CATALOG_URL. Here the zone
// must cross Catalog, Access and the gateway.
//
// Asia/Kolkata on purpose: it is neither UTC nor a whole-hour offset, so no default can
// stand in for it.
func TestTicketBundleCarriesThePerformanceTimeZone(t *testing.T) {
	_, ticketType := setupCheckoutOfferIn(t, "zone", 1250, "Asia/Kolkata")
	// Fresh keys per run, so a rerun against a stack left up is a new purchase, not a replay.
	run := uuid.NewString()
	reservation := reserveCheckout(t, ticketType, "reserve-zone-"+run)
	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "order-zone-"+run, map[string]any{
		"reservation_id": reservation["reservation_id"], "name": "Zone Buyer", "email": "zone@example.test", "payment_token": "fake-ok",
	})
	if code != http.StatusOK {
		t.Fatalf("checkout %d %s", code, body)
	}
	var order struct {
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &order); err != nil || order.GuestOrderRef == "" {
		t.Fatalf("checkout body %s: %v", body, err)
	}

	retry(t, 10*time.Second, func() error {
		bundleCode, bundleBody, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+order.GuestOrderRef+"/tickets")
		if bundleCode != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", bundleCode, bundleBody)
		}
		var bundle struct {
			Tickets []map[string]json.RawMessage `json:"tickets"`
		}
		if err := json.Unmarshal(bundleBody, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 2 {
			return fmt.Errorf("issued tickets = %d, want 2", len(bundle.Tickets))
		}
		for i, ticket := range bundle.Tickets {
			if string(ticket["performance_timezone"]) != `"Asia/Kolkata"` {
				return fmt.Errorf("tickets[%d].performance_timezone = %s, want \"Asia/Kolkata\"", i, ticket["performance_timezone"])
			}
		}
		return nil
	})
}
