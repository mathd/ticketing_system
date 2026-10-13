//go:build smoke

// GA sellout end-to-end (TKT-341 / ADR-082): a published GA pool exhausted through the
// public buyer path answers `sold_out` through the gateway, on inventory's hold and on
// commerce's reservation. A partial shortage and a closed slot do not.
//
// The store test proves the classification and commerce's handler tests prove the
// allowlist. This proves the wiring between them: inventory's mapper, both services'
// response validators and enums, commerce's forwarding, and the gateway. Removing the
// forwarding in commerce leaves every inventory test green and turns this one red.
package smoke_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAnExhaustedGAPoolAnswersSoldOutThroughTheGateway(t *testing.T) {
	run := uuid.NewString()[:8]
	// GA capacity 5 (setupCheckoutOffer's venue).
	slot, ticketType := setupCheckoutOffer(t, "sold-out "+run)
	reservations := gatewayURL + "/api/commerce/reservations"
	holds := gatewayURL + "/api/inventory/holds"
	reserve := func(qty int, key string) (int, string) {
		t.Helper()
		code, body := postWithKey(t, reservations, "tkt341-"+run+"-"+key,
			map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": qty})
		return code, strings.TrimSpace(string(body))
	}
	hold := func(qty int, key string) (int, string) {
		t.Helper()
		code, body := postWithKey(t, holds, "tkt341-"+run+"-"+key,
			map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": qty})
		return code, strings.TrimSpace(string(body))
	}

	if code, body := reserve(4, "first"); code != 201 {
		t.Fatalf("first reservation: %d %s", code, body)
	}
	// One unit left, two asked for: a partial shortage keeps the code-less refusal on
	// both services, because the buyer can buy fewer now.
	if code, body := reserve(2, "partial"); code != 409 || body != `{"error":"inventory unavailable"}` {
		t.Fatalf("partial reservation: %d %s, want 409 without a code", code, body)
	}
	if code, body := hold(2, "partial-hold"); code != 409 || body != `{"error":"insufficient capacity"}` {
		t.Fatalf("partial hold: %d %s, want 409 without a code", code, body)
	}
	if code, body := reserve(1, "last"); code != 201 {
		t.Fatalf("last unit: %d %s", code, body)
	}

	// Zero public headroom, held out by live holds: a sellout on both services.
	if code, body := reserve(1, "after"); code != 409 || body != `{"code":"sold_out","error":"sold out"}` {
		t.Fatalf("reservation on an exhausted pool: %d %s, want 409 sold_out", code, body)
	}
	if code, body := hold(1, "after-hold"); code != 409 || body != `{"code":"sold_out","error":"insufficient capacity: sold out"}` {
		t.Fatalf("hold on an exhausted pool: %d %s, want 409 sold_out", code, body)
	}

	// A closed slot is not a sellout, even when it is also full: closure is judged
	// first, and commerce never relabels it.
	if code, body := postJSON(t, fmt.Sprintf("%s/api/catalog/performances/%s/close", gatewayURL, slot), map[string]any{"reason": "storm"}); code != 200 {
		t.Fatalf("close: %d %s", code, body)
	}
	waitForOffering(t, slot, "closed")
	if code, body := hold(1, "closed-hold"); code != 409 || conflictCode(t, []byte(body)) != "slot_closed" {
		t.Fatalf("hold on a closed, full slot: %d %s, want 409 slot_closed", code, body)
	}
	if code, body := reserve(1, "closed"); code == 201 || strings.Contains(body, "sold_out") {
		t.Fatalf("reservation on a closed, full slot: %d %s, want a refusal without sold_out", code, body)
	}
}
