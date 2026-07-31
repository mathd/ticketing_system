//go:build smoke

package smoke_test

// TKT-47: drivers for documented 2xx operations no feature test exercises.
// Each request flows through a validating helper, so the response is checked
// against the committed contract and recorded for the coverage gate
// (coverage_test.go). Feature behavior is asserted elsewhere — these drivers
// only pin the happy-path contract of otherwise-uncovered operations.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDocumentedOperationHappyPathDrivers(t *testing.T) {
	slot, tt := publishedSlot(t, "Coverage Hall", 20)
	actor := map[string]any{"actor": "staff:coverage", "reason": "contract coverage"}
	withActor := func(m map[string]any) map[string]any {
		for k, v := range actor {
			m[k] = v
		}
		return m
	}

	// inventory: hold placed publicly through the gateway, then released internally.
	code, body := postWithKey(t, gatewayURL+"/api/inventory/holds", "cov-hold-"+slot,
		map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("create hold: %d %s", code, body)
	}
	var hold struct {
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &hold); err != nil {
		t.Fatal(err)
	}
	// Hold transitions are internal-only (TKT-124): 404 at the gateway by the
	// /internal/ prefix registration, driven service-direct with the credential
	// (same path the offering-state test drives).
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/release?organizer_id=%s", inventoryURL, hold.HoldID, organizerID), "", nil); code != http.StatusOK {
		t.Fatalf("release hold: %d %s", code, body)
	}

	// inventory: operational hold placed, partially converted at inventory's
	// own seam (commerce's wrapper is covered by the operational-holds tests),
	// then released.
	code, body = internalJSON(t, http.MethodPost, inventoryURL+"/internal/operational-holds", "cov-op-place-"+slot,
		withActor(map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 2, "purpose": "house", "label": "coverage hold"}))
	if code != http.StatusCreated {
		t.Fatalf("place operational hold: %d %s", code, body)
	}
	var op struct {
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/operational-holds/%s/convert", inventoryURL, op.HoldID), "cov-op-convert-"+slot,
		withActor(map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1, "ticket_type_id": tt, "unit_amount": 2500, "currency": "EUR"})); code != http.StatusCreated {
		t.Fatalf("convert operational hold: %d %s", code, body)
	}
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/operational-holds/%s/release", inventoryURL, op.HoldID), "cov-op-release-"+slot,
		withActor(map[string]any{"organizer_id": organizerID, "quantity": 1})); code != http.StatusOK {
		t.Fatalf("release operational hold: %d %s", code, body)
	}

	// inventory: group reservation drawn down at inventory's own seam.
	code, body = internalJSON(t, http.MethodPost, inventoryURL+"/internal/group-reservations", "cov-grp-place-"+slot,
		withActor(map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 3,
			"counterparty": "Coverage Agency", "expires_at": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)}))
	if code != http.StatusCreated {
		t.Fatalf("place group reservation: %d %s", code, body)
	}
	var grp struct {
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(body, &grp); err != nil {
		t.Fatal(err)
	}
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/group-reservations/%s/draw-down", inventoryURL, grp.HoldID), "cov-grp-draw-"+slot,
		withActor(map[string]any{"organizer_id": organizerID, "slot_id": slot, "quantity": 1, "ticket_type_id": tt, "unit_amount": 2500, "currency": "EUR"})); code != http.StatusCreated {
		t.Fatalf("draw down group reservation: %d %s", code, body)
	}

	// inventory: capacity-adjustment audit read (after one raise).
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/slots/%s/capacity-adjustments", inventoryURL, slot), "cov-adjust-"+slot,
		withActor(map[string]any{"organizer_id": organizerID, "capacity": 25})); code != http.StatusCreated {
		t.Fatalf("adjust capacity: %d %s", code, body)
	}
	if code, body = internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/slots/%s/capacity-adjustments?organizer_id=%s", inventoryURL, slot, organizerID), "", nil); code != http.StatusOK {
		t.Fatalf("capacity adjustment history: %d %s", code, body)
	}

	// commerce: checkout, then the delivery-email internal read for the buyer.
	code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", "cov-reserve-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "cov-order-"+slot,
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Coverage Buyer",
			"email": "coverage@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("checkout: %d %s", code, body)
	}
	var order struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		t.Fatal(err)
	}
	if code, body = internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/buyers/%v/delivery-email", commerceURL, reservation["buyer_id"]), "", nil); code != http.StatusOK {
		t.Fatalf("delivery email: %d %s", code, body)
	}

	// commerce: refund one of the two tickets (TKT-156). Driven here rather than from a
	// fixture of its own because this order is the only completed, captured purchase the
	// suite builds — and a refund needs exactly that. It refunds 1 of 2, so the order stays
	// partially refunded and the two tickets the access assertions below depend on are
	// untouched (voiding them is TKT-157's).
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, order.OrderID), "cov-refund-"+slot,
		map[string]any{"organizer_id": organizerID, "quantity": 1, "actor": "coverage@example.test", "reason": "coverage drive"}); code != http.StatusOK {
		t.Fatalf("refund order: %d %s", code, body)
	}
	var refunded struct {
		RefundStatus     string `json:"refund_status"`
		RefundedQuantity int    `json:"refunded_quantity"`
		Replay           bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &refunded); err != nil {
		t.Fatal(err)
	}
	if refunded.RefundStatus != "partial" || refunded.RefundedQuantity != 1 || refunded.Replay {
		t.Fatalf("refund state = %+v, want partial/1/first-call", refunded)
	}

	// payments: journal fact, direct charge, and the recovery operation read.
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/facts", "",
		map[string]any{"fact_id": uuid.NewString(), "organizer_id": organizerID, "fact_type": "order.created",
			"occurred_at": time.Now().UTC().Format(time.RFC3339), "buyer_id": uuid.NewString(),
			"amount": 1000, "currency": "EUR", "payload": map[string]string{"order_id": uuid.NewString()}}); code != http.StatusOK {
		t.Fatalf("append fact: %d %s", code, body)
	}
	chargeKey := "cov-charge-" + slot
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/charges", chargeKey,
		map[string]any{"order_id": uuid.NewString(), "organizer_id": organizerID, "buyer_id": uuid.NewString(),
			"amount": 1000, "currency": "EUR", "payment_token": "fake-ok"}); code != http.StatusOK {
		t.Fatalf("charge: %d %s", code, body)
	}
	if code, body = internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=%s", paymentsURL, organizerID, chargeKey), "", nil); code != http.StatusOK {
		t.Fatalf("operation lookup: %d %s", code, body)
	}
	// A partial refund leg against that direct charge (TKT-156). Distinct from the
	// commerce refund above, which drives this same operation service-to-service where
	// the smoke client cannot observe it.
	if code, body = internalJSON(t, http.MethodPost, paymentsURL+"/internal/psp/partial-refund", "",
		map[string]any{"organizer_id": organizerID, "idempotency_key": chargeKey,
			"refund_key": "cov-leg-" + slot, "amount": 400, "currency": "EUR"}); code != http.StatusOK {
		t.Fatalf("partial refund leg: %d %s", code, body)
	}

	// access: offline reconciliation on an issued ticket. The live scan (scanTicket)
	// used to be driven here too, purely to make it visible to the coverage gate —
	// checkout_test.go exercised it through the raw postScan helper, which never
	// reached a chokepoint. TKT-95 removed that helper, so the feature test's own
	// scans now record coverage and this duplicate driver is gone. reconcileScans has
	// no other driver and stays.
	var bundle struct {
		Tickets []struct {
			QRPayload string `json:"qr_payload"`
		} `json:"tickets"`
	}
	retry(t, 10*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+order.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 2 {
			return fmt.Errorf("issued tickets = %d, want 2", len(bundle.Tickets))
		}
		return nil
	})
	if code, body = postWithKey(t, gatewayURL+"/api/access/scans/reconciliations", "cov-reconcile-"+slot,
		map[string]any{"occurrences": []map[string]any{{
			"qr_payload":    bundle.Tickets[1].QRPayload,
			"occurrence_id": "cov-occ-" + slot,
			"occurred_at":   time.Now().UTC().Format(time.RFC3339),
		}}}); code != http.StatusOK {
		t.Fatalf("reconcile scans: %d %s", code, body)
	}
}
