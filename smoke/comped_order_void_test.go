//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TKT-171: a comped (zero-price) order is REVERSED — its tickets stop admitting
// and its seat comes back — without any money moving.
//
// Through the real stack, because the two things this proves cannot be seen from
// one service. The reversal drives access (voiding) and inventory (capacity)
// service-to-service; the absence of money is a fact about commerce's tables and
// the payments journal. A unit test can see one or the other.
//
// The comped order is built by a zero-price ticket type rather than by writing
// unit_amount directly: the path a real comped ticket takes is a catalog price of
// 0, and a fixture that wrote the column would prove the void works on a state the
// system might never produce.
func TestACompedOrderIsVoidedAndItsSeatComesBack(t *testing.T) {
	ctx := context.Background()
	slot, tt := publishedSlot(t, "Comped Void Hall", 10)

	_, _, _, before := staffAvailability(t, slot)

	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "void-reserve-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("reserve comped: %d %s", code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "void-order-"+slot,
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Comped Buyer",
			"email": "comped@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("checkout comped: %d %s", code, body)
	}
	var order struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		t.Fatal(err)
	}

	// The seat really was taken — otherwise "the seat came back" below could pass
	// against a reservation that never held anything.
	if _, _, _, held := staffAvailability(t, slot); held != before-2 {
		t.Fatalf("available after the comped sale = %d, want %d — the fixture must actually hold a seat", held, before-2)
	}

	// The order is bought at face value and then made comped, rather than being
	// checked out at a zero price.
	//
	// Not a shortcut, and worth stating exactly: a zero-amount CHECKOUT does not
	// currently complete — it answers 202 payment_unknown, because the charge leg
	// does not resolve for a zero total. That is a real, separate limitation of the
	// purchase path, it predates this ticket, and it is filed rather than fixed
	// here (TKT-171 is about REVERSING a comped order, not about creating one).
	//
	// What this ticket needs is an order in the state a comped order occupies —
	// completed, with tickets issued, holding capacity, and unit_amount 0 — which
	// is exactly what the cancellation runner encounters. That state is what is
	// built here. The reversal path reads unit_amount under the order row lock, so
	// this fixture exercises the same predicate a genuinely comped order would.
	com0, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := com0.Exec(ctx, `UPDATE reservations SET unit_amount=0, total_amount=0
		WHERE id=(SELECT reservation_id FROM orders WHERE id=$1)`, order.OrderID); err != nil {
		t.Fatal(err)
	}
	_ = com0.Close(ctx)

	// AFTER issuance, deliberately: voiding drives access, which answers 503 until
	// the tickets exist (its outbox/JetStream path is asynchronous). Voiding earlier
	// would make tickets_voided a race rather than an assertion.
	retry(t, 20*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+order.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []struct {
				QRPayload string `json:"qr_payload"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != 2 {
			return fmt.Errorf("issued tickets = %d, want 2", len(bundle.Tickets))
		}
		return nil
	})

	code, body = internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/voids", commerceURL, order.OrderID), "void-"+slot,
		map[string]any{"organizer_id": organizerID, "actor": "staff:coverage", "reason": "event cancelled"})
	if code != http.StatusOK {
		t.Fatalf("void comped order: %d %s", code, body)
	}
	var voided struct {
		VoidID           string `json:"void_id"`
		Quantity         int    `json:"quantity"`
		TicketsVoided    bool   `json:"tickets_voided"`
		CapacityReturned bool   `json:"capacity_returned"`
		Replay           bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &voided); err != nil {
		t.Fatal(err)
	}
	if voided.Replay {
		t.Error("the first void is not a replay")
	}
	// Whole-order, from the reservation — the request carried no quantity.
	if voided.Quantity != 2 {
		t.Errorf("quantity = %d, want the reservation's 2", voided.Quantity)
	}
	if !voided.TicketsVoided {
		t.Fatalf("the void did not void its tickets: %+v", voided)
	}
	if !voided.CapacityReturned {
		t.Fatalf("the void did not return its capacity: %+v", voided)
	}

	// COS-1, second half, observed at inventory rather than believed from the
	// response: the seat is on sale again.
	if _, _, _, back := staffAvailability(t, slot); back != before {
		t.Fatalf("available after the void = %d, want the pre-sale %d — the seat did not come back", back, before)
	}

	// COS-2, asserted as an ABSOLUTE and derived from the rule ("a void moves
	// tickets and capacity and never money"), not from a count the code produced.
	// Read straight from commerce's tables, because a response that reported no
	// money would be exactly what a bug writing money looks like from outside.
	com, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = com.Close(ctx) }()

	var refunds, facts int
	if err := com.QueryRow(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1`, order.OrderID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 0 {
		t.Fatalf("the void wrote %d order_refunds rows; a void never writes money", refunds)
	}
	// Facts from the PURCHASE are expected — the order really was bought. What must
	// not exist is a REFUND fact, which is the only kind a reversal could add.
	if err := com.QueryRow(ctx,
		`SELECT count(*) FROM order_facts WHERE order_id=$1 AND fact_type LIKE '%refund%'`,
		order.OrderID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Fatalf("the void wrote %d refund facts; ADR-003 — the journal records what happened", facts)
	}
	var refundStatus string
	var refundedQty int
	if err := com.QueryRow(ctx, `SELECT refund_status, refunded_quantity FROM orders WHERE id=$1`, order.OrderID).Scan(&refundStatus, &refundedQty); err != nil {
		t.Fatal(err)
	}
	if refundStatus != "none" || refundedQty != 0 {
		t.Fatalf("the void moved the order's REFUND projection (%s/%d); it must not — nothing was refunded",
			refundStatus, refundedQty)
	}

	// COS-3 at this tier, from the durable record: the markers exist and the
	// ordering constraint that produced them held.
	var ticketsAt, capacityAt *time.Time
	if err := com.QueryRow(ctx, `SELECT tickets_voided_at, capacity_returned_at FROM order_voids WHERE order_id=$1`,
		order.OrderID).Scan(&ticketsAt, &capacityAt); err != nil {
		t.Fatal(err)
	}
	if ticketsAt == nil || capacityAt == nil {
		t.Fatalf("both markers must be recorded: tickets=%v capacity=%v", ticketsAt, capacityAt)
	}
	// Compared, not merely non-null: a fixture asserting non-null stays green
	// through a reordering, which is the one failure this ordering exists to stop.
	if capacityAt.Before(*ticketsAt) {
		t.Fatalf("capacity was returned (%v) before the tickets were voided (%v) — "+
			"the ADR-038 §1 sequence that oversells", capacityAt, ticketsAt)
	}

	// A replay converges on the same void rather than reversing twice — and it
	// arrives under a DIFFERENT idempotency key, which is the case the
	// order-derived identity exists for.
	code, body = internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/voids", commerceURL, order.OrderID), "void-replay-"+slot,
		map[string]any{"organizer_id": organizerID, "actor": "staff:coverage", "reason": "event cancelled"})
	if code != http.StatusOK {
		t.Fatalf("replay void: %d %s", code, body)
	}
	var replayed struct {
		VoidID string `json:"void_id"`
		Replay bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.VoidID != voided.VoidID || !replayed.Replay {
		t.Fatalf("a replay under a different key must converge on the same void: %+v vs %+v", replayed, voided)
	}
	if _, _, _, back := staffAvailability(t, slot); back != before {
		t.Fatalf("available after the replay = %d, want %d — a replay must not return capacity twice", back, before)
	}
}

// COS-5 through the real stack: a PAID order is refused by the void path. It has
// money to return and must go through the refund.
//
// The fixture passes every earlier predicate — completed, unexchanged, this
// organizer's — so the only thing it can fail on is the money check.
func TestAPaidOrderCannotBeVoided(t *testing.T) {
	slot, tt := publishedSlot(t, "Paid Not Voidable Hall", 10)

	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "paid-void-reserve-"+slot,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("reserve paid: %d %s", code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "paid-void-order-"+slot,
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Paying Buyer",
			"email": "paid@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("checkout paid: %d %s", code, body)
	}
	var order struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		t.Fatal(err)
	}

	code, body = internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/orders/%s/voids", commerceURL, order.OrderID), "paid-void-"+slot,
		map[string]any{"organizer_id": organizerID, "actor": "staff:coverage", "reason": "should be refused"})
	if code != http.StatusConflict {
		t.Fatalf("voiding a paid order = %d %s, want 409 — it must go through the refund", code, body)
	}
	// The refusal names the path to take. A status-only assertion would pass on any
	// 409, including one about something else entirely.
	if !strings.Contains(string(body), "must be refunded") {
		t.Fatalf("the refusal must say the order belongs to the refund path: %s", body)
	}
}
