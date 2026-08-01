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
	"strings"
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

	// commerce: refund one of the two tickets (TKT-156 money, TKT-157 voiding).
	// Driven here rather than from a fixture of its own because this order is the only
	// completed, captured purchase the suite builds — and a refund needs exactly that.
	//
	// AFTER issuance, deliberately: voiding drives access, and access answers 503 until
	// the tickets exist (the outbox/JetStream path is asynchronous). Refunding earlier
	// would make tickets_voided a race.
	refundKey := "cov-refund-" + slot
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", commerceURL, order.OrderID), refundKey,
		map[string]any{"organizer_id": organizerID, "quantity": 1, "actor": "coverage@example.test", "reason": "coverage drive"}); code != http.StatusOK {
		t.Fatalf("refund order: %d %s", code, body)
	}
	var refunded struct {
		RefundID         string `json:"refund_id"`
		RefundStatus     string `json:"refund_status"`
		RefundedQuantity int    `json:"refunded_quantity"`
		TicketsVoided    bool   `json:"tickets_voided"`
		CapacityReturned bool   `json:"capacity_returned"`
		Replay           bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &refunded); err != nil {
		t.Fatal(err)
	}
	if refunded.RefundStatus != "partial" || refunded.RefundedQuantity != 1 || refunded.Replay {
		t.Fatalf("refund state = %+v, want partial/1/first-call", refunded)
	}
	if !refunded.TicketsVoided {
		t.Fatalf("refund did not void its ticket: %+v", refunded)
	}
	// TKT-161: and the seat goes back on sale. Ordered after voiding by construction —
	// commerce refuses to attempt the return until tickets_voided_at is set, because
	// freeing capacity while the ticket still admits is the one order that can oversell.
	if !refunded.CapacityReturned {
		t.Fatalf("refund did not return its capacity: %+v", refunded)
	}

	// access: replay the voiding directly, which is also what registers refundTickets
	// with the ADR-030 coverage gate — commerce drives it service-to-service, where the
	// smoke client cannot observe it.
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/refunds", accessURL, order.OrderID), "",
		map[string]any{"organizer_id": organizerID, "refund_id": refunded.RefundID, "quantity": 1}); code != http.StatusOK {
		t.Fatalf("replay ticket voiding: %d %s", code, body)
	}
	var voided struct {
		TicketIDs []string `json:"ticket_ids"`
		Replay    bool     `json:"replay"`
	}
	if err := json.Unmarshal(body, &voided); err != nil {
		t.Fatal(err)
	}
	if !voided.Replay || len(voided.TicketIDs) != 1 {
		t.Fatalf("replay must return the same single ticket without re-voiding: %+v", voided)
	}

	// inventory: replay the capacity return directly. Same reason as the access replay —
	// commerce drives this service-to-service, where the smoke client cannot observe it,
	// so the ADR-030 coverage gate needs a direct call.
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/holds/%v/refund-capacity", inventoryURL, reservation["hold_id"]), "",
		map[string]any{"organizer_id": organizerID, "refund_id": refunded.RefundID, "quantity": 1}); code != http.StatusOK {
		t.Fatalf("replay capacity return: %d %s", code, body)
	}
	var returned struct {
		UnreturnedQuantity int  `json:"unreturned_quantity"`
		Replay             bool `json:"replay"`
	}
	if err := json.Unmarshal(body, &returned); err != nil {
		t.Fatal(err)
	}
	if !returned.Replay || returned.UnreturnedQuantity != 1 {
		t.Fatalf("replay must not decrement again: %+v", returned)
	}

	// inventory: the seating lookup an exchange uses to refuse a seated source before any
	// money moves (TKT-158). GA here, so it answers false.
	if code, body = internalJSON(t, http.MethodGet, fmt.Sprintf("%s/internal/holds/%v/seating?organizer_id=%s", inventoryURL, reservation["hold_id"], organizerID), "", nil); code != http.StatusOK {
		t.Fatalf("hold seating: %d %s", code, body)
	}
	var seating struct {
		Seated bool `json:"seated"`
	}
	if err := json.Unmarshal(body, &seating); err != nil {
		t.Fatal(err)
	}
	if seating.Seated {
		t.Fatalf("a GA claim reported as seated: %s", body)
	}

	// commerce: exchange a SECOND completed order onto another ticket type (TKT-158). It
	// needs its own order because the one above is already partly refunded, and an order
	// is reversed once — by a refund or by an exchange, never both.
	code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", "cov-exch-reserve-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("exchange source reserve: %d %s", code, body)
	}
	var exchangeReservation map[string]any
	if err := json.Unmarshal(body, &exchangeReservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "cov-exch-order-"+slot,
		map[string]any{"reservation_id": exchangeReservation["reservation_id"], "name": "Exchange Buyer",
			"email": "exchange@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("exchange source checkout: %d %s", code, body)
	}
	var exchangeSource struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &exchangeSource); err != nil {
		t.Fatal(err)
	}
	// A dearer and a cheaper ticket type on the same performance, so all three money
	// directions are exercised. The first version of this test used the SAME type and
	// asserted delta 0 — which meant both payment branches could be deleted and it still
	// passed (ai-review F5).
	dearerType := created(t, gatewayURL+"/api/catalog/ticket-types", map[string]any{
		"organizer_id": organizerID, "performance_id": slot,
		"name": map[string]string{"fr": "Cher", "en": "Dearer"},
		"price": map[string]any{"amount": 5000, "currency": "EUR"}})
	dearer := fmt.Sprint(dearerType["id"])

	// UPGRADE: exactly the difference is charged, once.
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, exchangeSource.OrderID), "cov-exchange-up-"+slot,
		map[string]any{"organizer_id": organizerID, "target_ticket_type_id": dearer,
			"actor": "coverage@example.test", "reason": "upgrade"}); code != http.StatusOK {
		t.Fatalf("upgrade exchange: %d %s", code, body)
	}
	var upgraded struct {
		ExchangeID       string `json:"exchange_id"`
		DeltaAmount      int64  `json:"delta_amount"`
		SourceTotal      int64  `json:"source_total"`
		TargetTotal      int64  `json:"target_total"`
		Status           string `json:"status"`
		TicketsExchanged bool   `json:"tickets_exchanged"`
		Replay           bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded.DeltaAmount != upgraded.TargetTotal-upgraded.SourceTotal || upgraded.DeltaAmount <= 0 {
		t.Fatalf("upgrade delta = %+v, want a positive target-minus-source", upgraded)
	}
	// PAYMENTS-side evidence, not commerce's own arithmetic (ai-review pass 2, P2-5). The
	// previous assertions read the delta out of commerce's response, so replacing either
	// payment call with a successful no-op left them all green. This asserts the provider
	// operation actually exists, for the exact amount, under the deterministic key.
	assertExchangeCharge(t, organizerID, upgraded.ExchangeID)
	// switch_pending is the point: settled, and the buyer still holds valid OLD tickets
	// until TKT-166 switches them.
	if upgraded.Status != "switch_pending" || upgraded.TicketsExchanged || upgraded.Replay {
		t.Fatalf("exchange state = %+v, want switch_pending on a first call", upgraded)
	}
	// The replay must not settle again.
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, exchangeSource.OrderID), "cov-exchange-up-"+slot,
		map[string]any{"organizer_id": organizerID, "target_ticket_type_id": dearer,
			"actor": "coverage@example.test", "reason": "upgrade"}); code != http.StatusOK {
		t.Fatalf("upgrade exchange replay: %d %s", code, body)
	}
	var upgradeReplay struct {
		DeltaAmount int64 `json:"delta_amount"`
		Replay      bool  `json:"replay"`
	}
	if err := json.Unmarshal(body, &upgradeReplay); err != nil {
		t.Fatal(err)
	}
	if !upgradeReplay.Replay || upgradeReplay.DeltaAmount != upgraded.DeltaAmount {
		t.Fatalf("replay = %+v, want the original delta reported as a replay", upgradeReplay)
	}

	// TKT-166: the switch is ASYNCHRONOUS — commerce owes an `order.exchanged` event, the
	// drainer publishes it, access voids the old tickets and issues the replacement in one
	// transaction, and only then does it call back so the old capacity can be returned.
	// Nothing above proves any of that ran; the settlement response is built before the
	// event is even published.
	if err := poll(30*time.Second, 250*time.Millisecond, func() error {
		code, body := internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, exchangeSource.OrderID), "cov-exchange-up-"+slot,
			map[string]any{"organizer_id": organizerID, "target_ticket_type_id": dearer,
				"actor": "coverage@example.test", "reason": "upgrade"})
		if code != http.StatusOK {
			return fmt.Errorf("exchange state: %d %s", code, body)
		}
		var state struct {
			Status           string `json:"status"`
			TicketsExchanged bool   `json:"tickets_exchanged"`
		}
		if err := json.Unmarshal(body, &state); err != nil {
			return err
		}
		if !state.TicketsExchanged || state.Status != "completed" {
			return fmt.Errorf("exchange is still %s (switched=%t)", state.Status, state.TicketsExchanged)
		}
		return nil
	}); err != nil {
		t.Fatalf("the entitlement never switched: %v", err)
	}

	// And the callback itself, driven directly for its documented 2xx. Deliberately AFTER
	// the wait above: this endpoint exists to be called once the switch has committed, and
	// calling it before would set `tickets_exchanged_at` for a switch that had not
	// happened — inverting in the smoke run the exact ordering it is here to protect. As a
	// replay it must still answer 200, and both halves must report discharged.
	code, body = internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/exchanges/%s/tickets-switched", commerceURL, upgraded.ExchangeID), "",
		map[string]any{"organizer_id": organizerID})
	if code != http.StatusOK {
		t.Fatalf("tickets-switched replay: %d %s", code, body)
	}
	var switched struct {
		TicketsExchanged bool `json:"tickets_exchanged"`
		CapacityReturned bool `json:"capacity_returned"`
	}
	if err := json.Unmarshal(body, &switched); err != nil {
		t.Fatal(err)
	}
	if !switched.TicketsExchanged || !switched.CapacityReturned {
		t.Fatalf("switch callback = %+v, want both halves discharged", switched)
	}

	// ai-review pass 2 F2, verified through the SSR layer rather than asserted about it.
	//
	// The replacement tickets share the SOURCE order's guest reference deliberately, so this
	// one link now carries both the voided originals and the live replacements. Rendering
	// every one of them as an identically numbered QR is how a buyer ends up at the gate
	// finding out which two work. The page must suppress the dead ones — and it must still
	// render, because the code path that decides this only runs for buyers who exchanged.
	if err := poll(20*time.Second, 250*time.Millisecond, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+exchangeSource.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []struct {
				History []struct {
					Type string `json:"type"`
				} `json:"history"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		var voided, live int
		for _, tk := range bundle.Tickets {
			isVoid := false
			for _, e := range tk.History {
				if e.Type == "exchanged" {
					isVoid = true
				}
			}
			if isVoid {
				voided++
			} else {
				live++
			}
		}
		if voided == 0 || live == 0 {
			return fmt.Errorf("bundle has %d voided and %d live tickets, want both under one reference", voided, live)
		}
		// The page itself, server-rendered: it must come back 200 and it must show one QR
		// per LIVE ticket, not one per ticket.
		pageCode, page := get(t, gatewayURL+"/en/tickets/"+exchangeSource.GuestOrderRef, nil)
		if pageCode != http.StatusOK {
			return fmt.Errorf("ticket page %d", pageCode)
		}
		if qrs := strings.Count(string(page), "<img"); qrs != live {
			return fmt.Errorf("ticket page renders %d QR images for %d live and %d voided tickets", qrs, live, voided)
		}
		if !strings.Contains(string(page), "No longer valid") {
			return fmt.Errorf("ticket page does not tell the buyer the exchanged tickets are dead")
		}
		return nil
	}); err != nil {
		t.Fatalf("buyer ticket page after an exchange: %v", err)
	}

	// DOWNGRADE: its own source order, since an order is reversed once. Buying the dearer
	// type and exchanging down to the cheaper one refunds exactly the difference.
	code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", "cov-down-reserve-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": dearer, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("downgrade source reserve: %d %s", code, body)
	}
	var downReservation map[string]any
	if err := json.Unmarshal(body, &downReservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "cov-down-order-"+slot,
		map[string]any{"reservation_id": downReservation["reservation_id"], "name": "Downgrade Buyer",
			"email": "downgrade@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("downgrade source checkout: %d %s", code, body)
	}
	var downSource struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(body, &downSource); err != nil {
		t.Fatal(err)
	}
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, downSource.OrderID), "cov-exchange-down-"+slot,
		map[string]any{"organizer_id": organizerID, "target_ticket_type_id": tt,
			"actor": "coverage@example.test", "reason": "downgrade"}); code != http.StatusOK {
		t.Fatalf("downgrade exchange: %d %s", code, body)
	}
	var downgraded struct {
		ExchangeID  string `json:"exchange_id"`
		DeltaAmount int64  `json:"delta_amount"`
		SourceTotal int64 `json:"source_total"`
		TargetTotal int64 `json:"target_total"`
	}
	if err := json.Unmarshal(body, &downgraded); err != nil {
		t.Fatal(err)
	}
	if downgraded.DeltaAmount != downgraded.TargetTotal-downgraded.SourceTotal || downgraded.DeltaAmount >= 0 {
		t.Fatalf("downgrade delta = %+v, want a negative target-minus-source", downgraded)
	}
	// The opposite leg: a refund of exactly the difference, and NO charge — asserting the
	// wrong branch did not also run is half the point.
	assertNoExchangeCharge(t, organizerID, downgraded.ExchangeID)

	// Same ticket type: an EQUAL exchange, which must settle no money at all and still
	// journal both gross legs.
	code, body = postWithKey(t, gatewayURL+"/api/commerce/reservations", "cov-eq-reserve-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("equal source reserve: %d %s", code, body)
	}
	var eqReservation map[string]any
	if err := json.Unmarshal(body, &eqReservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "cov-eq-order-"+slot,
		map[string]any{"reservation_id": eqReservation["reservation_id"], "name": "Equal Buyer",
			"email": "equal@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("equal source checkout: %d %s", code, body)
	}
	var eqSource struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(body, &eqSource); err != nil {
		t.Fatal(err)
	}
	if code, body = internalJSON(t, http.MethodPost, fmt.Sprintf("%s/internal/orders/%s/exchanges", commerceURL, eqSource.OrderID), "cov-exchange-eq-"+slot,
		map[string]any{"organizer_id": organizerID, "target_ticket_type_id": tt,
			"actor": "coverage@example.test", "reason": "coverage drive"}); code != http.StatusOK {
		t.Fatalf("exchange order: %d %s", code, body)
	}
	var exchanged struct {
		DeltaAmount      int64  `json:"delta_amount"`
		Status           string `json:"status"`
		TicketsExchanged bool   `json:"tickets_exchanged"`
		Replay           bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &exchanged); err != nil {
		t.Fatal(err)
	}
	if exchanged.DeltaAmount != 0 {
		t.Fatalf("an equal exchange settled %d, want 0", exchanged.DeltaAmount)
	}
	// switch_pending is the point: settled, and the buyer still holds valid OLD tickets
	// until TKT-166 switches them.
	if exchanged.Status != "switch_pending" || exchanged.TicketsExchanged || exchanged.Replay {
		t.Fatalf("exchange state = %+v, want switch_pending on a first call", exchanged)
	}
}


// assertExchangeCharge proves the UPGRADE leg reached payments: an operation bound under
// the exchange's deterministic key, captured, for exactly the delta. Reading commerce's
// own response cannot show this — that is the assertion gap ai-review pass 2 found, where
// replacing the payment call with a no-op left every check green.
func assertExchangeCharge(t *testing.T, organizerID, exchangeID string) {
	t.Helper()
	code, body := internalJSON(t, http.MethodGet,
		fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=exchange-charge:%s", paymentsURL, organizerID, exchangeID), "", nil)
	if code != http.StatusOK {
		t.Fatalf("exchange charge operation missing: %d %s", code, body)
	}
	var op struct {
		Resolved bool   `json:"resolved"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	// OperationState deliberately exposes no amount (it is provider-neutral evidence, not
	// a ledger read), so this proves the charge RAN, under the deterministic exchange key,
	// and captured. The amount is separately guaranteed by the database CHECK that delta =
	// target - source, and by payments deriving the charge from the amount commerce sent.
	if !op.Resolved || op.Status != "captured" {
		t.Fatalf("exchange charge = %+v, want a resolved captured operation", op)
	}
}

// assertNoExchangeCharge proves the DOWNGRADE did not also charge. A leg that runs when it
// should not is as wrong as one that does not run when it should.
func assertNoExchangeCharge(t *testing.T, organizerID, exchangeID string) {
	t.Helper()
	code, body := internalJSON(t, http.MethodGet,
		fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=exchange-charge:%s", paymentsURL, organizerID, exchangeID), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("a downgrade created a charge operation: %d %s", code, body)
	}
}
